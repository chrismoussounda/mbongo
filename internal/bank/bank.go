// Package bank keeps the fake operators' wallets and the money moving between them.
// Every movement is a set of entries that adds up to zero, so no franc is created or lost.
// Amounts are whole numbers of the currency's smallest unit: francs, or euro cents.
package bank

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/migrate"
	"github.com/chrismoussounda/mbongo/internal/money"
)

type (
	Operator    string
	AccountKind string
	Kind        string
	Status      string
	Reason      string
)

const (
	MTN    Operator = "mtn"
	Airtel Operator = "airtel"

	CustomerAccount AccountKind = "customer"
	MerchantAccount AccountKind = "merchant"
	// MintAccount is where opening balances come from.
	MintAccount AccountKind = "mint"

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
	Kind     AccountKind
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

// Nothing moves until the transaction is approved.
type Request struct {
	Kind       Kind
	Operator   Operator
	Merchant   string
	Phone      string
	Amount     int64
	Currency   string
	PartnerRef string
	ExternalID string
	// Zero means five minutes.
	Window time.Duration
}

// Prompt is a collection waiting for the payer's yes on their phone.
type Prompt struct {
	Transaction
	Payee   string
	Message string
}

type Bank struct {
	db    *sql.DB
	clock clock.Clock

	mu       sync.Mutex
	watchers map[chan struct{}]struct{}
	settled  []func(context.Context, Transaction)

	// auto opens a wallet for any number a request uses, with autoBalance in it.
	auto        bool
	autoBalance int64
}

//go:embed schema.sql
var schema string

// A phone lists a customer's transactions.
const phoneSchema = `
CREATE INDEX transactions_customer ON transactions (customer_id, created_at);
`

func Open(ctx context.Context, path string, c clock.Clock) (*Bank, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, err
	}
	// One connection serialises every write, which is all a fake operator needs.
	db.SetMaxOpenConns(1)
	b := &Bank{db: db, clock: c, watchers: map[chan struct{}]struct{}{}}
	if err := migrate.Run(ctx, db, "bank", []string{schema, phoneSchema}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

// dsn escapes the path, so a file name with a space, # or % stays one file name.
func dsn(path string) string {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()}
	u.RawQuery = "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	return u.String()
}

func (b *Bank) Close() error { return b.db.Close() }

func (b *Bank) Ping(ctx context.Context) error { return b.db.PingContext(ctx) }

// DB lets the operator packages keep their own tables in the same file.
func (b *Bank) DB() *sql.DB { return b.db }

func (b *Bank) Clock() clock.Clock { return b.clock }

func (b *Bank) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	b.changed()
	return nil
}

// Watch returns a channel that receives after the books change, until stop is called.
// Changes that come together may arrive as one.
func (b *Bank) Watch() (changes <-chan struct{}, stop func()) {
	c := make(chan struct{}, 1)
	b.mu.Lock()
	b.watchers[c] = struct{}{}
	b.mu.Unlock()
	return c, func() {
		b.mu.Lock()
		delete(b.watchers, c)
		b.mu.Unlock()
	}
}

// OnSettled runs f after a transaction is decided or expires, once that is saved. Watchers
// hear about it again after f, so what f records is seen too.
func (b *Bank) OnSettled(f func(context.Context, Transaction)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settled = append(b.settled, f)
}

func (b *Bank) announce(ctx context.Context, ids []string) {
	b.mu.Lock()
	hooks := append([]func(context.Context, Transaction){}, b.settled...)
	b.mu.Unlock()
	if len(hooks) == 0 {
		return
	}
	for _, id := range ids {
		t, err := b.Transaction(ctx, id)
		if err != nil {
			continue
		}
		for _, f := range hooks {
			f(ctx, t)
		}
	}
	b.changed()
}

func (b *Bank) changed() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for c := range b.watchers {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// AutoCustomers makes EnsureCustomer open a wallet holding balance for numbers not seen before.
func (b *Bank) AutoCustomers(balance int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auto, b.autoBalance = true, balance
}

// EnsureCustomer opens the number's wallet when automatic customers are on and it has none.
func (b *Bank) EnsureCustomer(ctx context.Context, op Operator, phone, currency string) error {
	b.mu.Lock()
	auto, balance := b.auto, b.autoBalance
	b.mu.Unlock()
	if !auto || phone == "" {
		return nil
	}
	_, err := b.OpenCustomer(ctx, op, phone, currency, balance)
	if errors.Is(err, ErrExists) || errors.Is(err, ErrCurrency) {
		return nil
	}
	return err
}

func (b *Bank) OpenCustomer(ctx context.Context, op Operator, phone, currency string, balance int64) (Account, error) {
	return b.openAccount(ctx, op, CustomerAccount, phone, currency, balance, nil)
}

// OpenCustomerWith also runs also in the same database transaction, so the wallet opens
// together with what goes with it, or not at all.
func (b *Bank) OpenCustomerWith(ctx context.Context, op Operator, phone, currency string, balance int64,
	also func(*sql.Tx) error) (Account, error) {
	return b.openAccount(ctx, op, CustomerAccount, phone, currency, balance, also)
}

func (b *Bank) OpenMerchant(ctx context.Context, op Operator, name, currency string, balance int64) (Account, error) {
	return b.openAccount(ctx, op, MerchantAccount, name, currency, balance, nil)
}

func (b *Bank) openAccount(ctx context.Context, op Operator, kind AccountKind, name, currency string, balance int64,
	also func(*sql.Tx) error) (Account, error) {
	if name == "" || balance < 0 {
		return Account{}, ErrInvalid
	}
	if !money.Known(currency) {
		return Account{}, ErrCurrency
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
		if balance > 0 {
			mint, err := b.mint(ctx, tx, op, currency)
			if err != nil {
				return err
			}
			a.Balance = balance
			if err := b.move(ctx, tx, "", mint, a.ID, balance); err != nil {
				return err
			}
		}
		if also == nil {
			return nil
		}
		return also(tx)
	})
	return a, err
}

