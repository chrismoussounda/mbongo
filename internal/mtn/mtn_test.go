package mtn_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/mtn"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

type callback struct {
	url  string
	body map[string]any
}

type env struct {
	t         *testing.T
	ctx       context.Context
	books     *bank.Bank
	clock     *clock.Manual
	svc       *mtn.Service
	server    *httptest.Server
	mu        sync.Mutex
	callbacks []callback
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), clock: clock.NewManual(start)}
	var err error
	e.books, err = bank.Open(e.ctx, filepath.Join(t.TempDir(), "m.db"), e.clock)
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = mtn.New(e.ctx, e.books, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ context.Context, url string, body []byte) error {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			e.mu.Lock()
			e.callbacks = append(e.callbacks, callback{url, m})
			e.mu.Unlock()
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	e.svc.Routes(mux)
	e.server = httptest.NewServer(mux)
	t.Cleanup(func() { e.server.Close(); _ = e.books.Close() })
	return e
}

type answer struct {
	status int
	body   map[string]any
}

func (e *env) call(method, path string, headers map[string]string, body any) answer {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	a := answer{status: res.StatusCode}
	_ = json.NewDecoder(res.Body).Decode(&a.body)
	return a
}

// merchant is an API user with a fresh token.
type merchant struct {
	e                  *env
	sub, user, key, at string
	environment        string
	product            string
}

func (e *env) merchant(environment string) *merchant {
	e.t.Helper()
	return e.user("collection", environment)
}

func (e *env) user(product, environment string) *merchant {
	e.t.Helper()
	sub, err := e.svc.CreateSubscription(e.ctx, product)
	if err != nil {
		e.t.Fatal(err)
	}
	user, key, err := e.svc.CreateAPIUser(e.ctx, sub, "shop.example", environment)
	if err != nil {
		e.t.Fatal(err)
	}
	m := &merchant{e: e, sub: sub, user: user, key: key, environment: environment, product: product}
	m.at = m.token().body["access_token"].(string)
	return m
}

func (m *merchant) token() answer {
	req, _ := http.NewRequestWithContext(m.e.ctx, http.MethodPost, "http://x", nil)
	req.SetBasicAuth(m.user, m.key)
	product := m.product
	if product == "" {
		product = "collection"
	}
	return m.e.call(http.MethodPost, "/"+product+"/token/", map[string]string{
		"Authorization": req.Header.Get("Authorization"), "Ocp-Apim-Subscription-Key": m.sub,
		"X-Target-Environment": m.environment,
	}, nil)
}

func (m *merchant) headers(extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + m.at, "Ocp-Apim-Subscription-Key": m.sub,
		"X-Target-Environment": m.environment}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

const ref1 = "5b7f3e4a-1c2d-4e8f-9a0b-1c2d3e4f5a6b"

func pay(phone, amount, currency string) map[string]any {
	return map[string]any{"amount": amount, "currency": currency, "externalId": "order-7",
		"payer":        map[string]string{"partyIdType": "MSISDN", "partyId": phone},
		"payerMessage": "Commande 7", "payeeNote": "Merci"}
}

func (m *merchant) requestToPay(ref string, body any, extra map[string]string) answer {
	return m.e.call(http.MethodPost, "/collection/v1_0/requesttopay",
		m.headers(map[string]string{"X-Reference-Id": ref, "X-Callback-Url": extra["X-Callback-Url"]}), body)
}

func (m *merchant) status(ref string) answer {
	return m.e.call(http.MethodGet, "/collection/v1_0/requesttopay/"+ref, m.headers(nil), nil)
}

