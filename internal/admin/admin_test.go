package admin_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/admin"
	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/mtn"
	"github.com/chrismoussounda/mbongo/internal/phone"
	"github.com/chrismoussounda/mbongo/internal/report"
)

const adminToken = "letmein"

type env struct {
	t      *testing.T
	server *httptest.Server
	books  *bank.Bank
	phones *phone.Service
}

func setup(t *testing.T, token string) *env {
	t.Helper()
	ctx := context.Background()
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "m.db"), clock.NewManual(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := mtn.New(ctx, books, logger, func(context.Context, string, []byte) (int, error) { return 200, nil })
	if err != nil {
		t.Fatal(err)
	}
	a, err := airtel.New(ctx, books, logger, func(context.Context, string, []byte) (int, error) { return 200, nil })
	if err != nil {
		t.Fatal(err)
	}
	rules, err := faults.New(ctx, books.DB())
	if err != nil {
		t.Fatal(err)
	}
	journal, err := report.New(ctx, books.DB(), books.Clock(), logger)
	if err != nil {
		t.Fatal(err)
	}
	m.WithFaults(rules).WithReport(journal)
	a.WithFaults(rules).WithReport(journal)
	mux := http.NewServeMux()
	m.Routes(mux)
	a.Routes(mux)
	phones, err := phone.New(ctx, books, map[bank.Operator]phone.Operator{bank.MTN: m, bank.Airtel: a}, logger)
	if err != nil {
		t.Fatal(err)
	}
	admin.Routes(mux, token, books, m, a, rules, phones, journal, logger)
	e := &env{t: t, server: httptest.NewServer(mux), books: books, phones: phones}
	t.Cleanup(func() { e.server.Close(); m.Wait(); a.Wait(); _ = books.Close() })
	return e
}

func (e *env) call(method, path string, headers map[string]string, body any) (int, map[string]any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, e.server.URL+path, reader)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (e *env) admin(path string, body any) (int, map[string]any) {
	return e.call(http.MethodPost, path, map[string]string{"Authorization": "Bearer " + adminToken}, body)
}

func TestOffWithoutAToken(t *testing.T) {
	e := setup(t, "")
	if status, _ := e.call(http.MethodPost, "/_mbongo/customers", map[string]string{"Authorization": "Bearer "}, map[string]string{}); status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", status)
	}
}

func TestWrongTokenIsRefused(t *testing.T) {
	e := setup(t, adminToken)
	for _, h := range []map[string]string{{}, {"Authorization": "Bearer nope"}, {"Authorization": adminToken}} {
		if status, _ := e.call(http.MethodPost, "/_mbongo/mtn/subscriptions", h, map[string]string{"product": "collection"}); status != http.StatusUnauthorized {
			t.Errorf("%v: status %d", h, status)
		}
	}
}

