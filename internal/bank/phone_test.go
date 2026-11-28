package bank_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

func TestWalletsAndCustomers(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 100)
	if _, err := f.bank.OpenCustomer(f.ctx, bank.MTN, "242061234567", "EUR", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.OpenCustomer(f.ctx, bank.Airtel, "055123456", "XAF", 0); err != nil {
		t.Fatal(err)
	}
	f.merchant("shop", 0)

	w, err := f.bank.Wallets(f.ctx, bank.MTN, "242061234567")
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 || w[0].Currency != "EUR" || w[0].Balance != 7 || w[1].Currency != "XAF" || w[1].Balance != 100 {
		t.Fatalf("wallets %+v", w)
	}
	if w, _ := f.bank.Wallets(f.ctx, bank.Airtel, "242061234567"); len(w) != 0 {
		t.Fatalf("other operator's wallets %+v", w)
	}
	all, err := f.bank.Customers(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Operator != bank.Airtel || all[0].Name != "055123456" {
		t.Fatalf("customers %+v", all)
	}
	for _, a := range all {
		if a.Kind != bank.CustomerAccount {
			t.Fatalf("not a customer: %+v", a)
		}
	}
}

func TestHistoryNewestFirst(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 10_000)
	first, err := f.request(bank.Collection, "ref-1", 100)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	second, err := f.request(bank.Payout, "ref-2", 200)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.OpenCustomer(f.ctx, bank.MTN, "242069999999", "XAF", 0); err != nil {
		t.Fatal(err)
	}

	h, err := f.bank.History(f.ctx, bank.MTN, "242061234567", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].ID != second.ID || h[1].ID != first.ID || h[0].Phone != "242061234567" {
		t.Fatalf("history %+v", h)
	}
	if h, _ := f.bank.History(f.ctx, bank.MTN, "242061234567", 1); len(h) != 1 || h[0].ID != second.ID {
		t.Fatalf("limit: %+v", h)
	}
	f.clock.Advance(5 * time.Minute)
	h, _ = f.bank.History(f.ctx, bank.MTN, "242061234567", 10)
	if h[1].Status != bank.Expired {
		t.Fatalf("overdue shown as %s", h[1].Status)
	}
	if h, _ := f.bank.History(f.ctx, bank.Airtel, "242061234567", 10); len(h) != 0 {
		t.Fatalf("other operator's history %+v", h)
	}
}

func TestWatchHearsChanges(t *testing.T) {
	f := setup(t)
	changes, stop := f.bank.Watch()
	f.customer("242061234567", 0)
	select {
	case <-changes:
	default:
		t.Fatal("no change after opening a wallet")
	}
	f.merchant("shop", 0)
	f.merchant("shop2", 0)
	<-changes
	select {
	case <-changes:
		t.Fatal("changes should come together")
	default:
	}
	if _, err := f.bank.Request(f.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN, Merchant: "nobody",
		Phone: "242061234567", Amount: 1, Currency: "XAF", PartnerRef: "r"}); !errors.Is(err, bank.ErrNoMerchant) {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-changes:
		t.Fatal("change after a failed request")
	default:
	}
	stop()
	f.merchant("shop3", 0)
	select {
	case <-changes:
		t.Fatal("change after stop")
	default:
	}
}

func TestOnSettledHearsEachOutcomeOnce(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	var got []string
	f.bank.OnSettled(func(_ context.Context, tx bank.Transaction) {
		got = append(got, tx.PartnerRef+":"+string(tx.Status))
	})
	paid, _ := f.request(bank.Collection, "paid", 100)
	refused, _ := f.request(bank.Collection, "refused", 100)
	late, _ := f.request(bank.Collection, "late", 100)
	if _, err := f.bank.Approve(f.ctx, paid.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.Refuse(f.ctx, refused.ID, bank.ReasonRefused); err != nil {
		t.Fatal(err)
	}
	if _, err := f.bank.Approve(f.ctx, paid.ID); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve twice: %v", err)
	}
	f.clock.Advance(5 * time.Minute)
	if _, err := f.bank.Approve(f.ctx, late.ID); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve late: %v", err)
	}
	if n, err := f.bank.ExpireDue(f.ctx); err != nil || n != 0 {
		t.Fatalf("expire again: %d %v", n, err)
	}
	want := "paid:successful refused:failed late:expired"
	if strings.Join(got, " ") != want {
		t.Fatalf("settled %v, want %s", got, want)
	}
}

func TestExpireDueAnnouncesEachExpiry(t *testing.T) {
	f := setup(t)
	f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	var got []string
	f.bank.OnSettled(func(_ context.Context, tx bank.Transaction) { got = append(got, tx.PartnerRef) })
	_, _ = f.request(bank.Collection, "a", 100)
	_, _ = f.request(bank.Collection, "b", 100)
	f.clock.Advance(5 * time.Minute)
	if n, err := f.bank.ExpireDue(f.ctx); err != nil || n != 2 || len(got) != 2 {
		t.Fatalf("expire: %d %v %v", n, err, got)
	}
}

func TestEnsureCustomerOnlyWhenAsked(t *testing.T) {
	f := setup(t)
	if err := f.bank.EnsureCustomer(f.ctx, bank.MTN, "242061234567", "XAF"); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.bank.Wallets(f.ctx, bank.MTN, "242061234567"); len(w) != 0 {
		t.Fatalf("opened while off: %+v", w)
	}
	f.bank.AutoCustomers(100_000)
	for range 2 {
		if err := f.bank.EnsureCustomer(f.ctx, bank.MTN, "242061234567", "XAF"); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := f.bank.Wallets(f.ctx, bank.MTN, "242061234567")
	if len(w) != 1 || w[0].Balance != 100_000 {
		t.Fatalf("wallets %+v", w)
	}
	if err := f.bank.EnsureCustomer(f.ctx, bank.Airtel, "", "XAF"); err != nil {
		t.Fatal(err)
	}
	if err := f.bank.EnsureCustomer(f.ctx, bank.Airtel, "055000001", "ZZZ"); err != nil {
		t.Fatalf("unknown currency: %v", err)
	}
	if all, _ := f.bank.Customers(f.ctx); len(all) != 1 {
		t.Fatalf("customers %+v", all)
	}
	f.check()
}

func TestOpenCustomerWithKeepsNothingWhenTheExtraStepFails(t *testing.T) {
	f := setup(t)
	boom := errors.New("boom")
	if _, err := f.bank.OpenCustomerWith(f.ctx, bank.MTN, "242061234567", "XAF", 500,
		func(*sql.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if w, _ := f.bank.Wallets(f.ctx, bank.MTN, "242061234567"); len(w) != 0 {
		t.Fatalf("wallet kept: %+v", w)
	}
	ran := false
	a, err := f.bank.OpenCustomerWith(f.ctx, bank.MTN, "242061234567", "XAF", 500, func(*sql.Tx) error { ran = true; return nil })
	if err != nil || !ran || a.Balance != 500 {
		t.Fatalf("open: %+v %v %v", a, err, ran)
	}
	f.check()
}