func (e *env) waitCallbacks(n int) []callback {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		got := append([]callback(nil), e.callbacks...)
		e.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func keys(m map[string]any) string {
	var k []string
	for name := range m {
		k = append(k, name)
	}
	sort.Strings(k)
	return strings.Join(k, ",")
}

func TestSandboxProvisioning(t *testing.T) {
	e := setup(t)
	sub, _ := e.svc.CreateSubscription(e.ctx, "collection")
	h := map[string]string{"Ocp-Apim-Subscription-Key": sub, "X-Reference-Id": ref1}

	if a := e.call(http.MethodPost, "/v1_0/apiuser", h, map[string]string{"providerCallbackHost": "shop.example"}); a.status != 201 {
		t.Fatalf("create: %d %v", a.status, a.body)
	}
	if a := e.call(http.MethodPost, "/v1_0/apiuser", h, map[string]string{"providerCallbackHost": "shop.example"}); a.status != 409 || a.body["code"] != "RESOURCE_ALREADY_EXIST" {
		t.Fatalf("again: %d %v", a.status, a.body)
	}
	a := e.call(http.MethodGet, "/v1_0/apiuser/"+ref1, h, nil)
	if a.status != 200 || a.body["providerCallbackHost"] != "shop.example" || a.body["targetEnvironment"] != "sandbox" {
		t.Fatalf("get: %d %v", a.status, a.body)
	}
	a = e.call(http.MethodPost, "/v1_0/apiuser/"+ref1+"/apikey", h, nil)
	key, _ := a.body["apiKey"].(string)
	if a.status != 201 || key == "" {
		t.Fatalf("key: %d %v", a.status, a.body)
	}

	m := &merchant{e: e, sub: sub, user: ref1, key: key, environment: "sandbox"}
	if tok := m.token(); tok.status != 200 || tok.body["expires_in"] != float64(3600) || tok.body["token_type"] != "access_token" {
		t.Fatalf("token: %d %v", tok.status, tok.body)
	}

	other, _ := e.svc.CreateSubscription(e.ctx, "collection")
	if a := e.call(http.MethodGet, "/v1_0/apiuser/"+ref1, map[string]string{"Ocp-Apim-Subscription-Key": other}, nil); a.status != 404 {
		t.Fatalf("user seen through another subscription: %d", a.status)
	}
	if a := e.call(http.MethodPost, "/v1_0/apiuser", map[string]string{"Ocp-Apim-Subscription-Key": "nope", "X-Reference-Id": ref1}, nil); a.status != 401 {
		t.Fatalf("bad subscription: %d", a.status)
	}
	if a := e.call(http.MethodPost, "/v1_0/apiuser", map[string]string{"Ocp-Apim-Subscription-Key": sub, "X-Reference-Id": "42"}, nil); a.status != 400 {
		t.Fatalf("bad reference: %d", a.status)
	}
}

func TestTokenRefusals(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")

	wrong := *m
	wrong.key = "wrong"
	if a := wrong.token(); a.status != 401 || a.body["error"] != "login_failed" {
		t.Fatalf("wrong key: %d %v", a.status, a.body)
	}
	noEnv := *m
	noEnv.environment = ""
	if a := noEnv.token(); a.status != 403 || a.body["code"] != "NOT_ALLOWED_TARGET_ENVIRONMENT" {
		t.Fatalf("no environment: %d %v", a.status, a.body)
	}
	otherSub := *m
	otherSub.sub, _ = e.svc.CreateSubscription(e.ctx, "collection")
	if a := otherSub.token(); a.status != 401 || a.body["error"] != "login_failed" {
		t.Fatalf("someone else's subscription: %d %v", a.status, a.body)
	}
	unknown := *m
	unknown.sub = "nope"
	if a := unknown.token(); a.status != 401 || a.body["statusCode"] != float64(401) {
		t.Fatalf("unknown subscription: %d %v", a.status, a.body)
	}
}

func TestCallsNeedAValidToken(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	if a := m.status(ref1); a.status != 404 {
		t.Fatalf("valid token: %d", a.status)
	}
	bad := *m
	bad.at = "nope"
	if a := bad.status(ref1); a.status != 401 {
		t.Fatalf("bad token: %d", a.status)
	}
	e.clock.Advance(time.Hour)
	if a := m.status(ref1); a.status != 401 {
		t.Fatalf("expired token: %d", a.status)
	}
	m.at = m.token().body["access_token"].(string)
	for _, sub := range []string{"", "nope"} {
		other := *m
		other.sub = sub
		if a := other.status(ref1); a.status != 401 || a.body["statusCode"] != float64(401) {
			t.Fatalf("subscription %q: %d %v", sub, a.status, a.body)
		}
	}
	if a := e.call(http.MethodGet, "/collection/v1_0/requesttopay/"+ref1, map[string]string{"Ocp-Apim-Subscription-Key": "nope"}, nil); a.body["statusCode"] != float64(401) {
		t.Fatalf("bad key and no token: the gateway must answer first: %v", a.body)
	}
	stranger := e.merchant("sandbox")
	borrowed := *m
	borrowed.sub = stranger.sub
	if a := borrowed.status(ref1); a.status != 401 {
		t.Fatalf("token with another merchant's key: %d", a.status)
	}
	m.environment = "mtncongo"
	if a := m.status(ref1); a.status != 403 {
		t.Fatalf("other environment: %d", a.status)
	}
}

func TestSandboxPaymentSucceeds(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	a := m.requestToPay(ref1, pay("46733123453", "25.50", "EUR"), map[string]string{"X-Callback-Url": "https://shop.example/momo"})
	if a.status != 202 || a.body != nil {
		t.Fatalf("request: %d %v", a.status, a.body)
	}
	s := m.status(ref1)
	if s.status != 200 || s.body["status"] != "SUCCESSFUL" || s.body["amount"] != "25.50" ||
		s.body["externalId"] != "order-7" || s.body["financialTransactionId"] == nil || s.body["reason"] != nil {
		t.Fatalf("status: %v", s.body)
	}
	want := "amount,currency,externalId,financialTransactionId,payeeNote,payer,payerMessage,status"
	if got := keys(s.body); got != want {
		t.Fatalf("fields %s, want %s", got, want)
	}
	b := e.call(http.MethodGet, "/collection/v1_0/account/balance", m.headers(nil), nil)
	if b.body["availableBalance"] != "25.50" || b.body["currency"] != "EUR" {
		t.Fatalf("balance: %v", b.body)
	}
	got := e.waitCallbacks(1)
	if len(got) != 1 || got[0].url != "https://shop.example/momo" || got[0].body["status"] != "SUCCESSFUL" {
		t.Fatalf("callbacks: %+v", got)
	}
	if err := e.books.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxTestNumbers(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	cases := []struct{ phone, ref, status, reason string }{
		{"46733123450", "11111111-1111-4111-8111-111111111111", "FAILED", "INTERNAL_PROCESSING_ERROR"},
		{"46733123451", "22222222-2222-4222-8222-222222222222", "FAILED", "APPROVAL_REJECTED"},
		{"46733123452", "33333333-3333-4333-8333-333333333333", "PENDING", ""},
		{"46733123454", "44444444-4444-4444-8444-444444444444", "PENDING", ""},
	}
	for _, c := range cases {
		if a := m.requestToPay(c.ref, pay(c.phone, "10", "EUR"), nil); a.status != 202 {
			t.Fatalf("%s: %d", c.phone, a.status)
		}
		s := m.status(c.ref)
		if s.body["status"] != c.status {
			t.Errorf("%s: status %v", c.phone, s.body["status"])
		}
		if c.reason != "" {
			reason, _ := s.body["reason"].(map[string]any)
			if reason["code"] != c.reason || reason["message"] == "" {
				t.Errorf("%s: reason %v", c.phone, s.body["reason"])
			}
		}
	}

	e.clock.Advance(5 * time.Minute)
	m.at = m.token().body["access_token"].(string)
	timedOut := m.status(cases[2].ref)
	if reason, _ := timedOut.body["reason"].(map[string]any); timedOut.body["status"] != "FAILED" || reason["code"] != "EXPIRED" {
		t.Fatalf("timeout number after 5 minutes: %v", timedOut.body)
	}
	if s := m.status(cases[3].ref); s.body["status"] != "PENDING" {
		t.Fatalf("pending number after 5 minutes: %v", s.body)
	}
}

func TestRequestToPayValidation(t *testing.T) {
	e := setup(t)
	m := e.merchant("sandbox")
	long := strings.Repeat("é", 161)
	cases := []struct {
		name, ref string
		body      any
		callback  string
		status    int
		code      string
	}{
		{"reference not a UUID", "123", pay("46733123453", "10", "EUR"), "", 400, "INVALID_REFERENCE_ID"},
		{"not JSON", ref1, "nope", "", 400, "VALIDATION_ERROR"},
		{"francs in the sandbox", ref1, pay("46733123453", "10", "XAF"), "", 400, "INVALID_CURRENCY"},
		{"three decimals", ref1, pay("46733123453", "10.123", "EUR"), "", 400, "VALIDATION_ERROR"},
		{"zero", ref1, pay("46733123453", "0", "EUR"), "", 400, "VALIDATION_ERROR"},
		{"letters in the number", ref1, pay("4673312345x", "10", "EUR"), "", 400, "VALIDATION_ERROR"},
		{"callback elsewhere", ref1, pay("46733123453", "10", "EUR"), "https://evil.example/x", 400, "INVALID_CALLBACK_URL_HOST"},
		{"message too long", ref1, map[string]any{"amount": "10", "currency": "EUR",
			"payer": map[string]string{"partyIdType": "MSISDN", "partyId": "46733123453"}, "payerMessage": long}, "", 400, "VALIDATION_ERROR"},
		{"body over 64 KiB", ref1, map[string]any{"amount": "10", "currency": "EUR", "externalId": strings.Repeat("x", 70_000),
			"payer": map[string]string{"partyIdType": "MSISDN", "partyId": "46733123453"}}, "", 400, "VALIDATION_ERROR"},
		{"email payer", ref1, map[string]any{"amount": "10", "currency": "EUR",
			"payer": map[string]string{"partyIdType": "EMAIL", "partyId": "46733123453"}}, "", 400, "VALIDATION_ERROR"},
	}
	for _, c := range cases {
		a := m.requestToPay(c.ref, c.body, map[string]string{"X-Callback-Url": c.callback})
		if a.status != c.status || a.body["code"] != c.code {
			t.Errorf("%s: %d %v", c.name, a.status, a.body)
		}
	}
	if a := m.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil); a.status != 202 {
		t.Fatalf("valid after refusals: %d", a.status)
	}
	if a := m.requestToPay(ref1, pay("46733123453", "99", "EUR"), nil); a.status != 409 || a.body["code"] != "RESOURCE_ALREADY_EXIST" {
		t.Fatalf("same reference: %d %v", a.status, a.body)
	}
	if s := m.status(ref1); s.body["amount"] != "10" {
		t.Fatalf("the duplicate changed the payment: %v", s.body)
	}
}

func TestReferencesAreSeparatePerMerchant(t *testing.T) {
	e := setup(t)
	a, b := e.merchant("sandbox"), e.merchant("sandbox")
	_ = a.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil)
	if s := b.status(ref1); s.status != 404 || s.body["code"] != "RESOURCE_NOT_FOUND" {
		t.Fatalf("another merchant sees it: %d %v", s.status, s.body)
	}
	if r := b.requestToPay(ref1, pay("46733123453", "10", "EUR"), nil); r.status != 202 {
		t.Fatalf("another merchant can't reuse the reference: %d", r.status)
	}
}