func TestACongoPaymentFromStartToFinish(t *testing.T) {
	e := setup(t, adminToken)
	status, sub := e.admin("/_mbongo/mtn/subscriptions", map[string]string{"product": "collection"})
	if status != http.StatusCreated {
		t.Fatalf("subscription: %d %v", status, sub)
	}
	key := sub["subscriptionKey"].(string)
	status, user := e.admin("/_mbongo/mtn/api-users", map[string]string{"subscriptionKey": key,
		"providerCallbackHost": "acme.example", "targetEnvironment": "mtncongo"})
	if status != http.StatusCreated {
		t.Fatalf("api user: %d %v", status, user)
	}
	status, customer := e.admin("/_mbongo/customers", map[string]string{"operator": "mtn",
		"phone": "242061234567", "currency": "XAF", "balance": "5000"})
	if status != http.StatusCreated || customer["balance"] != "5000" {
		t.Fatalf("customer: %d %v", status, customer)
	}
	if status, _ := e.admin("/_mbongo/customers", map[string]string{"operator": "mtn",
		"phone": "242061234567", "currency": "XAF"}); status != http.StatusConflict {
		t.Fatalf("same customer twice: %d", status)
	}

	basic := base64.StdEncoding.EncodeToString([]byte(user["apiUser"].(string) + ":" + user["apiKey"].(string)))
	_, tok := e.call(http.MethodPost, "/collection/token/", map[string]string{"Authorization": "Basic " + basic,
		"Ocp-Apim-Subscription-Key": key, "X-Target-Environment": "mtncongo"}, nil)
	h := map[string]string{"Authorization": "Bearer " + tok["access_token"].(string), "Ocp-Apim-Subscription-Key": key,
		"X-Target-Environment": "mtncongo", "X-Reference-Id": "5b7f3e4a-1c2d-4e8f-9a0b-1c2d3e4f5a6b"}
	status, _ = e.call(http.MethodPost, "/collection/v1_0/requesttopay", h, map[string]any{"amount": "2000",
		"currency": "XAF", "externalId": "o-1", "payer": map[string]string{"partyIdType": "MSISDN", "partyId": "242061234567"}})
	if status != http.StatusAccepted {
		t.Fatalf("request to pay: %d", status)
	}

	approve := "/_mbongo/mtn/" + user["apiUser"].(string) + "/requests/" + h["X-Reference-Id"] + "/approve"
	if status, body := e.admin(approve, nil); status != http.StatusNoContent {
		t.Fatalf("approve: %d %v", status, body)
	}
	if status, _ := e.admin(approve, nil); status != http.StatusConflict {
		t.Fatalf("approve twice: %d", status)
	}
	if status, _ := e.admin("/_mbongo/mtn/"+user["apiUser"].(string)+"/requests/00000000-0000-4000-8000-000000000000/refuse", nil); status != http.StatusNotFound {
		t.Fatalf("unknown request: %d", status)
	}
	_, got := e.call(http.MethodGet, "/collection/v1_0/requesttopay/"+h["X-Reference-Id"], h, nil)
	if got["status"] != "SUCCESSFUL" {
		t.Fatalf("status: %v", got)
	}
	if err := e.books.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBadInputs(t *testing.T) {
	e := setup(t, adminToken)
	cases := []struct {
		path string
		body any
	}{
		{"/_mbongo/mtn/subscriptions", map[string]string{"product": "remittance"}},
		{"/_mbongo/mtn/subscriptions", "not an object"},
		{"/_mbongo/mtn/api-users", map[string]string{"subscriptionKey": "nope", "targetEnvironment": "sandbox"}},
		{"/_mbongo/customers", map[string]string{"operator": "orange", "phone": "1", "currency": "XAF"}},
		{"/_mbongo/customers", map[string]string{"operator": "mtn", "phone": "1", "currency": "XAF", "balance": "1.5"}},
		{"/_mbongo/customers", map[string]string{"operator": "mtn", "phone": "1", "currency": "ZZZ"}},
	}
	for _, c := range cases {
		status, body := e.admin(c.path, c.body)
		if msg, _ := body["error"].(string); status != http.StatusBadRequest || msg == "" || strings.Contains(msg, ":") {
			t.Errorf("%s %v: %d %v", c.path, c.body, status, body)
		}
	}
}

func TestSecondSIMJoinsAnotherNumbersPhone(t *testing.T) {
	e := setup(t, adminToken)
	customer := func(body string) (int, map[string]any) {
		t.Helper()
		return e.admin("/_mbongo/customers", json.RawMessage(body))
	}
	if status, _ := customer(`{"operator":"mtn","phone":"242061234567","currency":"XAF"}`); status != http.StatusCreated {
		t.Fatalf("first SIM: %d", status)
	}
	status, out := customer(`{"operator":"airtel","phone":"055000001","currency":"XAF","sameDeviceAs":"242061234567"}`)
	if status != http.StatusCreated || out["sameDeviceAs"] != "242061234567" {
		t.Fatalf("second SIM: %d %v", status, out)
	}
	status, out = customer(`{"operator":"airtel","phone":"055000002","currency":"XAF","sameDeviceAs":"242061234567"}`)
	if status != http.StatusConflict || out["error"] != "that phone already holds two SIMs" {
		t.Fatalf("third SIM: %d %v", status, out)
	}
	status, out = customer(`{"operator":"airtel","phone":"055000003","currency":"XAF","sameDeviceAs":"242069999999"}`)
	if status != http.StatusBadRequest || !strings.Contains(out["error"].(string), "sameDeviceAs") {
		t.Fatalf("unknown number: %d %v", status, out)
	}
	for _, n := range []string{"055000002", "055000003"} {
		if w, _ := e.books.Wallets(context.Background(), bank.Airtel, n); len(w) != 0 {
			t.Fatalf("a refused SIM opened a wallet: %s", n)
		}
	}
}

func TestFundAPayoutAccount(t *testing.T) {
	e := setup(t, adminToken)
	_, sub := e.admin("/_mbongo/mtn/subscriptions", map[string]string{"product": "disbursement"})
	_, user := e.admin("/_mbongo/mtn/api-users", map[string]string{"subscriptionKey": sub["subscriptionKey"].(string),
		"providerCallbackHost": "acme.example", "targetEnvironment": "mtncongo"})
	path := "/_mbongo/mtn/" + user["apiUser"].(string) + "/fund"

	status, body := e.admin(path, map[string]string{"amount": "250000"})
	if status != http.StatusOK || body["balance"] != "250000" || body["currency"] != "XAF" {
		t.Fatalf("fund: %d %v", status, body)
	}
	for _, amount := range []string{"0", "1.5", "x", ""} {
		if status, _ := e.admin(path, map[string]string{"amount": amount}); status != http.StatusBadRequest {
			t.Errorf("amount %q: %d", amount, status)
		}
	}
	if status, _ := e.admin("/_mbongo/mtn/00000000-0000-4000-8000-000000000000/fund", map[string]string{"amount": "1"}); status != http.StatusNotFound {
		t.Fatalf("unknown user: %d", status)
	}
	if err := e.books.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAirtelAppPaysThroughTheAdmin(t *testing.T) {
	e := setup(t, adminToken)
	if status, _ := e.admin("/_mbongo/airtel/apps", map[string]string{}); status != 400 {
		t.Fatalf("app without a name: %d", status)
	}
	status, app := e.admin("/_mbongo/airtel/apps", map[string]string{"name": "Acme", "callbackUrl": "https://acme.example/cb"})
	if status != 201 || app["clientId"] == "" || app["clientSecret"] == "" || app["pin"] != "1234" {
		t.Fatalf("app: %d %v", status, app)
	}
	client := app["clientId"].(string)
	if status, out := e.admin("/_mbongo/airtel/"+client+"/fund", map[string]string{"amount": "2500"}); status != 200 || out["balance"] != "2500" {
		t.Fatalf("fund: %d %v", status, out)
	}
	if status, _ := e.admin("/_mbongo/airtel/nobody/fund", map[string]string{"amount": "1"}); status != 404 {
		t.Fatalf("fund unknown app: %d", status)
	}
	if status, _ := e.admin("/_mbongo/customers", map[string]string{"operator": "airtel", "phone": "055000001", "currency": "XAF", "balance": "1000"}); status != 201 {
		t.Fatalf("customer: %d", status)
	}

	_, tok := e.call(http.MethodPost, "/auth/oauth2/token", nil, map[string]string{
		"client_id": client, "client_secret": app["clientSecret"].(string), "grant_type": "client_credentials"})
	headers := map[string]string{"Authorization": "Bearer " + tok["access_token"].(string), "X-Country": "CG", "X-Currency": "XAF"}
	_, _ = e.call(http.MethodPost, "/merchant/v1/payments/", headers, map[string]any{"reference": "r",
		"subscriber": map[string]string{"msisdn": "055000001"}, "transaction": map[string]any{"amount": 400, "id": "order-1"}})

	if status, _ := e.admin("/_mbongo/airtel/"+client+"/payments/nothing/approve", nil); status != 404 {
		t.Fatalf("approve unknown: %d", status)
	}
	if status, _ := e.admin("/_mbongo/airtel/"+client+"/payments/order-1/approve", nil); status != 204 {
		t.Fatalf("approve: %d", status)
	}
	if status, _ := e.admin("/_mbongo/airtel/"+client+"/payments/order-1/refuse", nil); status != 409 {
		t.Fatalf("refuse after approval: %d", status)
	}
	_, s := e.call(http.MethodGet, "/standard/v1/payments/order-1", headers, nil)
	if tx := s["data"].(map[string]any)["transaction"].(map[string]any); tx["status"] != "TS" {
		t.Fatalf("status: %v", s)
	}
}

func (e *env) adminCall(method, path string, body any) (int, any) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, e.server.URL+path, reader)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestRulesThroughTheAdmin(t *testing.T) {
	e := setup(t, adminToken)
	for _, bad := range []map[string]any{
		{"operator": "orange", "fault": "slow"},
		{"operator": "mtn", "fault": "explode"},
		{"operator": "mtn", "fault": "ambiguous"},
		{"operator": "mtn", "fault": "slow", "times": -1},
		{"operator": "mtn", "fault": "slow", "delaySeconds": -1},
	} {
		if status, out := e.admin("/_mbongo/rules", bad); status != 400 {
			t.Errorf("%v: %d %v", bad, status, out)
		}
	}
	status, out := e.admin("/_mbongo/rules", map[string]any{"operator": "mtn", "operation": "token", "fault": "maintenance"})
	rule, _ := out["id"].(float64)
	if status != 201 || rule == 0 || out["times"] != float64(1) {
		t.Fatalf("add: %d %v", status, out)
	}
	_, slow := e.admin("/_mbongo/rules", map[string]any{"operator": "airtel", "fault": "slow", "times": 0})
	if slow["delaySeconds"] != float64(30) || slow["times"] != float64(0) {
		t.Fatalf("slow without a delay: %v", slow)
	}
	if status, list := e.adminCall(http.MethodGet, "/_mbongo/rules", nil); status != 200 || len(list.([]any)) != 2 {
		t.Fatalf("list: %d %v", status, list)
	}

	if status, _ := e.call(http.MethodPost, "/collection/token/", nil, nil); status != 503 {
		t.Fatalf("the rule should answer the token call: %d", status)
	}
	if status, _ := e.adminCall(http.MethodDelete, "/_mbongo/rules/"+strconv.Itoa(int(rule)), nil); status != 404 {
		t.Fatalf("a spent rule is gone: %d", status)
	}
	if status, _ := e.adminCall(http.MethodDelete, "/_mbongo/rules", nil); status != 204 {
		t.Fatalf("clear: %d", status)
	}
	if _, list := e.adminCall(http.MethodGet, "/_mbongo/rules", nil); len(list.([]any)) != 0 {
		t.Fatalf("after clear: %v", list)
	}
}
