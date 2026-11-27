package airtel_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/faults"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

type callback struct {
	url  string
	raw  []byte
	body map[string]any
}

type env struct {
	t         *testing.T
	ctx       context.Context
	clock     *clock.Manual
	books     *bank.Bank
	svc       *airtel.Service
	rules     *faults.Injector
	server    *httptest.Server
	mu        sync.Mutex
	callbacks []callback
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), clock: clock.NewManual(start)}
	var err error
	e.books, err = bank.Open(e.ctx, filepath.Join(t.TempDir(), "a.db"), e.clock)
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = airtel.New(e.ctx, e.books, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ context.Context, url string, body []byte) error {
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			e.mu.Lock()
			e.callbacks = append(e.callbacks, callback{url, body, m})
			e.mu.Unlock()
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if e.rules, err = faults.New(e.ctx, e.books.DB()); err != nil {
		t.Fatal(err)
	}
	e.svc.WithFaults(e.rules)
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

// envelope reads Airtel's status block.
func (a answer) envelope() (code string, success bool) {
	st, _ := a.body["status"].(map[string]any)
	code, _ = st["response_code"].(string)
	if code == "" {
		code, _ = st["result_code"].(string)
	}
	success, _ = st["success"].(bool)
	return code, success
}

func (a answer) data() map[string]any {
	d, _ := a.body["data"].(map[string]any)
	return d
}

func (a answer) transaction() map[string]any {
	t, _ := a.data()["transaction"].(map[string]any)
	return t
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
	req.Header.Set("Content-Type", "application/json")
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

// merchant is an app with a fresh token.
type merchant struct {
	e   *env
	app airtel.App
	at  string
}

func (e *env) merchant(app airtel.App) *merchant {
	e.t.Helper()
	if app.Name == "" {
		app.Name = "Shop"
	}
	if app.CallbackURL == "" {
		app.CallbackURL = "https://shop.example/airtel"
	}
	app, err := e.svc.CreateApp(e.ctx, app)
	if err != nil {
		e.t.Fatal(err)
	}
	m := &merchant{e: e, app: app}
	m.refresh()
	return m
}

func (m *merchant) refresh() {
	a := m.e.token(m.app.ClientID, &m.app.ClientSecret)
	m.at, _ = a.body["access_token"].(string)
}

func (e *env) token(clientID string, secret *string) answer {
	body := map[string]any{"client_id": clientID, "grant_type": "client_credentials"}
	if secret != nil {
		body["client_secret"] = *secret
	}
	return e.call(http.MethodPost, "/auth/oauth2/token", nil, body)
}

func (m *merchant) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + m.at, "X-Country": "CG", "X-Currency": "XAF", "Accept": "*/*"}
}

func (m *merchant) post(path string, body any) answer {
	m.e.t.Helper()
	return m.e.call(http.MethodPost, path, m.headers(), body)
}

func (m *merchant) get(path string) answer {
	m.e.t.Helper()
	return m.e.call(http.MethodGet, path, m.headers(), nil)
}

func (e *env) customer(msisdn string, balance int64) {
	e.t.Helper()
	if _, err := e.books.OpenCustomer(e.ctx, bank.Airtel, msisdn, "XAF", balance); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) balanceOf(msisdn string) int64 {
	e.t.Helper()
	a, err := e.books.FindAccount(e.ctx, bank.Airtel, bank.CustomerAccount, msisdn, "XAF")
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Balance
}

func (e *env) waitCallbacks(n int) []callback {
	e.t.Helper()
	e.svc.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.callbacks) != n {
		e.t.Fatalf("want %d callbacks, got %d: %+v", n, len(e.callbacks), e.callbacks)
	}
	return append([]callback(nil), e.callbacks...)
}

func collection(msisdn, id string, amount any) map[string]any {
	return map[string]any{"reference": "Order 7",
		"subscriber":  map[string]string{"country": "CG", "currency": "XAF", "msisdn": msisdn},
		"transaction": map[string]any{"amount": amount, "country": "CG", "currency": "XAF", "id": id}}
}

