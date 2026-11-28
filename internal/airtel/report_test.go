package airtel_test

import (
	"bytes"
	"encoding/json"
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
		pay.Order != "" || pay.Phone != "055000001" || pay.Amount != "1000" || pay.Currency != "XAF" ||
		pay.Token != tok.Token || pay.Outcome != report.Pending || pay.Operator != "airtel" {
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
	if len(cbs) != 1 || cbs[0].Client != m.app.ClientID || cbs[0].Reference != "order-1" || cbs[0].Operation != "collection" ||
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

func TestTheLedgerTellsOutcomesAndRefundTargets(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	if got, err := e.svc.Outcome(e.ctx, m.app.ClientID, "order-1", "collection"); err != nil || got != report.Pending {
		t.Fatalf("pending %q %v", got, err)
	}
	if err := e.svc.Approve(e.ctx, m.app.ClientID, "order-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.Outcome(e.ctx, m.app.ClientID, "order-1", "collection"); err != nil || got != report.Successful {
		t.Fatalf("paid %q %v", got, err)
	}
	if got, err := e.svc.Outcome(e.ctx, m.app.ClientID, "order-1", "disbursement"); err != nil || got != "" {
		t.Fatalf("other family %q %v", got, err)
	}
	moneyID, _ := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"].(string)
	target, found, err := e.svc.RefundTarget(e.ctx, m.app.ClientID, moneyID)
	if err != nil || !found || target != (report.Target{Reference: "order-1", Operation: "collection", Amount: "1000", Currency: "XAF"}) {
		t.Fatalf("target %+v %v %v", target, found, err)
	}
	if name, err := e.svc.ClientName(e.ctx, m.app.ClientID); err != nil || name != "Acme" {
		t.Fatalf("name %q %v", name, err)
	}
	if name, err := e.svc.ClientName(e.ctx, "nobody"); err != nil || name != "" {
		t.Fatalf("unknown %q %v", name, err)
	}
	if _, found, err := e.svc.RefundTarget(e.ctx, m.app.ClientID, "MP0"); err != nil || found {
		t.Fatalf("unknown found: %v %v", found, err)
	}

	refund := m.post("/standard/v1/payments/refund", map[string]any{"transaction": map[string]any{"airtel_money_id": moneyID}})
	refundID, _ := refund.transaction()["airtel_money_id"].(string)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	calls, _ := e.recorded()
	byOp := map[string]report.Call{}
	for _, c := range calls {
		byOp[c.Operation] = c
	}
	if c := byOp["refund"]; refundID == "" || c.Reference != refundID || c.RefundOf != moneyID {
		t.Fatalf("refund %+v, id %q", c, refundID)
	}
	if got, err := e.svc.Outcome(e.ctx, m.app.ClientID, refundID, "refund"); err != nil || got != report.Successful {
		t.Fatalf("refund outcome %q %v", got, err)
	}
	if target, found, err := e.svc.RefundTarget(e.ctx, m.app.ClientID, refundID); err != nil || !found ||
		target.Operation != "refund" {
		t.Fatalf("a refund as target %+v %v %v", target, found, err)
	}
	if c := byOp["collection"]; c.Outcome != report.Refused {
		t.Fatalf("duplicate %+v", c)
	}
}

func TestAnswersThatSettleAtOnceTellTheApp(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	if _, err := e.svc.Fund(e.ctx, m.app.ClientID, 10_000); err != nil {
		t.Fatal(err)
	}
	_ = m.post("/standard/v1/disbursements/", payout(m, "055000001", "d-1", 500))
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	_ = e.svc.Approve(e.ctx, m.app.ClientID, "order-1")
	moneyID, _ := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"].(string)
	e.rule(faults.Rule{Operator: "airtel", Operation: "refund", Fault: faults.AnswerLost, Times: 1})
	raw, _ := json.Marshal(map[string]any{"transaction": map[string]any{"airtel_money_id": moneyID}})
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, e.server.URL+"/standard/v1/payments/refund", bytes.NewReader(raw))
	for k, v := range m.headers() {
		req.Header.Set(k, v)
	}
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the refund's answer was not lost")
	}
	_ = e.token(m.app.ClientID, new(string))
	calls, _ := e.recorded()
	byOp := map[string]report.Call{}
	for _, c := range calls {
		byOp[c.Operation] = c
	}
	if c := byOp["disbursement"]; c.Outcome != report.Successful {
		t.Fatalf("disbursement %+v", c)
	}
	refund := byOp["refund"]
	if refund.Status != 0 || refund.Reference == "" {
		t.Fatalf("lost refund %+v", refund)
	}
	if got, _ := e.svc.Outcome(e.ctx, m.app.ClientID, refund.Reference, "refund"); got != report.Successful {
		t.Fatalf("the lost refund's reference is not the refund: %q", got)
	}
	if c := calls[len(calls)-1]; c.Operation != "token" || c.Client != "" {
		t.Fatalf("refused token %+v", c)
	}
}

func TestAnswersStillInProgressArePending(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	if _, err := e.svc.Fund(e.ctx, m.app.ClientID, 10_000); err != nil {
		t.Fatal(err)
	}
	out := payout(m, "055000001", "d-1", 500)
	in := map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
		"transaction": map[string]string{"amount": "700", "id": "in-1"}, "reference": "r", "pin": m.encryptedPIN("1234")}
	e.rule(faults.Rule{Operator: "airtel", Operation: "disbursement", Fault: faults.Ambiguous, Times: 1})
	e.rule(faults.Rule{Operator: "airtel", Operation: "cashin", Fault: faults.Ambiguous, Times: 1})
	_ = m.post("/standard/v1/disbursements/", out)
	_ = m.post("/standard/v1/cashin/", in)
	calls, _ := e.recorded()
	for _, c := range calls[len(calls)-2:] {
		if c.Fault != faults.Ambiguous || c.Outcome != report.Pending {
			t.Fatalf("%s %+v", c.Operation, c)
		}
	}
}
