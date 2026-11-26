package bank_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
)

var start = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

type fixture struct {
	t     *testing.T
	ctx   context.Context
	bank  *bank.Bank
	clock *clock.Manual
	path  string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), clock: clock.NewManual(start),
		path: filepath.Join(t.TempDir(), "mbongo.db")}
	f.bank = f.open()
	t.Cleanup(func() { _ = f.bank.Close() })
	return f
}

func (f *fixture) open() *bank.Bank {
	f.t.Helper()
	b, err := bank.Open(f.ctx, f.path, f.clock)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) customer(phone string, balance int64) bank.Account {
	f.t.Helper()
	a, err := f.bank.OpenCustomer(f.ctx, bank.MTN, phone, "XAF", balance)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) merchant(name string, balance int64) bank.Account {
	f.t.Helper()
	a, err := f.bank.OpenMerchant(f.ctx, bank.MTN, name, "XAF", balance)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) balance(a bank.Account) int64 {
	f.t.Helper()
	got, err := f.bank.Account(f.ctx, a.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got.Balance
}

func (f *fixture) check() {
	f.t.Helper()
	if err := f.bank.Check(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) request(kind bank.Kind, ref string, amount int64) (bank.Transaction, error) {
	return f.bank.Request(f.ctx, bank.Request{
		Kind: kind, Operator: bank.MTN, Merchant: "shop", Phone: "242061234567",
		Amount: amount, Currency: "XAF", PartnerRef: ref, ExternalID: "order-1",
		Window: 5 * time.Minute,
	})
}

func TestOpeningBalancesComeFromSomewhere(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 10_000)
	m := f.merchant("shop", 0)
	if f.balance(c) != 10_000 || f.balance(m) != 0 {
		t.Fatalf("balances %d %d", f.balance(c), f.balance(m))
	}
	if c.Kind != bank.CustomerAccount || m.Kind != bank.MerchantAccount {
		t.Fatalf("kinds %s %s", c.Kind, m.Kind)
	}
	f.check()
}

func TestSameAccountTwiceIsRefused(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 0)
	_, err := f.bank.OpenCustomer(f.ctx, bank.MTN, "242061234567", "XAF", 0)
	if !errors.Is(err, bank.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if _, err := f.bank.OpenCustomer(f.ctx, bank.Airtel, "242061234567", "XAF", 0); err != nil {
		t.Fatalf("same number at Airtel: %v", err)
	}
}

func TestCollectionMovesNothingUntilApproved(t *testing.T) {
	f := setup(t)
	c, m := f.customer("242061234567", 10_000), f.merchant("shop", 0)

	tx, err := f.request(bank.Collection, "ref-1", 2_500)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != bank.Pending || f.balance(c) != 10_000 {
		t.Fatalf("status %s, customer %d", tx.Status, f.balance(c))
	}

	tx, err = f.bank.Approve(f.ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != bank.Successful || f.balance(c) != 7_500 || f.balance(m) != 2_500 {
		t.Fatalf("status %s, customer %d, merchant %d", tx.Status, f.balance(c), f.balance(m))
	}
	f.check()
}

func TestCollectionFailsWithoutFunds(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 1_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)

	tx, err := f.bank.Approve(f.ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != bank.Failed || tx.Reason != bank.ReasonNotEnoughFunds || f.balance(c) != 1_000 {
		t.Fatalf("%+v, customer %d", tx, f.balance(c))
	}
	f.check()
}

func TestPayoutMovesMoneyFromTheMerchant(t *testing.T) {
	f := setup(t)
	c, m := f.customer("242061234567", 0), f.merchant("shop", 5_000)
	tx, _ := f.request(bank.Payout, "ref-1", 3_000)

	tx, err := f.bank.Approve(f.ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != bank.Successful || f.balance(c) != 3_000 || f.balance(m) != 2_000 {
		t.Fatalf("status %s, customer %d, merchant %d", tx.Status, f.balance(c), f.balance(m))
	}

	tx2, _ := f.request(bank.Payout, "ref-2", 3_000)
	tx2, _ = f.bank.Approve(f.ctx, tx2.ID)
	if tx2.Status != bank.Failed || tx2.Reason != bank.ReasonNotEnoughFunds {
		t.Fatalf("second payout %+v", tx2)
	}
	f.check()
}

func TestRefusedCollectionMovesNothing(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)

	tx, err := f.bank.Refuse(f.ctx, tx.ID, bank.ReasonRefused)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != bank.Failed || tx.Reason != bank.ReasonRefused || f.balance(c) != 10_000 {
		t.Fatalf("%+v", tx)
	}
}

func TestDecidedTransactionCannotBeDecidedAgain(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	_, _ = f.bank.Approve(f.ctx, tx.ID)

	if _, err := f.bank.Approve(f.ctx, tx.ID); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve twice: %v", err)
	}
	if _, err := f.bank.Refuse(f.ctx, tx.ID, bank.ReasonRefused); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("refuse after approve: %v", err)
	}
	f.check()
}

