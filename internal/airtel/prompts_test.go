package airtel_test

import (
	"errors"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/faults"
)

func TestPromptsAreTheWaitingCollections(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{Name: "Boutique Mama"})
	e.customer("055000001", 5000)
	m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	e.clock.Advance(time.Second)
	m.post("/merchant/v1/payments/", collection("055000001", "order-2", 300))
	m.post("/merchant/v1/payments/", collection("055000002", "order-3", 300))

	prompts, err := e.svc.Prompts(e.ctx, "055000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[1].Amount != 300 {
		t.Fatalf("prompts %+v", prompts)
	}
	if all, err := e.svc.Waiting(e.ctx); err != nil || len(all) != 2 || all[1].Amount != 300 {
		t.Fatalf("waiting %v %+v", err, all)
	}
	p := prompts[0]
	if p.Payee != "Boutique Mama" || p.Message != "Order 7" || p.Amount != 1000 || p.Currency != "XAF" ||
		!p.ExpiresAt.Equal(p.CreatedAt.Add(5*time.Minute)) {
		t.Fatalf("first prompt %+v", p)
	}

	if err := e.svc.Answer(e.ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if s := m.get("/standard/v1/payments/order-1"); s.transaction()["status"] != "TS" || e.balanceOf("055000001") != 4000 {
		t.Fatalf("after yes: %v", s.body)
	}
	if err := e.svc.Answer(e.ctx, p.ID, true); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("answer twice: %v", err)
	}
	if err := e.svc.Answer(e.ctx, prompts[1].ID, false); err != nil {
		t.Fatal(err)
	}
	if s := m.get("/standard/v1/payments/order-2"); s.transaction()["status"] != "TF" {
		t.Fatalf("after no: %v", s.body)
	}
	e.waitCallbacks(2)
	if left, _ := e.svc.Prompts(e.ctx, "055000001"); len(left) != 0 {
		t.Fatalf("decided prompts still shown %+v", left)
	}
	e.clock.Advance(5 * time.Minute)
	if left, _ := e.svc.Prompts(e.ctx, "055000002"); len(left) != 0 {
		t.Fatalf("expired prompt still shown %+v", left)
	}
}

func TestCashOutIsNoPrompt(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	e.rule(faults.Rule{Operation: "cashout", Fault: faults.Stuck})
	m.post("/standard/v1/cashout/", collection("055000001", "out-1", 1000))
	if p, _ := e.svc.Prompts(e.ctx, "055000001"); len(p) != 0 {
		t.Fatalf("prompts %+v", p)
	}
	h, err := e.books.History(e.ctx, bank.Airtel, "055000001", 1)
	if err != nil || len(h) != 1 || h[0].Status != bank.Pending {
		t.Fatalf("history %v %+v", err, h)
	}
	if err := e.svc.Answer(e.ctx, h[0].ID, false); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("answering a cash-out: %v", err)
	}
}

func TestDescribeGivesTheAppAndMoneyID(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{Name: "Kiosque"})
	e.customer("055000001", 5000)
	m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	prompts, _ := e.svc.Prompts(e.ctx, "055000001")
	if err := e.svc.Answer(e.ctx, prompts[0].ID, true); err != nil {
		t.Fatal(err)
	}
	e.waitCallbacks(1)
	who, ref, err := e.svc.Describe(e.ctx, prompts[0].Transaction)
	moneyID := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"]
	if err != nil || who != "Kiosque" || ref == "" || ref != moneyID {
		t.Fatalf("describe: %q %q %v (money id %v)", who, ref, err, moneyID)
	}
	if _, _, err := e.svc.Describe(e.ctx, bank.Transaction{ID: "nope"}); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestAutoCustomersMakeAnyNumberAWallet(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	expect(t, m.post("/merchant/v1/payments/", collection("055000009", "order-1", 1000)), "DP00800001010", false)
	if u := m.get("/standard/v1/users/055000008"); u.body["data"].(map[string]any)["msisdn"] != nil {
		t.Fatalf("lookup while off: %v", u.body)
	}
	e.books.AutoCustomers(100_000)
	expect(t, m.post("/merchant/v1/payments/", collection("055000009", "order-2", 1000)), "DP00800001006", true)
	if e.balanceOf("055000009") != 100_000 {
		t.Fatalf("balance %d", e.balanceOf("055000009"))
	}
	if u := m.get("/standard/v1/users/055000008"); u.body["data"].(map[string]any)["msisdn"] != "055000008" {
		t.Fatalf("lookup while on: %v", u.body)
	}
	expect(t, m.post("/merchant/v1/payments/", collection("045000009", "order-3", 100)), "DP00800001006", true)
	for i, n := range []string{"065000009", "242055000009", "05500000", "0550000099"} {
		expect(t, m.post("/merchant/v1/payments/", collection(n, "bad-"+string(rune('a'+i)), 100)), "DP00800001010", false)
		if _, err := e.books.FindAccount(e.ctx, bank.Airtel, bank.CustomerAccount, n, "XAF"); err == nil {
			t.Errorf("%s became a customer", n)
		}
	}
}
