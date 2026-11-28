package report

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (s *script) page(path string) (int, string, http.Header) {
	s.t.Helper()
	mux := http.NewServeMux()
	s.rec.Routes(mux, map[string]Ledger{"mtn": s.ledger}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code, w.Body.String(), w.Header()
}

func has(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Fatalf("page lacks %q:\n%s", p, body)
		}
	}
}

func TestTheIndexListsClients(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.ends("r1", Successful)
	s.call(Call{Client: "u2", Operation: "balance"})
	status, body, header := s.page("/_mbongo/report/")
	if status != 200 || !strings.HasPrefix(header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("%d %v", status, header)
	}
	has(t, body, "Run 1", "1 error", "0 warnings", `href="/_mbongo/report/mtn/u1?run=1"`, "name of u1", "MTN MoMo",
		"clean", "What this report cannot see", "POST /_mbongo/report/runs")
}

func TestAClientPageShowsFindingsAndTraffic(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Outcome: "lost", Path: "/collection/v1_0/requesttopay", Method: "POST"})
	s.pay("r2", "o-1", "242061234567", "1000")
	s.callback("r2", Successful, 500)
	s.ends("r2", Successful)
	status, body, _ := s.page("/_mbongo/report/mtn/u1")
	if status != 200 {
		t.Fatalf("%d", status)
	}
	has(t, body, "name of u1", "Money moved twice", "lost_answer_new_reference", "Trust and follow-up",
		"money_unconfirmed", "API hygiene", "callback_failing", "POST /collection/v1_0/requesttopay → no answer",
		"Nothing found.", "<td>none</td>", "<td>500</td>")
}

func TestOldRunsStayReadable(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.ends("r1", Successful)
	if _, err := s.rec.NewRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, latest, _ := s.page("/_mbongo/report/")
	has(t, latest, "Run 2", "No client has called", `href="/_mbongo/report/?run=1"`)
	_, old, _ := s.page("/_mbongo/report/?run=1")
	has(t, old, "Run 1", "1 error")
	_, client, _ := s.page("/_mbongo/report/mtn/u1?run=1")
	has(t, client, "money_unconfirmed", `href="/_mbongo/report/mtn/u1?run=2"`)
}

func TestMissingPages(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	for _, path := range []string{"/_mbongo/report/?run=9", "/_mbongo/report/?run=x", "/_mbongo/report/mtn/nobody",
		"/_mbongo/report/airtel/u1", "/_mbongo/report/mtn/u1?run=9"} {
		if status, body, _ := s.page(path); status != 404 || !strings.Contains(body, "Not found") {
			t.Fatalf("%s: %d", path, status)
		}
	}
	if status, _, _ := s.page("/_mbongo/report/static/report.css"); status != 200 {
		t.Fatalf("css: %d", status)
	}
}

func TestThePageEscapesWhatClientsSend(t *testing.T) {
	s := newScript(t)
	s.call(Call{Client: "<b>x</b>", Operation: "requesttopay", Reference: "<script>alert(1)</script>"})
	_, body, _ := s.page("/_mbongo/report/")
	if strings.Contains(body, "<b>x</b>") {
		t.Fatalf("client id not escaped:\n%s", body)
	}
	_, body, _ = s.page("/_mbongo/report/mtn/%3Cb%3Ex%3C%2Fb%3E")
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("reference not escaped:\n%s", body)
	}
}

func TestAClientPageShowsOnlyItsOwnTraffic(t *testing.T) {
	s := newScript(t)
	s.pay("mine", "o-1", "242061234567", "1000")
	s.call(Call{Client: "u2", Operation: "requesttopay", Reference: "theirs"})
	s.call(Call{Operator: "airtel", Client: "u1", Operation: "collection", Reference: "same-name-other-operator"})
	if err := s.rec.Callback(t.Context(), Callback{Operator: "mtn", Client: "u2", Reference: "their-callback",
		HTTPStatus: 200, At: s.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	_, body, _ := s.page("/_mbongo/report/mtn/u1")
	has(t, body, "mine", "<strong>1</strong>")
	for _, other := range []string{"theirs", "same-name-other-operator", "their-callback", `href="/_mbongo/report/mtn/u1?run=1"`} {
		if strings.Contains(body, other) {
			t.Fatalf("page shows %q", other)
		}
	}
}
