// Package bank keeps the fake operators' wallets and the money moving between them.
// Every movement is a set of entries that adds up to zero, so no franc is created or lost.
package bank

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/chrismoussounda/mbongo/internal/clock"
)

type (
	Operator string
	Kind     string
	Status   string
	Reason   string
)

const (
	MTN    Operator = "mtn"
	Airtel Operator = "airtel"

	Collection Kind = "collection"
	Payout     Kind = "payout"

	Pending    Status = "pending"
	Successful Status = "successful"
	Failed     Status = "failed"
	Expired    Status = "expired"

	ReasonNotEnoughFunds Reason = "not_enough_funds"
	ReasonRefused        Reason = "refused"
)

var (
	ErrExists     = errors.New("bank: account already exists")
	ErrNotFound   = errors.New("bank: not found")
	ErrNoCustomer = errors.New("bank: no such customer")
	ErrNoMerchant = errors.New("bank: no such merchant")
	ErrDuplicate  = errors.New("bank: partner reference already used")
	ErrDecided    = errors.New("bank: transaction already decided")
	ErrCurrency   = errors.New("bank: wrong currency for this account")
	ErrInvalid    = errors.New("bank: invalid request")
)

type Account struct {
	ID       int64
	Operator Operator
	Kind     string
	Name     string
	Currency string
	Balance  int64
}

type Transaction struct {
	ID         string
	Operator   Operator
	Kind       Kind
	MerchantID int64
	CustomerID int64
	Phone      string
	Amount     int64
	Currency   string
	PartnerRef string
	ExternalID string
	Status     Status
	Reason     Reason
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DecidedAt  time.Time
}

// Request asks for money to move. Nothing moves until the transaction is approved.
type Request struct {
	Kind       Kind
	Operator   Operator
	Merchant   string
	Phone      string
	Amount     int64
	Currency   string
	PartnerRef string
	ExternalID string
	// Window is how long the transaction may stay pending. Zero means five minutes.
	Window time.Duration
}

type Bank struct {
	db    *sql.DB
	clock clock.Clock
}

//go:embed schema.sql
var schema string

const schemaVersion = 1

// Open opens or creates the database at path.
func Open(ctx context.Context, path string, c clock.Clock) (*Bank, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises every write, which is all a fake operator needs.
	db.SetMaxOpenConns(1)
	b := &Bank{db: db, clock: c}
	if err := b.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

func (b *Bank) Close() error { return b.db.Close() }

func (b *Bank) Ping(ctx context.Context) error { return b.db.PingContext(ctx) }

func (b *Bank) migrate(ctx context.Context) error {
	var version int
	if err := b.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch version {
	case schemaVersion:
		return nil
	case 0:
		return b.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, schema); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
			return err
		})
	default:
		return fmt.Errorf("bank: database schema %d is newer than this program (%d)", version, schemaVersion)
	}
}

func (b *Bank) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (b *Bank) OpenCustomer(ctx context.Context, op Operator, phone, currency string, balance int64) (Account, error) {
	return b.openAccount(ctx, op, "customer", phone, currency, balance)
}

func (b *Bank) OpenMerchant(ctx context.Context, op Operator, name, currency string, balance int64) (Account, error) {
	return b.openAccount(ctx, op, "merchant", name, currency, balance)
}

func (b *Bank) openAccount(ctx context.Context, op Operator, kind, name, currency string, balance int64) (Account, error) {
	if name == "" || currency == "" || balance < 0 {
		return Account{}, ErrInvalid
	}
	a := Account{Operator: op, Kind: kind, Name: name, Currency: currency}
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"INSERT INTO accounts (operator, kind, name, currency, balance) VALUES (?, ?, ?, ?, 0)",
			op, kind, name, currency)
		if isUnique(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		if a.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if balance == 0 {
			return nil
		}
		mint, err := b.mint(ctx, tx, op, currency)
		if err != nil {
			return err
		}
		a.Balance = balance
		return b.move(ctx, tx, "", mint, a.ID, balance)
	})
	return a, err
}

