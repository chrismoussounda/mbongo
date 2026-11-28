package admin_test

import (
	"net/http"
	"testing"
)

// unconfirmedPayment has an Airtel app without a callback URL take a payment it never checks.
func (e *env) unconfirmedPayment() string {
	e.t.Helper()
	_, app := e.admin("/_mbongo/airtel/apps", map[string]string{"name": "Acme"})
	client := app["clientId"].(string)
	_, _ = e.admin("/_mbongo/customers", map[string]string{"operator": "airtel", "phone": "055000001", "currency": "XAF", "balance": "1000"})
	_, tok := e.call(http.MethodPost, "/auth/oauth2/token", nil, map[string]string{
		"client_id": client, "client_secret": app["clientSecret"].(string), "grant_type": "client_credentials"})
	headers := map[string]string{"Authorization": "Bearer " + tok["access_token"].(string), "X-Country": "CG", "X-Currency": "XAF"}
	_, _ = e.call(http.MethodPost, "/merchant/v1/payments/", headers, map[string]any{"reference": "r",
		"subscriber": map[string]string{"msisdn": "055000001"}, "transaction": map[string]any{"amount": 400, "id": "order-1"}})
	if status, _ := e.admin("/_mbongo/airtel/"+client+"/payments/order-1/approve", nil); status != 204 {
		e.t.Fatalf("approve: %d", status)
	}
	return client
}

func TestTheReportThroughTheAdmin(t *testing.T) {
	e := setup(t, adminToken)
	client := e.unconfirmedPayment()

	status, out := e.adminCall(http.MethodGet, "/_mbongo/report", nil)
	s, _ := out.(map[string]any)
	if status != 200 || s["errors"] != 1.0 || s["warnings"] != 0.0 || len(s["limits"].([]any)) == 0 {
		t.Fatalf("report: %d %v", status, out)
	}
	clients := s["clients"].([]any)
	first := clients[0].(map[string]any)
	finding := first["findings"].([]any)[0].(map[string]any)
	if len(clients) != 1 || first["client"] != client || first["operator"] != "airtel" ||
		finding["check"] != "money_unconfirmed" || finding["severity"] != "error" || finding["reference"] != "order-1" {
		t.Fatalf("clients: %v", clients)
	}
	if run := s["run"].(map[string]any); run["id"] != 1.0 || run["startedAt"] == "" {
		t.Fatalf("run: %v", run)
	}
	if _, out := e.adminCall(http.MethodGet, "/_mbongo/report?operator=mtn", nil); len(out.(map[string]any)["clients"].([]any)) != 0 {
		t.Fatalf("mtn only: %v", out)
	}
	if _, out := e.adminCall(http.MethodGet, "/_mbongo/report?client="+client, nil); out.(map[string]any)["errors"] != 1.0 {
		t.Fatalf("one client: %v", out)
	}
	if _, out := e.adminCall(http.MethodGet, "/_mbongo/report?client=nobody", nil); len(out.(map[string]any)["clients"].([]any)) != 0 {
		t.Fatalf("another client: %v", out)
	}

	status, out = e.adminCall(http.MethodPost, "/_mbongo/report/runs", nil)
	if status != 201 || out.(map[string]any)["id"] != 2.0 {
		t.Fatalf("new run: %d %v", status, out)
	}
	if _, out := e.adminCall(http.MethodGet, "/_mbongo/report", nil); out.(map[string]any)["errors"] != 0.0 ||
		out.(map[string]any)["run"].(map[string]any)["id"] != 2.0 {
		t.Fatalf("new run report: %v", out)
	}
	if _, out := e.adminCall(http.MethodGet, "/_mbongo/report?run=1", nil); out.(map[string]any)["errors"] != 1.0 {
		t.Fatalf("old run: %v", out)
	}
	if status, out := e.adminCall(http.MethodGet, "/_mbongo/report/runs", nil); status != 200 || len(out.([]any)) != 2 {
		t.Fatalf("runs: %d %v", status, out)
	}
	for path, want := range map[string]int{"/_mbongo/report?run=9": 404, "/_mbongo/report?run=x": 400, "/_mbongo/report?run=0": 400} {
		if status, _ := e.adminCall(http.MethodGet, path, nil); status != want {
			t.Fatalf("%s: %d", path, status)
		}
	}
	if status, _ := e.call(http.MethodGet, "/_mbongo/report", nil, nil); status != 401 {
		t.Fatalf("without the token: %d", status)
	}
	if status, _ := e.call(http.MethodPost, "/_mbongo/report/runs", nil, nil); status != 401 {
		t.Fatalf("new run without the token: %d", status)
	}
}