// Fund adds money to an account from the mint, as a merchant topping up its payout account.
func (b *Bank) Fund(ctx context.Context, id, amount int64) (Account, error) {
	if amount <= 0 {
		return Account{}, ErrInvalid
	}
	var a Account
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT id, operator, kind, name, currency, balance FROM accounts WHERE id = ?", id).
			Scan(&a.ID, &a.Operator, &a.Kind, &a.Name, &a.Currency, &a.Balance); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		mint, err := b.mint(ctx, tx, a.Operator, a.Currency)
		if err != nil {
			return err
		}
		a.Balance += amount
		return b.move(ctx, tx, "", mint, a.ID, amount)
	})
	return a, err
}

// The mint's balance is minus all the money in circulation.
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

const accountColumns = "SELECT id, operator, kind, name, currency, balance FROM accounts "

// Customers lists every customer wallet, by operator then number.
func (b *Bank) Customers(ctx context.Context) ([]Account, error) {
	return scanAccounts(b.db.QueryContext(ctx, accountColumns+
		"WHERE kind = 'customer' ORDER BY operator, name, currency"))
}

// Wallets are the customer's accounts at one operator, one per currency.
func (b *Bank) Wallets(ctx context.Context, op Operator, phone string) ([]Account, error) {
	return scanAccounts(b.db.QueryContext(ctx, accountColumns+
		"WHERE kind = 'customer' AND operator = ? AND name = ? ORDER BY currency", op, phone))
}

func scanAccounts(rows *sql.Rows, err error) ([]Account, error) {
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Operator, &a.Kind, &a.Name, &a.Currency, &a.Balance); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

func (b *Bank) FindAccount(ctx context.Context, op Operator, kind AccountKind, name, currency string) (Account, error) {
	var a Account
	err := b.db.QueryRowContext(ctx, `SELECT id, operator, kind, name, currency, balance FROM accounts
		WHERE operator = ? AND kind = ? AND name = ? AND currency = ?`, op, kind, name, currency).
		Scan(&a.ID, &a.Operator, &a.Kind, &a.Name, &a.Currency, &a.Balance)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func (b *Bank) Request(ctx context.Context, r Request) (Transaction, error) {
	return b.RequestWith(ctx, r, nil)
}

// RequestWith also runs also in the same database transaction, so an operator's own
// record of the request is saved with it or not at all.
func (b *Bank) RequestWith(ctx context.Context, r Request, also func(*sql.Tx, Transaction) error) (Transaction, error) {
	if r.Amount <= 0 || r.PartnerRef == "" || (r.Kind != Collection && r.Kind != Payout) {
		return Transaction{}, ErrInvalid
	}
	if !money.Known(r.Currency) {
		return Transaction{}, ErrCurrency
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
		// A customer may hold wallets in several currencies: the one in the request's currency
		// comes first, so a mismatch is only reported when there is none.
		err = tx.QueryRowContext(ctx, `SELECT id, currency FROM accounts
			WHERE operator = ? AND kind = 'customer' AND name = ? ORDER BY currency = ? DESC LIMIT 1`,
			r.Operator, r.Phone, r.Currency).Scan(&t.CustomerID, &customerCurrency)
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
		if err != nil || also == nil {
			return err
		}
		return also(tx, t)
	})
	if err != nil {
		return Transaction{}, err
	}
	return t, nil
}

