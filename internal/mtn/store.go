package mtn

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/migrate"
)

const schema = `
CREATE TABLE mtn_subscriptions (
    key     TEXT PRIMARY KEY,
    product TEXT NOT NULL CHECK (product IN ('collection', 'disbursement'))
);
CREATE TABLE mtn_api_users (
    id            TEXT PRIMARY KEY,
    subscription  TEXT NOT NULL REFERENCES mtn_subscriptions (key),
    callback_host TEXT NOT NULL,
    environment   TEXT NOT NULL CHECK (environment IN ('sandbox', 'mtncongo')),
    api_key       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE mtn_tokens (
    token      TEXT PRIMARY KEY,
    api_user   TEXT NOT NULL REFERENCES mtn_api_users (id),
    product    TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE TABLE mtn_requests (
    api_user      TEXT NOT NULL REFERENCES mtn_api_users (id),
    reference_id  TEXT NOT NULL,
    product       TEXT NOT NULL,
    transaction_id TEXT REFERENCES transactions (id),
    party_id      TEXT NOT NULL,
    amount        TEXT NOT NULL,
    currency      TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    payer_message TEXT NOT NULL,
    payee_note    TEXT NOT NULL,
    callback_url  TEXT NOT NULL,
    failure       TEXT NOT NULL DEFAULT '',
    notified      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (api_user, reference_id)
);
`

const (
	Sandbox  = "sandbox"
	MTNCongo = "mtncongo"
)

var (
	ErrNotFound            = errors.New("mtn: no such request")
	ErrUnknownProduct      = errors.New("mtn: product must be collection")
	ErrUnknownEnvironment  = errors.New("mtn: targetEnvironment must be sandbox or mtncongo")
	ErrUnknownSubscription = errors.New("mtn: unknown subscription key")
)

var (
	errDuplicate = errors.New("mtn: reference already used")
)

type apiUser struct {
	ID           string
	Subscription string
	CallbackHost string
	Environment  string
	APIKey       string
}

// currency is the only currency each environment takes.
func currency(environment string) string {
	if environment == Sandbox {
		return "EUR"
	}
	return "XAF"
}

type store struct {
	db    *sql.DB
	books *bank.Bank
}

func newStore(ctx context.Context, books *bank.Bank) (*store, error) {
	if err := migrate.Run(ctx, books.DB(), "mtn", []string{schema}); err != nil {
		return nil, err
	}
	return &store{db: books.DB(), books: books}, nil
}

func (s *store) addSubscription(ctx context.Context, product string) (string, error) {
	key := randomHex()
	_, err := s.db.ExecContext(ctx, "INSERT INTO mtn_subscriptions (key, product) VALUES (?, ?)", key, product)
	return key, err
}

func (s *store) subscriptionProduct(ctx context.Context, key string) (string, error) {
	var product string
	err := s.db.QueryRowContext(ctx, "SELECT product FROM mtn_subscriptions WHERE key = ?", key).Scan(&product)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return product, err
}

// addAPIUser creates the user and the merchant wallet its money lands in.
func (s *store) addAPIUser(ctx context.Context, u apiUser) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO mtn_api_users (id, subscription, callback_host, environment, api_key)
		VALUES (?, ?, ?, ?, ?)`, u.ID, u.Subscription, u.CallbackHost, u.Environment, u.APIKey); err != nil {
		return err
	}
	_, err := s.books.OpenMerchant(ctx, bank.MTN, u.ID, currency(u.Environment), 0)
	return err
}

func (s *store) apiUser(ctx context.Context, id string) (apiUser, error) {
	var u apiUser
	err := s.db.QueryRowContext(ctx, `SELECT id, subscription, callback_host, environment, api_key
		FROM mtn_api_users WHERE id = ?`, id).Scan(&u.ID, &u.Subscription, &u.CallbackHost, &u.Environment, &u.APIKey)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *store) setAPIKey(ctx context.Context, id, key string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE mtn_api_users SET api_key = ? WHERE id = ?", key, id)
	return err
}

func (s *store) addToken(ctx context.Context, user, product string, expires time.Time) (string, error) {
	token := randomHex() + randomHex()
	_, err := s.db.ExecContext(ctx, "INSERT INTO mtn_tokens (token, api_user, product, expires_at) VALUES (?, ?, ?, ?)",
		token, user, product, expires.UTC().Format(bank.TimeLayout))
	return token, err
}

func (s *store) token(ctx context.Context, token string) (user, product string, expires time.Time, err error) {
	var at string
	err = s.db.QueryRowContext(ctx, "SELECT api_user, product, expires_at FROM mtn_tokens WHERE token = ?", token).
		Scan(&user, &product, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	expires, err = time.Parse(bank.TimeLayout, at)
	return user, product, expires, err
}

type request struct {
	APIUser       string
	ReferenceID   string
	Product       string
	TransactionID sql.NullString
	PartyID       string
	Amount        string
	Currency      string
	ExternalID    string
	PayerMessage  string
	PayeeNote     string
	CallbackURL   string
	Failure       string
}

func (s *store) request(ctx context.Context, user, ref string) (request, error) {
	var r request
	err := s.db.QueryRowContext(ctx, `SELECT api_user, reference_id, product, transaction_id, party_id, amount,
		currency, external_id, payer_message, payee_note, callback_url, failure
		FROM mtn_requests WHERE api_user = ? AND reference_id = ?`, user, ref).Scan(&r.APIUser, &r.ReferenceID,
		&r.Product, &r.TransactionID, &r.PartyID, &r.Amount, &r.Currency, &r.ExternalID, &r.PayerMessage,
		&r.PayeeNote, &r.CallbackURL, &r.Failure)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *store) insertRequest(ctx context.Context, r request) error {
	return insertRequest(ctx, s.db, r)
}

func insertRequest(ctx context.Context, db execer, r request) error {
	_, err := db.ExecContext(ctx, `INSERT INTO mtn_requests (api_user, reference_id, product, transaction_id,
		party_id, amount, currency, external_id, payer_message, payee_note, callback_url, failure)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.APIUser, r.ReferenceID, r.Product, r.TransactionID, r.PartyID, r.Amount, r.Currency, r.ExternalID,
		r.PayerMessage, r.PayeeNote, r.CallbackURL, r.Failure)
	if err != nil && isUnique(err) {
		return errDuplicate
	}
	return err
}

func (s *store) merchant(ctx context.Context, user string) (bank.Account, error) {
	u, err := s.apiUser(ctx, user)
	if err != nil {
		return bank.Account{}, err
	}
	return s.books.FindAccount(ctx, bank.MTN, bank.MerchantAccount, user, currency(u.Environment))
}

func (s *store) hasCustomer(ctx context.Context, phone, cur string) (bool, error) {
	_, err := s.books.FindAccount(ctx, bank.MTN, bank.CustomerAccount, phone, cur)
	if errors.Is(err, bank.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func isUnique(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

func (s *store) purgeTokens(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM mtn_tokens WHERE expires_at <= ?", now.UTC().Format(bank.TimeLayout))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// markNotified reports whether this call is the one that should send the callback.
func (s *store) markNotified(ctx context.Context, user, ref string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE mtn_requests SET notified = 1
		WHERE api_user = ? AND reference_id = ? AND notified = 0`, user, ref)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
