package phone_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/phone"
)

const (
	number  = "242061234567"
	airtelN = "055000001"
	airtel2 = "055000002"
	sim     = "/_mbongo/phone/mtn/" + number
	page    = "/_mbongo/phone/p/1"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// operator answers like an operator package, straight from the books.
type operator struct {
	op      bank.Operator
	books   *bank.Bank
	mu      sync.Mutex
	answers []string
}

func (o *operator) Prompts(ctx context.Context, phone string) ([]bank.Prompt, error) {
	return o.books.PromptsFrom(ctx, `SELECT t.id, 'Acme <b>Pay</b>', t.external_id
		FROM transactions t JOIN accounts c ON c.id = t.customer_id
		WHERE t.operator = ? AND c.name = ? AND t.kind = 'collection' AND t.external_id != 'cashout'
			AND t.status = 'pending' AND t.expires_at > ?
		ORDER BY t.created_at, t.id`, o.op, phone, o.books.Clock().Now().UTC().Format(bank.TimeLayout))
}

func (o *operator) Waiting(ctx context.Context) ([]bank.Prompt, error) {
	return o.books.PromptsFrom(ctx, `SELECT t.id, 'Acme <b>Pay</b>', t.external_id
		FROM transactions t
		WHERE t.operator = ? AND t.kind = 'collection' AND t.external_id != 'cashout'
			AND t.status = 'pending' AND t.expires_at > ?
		ORDER BY t.created_at, t.id`, o.op, o.books.Clock().Now().UTC().Format(bank.TimeLayout))
}

func (o *operator) Answer(ctx context.Context, id string, approve bool) error {
	o.mu.Lock()
	o.answers = append(o.answers, id)
	o.mu.Unlock()
	var err error
	if approve {
		_, err = o.books.Approve(ctx, id)
	} else {
		_, err = o.books.Refuse(ctx, id, bank.ReasonRefused)
	}
	return err
}

func (o *operator) Describe(_ context.Context, t bank.Transaction) (string, string, error) {
	return "Acme", "REF-" + t.PartnerRef, nil
}

type env struct {
	t      *testing.T
	ctx    context.Context
	books  *bank.Bank
	clock  *clock.Manual
	mtn    *operator
	svc    *phone.Service
	server *httptest.Server
	client *http.Client
	seq    int
	// stop is the server shutting down.
	stop func()
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), clock: clock.NewManual(start)}
	var err error
	if e.books, err = bank.Open(e.ctx, filepath.Join(t.TempDir(), "p.db"), e.clock); err != nil {
		t.Fatal(err)
	}
	e.mtn = &operator{op: bank.MTN, books: e.books}
	e.svc, err = phone.New(e.ctx, e.books, map[bank.Operator]phone.Operator{bank.MTN: e.mtn,
		bank.Airtel: &operator{op: bank.Airtel, books: e.books}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc := e.svc
	stopping, stop := context.WithCancel(e.ctx)
	e.stop = stop
	mux := http.NewServeMux()
	svc.Routes(stopping, mux)
	e.server = httptest.NewServer(mux)
	e.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	t.Cleanup(func() { stop(); e.server.Close(); _ = e.books.Close() })
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, number, "XAF", 10_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.books.OpenMerchant(e.ctx, bank.MTN, "acme", "XAF", 50_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.books.OpenCustomer(e.ctx, bank.Airtel, airtelN, "XAF", 5_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.books.OpenMerchant(e.ctx, bank.Airtel, "acme", "XAF", 50_000); err != nil {
		t.Fatal(err)
	}
	return e
}

type reply struct {
	status   int
	location string
	body     string
	header   http.Header
}

func (e *env) do(method, path string, form url.Values, headers map[string]string) reply {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.server.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return reply{status: res.StatusCode, location: res.Header.Get("Location"), body: string(raw), header: res.Header}
}

func (e *env) get(path string) reply { e.t.Helper(); return e.do(http.MethodGet, path, nil, nil) }

func (e *env) post(path string, form url.Values) reply {
	e.t.Helper()
	return e.do(http.MethodPost, path, form, nil)
}

// open opens the MTN number's phone, which is phone 1.
func (e *env) open() {
	e.t.Helper()
	if r := e.get(sim); r.status != http.StatusSeeOther || r.location != page {
		e.t.Fatalf("open: %d %q", r.status, r.location)
	}
}

func (e *env) request(op bank.Operator, phone string, kind bank.Kind, amount int64) bank.Transaction {
	e.t.Helper()
	e.seq++
	t, err := e.books.Request(e.ctx, bank.Request{Kind: kind, Operator: op, Merchant: "acme",
		Phone: phone, Amount: amount, Currency: "XAF", PartnerRef: "ref-" + string(rune('a'+e.seq)),
		ExternalID: "Order <7>"})
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

func (e *env) collect(amount int64) bank.Transaction {
	e.t.Helper()
	return e.request(bank.MTN, number, bank.Collection, amount)
}

func (e *env) status(id string) bank.Status {
	e.t.Helper()
	t, err := e.books.Transaction(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return t.Status
}

func (e *env) answer(tx bank.Transaction, form url.Values) reply {
	e.t.Helper()
	return e.post(page+"/prompts/"+tx.ID, form)
}

func TestCrossSitePostsAreRefused(t *testing.T) {
	e := setup(t)
	e.open()
	tx := e.collect(100)
	r := e.do(http.MethodPost, page+"/prompts/"+tx.ID, url.Values{"action": {"approve"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.status != http.StatusForbidden || e.status(tx.ID) != bank.Pending {
		t.Fatalf("cross-site approve: %d %s", r.status, e.status(tx.ID))
	}
}

func TestANumberOpensItsOwnPhone(t *testing.T) {
	e := setup(t)
	e.open()
	if r := e.get(sim); r.location != page {
		t.Fatalf("second visit: %q", r.location)
	}
	if r := e.get("/_mbongo/phone/airtel/" + airtelN); r.location != "/_mbongo/phone/p/2" {
		t.Fatalf("another number: %q", r.location)
	}
	if r := e.get("/_mbongo/phone/orange/" + number); r.status != http.StatusNotFound {
		t.Errorf("unknown operator: %d", r.status)
	}
	r := e.get("/_mbongo/phone/mtn/242060000000")
	if r.status != http.StatusNotFound || !strings.Contains(r.body, "has no MTN MoMo wallet") {
		t.Errorf("unknown number: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/_mbongo/phone/p/9", "/_mbongo/phone/p/x", "/_mbongo/phone/p/9/events"} {
		if r := e.get(path); r.status != http.StatusNotFound {
			t.Errorf("%s: %d", path, r.status)
		}
	}
}

func TestHomeScreenShowsTheApps(t *testing.T) {
	e := setup(t)
	e.open()
	r := e.get(page)
	for _, want := range []string{
		`href="` + page + `?app=messages"`, `href="` + page + `?app=sim1"`, "MTN MoMo", "SIM 1: MTN MoMo " + number,
		`class="home-clock" data-clock>10:00`, `src="/_mbongo/phone/static/phone.js"`, `data-events="` + page + `/events"`,
		`id="prompt" data-tx=""`,
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(r.body, "?app=sim2") {
		t.Error("a one-SIM phone shows a second SIM")
	}
	csp := r.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", r.header)
	}
	if r := e.get(page + "?app=sim7"); !strings.Contains(r.body, `class="home-clock"`) {
		t.Error("an unknown app should show the home screen")
	}
}

func TestPromptPopsUpOverAnyScreen(t *testing.T) {
	e := setup(t)
	e.open()
	first := e.collect(2_500)
	e.clock.Advance(30 * time.Second)
	e.collect(100)
	for _, app := range []string{"", "?app=messages", "?app=sim1"} {
		r := e.get(page + app)
		for _, want := range []string{
			`data-tx="` + first.ID + `"`, "SIM 1 · MTN MoMo",
			"Acme &lt;b&gt;Pay&lt;/b&gt;", "2\u00a0500\u00a0XAF", "Order &lt;7&gt;",
			"Expires at 10:05:00", `data-left="270"`,
			`action="` + page + `/prompts/` + first.ID + `"`, "1 more waiting",
		} {
			if !strings.Contains(r.body, want) {
				t.Errorf("%q lacks %q", app, want)
			}
		}
		if strings.Contains(r.body, "<b>Pay") {
			t.Error("payee not escaped")
		}
	}
	wallet := e.get(page + "?app=sim1").body
	for _, want := range []string{"10\u00a0000\u00a0XAF", "−100\u00a0XAF", "waiting", "Approvals", `value="show"`} {
		if !strings.Contains(wallet, want) {
			t.Errorf("wallet lacks %q", want)
		}
	}
	if !strings.Contains(e.get(page).body, `aria-label="2 waiting"`) {
		t.Error("the wallet app shows no waiting count")
	}
}

func TestStuckPaymentHasNoCountdown(t *testing.T) {
	e := setup(t)
	e.open()
	if _, err := e.books.Request(e.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN, Merchant: "acme",
		Phone: number, Amount: 1, Currency: "XAF", PartnerRef: "stuck", Window: 100 * 365 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if r := e.get(page); !strings.Contains(r.body, "No time limit") || strings.Contains(r.body, "data-left") {
		t.Fatalf("stuck prompt: %s", r.body)
	}
}

func TestApprove(t *testing.T) {
	e := setup(t)
	e.open()
	tx := e.collect(2_500)
	r := e.answer(tx, url.Values{"action": {"approve"}, "app": {"evil"}})
	if r.location != page+"?notice=approved" || e.status(tx.ID) != bank.Successful {
		t.Fatalf("approve: %q %s", r.location, e.status(tx.ID))
	}
	after := e.get(page + "?notice=approved&app=sim1").body
	if !strings.Contains(after, "Payment approved") || !strings.Contains(after, "7\u00a0500\u00a0XAF") ||
		!strings.Contains(after, `data-tx=""`) {
		t.Fatalf("after approval: %s", after)
	}
	if r := e.answer(tx, url.Values{"action": {"refuse"}}); r.location != page+"?notice=decided" {
		t.Fatalf("answer twice: %q", r.location)
	}
	if len(e.mtn.answers) != 1 {
		t.Fatalf("operator asked %d times", len(e.mtn.answers))
	}
}

func TestRefuse(t *testing.T) {
	e := setup(t)
	e.open()
	tx := e.collect(2_500)
	r := e.answer(tx, url.Values{"action": {"refuse"}, "app": {"sim1"}})
	if r.location != page+"?notice=refused&app=sim1" || e.status(tx.ID) != bank.Failed {
		t.Fatalf("refuse: %q %s", r.location, e.status(tx.ID))
	}
	if !strings.Contains(e.get(page+"?app=sim1").body, "refused") {
		t.Fatal("history does not say refused")
	}
}

func TestIgnoredPromptWaitsUnderApprovals(t *testing.T) {
	e := setup(t)
	e.open()
	first := e.collect(2_500)
	second := e.collect(100)
	r := e.answer(first, url.Values{"action": {"ignore"}})
	if r.location != page+"?notice=ignored" || e.status(first.ID) != bank.Pending {
		t.Fatalf("ignore: %q %s", r.location, e.status(first.ID))
	}
	body := e.get(page).body
	if strings.Contains(body, `data-tx="`+first.ID+`"`) || !strings.Contains(body, `data-tx="`+second.ID+`"`) ||
		strings.Contains(body, "more waiting") {
		t.Fatalf("after ignore: %s", body)
	}
	if !strings.Contains(e.get(page+"?app=sim1").body, `action="`+page+`/prompts/`+first.ID+`"`) {
		t.Fatal("ignored payment missing from Approvals")
	}
	_ = e.answer(second, url.Values{"action": {"ignore"}})
	if r := e.answer(first, url.Values{"action": {"show"}, "app": {"sim1"}}); r.location != page+"?notice=&app=sim1" {
		t.Fatalf("show: %q", r.location)
	}
	if !strings.Contains(e.get(page).body, `data-tx="`+first.ID+`"`) {
		t.Fatal("shown prompt does not pop up")
	}
	e.clock.Advance(5 * time.Minute)
	if e.status(first.ID) != bank.Expired {
		t.Fatalf("ignored prompt: %s", e.status(first.ID))
	}
	if body := e.get(page).body; !strings.Contains(body, `data-tx=""`) {
		t.Fatal("expired prompt still shown")
	}
	if len(e.mtn.answers) != 0 {
		t.Fatalf("ignore answered the operator: %v", e.mtn.answers)
	}
}

func TestOnlyThisPhonesCollectionsCanBeAnswered(t *testing.T) {
	e := setup(t)
	e.open()
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242069999999", "XAF", 10_000); err != nil {
		t.Fatal(err)
	}
	other := e.request(bank.MTN, "242069999999", bank.Collection, 1)
	payout := e.request(bank.MTN, number, bank.Payout, 1)
	airtel := e.request(bank.Airtel, airtelN, bank.Collection, 1)
	for _, tx := range []bank.Transaction{other, payout, airtel, {ID: "nope"}} {
		if r := e.answer(tx, url.Values{"action": {"refuse"}}); r.status != http.StatusNotFound {
			t.Errorf("%s: %d", tx.ID, r.status)
		}
	}
	if e.status(other.ID) != bank.Pending || e.status(payout.ID) != bank.Pending || e.status(airtel.ID) != bank.Pending {
		t.Fatal("a foreign transaction was decided")
	}
	tx := e.collect(1)
	if r := e.answer(tx, url.Values{"action": {"steal"}}); r.status != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", r.status)
	}
	e.clock.Advance(5 * time.Minute)
	if r := e.answer(tx, url.Values{"action": {"approve"}}); r.location != page+"?notice=decided" {
		t.Fatalf("expired: %q", r.location)
	}
}

func TestSMSForEveryMoneyMove(t *testing.T) {
	e := setup(t)
	e.open()
	cases := []struct {
		do   func()
		want string
	}{
		{func() { tx := e.collect(2_500); _ = e.answer(tx, url.Values{"action": {"approve"}}) },
			"Vous avez payé 2 500 FCFA à Acme. Frais: 0 FCFA. Nouveau solde: 7 500 FCFA. Ref: REF-ref-b."},
		{func() { tx := e.collect(100); _ = e.answer(tx, url.Values{"action": {"refuse"}}) },
			"Vous avez refusé le paiement de 100 FCFA demandé par Acme."},
		{func() { tx := e.collect(99_999); _ = e.answer(tx, url.Values{"action": {"approve"}}) },
			"Echec du paiement de 99 999 FCFA à Acme: solde insuffisant. Solde: 7 500 FCFA."},
		{func() { e.collect(300); e.clock.Advance(5 * time.Minute); _, _ = e.books.ExpireDue(e.ctx) },
			"La demande de paiement de 300 FCFA de Acme a expiré sans réponse."},
		{func() {
			tx := e.request(bank.MTN, number, bank.Payout, 1_000)
			_, _ = e.books.Approve(e.ctx, tx.ID)
		}, "Vous avez reçu 1 000 FCFA de Acme. Nouveau solde: 8 500 FCFA. Ref: REF-ref-f."},
	}
	for i, c := range cases {
		c.do()
		thread := e.get(page + "?app=messages&sim=1").body
		if !strings.Contains(thread, c.want) {
			t.Fatalf("case %d: thread lacks %q:\n%s", i, c.want, thread)
		}
	}
	tx := e.request(bank.MTN, number, bank.Payout, 1)
	_, _ = e.books.Refuse(e.ctx, tx.ID, "internal_processing_error")
	if n := strings.Count(e.get(page+"?app=messages&sim=1").body, `class="bubble`); n != 5 {
		t.Fatalf("%d messages, want 5: a failed payout sends none", n)
	}
}

func TestUnreadMessagesShowUntilRead(t *testing.T) {
	e := setup(t)
	e.open()
	tx := e.collect(100)
	_ = e.answer(tx, url.Values{"action": {"refuse"}})
	if !strings.Contains(e.get(page).body, `aria-label="1 unread"`) {
		t.Fatal("no unread badge on the home screen")
	}
	list := e.get(page + "?app=messages").body
	if !strings.Contains(list, "MoMo") || !strings.Contains(list, "Vous avez refusé") || !strings.Contains(list, `avatar avatar-mtn`) ||
		!strings.Contains(list, `class="badge">1<`) {
		t.Fatalf("threads: %s", list)
	}
	if !strings.Contains(e.get(page+"?app=messages&sim=1").body, `class="bubble unread"`) {
		t.Fatal("the new message is not marked on first read")
	}
	if body := e.get(page).body; strings.Contains(body, "unread") {
		t.Fatal("badge stays after reading")
	}
	if strings.Contains(e.get(page+"?app=messages&sim=1").body, `class="bubble unread"`) {
		t.Fatal("still unread on the second visit")
	}
}

func TestReadingOneSIMLeavesTheOtherUnread(t *testing.T) {
	e := setup(t)
	e.open()
	if _, err := e.svc.OpenInPhoneOf(e.ctx, bank.Airtel, airtel2, "XAF", 5_000, number); err != nil {
		t.Fatal(err)
	}
	for _, tx := range []bank.Transaction{e.collect(1), e.request(bank.Airtel, airtel2, bank.Collection, 1)} {
		_ = e.answer(tx, url.Values{"action": {"refuse"}})
	}
	if !strings.Contains(e.get(page).body, `aria-label="2 unread"`) {
		t.Fatal("want 2 unread")
	}
	_ = e.get(page + "?app=messages&sim=1")
	if !strings.Contains(e.get(page).body, `aria-label="1 unread"`) {
		t.Fatal("reading SIM 1 read SIM 2 too")
	}
}

func (e *env) phones() int {
	e.t.Helper()
	var n int
	if err := e.books.DB().QueryRowContext(e.ctx, "SELECT COUNT(*) FROM phones").Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestASecondSIMOpensInAPhone(t *testing.T) {
	e := setup(t)
	e.open()
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242069999999", "XAF", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.OpenInPhoneOf(e.ctx, bank.Airtel, airtel2, "XAF", 5_000, number); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/_mbongo/phone/airtel/" + airtel2); r.location != page {
		t.Fatalf("second SIM opens %q", r.location)
	}
	if e.phones() != 1 {
		t.Fatalf("%d phones, want 1", e.phones())
	}
	if _, err := e.svc.OpenInPhoneOf(e.ctx, bank.MTN, number, "EUR", 0, number); err != nil {
		t.Fatalf("a second wallet on a SIM already there: %v", err)
	}
	if r := e.get(sim); r.location != page || e.phones() != 1 {
		t.Fatalf("the SIM moved or a phone appeared: %q, %d phones", r.location, e.phones())
	}
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "050000777", "XAF", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.books.OpenCustomer(e.ctx, bank.Airtel, "050000777", "XAF", 0); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]error{number: phone.ErrFull, "242060000000": phone.ErrUnknownNumber,
		"050000777": phone.ErrAmbiguous} {
		if _, err := e.svc.OpenInPhoneOf(e.ctx, bank.Airtel, "055000003", "XAF", 0, n); !errors.Is(err, want) {
			t.Errorf("into %s: %v, want %v", n, err, want)
		}
		if w, _ := e.books.Wallets(e.ctx, bank.Airtel, "055000003"); len(w) != 0 {
			t.Fatalf("a refused SIM kept its wallet")
		}
	}
	if _, err := e.svc.OpenInPhoneOf(e.ctx, bank.Airtel, "055000004", "XAF", 0, "242069999999"); err != nil {
		t.Fatalf("into a number without a phone yet: %v", err)
	}

	mtnTx := e.collect(100)
	e.clock.Advance(time.Second)
	airtelTx := e.request(bank.Airtel, airtel2, bank.Collection, 200)
	body := e.get(page).body
	for _, want := range []string{"?app=sim1", "?app=sim2", "Airtel Money", `data-tx="` + mtnTx.ID + `"`, "1 more waiting",
		`class="icon icon-mtn"`, `class="icon icon-airtel"`} {
		if !strings.Contains(body, want) {
			t.Errorf("two-SIM phone lacks %q", want)
		}
	}
	if r := e.answer(airtelTx, url.Values{"action": {"approve"}}); r.location != page+"?notice=approved" ||
		e.status(airtelTx.ID) != bank.Successful {
		t.Fatalf("SIM 2 approve: %q %s", r.location, e.status(airtelTx.ID))
	}
	if !strings.Contains(e.get(page+"?app=sim2").body, "4\u00a0800\u00a0XAF") {
		t.Fatal("SIM 2 balance")
	}
	index := e.get("/_mbongo/phone/").body
	if !strings.Contains(index, "MTN MoMo "+number+" + Airtel Money "+airtel2) || strings.Count(index, airtel2) != 1 ||
		!strings.Contains(index, "MTN MoMo 242069999999 + Airtel Money 055000004") {
		t.Fatalf("index: %s", index)
	}
}

func TestListFeedAnnouncesNewPayments(t *testing.T) {
	e := setup(t)
	e.collect(100)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.server.URL+"/_mbongo/phone/events", nil)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("feed: %d %v", res.StatusCode, res.Header)
	}
	lines := make(chan string, 16)
	go func() {
		s := bufio.NewScanner(res.Body)
		for s.Scan() {
			if l := s.Text(); l != "" && !strings.HasPrefix(l, ":") {
				lines <- l
			}
		}
		close(lines)
	}()
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(2 * time.Second):
			return ""
		}
	}
	e.request(bank.Airtel, airtelN, bank.Collection, 1_500)
	if l := next(); l != "event: prompt" {
		t.Fatalf("event: %q", l)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(next(), "data: ")), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"url": "/_mbongo/phone/airtel/" + airtelN, "number": airtelN, "operator": "Airtel Money",
		"payee": "Acme <b>Pay</b>", "amount": "1 500 XAF"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242069999999", "XAF", 5); err != nil {
		t.Fatal(err)
	}
	if l := next(); l != "" {
		t.Fatalf("a change without a new payment was announced: %q", l)
	}
	e.stop()
	select {
	case _, open := <-lines:
		for open {
			_, open = <-lines
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the feed outlived the shutdown")
	}
}

func TestOnlyPromptsCanBeAnswered(t *testing.T) {
	e := setup(t)
	e.open()
	cashOut, err := e.books.Request(e.ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN, Merchant: "acme",
		Phone: number, Amount: 1, Currency: "XAF", PartnerRef: "cash", ExternalID: "cashout"})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"approve", "refuse", "ignore", "show"} {
		if r := e.answer(cashOut, url.Values{"action": {action}}); r.status != http.StatusNotFound {
			t.Errorf("%s a cash-out: %d", action, r.status)
		}
	}
	if e.status(cashOut.ID) != bank.Pending || len(e.mtn.answers) != 0 {
		t.Fatal("a cash-out was answered")
	}
}

func TestTheLastSecondStillCountsDown(t *testing.T) {
	e := setup(t)
	e.open()
	e.collect(100)
	e.clock.Advance(5*time.Minute - 300*time.Millisecond)
	body := e.get(page).body
	if strings.Contains(body, "No time limit") || !strings.Contains(body, `data-left="1"`) {
		t.Fatalf("0.3 s before expiry: %s", body)
	}
}

func TestStaticFiles(t *testing.T) {
	e := setup(t)
	r := e.get("/_mbongo/phone/static/phone.js")
	if r.status != http.StatusOK || !strings.Contains(r.header.Get("Content-Type"), "javascript") ||
		!strings.Contains(r.body, "EventSource") || !strings.Contains(r.body, "Notification") {
		t.Fatalf("js: %d %v", r.status, r.header)
	}
	if r := e.get("/_mbongo/phone/static/phone.css"); r.status != http.StatusOK ||
		!strings.Contains(r.header.Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d", r.status)
	}
	if r := e.get("/_mbongo/phone/static/nope.js"); r.status != http.StatusNotFound {
		t.Fatalf("missing file: %d", r.status)
	}
}

func TestEventsSayWhenThePageChanges(t *testing.T) {
	e := setup(t)
	e.open()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.server.URL+page+"/events", nil)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %v", res.StatusCode, res.Header)
	}
	lines := make(chan string, 16)
	go func() {
		s := bufio.NewScanner(res.Body)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
	}()
	next := func() string {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatal("stream closed")
				}
				if l != "" && !strings.HasPrefix(l, ":") {
					return l
				}
			case <-time.After(3 * time.Second):
				return ""
			}
		}
	}

	if _, err := e.books.OpenCustomer(e.ctx, bank.MTN, "242069999999", "XAF", 5); err != nil {
		t.Fatal(err)
	}
	e.collect(100)
	if l := next(); l != "event: change" {
		t.Fatalf("after a new prompt: %q", l)
	}
	if l := next(); l != "data: " {
		t.Fatalf("data line: %q", l)
	}
	if l := next(); l != "" {
		t.Fatalf("another number's wallet changed this page: %q", l)
	}
	e.stop()
	select {
	case _, open := <-lines:
		for open {
			_, open = <-lines
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream outlived the shutdown")
	}
}