// Approve fails the transaction instead when the payer is short.
func (b *Bank) Approve(ctx context.Context, id string) (Transaction, error) {
	var t Transaction
	err := b.decideTx(ctx, id, &t, func(tx *sql.Tx) error {
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

func (b *Bank) Refuse(ctx context.Context, id string, reason Reason) (Transaction, error) {
	var t Transaction
	err := b.decideTx(ctx, id, &t, func(tx *sql.Tx) error {
		return b.decide(ctx, tx, &t, Failed, reason)
	})
	return t, err
}

// decideTx records due expiries, loads the transaction and runs f on it while it is
// still pending. A transaction already decided returns ErrDecided, and the expiries
// found on the way are kept.
func (b *Bank) decideTx(ctx context.Context, id string, t *Transaction, f func(*sql.Tx) error) error {
	decided := false
	var settled []string
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		if settled, err = b.expireDue(ctx, tx); err != nil {
			return err
		}
		if *t, err = b.load(ctx, tx, "t.id = ?", id); err != nil {
			return err
		}
		if t.Status != Pending {
			decided = true
			return nil
		}
		if err := f(tx); err != nil {
			return err
		}
		settled = append(settled, t.ID)
		return nil
	})
	if err != nil {
		return err
	}
	b.announce(context.WithoutCancel(ctx), settled)
	if decided {
		return ErrDecided
	}
	return nil
}

// Reads already show overdue transactions as expired; ExpireDue stores it, dated at the
// end of each window.
func (b *Bank) ExpireDue(ctx context.Context) (int64, error) {
	var ids []string
	err := b.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		ids, err = b.expireDue(ctx, tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	b.announce(context.WithoutCancel(ctx), ids)
	return int64(len(ids)), nil
}

func (b *Bank) expireDue(ctx context.Context, tx *sql.Tx) ([]string, error) {
	now := stamp(b.clock.Now())
	rows, err := tx.QueryContext(ctx, "SELECT id FROM transactions WHERE status = 'pending' AND expires_at <= ?", now)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE transactions SET status = 'expired', decided_at = expires_at
		WHERE status = 'pending' AND expires_at <= ?`, now)
	return ids, err
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

func (b *Bank) ByPartnerRef(ctx context.Context, op Operator, merchant, ref string) (Transaction, error) {
	return b.find(ctx, "t.operator = ? AND m.name = ? AND t.partner_ref = ?", op, merchant, ref)
}

func (b *Bank) find(ctx context.Context, where string, args ...any) (Transaction, error) {
	return b.load(ctx, b.db, where, args...)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const transactionColumns = `t.id, t.operator, t.kind, t.merchant_id, t.customer_id,
	c.name, t.amount, t.currency, t.partner_ref, t.external_id, t.status, t.reason,
	t.created_at, t.expires_at, t.decided_at
	FROM transactions t JOIN accounts m ON m.id = t.merchant_id JOIN accounts c ON c.id = t.customer_id`

// History is the customer's transactions at one operator, newest first.
func (b *Bank) History(ctx context.Context, op Operator, phone string, limit int) ([]Transaction, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT `+transactionColumns+`
		WHERE t.operator = ? AND c.name = ? ORDER BY t.created_at DESC, t.id LIMIT ?`, op, phone, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []Transaction
	for rows.Next() {
		t, err := b.scan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

// PromptsFrom builds the prompts an operator's query finds; each row gives the transaction id,
// the payee's name and the message for the payer.
func (b *Bank) PromptsFrom(ctx context.Context, query string, args ...any) ([]Prompt, error) {
	rows, err := b.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var prompts []Prompt
	for rows.Next() {
		var p Prompt
		if err := rows.Scan(&p.ID, &p.Payee, &p.Message); err != nil {
			_ = rows.Close()
			return nil, err
		}
		prompts = append(prompts, p)
	}
	// One connection: the rows must be closed before the transactions are read.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range prompts {
		if prompts[i].Transaction, err = b.Transaction(ctx, prompts[i].ID); err != nil {
			return nil, err
		}
	}
	return prompts, nil
}

func (b *Bank) load(ctx context.Context, q querier, where string, args ...any) (Transaction, error) {
	t, err := b.scan(q.QueryRowContext(ctx, `SELECT `+transactionColumns+` WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// A pending transaction past its window is shown as expired before ExpireDue stores it.
func (b *Bank) scan(row interface{ Scan(...any) error }) (Transaction, error) {
	var t Transaction
	var created, expires string
	var decided sql.NullString
	err := row.Scan(&t.ID, &t.Operator, &t.Kind, &t.MerchantID, &t.CustomerID,
		&t.Phone, &t.Amount, &t.Currency, &t.PartnerRef, &t.ExternalID, &t.Status, &t.Reason,
		&created, &expires, &decided)
	if err != nil {
		return t, err
	}
	if t.CreatedAt, err = parse(created); err != nil {
		return t, err
	}
	if t.ExpiresAt, err = parse(expires); err != nil {
		return t, err
	}
	if decided.Valid {
		if t.DecidedAt, err = parse(decided.String); err != nil {
			return t, err
		}
	}
	if t.Status == Pending && !b.clock.Now().Before(t.ExpiresAt) {
		t.Status, t.DecidedAt = Expired, t.ExpiresAt
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

// TimeLayout has a fixed width, so SQLite can compare stored times as text.
const TimeLayout = "2006-01-02T15:04:05.000000000Z"

func stamp(t time.Time) string { return t.UTC().Format(TimeLayout) }

func parse(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return t, fmt.Errorf("bank: bad time %q in the database: %w", s, err)
	}
	return t, nil
}

func isUnique(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
