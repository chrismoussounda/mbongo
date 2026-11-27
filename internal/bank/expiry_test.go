package bank_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

func (f *fixture) raw(query string, args ...any) string {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var s string
	if err := db.QueryRowContext(f.ctx, query, args...).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) storedStatus(id string) string {
	return f.raw("SELECT status FROM transactions WHERE id = ?", id)
}

func TestReadingDoesNotWrite(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	f.clock.Advance(time.Hour)

	got, _ := f.bank.Transaction(f.ctx, tx.ID)
	if got.Status != bank.Expired || !got.DecidedAt.Equal(tx.ExpiresAt) {
		t.Fatalf("shown as %s at %v", got.Status, got.DecidedAt)
	}
	if s := f.storedStatus(tx.ID); s != "pending" {
		t.Fatalf("a read stored %s", s)
	}
}

func TestExpireDueRecordsTheEndOfTheWindow(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	due, _ := f.request(bank.Collection, "ref-1", 2_500)
	f.clock.Advance(4 * time.Minute)
	later, _ := f.request(bank.Collection, "ref-2", 100)

	// Half a second past the end: a time stored without its fraction must still compare right.
	f.clock.Advance(time.Minute + 500*time.Millisecond)
	n, err := f.bank.ExpireDue(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || f.storedStatus(due.ID) != "expired" || f.storedStatus(later.ID) != "pending" {
		t.Fatalf("expired %d; stored %s and %s", n, f.storedStatus(due.ID), f.storedStatus(later.ID))
	}
	got, _ := f.bank.Transaction(f.ctx, due.ID)
	if !got.DecidedAt.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("decided at %v", got.DecidedAt)
	}
}

func TestApprovingAnExpiredPaymentRecordsTheExpiry(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	f.clock.Advance(time.Hour)

	if _, err := f.bank.Approve(f.ctx, tx.ID); err == nil {
		t.Fatal("approved an expired payment")
	}
	if s := f.storedStatus(tx.ID); s != "expired" {
		t.Fatalf("stored %s", s)
	}
}

func TestBadTimeInTheDatabaseIsAnError(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	_ = f.bank.Close()
	db, err := sql.Open("sqlite", "file:"+f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(f.ctx, "UPDATE transactions SET created_at = 'yesterday'"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	f.bank = f.open()

	if _, err := f.bank.Transaction(f.ctx, tx.ID); err == nil {
		t.Fatal("a broken date was read without an error")
	}
}

func TestExpireDueAtTheExactEnd(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	_, _ = f.request(bank.Collection, "ref-1", 2_500)
	f.clock.Advance(5 * time.Minute)
	if n, err := f.bank.ExpireDue(f.ctx); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
}