func (m *merchant) encryptedPIN(pin string) string {
	m.e.t.Helper()
	a := m.get("/v1/rsa/encryption-keys")
	der, err := base64.StdEncoding.DecodeString(a.data()["key"].(string))
	if err != nil {
		m.e.t.Fatal(err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		m.e.t.Fatal(err)
	}
	out, err := rsa.EncryptPKCS1v15(rand.Reader, pub.(*rsa.PublicKey), []byte(pin)) //nolint:staticcheck // Airtel's scheme
	if err != nil {
		m.e.t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(out)
}

func expect(t *testing.T, a answer, code string, success bool) {
	t.Helper()
	if got, ok := a.envelope(); a.status != 200 || got != code || ok != success {
		t.Fatalf("want 200 %s success=%v, got %d %v", code, success, a.status, a.body)
	}
}

func TestTokenLastsThreeMinutesAsText(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	a := e.token(m.app.ClientID, &m.app.ClientSecret)
	if a.status != 200 || a.body["expires_in"] != "180" || a.body["token_type"] != "bearer" || a.body["access_token"] == "" {
		t.Fatalf("%d %v", a.status, a.body)
	}
	if a := m.get("/standard/v1/users/balance"); a.status != 200 {
		t.Fatalf("fresh token: %d %v", a.status, a.body)
	}
	e.clock.Advance(180 * time.Second)
	if a := m.get("/standard/v1/users/balance"); a.status != 401 || a.body["error"] != "invalid_token" {
		t.Fatalf("expired token: %d %v", a.status, a.body)
	}
}

func TestTokenWithoutSecretIsIssued(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	if a := e.token(m.app.ClientID, nil); a.status != 200 {
		t.Fatalf("%d %v", a.status, a.body)
	}
}

func TestTokenRefusesWrongCredentials(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	wrong := "wrong"
	if a := e.token(m.app.ClientID, &wrong); a.status != 401 {
		t.Fatalf("wrong secret: %d %v", a.status, a.body)
	}
	if a := e.token("nobody", nil); a.status != 401 {
		t.Fatalf("unknown client: %d %v", a.status, a.body)
	}
	a := e.call(http.MethodPost, "/auth/oauth2/token", nil, map[string]string{"client_id": m.app.ClientID, "grant_type": "password"})
	if a.status != 400 {
		t.Fatalf("wrong grant: %d %v", a.status, a.body)
	}
}

func TestEveryCallNeedsCountryAndCurrency(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	for _, c := range []struct {
		header, value, code string
	}{{"X-Country", "", "ROUTER003"}, {"X-Country", "CD", "ROUTER006"}, {"X-Currency", "EUR", "ROUTER112"}} {
		h := m.headers()
		h[c.header] = c.value
		a := e.call(http.MethodGet, "/standard/v1/users/balance", h, nil)
		if code, _ := a.envelope(); a.status != 400 || code != c.code {
			t.Fatalf("%s=%q: %d %v", c.header, c.value, a.status, a.body)
		}
	}
}

func TestCollectionWaitsForTheCustomer(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)

	a := m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	expect(t, a, "DP00800001006", true)
	if tx := a.transaction(); tx["id"] != "order-1" || tx["status"] != "SUCCESS" {
		t.Fatalf("%v", a.body)
	}
	s := m.get("/standard/v1/payments/order-1")
	expect(t, s, "DP00800001006", true)
	if tx := s.transaction(); tx["status"] != "TIP" || tx["airtel_money_id"] != nil {
		t.Fatalf("pending: %v", s.body)
	}

	if err := e.svc.Approve(e.ctx, m.app.ClientID, "order-1"); err != nil {
		t.Fatal(err)
	}
	s = m.get("/standard/v1/payments/order-1")
	expect(t, s, "DP00800001001", true)
	moneyID, _ := s.transaction()["airtel_money_id"].(string)
	if s.transaction()["status"] != "TS" || !strings.HasPrefix(moneyID, "MP") || e.balanceOf("055000001") != 4000 {
		t.Fatalf("paid: %v", s.body)
	}
	cb := e.waitCallbacks(1)[0]
	tx, _ := cb.body["transaction"].(map[string]any)
	if cb.url != "https://shop.example/airtel" || tx["status_code"] != "TS" || tx["id"] != "order-1" ||
		tx["airtel_money_id"] != moneyID || !strings.Contains(tx["message"].(string), "XAF 1,000 to SHOP") ||
		cb.body["hash"] != nil {
		t.Fatalf("callback: %+v", cb.body)
	}
	if err := e.svc.Approve(e.ctx, m.app.ClientID, "order-1"); !errors.Is(err, airtel.ErrDecided) {
		t.Fatalf("second approval: %v", err)
	}
}

func TestCollectionFailures(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 500)

	expect(t, m.post("/merchant/v1/payments/", collection("242055000001", "with-code", 100)), "DP00800001010", false)
	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "zero", 0)), "DP00800001004", false)
	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "cents", "10.5")), "DP00800001004", false)

	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "a", 100)), "DP00800001006", true)
	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "a", 100)), "DP00800001005", false)
	if err := e.svc.Refuse(e.ctx, m.app.ClientID, "a"); err != nil {
		t.Fatal(err)
	}
	s := m.get("/standard/v1/payments/a")
	expect(t, s, "DP00800001008", true)
	if s.transaction()["status"] != "TF" {
		t.Fatalf("%v", s.body)
	}

	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "b", 1000)), "DP00800001006", true)
	_ = e.svc.Approve(e.ctx, m.app.ClientID, "b")
	expect(t, m.get("/standard/v1/payments/b"), "DP00800001007", true)

	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "c", 100)), "DP00800001006", true)
	e.clock.Advance(5 * time.Minute)
	if _, err := e.books.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	s = m.get("/standard/v1/payments/c")
	expect(t, s, "DP00800001024", true)
	if s.transaction()["status"] != "TE" {
		t.Fatalf("%v", s.body)
	}

	expect(t, m.get("/standard/v1/payments/nothing"), "DP00800001025", false)
	cbs := e.waitCallbacks(2)
	for _, cb := range cbs {
		if tx := cb.body["transaction"].(map[string]any); tx["status_code"] != "TF" {
			t.Fatalf("%+v", cb.body)
		}
	}
	if a := m.post("/merchant/v1/payments/", map[string]any{"reference": "x"}); a.status != 400 {
		t.Fatalf("missing fields: %d %v", a.status, a.body)
	}
}

