package airtel_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/faults"
)

func (e *env) rule(r faults.Rule) {
	e.t.Helper()
	r.Operator = "airtel"
	if r.Times == 0 {
		r.Times = 1
	}
	if _, err := e.rules.Add(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) callbackCount() int {
	e.svc.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.callbacks)
}

func payout(m *merchant, msisdn, id string, amount int) map[string]any {
	return map[string]any{"payee": map[string]string{"msisdn": msisdn}, "reference": "r", "pin": m.encryptedPIN("1234"),
		"transaction": map[string]any{"amount": amount, "id": id}}
}

func TestAmbiguousPayoutShowsTAThenTheOutcome(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 0)
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 5000)
	e.rule(faults.Rule{Operation: "disbursement", Fault: faults.Ambiguous})

	a := m.post("/standard/v1/disbursements/", payout(m, "055000001", "pay-1", 1000))
	expect(t, a, "DP00900001000", true)
	if a.transaction()["status"] != "TA" || e.balanceOf("055000001") != 1000 {
		t.Fatalf("the money moves, the answer says ambiguous: %v", a.body)
	}
	if s := m.get("/standard/v1/disbursements/pay-1"); s.transaction()["status"] != "TA" {
		t.Fatalf("%v", s.body)
	}
	e.clock.Advance(3 * time.Minute)
	m.refresh()
	if s := m.get("/standard/v1/disbursements/pay-1"); s.transaction()["status"] != "TS" {
		t.Fatalf("after three minutes: %v", s.body)
	}
}

func TestAmbiguousCashOutCallbackWaits(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 500)
	e.rule(faults.Rule{Operation: "cashout", Fault: faults.Ambiguous, Delay: time.Minute})
	a := m.post("/standard/v1/cashout/", map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
		"transaction": map[string]string{"amount": "100", "id": "out-1"}, "reference": "r"})
	if a.data()["status"] != "TA" {
		t.Fatalf("%v", a.body)
	}
	if n := e.callbackCount(); n != 0 {
		t.Fatalf("callback during the ambiguity: %d", n)
	}
	e.clock.Advance(time.Minute)
	cbs := e.waitCallbacks(1)
	if tx := cbs[0].body["transaction"].(map[string]any); tx["status_code"] != "TS" {
		t.Fatalf("%v", cbs[0].body)
	}
}

func TestStuckCashInStaysInProgress(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 0)
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 5000)
	e.rule(faults.Rule{Phone: "055000001", Fault: faults.Stuck})
	a := m.post("/standard/v1/cashin/", map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
		"transaction": map[string]string{"amount": "700", "id": "in-1"}, "reference": "r", "pin": m.encryptedPIN("1234")})
	expect(t, a, "DP01000001006", true)
	e.clock.Advance(48 * time.Hour)
	if _, err := e.books.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	if s := m.get("/standard/v1/payments/in-1"); s.transaction()["status"] != "TIP" || e.balanceOf("055000001") != 0 {
		t.Fatalf("%v", s.body)
	}
}

func TestAirtelRefusalsAndLostAnswer(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 500)
	e.rule(faults.Rule{Operation: "collection", Fault: faults.RateLimited})
	if a := m.post("/merchant/v1/payments/", collection("055000001", "order-1", 100)); a.status != 429 {
		t.Fatalf("%d %v", a.status, a.body)
	}
	e.rule(faults.Rule{Operation: "balance", Fault: faults.TokenRevoked})
	if a := m.get("/standard/v1/users/balance"); a.status != 401 || a.body["error"] != "invalid_token" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	e.rule(faults.Rule{Operation: "collection", Fault: faults.AnswerLost})
	raw, _ := json.Marshal(collection("055000001", "order-1", 100))
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.server.URL+"/merchant/v1/payments/", bytes.NewReader(raw))
	for k, v := range m.headers() {
		req.Header.Set(k, v)
	}
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the connection should have been dropped")
	}
	if s := m.get("/standard/v1/payments/order-1"); s.transaction()["status"] != "TIP" {
		t.Fatalf("the payment was made all the same: %v", s.body)
	}
}