func TestPendingCollectionExpires(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	untouched, _ := f.request(bank.Collection, "ref-2", 100)

	f.clock.Advance(5*time.Minute - time.Second)
	if got, _ := f.bank.Transaction(f.ctx, tx.ID); got.Status != bank.Pending {
		t.Fatalf("expired early: %s", got.Status)
	}
	f.clock.Advance(time.Second)
	got, err := f.bank.Transaction(f.ctx, tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != bank.Expired || !got.DecidedAt.Equal(start.Add(5*time.Minute)) {
		t.Fatalf("%+v", got)
	}
	if _, err := f.bank.Approve(f.ctx, tx.ID); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve after expiry: %v", err)
	}
	if f.balance(c) != 10_000 {
		t.Fatal("expiry moved money")
	}
	f.clock.Advance(time.Hour)
	if late, _ := f.bank.Transaction(f.ctx, untouched.ID); !late.DecidedAt.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("expired at %v, want the end of its window", late.DecidedAt)
	}
}

func TestPartnerRefIsUniquePerMerchant(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	first, _ := f.request(bank.Collection, "ref-1", 2_500)

	_, err := f.request(bank.Collection, "ref-1", 9_999)
	if !errors.Is(err, bank.ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
	found, err := f.bank.ByPartnerRef(f.ctx, bank.MTN, "shop", "ref-1")
	if err != nil || found.ID != first.ID || found.Amount != 2_500 {
		t.Fatalf("found %+v, %v", found, err)
	}
}

func TestUnknownCustomerAndMerchant(t *testing.T) {
	f := setup(t)
	f.merchant("shop", 0)
	if _, err := f.request(bank.Collection, "ref-1", 100); !errors.Is(err, bank.ErrNoCustomer) {
		t.Fatalf("err = %v, want ErrNoCustomer", err)
	}
	f.customer("242061234567", 0)
	_, err := f.bank.Request(f.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN,
		Merchant: "nobody", Phone: "242061234567", Amount: 1, Currency: "XAF", PartnerRef: "r"})
	if !errors.Is(err, bank.ErrNoMerchant) {
		t.Fatalf("err = %v, want ErrNoMerchant", err)
	}
	if _, err := f.bank.Transaction(f.ctx, "missing"); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRequestIsValidated(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	for _, amount := range []int64{0, -5} {
		if _, err := f.request(bank.Collection, "r", amount); !errors.Is(err, bank.ErrInvalid) {
			t.Errorf("amount %d: %v", amount, err)
		}
	}
	_, err := f.bank.Request(f.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN,
		Merchant: "shop", Phone: "242061234567", Amount: 1, Currency: "EUR", PartnerRef: "r"})
	if !errors.Is(err, bank.ErrCurrency) {
		t.Fatalf("err = %v, want ErrCurrency", err)
	}
	if _, err := f.bank.OpenCustomer(f.ctx, bank.MTN, "242069999999", "EUR", 100); err != nil {
		t.Fatal(err)
	}
	_, err = f.bank.Request(f.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN,
		Merchant: "shop", Phone: "242069999999", Amount: 1, Currency: "XAF", PartnerRef: "r"})
	if !errors.Is(err, bank.ErrCurrency) {
		t.Fatalf("customer in EUR: err = %v, want ErrCurrency", err)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)
	_, _ = f.bank.Approve(f.ctx, tx.ID)
	if err := f.bank.Close(); err != nil {
		t.Fatal(err)
	}

	f.bank = f.open()
	if f.balance(c) != 7_500 {
		t.Fatalf("balance after restart %d", f.balance(c))
	}
	got, err := f.bank.Transaction(f.ctx, tx.ID)
	if err != nil || got.Status != bank.Successful || got.ExternalID != "order-1" {
		t.Fatalf("%+v, %v", got, err)
	}
	f.check()
}