func TestAnotherAppSeesNothing(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	other := e.merchant(airtel.App{})
	e.customer("055000001", 500)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "mine", 100))
	expect(t, other.get("/standard/v1/payments/mine"), "DP00800001025", false)
	expect(t, other.post("/merchant/v1/payments/", collection("055000001", "mine", 100)), "DP00800001006", true)
}

func TestRefundPaysBackOnce(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	expect(t, m.post("/standard/v1/payments/refund", map[string]any{"transaction": map[string]string{"airtel_money_id": "MP0"}}),
		"DP00800001025", false)
	_ = e.svc.Approve(e.ctx, m.app.ClientID, "order-1")
	moneyID := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"]

	refund := map[string]any{"transaction": map[string]any{"airtel_money_id": moneyID}}
	a := m.post("/standard/v1/payments/refund", refund)
	expect(t, a, "DP00800001001", true)
	if a.transaction()["status"] != "SUCCESS" || a.transaction()["airtel_money_id"] == moneyID || e.balanceOf("055000001") != 5000 {
		t.Fatalf("%v", a.body)
	}
	expect(t, m.post("/standard/v1/payments/refund", refund), "DP00800001008", false)
}

func TestCashOutTakesFromTheCustomerAtOnce(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 300)
	body := map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
		"transaction": map[string]string{"amount": "100", "id": "out-1"}, "additional_info": []any{}, "reference": "r"}
	a := m.post("/standard/v1/cashout/", body)
	expect(t, a, "DP01000001001", true)
	if d := a.data(); d["id"] != "out-1" || d["status"] != "SUCCESS" || e.balanceOf("055000001") != 200 {
		t.Fatalf("%v", a.body)
	}
	if s := m.get("/standard/v1/payments/out-1"); s.transaction()["status"] != "TS" {
		t.Fatalf("%v", s.body)
	}
	body["transaction"] = map[string]string{"amount": "1000", "id": "out-2"}
	expect(t, m.post("/standard/v1/cashout/", body), "DP01000001007", false)
	e.waitCallbacks(2)
}

