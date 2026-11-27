package mtn_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/faults"
)

const ref4 = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"

func (e *env) rule(r faults.Rule) {
	e.t.Helper()
	r.Operator = "mtn"
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

// raw sends a request and reports a broken connection instead of failing the test.
func (e *env) raw(method, path string, headers map[string]string, body any) error {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(e.ctx, method, e.server.URL+path, bytes.NewReader(raw))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = res.Body.Close()
	}
	return err
}

func TestRefusalsLeaveNoPayment(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	for _, c := range []struct {
		fault  string
		status int
	}{{faults.RateLimited, 429}, {faults.Maintenance, 503}, {faults.TokenRevoked, 401}} {
		e.rule(faults.Rule{Operation: "requesttopay", Fault: c.fault})
		if a := m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil); a.status != c.status {
			t.Fatalf("%s: %d %v", c.fault, a.status, a.body)
		}
		if s := m.status(ref1); s.status != 404 {
			t.Fatalf("%s left a payment behind: %d", c.fault, s.status)
		}
	}
	if a := m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil); a.status != 202 {
		t.Fatalf("after the rules ran out: %d", a.status)
	}
}

func TestLostAnswerStillMovesTheMoney(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	e.rule(faults.Rule{Operation: "transfer", Phone: "46733123453", Fault: faults.AnswerLost})
	if err := e.raw(http.MethodPost, "/disbursement/v1_0/transfer", d.headers(map[string]string{"X-Reference-Id": ref1}),
		send("46733123453", "10", "EUR")); err == nil {
		t.Fatal("the connection should have been dropped")
	}
	if s := d.get("/disbursement/v1_0/transfer/" + ref1); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("the transfer should have gone through: %v", s.body)
	}
	if a := d.post("transfer", ref1, send("46733123453", "10", "EUR")); a.status != 409 {
		t.Fatalf("a retry with the same reference must not pay twice: %d", a.status)
	}
}

func TestErrorAfterTheMoneyMoved(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	e.rule(faults.Rule{Operation: "deposit", Fault: faults.ErrorAfterMoney})
	if a := d.post("deposit", ref1, send("46733123453", "10", "EUR")); a.status != 500 || a.body["code"] != "INTERNAL_PROCESSING_ERROR" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if s := d.get("/disbursement/v1_0/deposit/" + ref1); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("%v", s.body)
	}
}

func TestSlowAnswer(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	e.rule(faults.Rule{Operation: "status", Fault: faults.Slow, Delay: 200 * time.Millisecond})
	_ = m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil)
	began := time.Now()
	if s := m.status(ref1); s.status != 200 || time.Since(began) < 200*time.Millisecond {
		t.Fatalf("%d after %v", s.status, time.Since(began))
	}
}

func TestStuckPaymentNeverEnds(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	e.rule(faults.Rule{Phone: "46733123453", Fault: faults.Stuck})
	_ = m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil)
	e.clock.Advance(48 * time.Hour)
	if _, err := e.books.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	m.at = m.token().body["access_token"].(string)
	if s := m.status(ref1); s.body["status"] != "PENDING" {
		t.Fatalf("%v", s.body)
	}
}

func TestCallbackRules(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	cb := map[string]string{"X-Callback-Url": "https://shop.example/cb"}

	e.rule(faults.Rule{Operation: faults.Callback, Fault: faults.CallbackNever})
	_ = m.requestToPay(ref1, pay("46733123453", "10", "EUR"), cb)
	if n := e.callbackCount(); n != 0 {
		t.Fatalf("never: %d callbacks", n)
	}

	e.rule(faults.Rule{Operation: faults.Callback, Fault: faults.CallbackTwice})
	_ = m.requestToPay(ref2, pay("46733123453", "10", "EUR"), cb)
	if n := e.callbackCount(); n != 2 {
		t.Fatalf("twice: %d callbacks", n)
	}

	e.rule(faults.Rule{Operation: faults.Callback, Fault: faults.CallbackLate, Delay: time.Minute})
	_ = m.requestToPay(ref3, pay("46733123453", "10", "EUR"), cb)
	if n := e.callbackCount(); n != 2 {
		t.Fatalf("late, before its time: %d callbacks", n)
	}
	e.clock.Advance(time.Minute)
	if got := e.waitCallbacks(3); len(got) != 3 {
		t.Fatalf("late, after its time: %d callbacks", len(got))
	}

	e.rule(faults.Rule{Operation: faults.Callback, Fault: faults.CallbackBeforeAnswer})
	e.slowReceiver = 200 * time.Millisecond
	_ = m.requestToPay(ref4, pay("46733123453", "10", "EUR"), cb)
	e.mu.Lock()
	n := len(e.callbacks)
	e.mu.Unlock()
	if n != 4 {
		t.Fatalf("before the answer: %d callbacks when the answer came", n)
	}
}
