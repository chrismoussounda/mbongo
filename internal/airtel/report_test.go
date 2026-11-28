package airtel_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/faults"
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
	e.receiver = http.StatusServiceUnavailable
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	_ = m.get("/standard/v1/payments/order-1")
	if err := e.svc.Approve(e.ctx, m.app.ClientID, "order-1"); err != nil {
		t.Fatal(err)
	}
	moneyID, _ := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"].(string)
	_ = m.post("/standard/v1/payments/refund", map[string]any{"transaction": map[string]any{"airtel_money_id": moneyID}})

	calls, cbs := e.recorded()
	if len(calls) != 5 {
		t.Fatalf("calls %+v", calls)
	}
	tok, pay, pending, paid, refund := calls[0], calls[1], calls[2], calls[3], calls[4]
	if tok.Operation != "token" || tok.Client != m.app.ClientID || tok.Token != report.Fingerprint(m.at) {
		t.Fatalf("token %+v", tok)
	}
	if pay.Operation != "collection" || pay.Client != m.app.ClientID || pay.Reference != "order-1" ||
		pay.Order != "Order 7" || pay.Phone != "055000001" || pay.Amount != "1000" || pay.Currency != "XAF" ||
		pay.Token != tok.Token || pay.Outcome != "" || pay.Operator != "airtel" {
		t.Fatalf("collection %+v", pay)
	}
	if pending.Operation != "status" || pending.Reference != "order-1" || pending.Outcome != report.Pending {
		t.Fatalf("pending %+v", pending)
	}
	if paid.Outcome != report.Successful {
		t.Fatalf("paid %+v", paid)
	}
	if refund.Operation != "refund" || refund.RefundOf != moneyID || refund.Client != m.app.ClientID {
		t.Fatalf("refund %+v", refund)
	}
	if len(cbs) != 1 || cbs[0].Client != m.app.ClientID || cbs[0].Reference != "order-1" ||
		cbs[0].Outcome != report.Successful || cbs[0].HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("callbacks %+v", cbs)
	}
}

func TestTheReportKnowsWhoseFaultItWas(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	e.rule(faults.Rule{Operator: "airtel", Operation: "collection", Fault: faults.Maintenance, Times: 1})
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	calls, _ := e.recorded()
	if c := calls[1]; c.Fault != faults.Maintenance || c.Status != http.StatusServiceUnavailable || c.Client != m.app.ClientID {
		t.Fatalf("call %+v", c)
	}
}

func TestAnExpiredTokenStillNamesItsApp(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.clock.Advance(10 * time.Minute)
	if err := e.svc.PurgeTokens(e.ctx); err != nil {
		t.Fatal(err)
	}
	if a := m.get("/standard/v1/payments/order-1"); a.status != 401 {
		t.Fatalf("status %d", a.status)
	}
	calls, _ := e.recorded()
	if c := calls[1]; c.Client != m.app.ClientID || c.Status != 401 {
		t.Fatalf("call %+v", c)
	}
}