func TestCashInNeedsThePINAndTheMoney(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{PIN: "4321"})
	e.customer("055000001", 0)
	body := func(id, pin string) map[string]any {
		return map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
			"transaction": map[string]string{"amount": "700", "id": id}, "reference": "r", "pin": pin}
	}
	expect(t, m.post("/standard/v1/cashin/", body("in-1", m.encryptedPIN("1234"))), "ROUTER115", false)
	expect(t, m.post("/standard/v1/cashin/", body("in-1", "not-encrypted")), "ROUTER116", false)
	pin := m.encryptedPIN("4321")
	expect(t, m.post("/standard/v1/cashin/", body("in-1", pin)), "DP01000001007", false)

	if _, err := e.svc.Fund(e.ctx, m.app.ClientID, 1000); err != nil {
		t.Fatal(err)
	}
	a := m.post("/standard/v1/cashin/", body("in-2", pin))
	expect(t, a, "DP01000001001", true)
	if a.data()["status"] != "SUCCESS" || a.transaction()["id"] != "in-2" || e.balanceOf("055000001") != 700 {
		t.Fatalf("%v", a.body)
	}
}

func TestDisbursementPaysTheCustomer(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 0)
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 5000)
	pin := m.encryptedPIN("1234")
	body := map[string]any{"payee": map[string]string{"msisdn": "055000001"}, "reference": "salary", "pin": pin,
		"transaction": map[string]any{"amount": "1000", "id": "pay-1"}}

	a := m.post("/standard/v1/disbursements/", body)
	expect(t, a, "DP00900001001", true)
	if tx := a.transaction(); tx["status"] != "TS" || tx["id"] != "pay-1" || tx["reference_id"] == "" ||
		e.balanceOf("055000001") != 1000 {
		t.Fatalf("%v", a.body)
	}
	expect(t, m.post("/standard/v1/disbursements/", body), "DP00900001011", false)
	s := m.get("/standard/v1/disbursements/pay-1")
	expect(t, s, "DP00900001001", true)
	expect(t, m.get("/standard/v1/payments/pay-1"), "DP00800001025", false)

	body["transaction"] = map[string]any{"amount": 9000, "id": "pay-2"}
	expect(t, m.post("/standard/v1/disbursements/", body), "DP00900001007", false)
	body["payee"] = map[string]string{"msisdn": "242055000001"}
	body["transaction"] = map[string]any{"amount": 10, "id": "pay-3"}
	expect(t, m.post("/standard/v1/disbursements/", body), "DP00900001012", false)
}

func TestBalanceIsFormattedText(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 37600)
	a := m.get("/standard/v1/users/balance")
	expect(t, a, "DP02100000001", true)
	if d := a.data(); d["balance"] != "37,600.00" || d["currency"] != "XAF" || d["account_status"] != "Active" {
		t.Fatalf("%v", a.body)
	}
}

func TestUserLookup(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 0)
	a := m.get("/standard/v1/users/055000001")
	if _, ok := a.envelope(); !ok || a.data()["msisdn"] != "055000001" || a.data()["is_barred"] != false {
		t.Fatalf("%v", a.body)
	}
	if _, ok := m.get("/standard/v1/users/242055000001").envelope(); ok {
		t.Fatal("a number with the country code was found")
	}
}

