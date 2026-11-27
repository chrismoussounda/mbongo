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
	"strings"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/admin"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/mtn"
)

const adminToken = "letmein"

type env struct {
	t      *testing.T
	server *httptest.Server
	books  *bank.Bank
}

func setup(t *testing.T, token string) *env {
	t.Helper()
	ctx := context.Background()
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "m.db"), clock.NewManual(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := mtn.New(ctx, books, logger)
	if err != nil {
		t.Fatal(err)
	}
	m.Send = func(context.Context, string, []byte) error { return nil }
	mux := http.NewServeMux()
	m.Routes(mux)
	admin.Routes(mux, token, books, m, logger)
	e := &env{t: t, server: httptest.NewServer(mux), books: books}
	t.Cleanup(func() { e.server.Close(); m.Wait(); _ = books.Close() })
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
		"providerCallbackHost": "shop.example", "targetEnvironment": "mtncongo"})
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
