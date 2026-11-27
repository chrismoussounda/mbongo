package airtel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/migrate"
)

const schema = `
CREATE TABLE airtel_apps (
    client_id     TEXT PRIMARY KEY,
    client_secret TEXT NOT NULL,
    name          TEXT NOT NULL,
    callback_url  TEXT NOT NULL,
    callback_key  TEXT NOT NULL,
    pin           TEXT NOT NULL
);
CREATE TABLE airtel_tokens (
    token      TEXT PRIMARY KEY,
    client_id  TEXT NOT NULL REFERENCES airtel_apps (client_id),
    expires_at TEXT NOT NULL
);
CREATE TABLE airtel_keys (
    key_id      INTEGER PRIMARY KEY,
    private_der BLOB NOT NULL,
    valid_upto  TEXT NOT NULL
);
CREATE TABLE airtel_payments (
    client_id       TEXT NOT NULL REFERENCES airtel_apps (client_id),
    family          TEXT NOT NULL CHECK (family IN ('payment', 'disbursement', 'refund')),
    id              TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('collection', 'cashout', 'cashin', 'disbursement', 'refund')),
    transaction_id  TEXT NOT NULL REFERENCES transactions (id),
    msisdn          TEXT NOT NULL,
    amount          INTEGER NOT NULL,
    reference       TEXT NOT NULL,
    airtel_money_id TEXT NOT NULL UNIQUE,
    refund_of       TEXT NOT NULL DEFAULT '',
    notified        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (client_id, family, id)
);
CREATE INDEX airtel_refunds ON airtel_payments (refund_of) WHERE refund_of != '';
`

var (
	ErrNotFound    = errors.New("airtel: not found")
	errDuplicate   = errors.New("airtel: transaction id already used")
	errMoneyIDUsed = errors.New("airtel: airtel_money_id already given")
	errRefunded    = errors.New("airtel: already refunded")
)

// family is the set of transactions whose ids share one status path, so must not repeat:
// /payments/{id} answers for collections, cash-in and cash-out alike.
func family(kind string) string {
	switch kind {
	case kindDisbursement, kindRefund:
		return kind
	}
	return "payment"
}

type store struct {
	db    *sql.DB
	books *bank.Bank
}

func newStore(ctx context.Context, books *bank.Bank) (*store, error) {
	if err := migrate.Run(ctx, books.DB(), "airtel", []string{schema}); err != nil {
		return nil, err
	}
	return &store{db: books.DB(), books: books}, nil
}

func (s *store) addApp(ctx context.Context, app App) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO airtel_apps (client_id, client_secret, name, callback_url, callback_key, pin)
		VALUES (?, ?, ?, ?, ?, ?)`, app.ClientID, app.ClientSecret, app.Name, app.CallbackURL, app.CallbackKey, app.PIN)
	if err != nil {
		return err
	}
	_, err = s.books.OpenMerchant(ctx, bank.Airtel, app.ClientID, currency, 0)
	return err
}

func (s *store) app(ctx context.Context, clientID string) (App, error) {
	var a App
	err := s.db.QueryRowContext(ctx, `SELECT client_id, client_secret, name, callback_url, callback_key, pin
		FROM airtel_apps WHERE client_id = ?`, clientID).
		Scan(&a.ClientID, &a.ClientSecret, &a.Name, &a.CallbackURL, &a.CallbackKey, &a.PIN)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *store) addToken(ctx context.Context, clientID string, expires time.Time) (string, error) {
	token := randomHex() + randomHex()
	_, err := s.db.ExecContext(ctx, "INSERT INTO airtel_tokens (token, client_id, expires_at) VALUES (?, ?, ?)",
		token, clientID, expires.UTC().Format(bank.TimeLayout))
	return token, err
}

// tokenOwner finds the app a token belongs to, if the token is still alive at now.
func (s *store) tokenOwner(ctx context.Context, token string, now time.Time) (string, error) {
	var clientID string
	err := s.db.QueryRowContext(ctx, "SELECT client_id FROM airtel_tokens WHERE token = ? AND expires_at > ?",
		token, now.UTC().Format(bank.TimeLayout)).Scan(&clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return clientID, err
}

func (s *store) purgeTokens(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM airtel_tokens WHERE expires_at <= ?", now.UTC().Format(bank.TimeLayout))
	return err
}

type payment struct {
	ClientID      string
	ID            string
	Kind          string
	TransactionID string
	MSISDN        string
	Amount        int64
	Reference     string
	AirtelMoneyID string
	RefundOf      string
}

const paymentColumns = "client_id, id, kind, transaction_id, msisdn, amount, reference, airtel_money_id, refund_of"

func (p *payment) fields() []any {
	return []any{&p.ClientID, &p.ID, &p.Kind, &p.TransactionID, &p.MSISDN, &p.Amount, &p.Reference,
		&p.AirtelMoneyID, &p.RefundOf}
}

func insertPayment(ctx context.Context, tx *sql.Tx, p payment) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO airtel_payments (family, "+paymentColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		family(p.Kind), p.ClientID, p.ID, p.Kind, p.TransactionID, p.MSISDN, p.Amount, p.Reference, p.AirtelMoneyID, p.RefundOf)
	switch {
	case !isUnique(err):
		return err
	case strings.Contains(err.Error(), "airtel_money_id"):
		return errMoneyIDUsed
	}
	return errDuplicate
}

// refunded reports whether a refund of moneyID is paid or under way.
func refunded(ctx context.Context, tx *sql.Tx, clientID, moneyID string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM airtel_payments p JOIN transactions t ON t.id = p.transaction_id
		WHERE p.client_id = ? AND p.refund_of = ? AND t.status != 'failed'`, clientID, moneyID).Scan(&n)
	return n > 0, err
}

// payment finds one of the app's transactions by its id, among kinds of one family.
func (s *store) payment(ctx context.Context, clientID, id string, kinds ...string) (payment, error) {
	return s.paymentWhere(ctx, "client_id = ? AND id = ?", []any{clientID, id}, kinds)
}

func (s *store) paymentByMoneyID(ctx context.Context, clientID, moneyID string, kinds ...string) (payment, error) {
	return s.paymentWhere(ctx, "client_id = ? AND airtel_money_id = ?", []any{clientID, moneyID}, kinds)
}

func (s *store) paymentWhere(ctx context.Context, where string, args []any, kinds []string) (payment, error) {
	in := strings.TrimSuffix(strings.Repeat("?, ", len(kinds)), ", ")
	for _, k := range kinds {
		args = append(args, k)
	}
	var p payment
	err := s.db.QueryRowContext(ctx, "SELECT "+paymentColumns+" FROM airtel_payments WHERE "+where+
		" AND kind IN ("+in+")", args...).Scan(p.fields()...)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// markNotified reports whether this call is the one that should send the callback.
func (s *store) markNotified(ctx context.Context, clientID, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE airtel_payments SET notified = 1
		WHERE client_id = ? AND family = 'payment' AND id = ? AND notified = 0`, clientID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func isUnique(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