func TestCallbackHashWhenAKeyIsSet(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{CallbackKey: "private-key"})
	e.customer("055000001", 500)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 100))
	_ = e.svc.Approve(e.ctx, m.app.ClientID, "order-1")
	cb := e.waitCallbacks(1)[0]
	var body struct {
		Transaction json.RawMessage `json:"transaction"`
		Hash        string          `json:"hash"`
	}
	if err := json.Unmarshal(cb.raw, &body); err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("private-key"))
	mac.Write(body.Transaction)
	if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); body.Hash != want {
		t.Fatalf("hash %q, want %q", body.Hash, want)
	}
}

func TestEncryptionKeyExpiresAndIsReplaced(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	a := m.get("/v1/rsa/encryption-keys")
	expect(t, a, "DP02010001001", true)
	der, _ := base64.StdEncoding.DecodeString(a.data()["key"].(string))
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil || pub.(*rsa.PublicKey).N.BitLen() != 1024 {
		t.Fatalf("key: %v", err)
	}
	until, err := time.Parse("02-01-2006 15:04:05", a.data()["valid_upto"].(string))
	if err != nil || !until.After(start) {
		t.Fatalf("valid_upto %v: %v", a.data()["valid_upto"], err)
	}
	e.clock.Advance(until.Sub(start))
	m.refresh()
	if b := m.get("/v1/rsa/encryption-keys"); b.data()["key_id"] == a.data()["key_id"] {
		t.Fatalf("the expired key is still served: %v", b.body)
	}
}

func TestFailedRefundCanBeRetried(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	e.customer("055000002", 0)
	_ = m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000))
	_ = e.svc.Approve(e.ctx, m.app.ClientID, "order-1")
	moneyID := m.get("/standard/v1/payments/order-1").transaction()["airtel_money_id"]
	expect(t, m.post("/standard/v1/disbursements/", map[string]any{"payee": map[string]string{"msisdn": "055000002"},
		"reference": "r", "pin": m.encryptedPIN("1234"), "transaction": map[string]any{"amount": 1000, "id": "spent"}}),
		"DP00900001001", true)

	refund := map[string]any{"transaction": map[string]any{"airtel_money_id": moneyID}}
	expect(t, m.post("/standard/v1/payments/refund", refund), "DP00800001007", false)
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 1000)
	expect(t, m.post("/standard/v1/payments/refund", refund), "DP00800001001", true)
	expect(t, m.post("/standard/v1/payments/refund", refund), "DP00800001008", false)
	if e.balanceOf("055000001") != 5000 {
		t.Fatalf("customer has %d", e.balanceOf("055000001"))
	}
}

func TestPayoutIdsAreSeparateFromPaymentIds(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	e.customer("055000001", 5000)
	_, _ = e.svc.Fund(e.ctx, m.app.ClientID, 5000)
	expect(t, m.post("/merchant/v1/payments/", collection("055000001", "order-1", 1000)), "DP00800001006", true)
	expect(t, m.post("/standard/v1/disbursements/", map[string]any{"payee": map[string]string{"msisdn": "055000001"},
		"reference": "r", "pin": m.encryptedPIN("1234"), "transaction": map[string]any{"amount": 300, "id": "order-1"}}),
		"DP00900001001", true)
	if s := m.get("/standard/v1/payments/order-1"); s.transaction()["status"] != "TIP" {
		t.Fatalf("collection: %v", s.body)
	}
	if s := m.get("/standard/v1/disbursements/order-1"); s.transaction()["status"] != "TS" {
		t.Fatalf("payout: %v", s.body)
	}
}

func TestPathsAreExact(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	if a := m.post("/standard/v1/disbursements/extra", map[string]any{}); a.status != 404 && a.status != 405 {
		t.Fatalf("%d %v", a.status, a.body)
	}
}

func TestMissingPINIsAMissingField(t *testing.T) {
	e := setup(t)
	m := e.merchant(airtel.App{})
	a := m.post("/standard/v1/cashin/", map[string]any{"subscriber": map[string]string{"msisdn": "055000001"},
		"transaction": map[string]string{"amount": "1", "id": "x"}, "reference": "r"})
	if code, _ := a.envelope(); a.status != 400 || code != "ROUTER003" {
		t.Fatalf("%d %v", a.status, a.body)
	}
}