// mint is where opening balances come from. Its balance is minus all the money in circulation.
func (b *Bank) mint(ctx context.Context, tx *sql.Tx, op Operator, currency string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (operator, kind, name, currency, balance)
		VALUES (?, 'mint', 'mint', ?, 0) ON CONFLICT DO NOTHING`, op, currency); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM accounts WHERE operator = ? AND kind = 'mint' AND currency = ?", op, currency).Scan(&id)
	return id, err
}

func (b *Bank) move(ctx context.Context, tx *sql.Tx, txID string, from, to, amount int64) error {
	at := stamp(b.clock.Now())
	var ref any
	if txID != "" {
		ref = txID
	}
	for _, leg := range []struct{ account, amount int64 }{{from, -amount}, {to, amount}} {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO entries (transaction_id, account_id, amount, at) VALUES (?, ?, ?, ?)",
			ref, leg.account, leg.amount, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE accounts SET balance = balance + ? WHERE id = ?", leg.amount, leg.account); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bank) Account(ctx context.Context, id int64) (Account, error) {
	var a Account
	err := b.db.QueryRowContext(ctx,
		"SELECT id, operator, kind, name, currency, balance FROM accounts WHERE id = ?", id).
		Scan(&a.ID, &a.Operator, &a.Kind, &a.Name, &a.Currency, &a.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// Request records a pending transaction.
func (b *Bank) Request(ctx context.Context, r Request) (Transaction, error) {
	if r.Amount <= 0 || r.PartnerRef == "" || (r.Kind != Collection && r.Kind != Payout) {
		return Transaction{}, ErrInvalid
	}
	if r.Window == 0 {
		r.Window = 5 * time.Minute
	}
	now := b.clock.Now()
	t := Transaction{
		ID: newID(), Operator: r.Operator, Kind: r.Kind, Phone: r.Phone, Amount: r.Amount,
		Currency: r.Currency, PartnerRef: r.PartnerRef, ExternalID: r.ExternalID,
		Status: Pending, CreatedAt: now, ExpiresAt: now.Add(r.Window),
	}
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var merchantCurrency, customerCurrency string
		err := tx.QueryRowContext(ctx,
			"SELECT id, currency FROM accounts WHERE operator = ? AND kind = 'merchant' AND name = ?",
			r.Operator, r.Merchant).Scan(&t.MerchantID, &merchantCurrency)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoMerchant
		}
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx,
			"SELECT id, currency FROM accounts WHERE operator = ? AND kind = 'customer' AND name = ?",
			r.Operator, r.Phone).Scan(&t.CustomerID, &customerCurrency)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCustomer
		}
		if err != nil {
			return err
		}
		if r.Currency != merchantCurrency || r.Currency != customerCurrency {
			return ErrCurrency
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO transactions (id, operator, kind, merchant_id,
			customer_id, amount, currency, partner_ref, external_id, status, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.Operator, t.Kind, t.MerchantID, t.CustomerID, t.Amount, t.Currency,
			t.PartnerRef, t.ExternalID, t.Status, stamp(t.CreatedAt), stamp(t.ExpiresAt))
		if isUnique(err) {
			return ErrDuplicate
		}
		return err
	})
	if err != nil {
		return Transaction{}, err
	}
	return t, nil
}

// Approve moves the money, or fails the transaction when the payer is short.
func (b *Bank) Approve(ctx context.Context, id string) (Transaction, error) {
	var t Transaction
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if t, err = b.pending(ctx, tx, id); err != nil {
			return err
		}
		from, to := t.CustomerID, t.MerchantID
		if t.Kind == Payout {
			from, to = to, from
		}
		var available int64
		if err := tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = ?", from).Scan(&available); err != nil {
			return err
		}
		if available < t.Amount {
			return b.decide(ctx, tx, &t, Failed, ReasonNotEnoughFunds)
		}
		if err := b.move(ctx, tx, t.ID, from, to, t.Amount); err != nil {
			return err
		}
		return b.decide(ctx, tx, &t, Successful, "")
	})
	return t, err
}

// Refuse fails a pending transaction without moving money.
func (b *Bank) Refuse(ctx context.Context, id string, reason Reason) (Transaction, error) {
	var t Transaction
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if t, err = b.pending(ctx, tx, id); err != nil {
			return err
		}
		return b.decide(ctx, tx, &t, Failed, reason)
	})
	return t, err
}

func (b *Bank) pending(ctx context.Context, tx *sql.Tx, id string) (Transaction, error) {
	t, err := b.load(ctx, tx, "t.id = ?", id)
	if err != nil {
		return t, err
	}
	if t.Status != Pending {
		return t, ErrDecided
	}
	return t, nil
}

func (b *Bank) decide(ctx context.Context, tx *sql.Tx, t *Transaction, s Status, r Reason) error {
	t.Status, t.Reason, t.DecidedAt = s, r, b.clock.Now()
	_, err := tx.ExecContext(ctx,
		"UPDATE transactions SET status = ?, reason = ?, decided_at = ? WHERE id = ? AND status = 'pending'",
		s, r, stamp(t.DecidedAt), t.ID)
	return err
}

func (b *Bank) Transaction(ctx context.Context, id string) (Transaction, error) {
	return b.find(ctx, "t.id = ?", id)
}

// ByPartnerRef finds a transaction by the reference the merchant chose.
func (b *Bank) ByPartnerRef(ctx context.Context, op Operator, merchant, ref string) (Transaction, error) {
	return b.find(ctx, "t.operator = ? AND m.name = ? AND t.partner_ref = ?", op, merchant, ref)
}

func (b *Bank) find(ctx context.Context, where string, args ...any) (Transaction, error) {
	var t Transaction
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		t, err = b.load(ctx, tx, where, args...)
		return err
	})
	return t, err
}

// load reads one transaction, expiring it first if its window has passed.
func (b *Bank) load(ctx context.Context, tx *sql.Tx, where string, args ...any) (Transaction, error) {
	var t Transaction
	var created, expires string
	var decided sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT t.id, t.operator, t.kind, t.merchant_id, t.customer_id,
		c.name, t.amount, t.currency, t.partner_ref, t.external_id, t.status, t.reason,
		t.created_at, t.expires_at, t.decided_at
		FROM transactions t JOIN accounts m ON m.id = t.merchant_id JOIN accounts c ON c.id = t.customer_id
		WHERE `+where, args...).Scan(&t.ID, &t.Operator, &t.Kind, &t.MerchantID, &t.CustomerID,
		&t.Phone, &t.Amount, &t.Currency, &t.PartnerRef, &t.ExternalID, &t.Status, &t.Reason,
		&created, &expires, &decided)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.CreatedAt, t.ExpiresAt = parse(created), parse(expires)
	if decided.Valid {
		t.DecidedAt = parse(decided.String)
	}
	if t.Status == Pending && !b.clock.Now().Before(t.ExpiresAt) {
		t.Status, t.DecidedAt = Expired, t.ExpiresAt
		if _, err := tx.ExecContext(ctx,
			"UPDATE transactions SET status = 'expired', decided_at = ? WHERE id = ?",
			stamp(t.DecidedAt), t.ID); err != nil {
			return t, err
		}
	}
	return t, nil
}