func TestCongoPaymentWaitsForThePayer(t *testing.T) {
	e := setup(t)
	m := e.merchant("mtncongo")
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 5_000); err != nil {
		t.Fatal(err)
	}
	holder := func(phone string) any {
		return e.call(http.MethodGet, "/collection/v1_0/accountholder/msisdn/"+phone+"/active", m.headers(nil), nil).body["result"]
	}
	if holder("242061234567") != true || holder("242069999999") != false {
		t.Fatal("account holder check is wrong")
	}

	_ = m.requestToPay(ref1, pay("242061234567", "2500", "XAF"), map[string]string{"X-Callback-Url": "https://shop.example/cb"})
	if s := m.status(ref1); s.body["status"] != "PENDING" || s.body["financialTransactionId"] != nil || s.body["reason"] != nil {
		t.Fatalf("before approval: %v", s.body)
	}
	if got := e.waitCallbacks(0); len(got) != 0 {
		t.Fatalf("callback before a decision: %v", got)
	}
	if err := e.svc.Approve(e.ctx, m.user, ref1); err != nil {
		t.Fatal(err)
	}
	if s := m.status(ref1); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("after approval: %v", s.body)
	}
	if err := e.svc.Approve(e.ctx, m.user, ref1); !errors.Is(err, bank.ErrDecided) {
		t.Fatalf("approve twice: %v", err)
	}
	if got := e.waitCallbacks(2); len(got) != 1 || got[0].body["status"] != "SUCCESSFUL" {
		t.Fatalf("callbacks: %+v", got)
	}
	if b := e.call(http.MethodGet, "/collection/v1_0/account/balance", m.headers(nil), nil); b.body["availableBalance"] != "2500" || b.body["currency"] != "XAF" {
		t.Fatalf("balance: %v", b.body)
	}
}

