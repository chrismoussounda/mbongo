package mtn_test

import (
	"errors"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

func TestPromptsAreTheWaitingRequestsToPay(t *testing.T) {
	e := setup(t)
	m := e.partnerUser("Boutique Mama", "collection", "mtncongo")
	payouts := e.partnerUser("Boutique Mama", "disbursement", "mtncongo")
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 5_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Fund(e.ctx, payouts.user, 10_000); err != nil {
		t.Fatal(err)
	}
	_ = m.requestToPay(ref1, pay("242061234567", "2500", "XAF"), map[string]string{"X-Callback-Url": "https://shop.example/cb"})
	e.clock.Advance(time.Second)
	_ = m.requestToPay(ref2, pay("242061234567", "100", "XAF"), nil)

	prompts, err := e.svc.Prompts(e.ctx, "242061234567")
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts %+v", prompts)
	}
	p := prompts[0]
	if p.Payee != "Boutique Mama" || p.Message != "Commande 7" || p.Amount != 2_500 || p.Currency != "XAF" ||
		p.Phone != "242061234567" || p.Status != bank.Pending || !p.ExpiresAt.Equal(start.Add(5*time.Minute)) {
		t.Fatalf("first prompt %+v", p)
	}
	if prompts[1].Amount != 100 {
		t.Fatalf("order %+v", prompts)
	}
	if other, _ := e.svc.Prompts(e.ctx, "242069999999"); len(other) != 0 {
		t.Fatalf("another number's prompts %+v", other)
	}
	if all, err := e.svc.Waiting(e.ctx); err != nil || len(all) != 2 || all[0].ID != p.ID {
		t.Fatalf("waiting %v %+v", err, all)
	}

	if err := e.svc.Answer(e.ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if s := m.status(ref1); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("after yes: %v", s.body)
	}
	if got := e.waitCallbacks(1); len(got) != 1 || got[0].body["status"] != "SUCCESSFUL" {
		t.Fatalf("callbacks %+v", got)
	}
	if err := e.svc.Answer(e.ctx, p.ID, false); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("answer twice: %v", err)
	}
	if err := e.svc.Answer(e.ctx, prompts[1].ID, false); err != nil {
		t.Fatal(err)
	}
	if s := m.status(ref2); s.body["status"] != "FAILED" {
		t.Fatalf("after no: %v", s.body)
	}
	if left, _ := e.svc.Prompts(e.ctx, "242061234567"); len(left) != 0 {
		t.Fatalf("decided prompts still shown %+v", left)
	}
}

func TestExpiredRequestIsNoPrompt(t *testing.T) {
	e := setup(t)
	m := e.merchant("mtncongo")
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 5_000); err != nil {
		t.Fatal(err)
	}
	_ = m.requestToPay(ref1, pay("242061234567", "2500", "XAF"), nil)
	prompts, _ := e.svc.Prompts(e.ctx, "242061234567")
	if len(prompts) != 1 {
		t.Fatalf("prompts %+v", prompts)
	}
	e.clock.Advance(5 * time.Minute)
	if left, _ := e.svc.Prompts(e.ctx, "242061234567"); len(left) != 0 {
		t.Fatalf("expired still shown %+v", left)
	}
	if err := e.svc.Answer(e.ctx, prompts[0].ID, true); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve after expiry: %v", err)
	}
}

func TestOnlyRequestsToPayCanBeAnswered(t *testing.T) {
	e := setup(t)
	payouts := e.user("disbursement", "mtncongo")
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Fund(e.ctx, payouts.user, 10_000); err != nil {
		t.Fatal(err)
	}
	a := e.call("POST", "/disbursement/v1_0/transfer", payouts.headers(map[string]string{"X-Reference-Id": ref1}),
		map[string]any{"amount": "100", "currency": "XAF", "externalId": "x",
			"payee": map[string]string{"partyIdType": "MSISDN", "partyId": "242061234567"}, "payerMessage": "", "payeeNote": ""})
	if a.status != 202 {
		t.Fatalf("transfer %d %v", a.status, a.body)
	}
	h, err := e.books.History(e.ctx, bank.MTN, "242061234567", 1)
	if err != nil || len(h) != 1 {
		t.Fatalf("history %v %+v", err, h)
	}
	if err := e.svc.Answer(e.ctx, h[0].ID, false); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("answering a transfer: %v", err)
	}
	if err := e.svc.Answer(e.ctx, "nope", true); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestDescribeGivesThePartnerAndReference(t *testing.T) {
	e := setup(t)
	m := e.partnerUser("Boutique Mama", "collection", "mtncongo")
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 5_000); err != nil {
		t.Fatal(err)
	}
	_ = m.requestToPay(ref1, pay("242061234567", "2500", "XAF"), nil)
	prompts, _ := e.svc.Prompts(e.ctx, "242061234567")
	if err := e.svc.Answer(e.ctx, prompts[0].ID, true); err != nil {
		t.Fatal(err)
	}
	who, ref, err := e.svc.Describe(e.ctx, prompts[0].Transaction)
	status := m.status(ref1)
	if err != nil || who != "Boutique Mama" || ref == "" || status.body["financialTransactionId"] != ref {
		t.Fatalf("describe: %q %q %v, status %v", who, ref, err, status.body)
	}
	if _, _, err := e.svc.Describe(e.ctx, bank.Transaction{ID: "nope"}); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestAutoCustomersMakeAnyNumberAWallet(t *testing.T) {
	e := setup(t)
	m := e.merchant("mtncongo")
	_ = m.requestToPay(ref1, pay("242060000001", "2500", "XAF"), nil)
	if s := m.status(ref1); s.body["status"] != "FAILED" || s.body["reason"] != "PAYER_NOT_FOUND" {
		t.Fatalf("off: %v", s.body)
	}
	e.books.AutoCustomers(100_000)
	_ = m.requestToPay(ref2, pay("242060000001", "2500", "XAF"), nil)
	if s := m.status(ref2); s.body["status"] != "PENDING" {
		t.Fatalf("on: %v", s.body)
	}
	w, _ := e.books.Wallets(e.ctx, bank.MTN, "242060000001")
	if len(w) != 1 || w[0].Balance != 100_000 || w[0].Currency != "XAF" {
		t.Fatalf("wallet %+v", w)
	}
	holder := e.call("GET", "/collection/v1_0/accountholder/msisdn/242060000002/active", m.headers(nil), nil)
	if holder.body["result"] != true {
		t.Fatalf("lookup of a new number: %v", holder.body)
	}
	for i, n := range []string{"055000001", "242050000001", "2420600000011", "060000001"} {
		ref := []string{"7d9a5f6c-3e4f-4a0b-9c2d-3e4f5a6b7c8d", "8e0b6a7d-4f5a-4b1c-8d3e-4f5a6b7c8d9e",
			"9f1c7b8e-5a6b-4c2d-9e4f-5a6b7c8d9e0f", "a02d8c9f-6b7c-4d3e-8f5a-6b7c8d9e0f1a"}[i]
		_ = m.requestToPay(ref, pay(n, "100", "XAF"), nil)
		if s := m.status(ref); s.body["reason"] != "PAYER_NOT_FOUND" {
			t.Errorf("%s is no MTN Congo line but got %v", n, s.body)
		}
		if w, _ := e.books.Wallets(e.ctx, bank.MTN, n); len(w) != 0 {
			t.Errorf("%s became a customer", n)
		}
	}
}