// Check verifies the books: each account's balance is the sum of its entries, every
// movement adds up to zero, and only the mint is ever negative.
func (b *Bank) Check(ctx context.Context) error {
	var problems []string
	rows, err := b.db.QueryContext(ctx, `SELECT a.id, a.balance, COALESCE(SUM(e.amount), 0)
		FROM accounts a LEFT JOIN entries e ON e.account_id = a.id GROUP BY a.id
		HAVING a.balance <> COALESCE(SUM(e.amount), 0)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, balance, sum int64
		if err := rows.Scan(&id, &balance, &sum); err != nil {
			_ = rows.Close()
			return err
		}
		problems = append(problems, fmt.Sprintf("account %d: balance %d, entries %d", id, balance, sum))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var unbalanced, total int64
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT transaction_id FROM entries
		WHERE transaction_id IS NOT NULL GROUP BY transaction_id HAVING SUM(amount) <> 0)`).Scan(&unbalanced); err != nil {
		return err
	}
	if unbalanced > 0 {
		problems = append(problems, fmt.Sprintf("%d movements do not add up to zero", unbalanced))
	}
	if err := b.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(amount), 0) FROM entries").Scan(&total); err != nil {
		return err
	}
	if total != 0 {
		problems = append(problems, fmt.Sprintf("entries add up to %d", total))
	}
	if len(problems) > 0 {
		return errors.New("bank: books do not balance: " + strings.Join(problems, "; "))
	}
	return nil
}

func newID() string {
	return rand.Text()
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func isUnique(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