func TestCongoFailuresUseBareReasons(t *testing.T) {
	e := setup(t)
	m := e.merchant("mtncongo")
	_, _ = e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 1_000)
	refs := []string{
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc",
	}
	_ = m.requestToPay(refs[0], pay("242069999999", "100", "XAF"), nil)
	_ = m.requestToPay(refs[1], pay("242061234567", "2500", "XAF"), nil)
	_ = m.requestToPay(refs[2], pay("242061234567", "100", "XAF"), nil)
	_ = e.svc.Approve(e.ctx, m.user, refs[1])
	_ = e.svc.Refuse(e.ctx, m.user, refs[2])

	_, _ = e.books.OpenCustomer(e.ctx, bank.MTN, "242069999999", "XAF", 1_000)
	if a := m.requestToPay(refs[0], pay("242069999999", "100", "XAF"), nil); a.status != 409 {
		t.Fatalf("reusing the reference of a payer-not-found request: %d", a.status)
	}
	if _, err := e.books.ByPartnerRef(e.ctx, bank.MTN, m.user, refs[0]); !errors.Is(err, bank.ErrNotFound) {
		t.Fatalf("the refused duplicate left a bank transaction behind: %v", err)
	}
	want := []string{"PAYER_NOT_FOUND", "LOW_BALANCE_OR_PAYEE_LIMIT_REACHED_OR_NOT_ALLOWED", "APPROVAL_REJECTED"}
	for i, ref := range refs {
		if s := m.status(ref); s.body["status"] != "FAILED" || s.body["reason"] != want[i] {
			t.Errorf("%s: %v", want[i], s.body)
		}
	}
}

func TestExpiredTokensArePurged(t *testing.T) {
	e := setup(t)
	e.merchant("sandbox")
	e.clock.Advance(500 * time.Millisecond)
	e.merchant("sandbox")
	count := func() int {
		var n int
		if err := e.books.DB().QueryRowContext(e.ctx, "SELECT COUNT(*) FROM mtn_tokens").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.clock.Advance(time.Hour - time.Second)
	if err := e.svc.PurgeTokens(e.ctx); err != nil || count() != 2 {
		t.Fatalf("live tokens were purged: %d left, %v", count(), err)
	}
	// Now one token is exactly due, and the other is due half a second ago with no fraction in
	// its stored time: both must go.
	e.clock.Advance(time.Second)
	if err := e.svc.PurgeTokens(e.ctx); err != nil || count() != 0 {
		t.Fatalf("expired tokens survived: %d left, %v", count(), err)
	}
}
