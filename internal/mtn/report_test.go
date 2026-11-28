package mtn_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/mtn"
	"github.com/chrismoussounda/mbongo/internal/report"
)

func (e *env) recorded() ([]report.Call, []report.Callback) {
	e.t.Helper()
	e.svc.Wait()
	calls, err := e.journal.Calls(e.ctx, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	cbs, err := e.journal.Callbacks(e.ctx, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	return calls, cbs
}

func TestTheReportSeesCallsAndCallbacks(t *testing.T) {
	e := setup(t)
	e.receiver = http.StatusInternalServerError
	m := e.merchant(mtn.Sandbox)
	if a := m.requestToPay(ref1, pay("46733123453", "10", "EUR"), map[string]string{"X-Callback-Url": "https://acme.example/cb"}); a.status != 202 {
		t.Fatalf("request to pay: %d", a.status)
	}
	if a := m.status(ref1); a.status != 200 {
		t.Fatalf("status: %d", a.status)
	}
	calls, cbs := e.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls %+v", calls)
	}
	tok, pay, st := calls[0], calls[1], calls[2]
	if tok.Operation != "token" || tok.Client != m.user || tok.Token != report.Fingerprint(m.at) || tok.Status != 200 {
		t.Fatalf("token %+v", tok)
	}
	if pay.Operation != "requesttopay" || pay.Client != m.user || pay.Reference != ref1 || pay.Order != "order-7" ||
		pay.Phone != "46733123453" || pay.Amount != "10" || pay.Currency != "EUR" || pay.Token != tok.Token ||
		pay.Status != 202 || pay.Operator != "mtn" {
		t.Fatalf("request to pay %+v", pay)
	}
	if st.Operation != "status" || st.Reference != ref1 || st.Outcome != report.Successful || st.Client != m.user {
		t.Fatalf("status %+v", st)
	}
	if len(cbs) != 1 || cbs[0].Client != m.user || cbs[0].Reference != ref1 || cbs[0].Outcome != report.Successful ||
		cbs[0].HTTPStatus != http.StatusInternalServerError || cbs[0].Fault != "" {
		t.Fatalf("callbacks %+v", cbs)
	}
}

func TestTheReportKnowsWhoseFaultItWas(t *testing.T) {
	e := setup(t)
	m := e.merchant(mtn.Sandbox)
	e.rule(faults.Rule{Operator: "mtn", Operation: "requesttopay", Fault: faults.RateLimited, Times: 1})
	e.rule(faults.Rule{Operator: "mtn", Fault: faults.CallbackTwice, Times: 1})
	_ = m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil)
	const ref2 = "6c8f4e5b-2d3e-4f9a-8b1c-2d3e4f5a6b7c"
	_ = m.requestToPay(ref2, pay("46733123453", "10", "EUR"), map[string]string{"X-Callback-Url": "https://acme.example/cb"})
	calls, cbs := e.recorded()
	if c := calls[1]; c.Fault != faults.RateLimited || c.Status != http.StatusTooManyRequests || c.Client != m.user {
		t.Fatalf("limited call %+v", c)
	}
	if c := calls[2]; c.Fault != "" || c.Status != 202 {
		t.Fatalf("next call %+v", c)
	}
	if len(cbs) != 2 || cbs[0].Fault != faults.CallbackTwice || cbs[1].Fault != faults.CallbackTwice {
		t.Fatalf("callbacks %+v", cbs)
	}
}

func TestAnExpiredTokenStillNamesItsUser(t *testing.T) {
	e := setup(t)
	m := e.merchant(mtn.Sandbox)
	e.clock.Advance(2 * time.Hour)
	if err := e.svc.PurgeTokens(e.ctx); err != nil {
		t.Fatal(err)
	}
	if a := m.status(ref1); a.status != 401 {
		t.Fatalf("status %d", a.status)
	}
	calls, _ := e.recorded()
	if c := calls[1]; c.Client != m.user || c.Token != report.Fingerprint(m.at) || c.Status != 401 {
		t.Fatalf("call %+v", c)
	}
}
