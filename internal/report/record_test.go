package report_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/report"
)

func setup(t *testing.T) (*report.Recorder, *clock.Manual) {
	t.Helper()
	ctx := context.Background()
	c := clock.NewManual(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "r.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = books.Close() })
	rec, err := report.New(ctx, books.DB(), c)
	if err != nil {
		t.Fatal(err)
	}
	return rec, c
}

// describe says who called from a header and copies the body and answer it was given.
func describe(seen *report.Exchange) report.Describe {
	return func(_ context.Context, e report.Exchange) report.Call {
		*seen = e
		return report.Call{Client: e.Request.Header.Get("X-Client"), Reference: string(e.Body),
			Outcome: string(e.Answer)}
	}
}

func serve(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/pay", strings.NewReader(body))
	r.Header.Set("X-Client", "app-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func onlyCall(t *testing.T, rec *report.Recorder, run int64) report.Call {
	t.Helper()
	calls, err := rec.Calls(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %+v", calls)
	}
	return calls[0]
}

func TestRecordsACallAndLeavesItAlone(t *testing.T) {
	rec, c := setup(t)
	var seen report.Exchange
	h := rec.Wrap("mtn", "requesttopay", describe(&seen), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.Advance(250 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(append([]byte("got "), b...))
	}))
	w := serve(h, "ref-1")
	if w.Code != http.StatusAccepted || w.Body.String() != "got ref-1" {
		t.Fatalf("the client's answer changed: %d %q", w.Code, w.Body)
	}
	got := onlyCall(t, rec, 1)
	if got.Client != "app-1" || got.Reference != "ref-1" || got.Outcome != "got ref-1" || got.Status != http.StatusAccepted ||
		got.Operator != "mtn" || got.Operation != "requesttopay" || got.Method != http.MethodPost || got.Path != "/pay" ||
		got.Millis != 250 || got.Run != 1 {
		t.Fatalf("recorded %+v", got)
	}
	if seen.Status != http.StatusAccepted {
		t.Fatalf("describe saw status %d", seen.Status)
	}
}

func TestADroppedAnswerIsRecordedAsNone(t *testing.T) {
	rec, _ := setup(t)
	var seen report.Exchange
	h := rec.Wrap("airtel", "collection", describe(&seen), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if err := recover(); err != http.ErrAbortHandler { //nolint:errorlint // recover returns the value itself
			t.Fatalf("panic not passed on: %v", err)
		}
		if got := onlyCall(t, rec, 1); got.Status != 0 {
			t.Fatalf("status %d", got.Status)
		}
	}()
	serve(h, "x")
}

func TestAFaultIsNoted(t *testing.T) {
	rec, _ := setup(t)
	var seen report.Exchange
	h := rec.Wrap("mtn", "transfer", describe(&seen), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		report.NoteFault(r.Context(), "rate_limited")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	serve(h, "x")
	if got := onlyCall(t, rec, 1); got.Fault != "rate_limited" || got.Status != http.StatusTooManyRequests {
		t.Fatalf("recorded %+v", got)
	}
}

func TestAnAnswerWithoutHeaderIs200(t *testing.T) {
	rec, _ := setup(t)
	var seen report.Exchange
	h := rec.Wrap("mtn", "balance", describe(&seen), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	serve(h, "")
	if got := onlyCall(t, rec, 1); got.Status != http.StatusOK {
		t.Fatalf("status %d", got.Status)
	}
}

func TestAnEmptyAnswerIs200UnlessTheClientHungUp(t *testing.T) {
	rec, _ := setup(t)
	var seen report.Exchange
	h := rec.Wrap("mtn", "balance", describe(&seen), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve(h, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/balance", nil))
	calls, err := rec.Calls(context.Background(), 1)
	if err != nil || len(calls) != 2 || calls[0].Status != http.StatusOK || calls[1].Status != 0 {
		t.Fatalf("calls %+v %v", calls, err)
	}
}

func TestRunsKeepTheirOwnTraffic(t *testing.T) {
	rec, c := setup(t)
	ctx := context.Background()
	var seen report.Exchange
	h := rec.Wrap("mtn", "status", describe(&seen), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve(h, "first")
	c.Advance(time.Minute)
	run, err := rec.NewRun(ctx)
	if err != nil || run.ID != 2 {
		t.Fatalf("new run %+v %v", run, err)
	}
	serve(h, "second")
	if err := rec.Callback(ctx, report.Callback{Operator: "mtn", Client: "app-1", Reference: "second",
		Outcome: report.Successful, HTTPStatus: 500, At: c.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := onlyCall(t, rec, 1); got.Reference != "first" {
		t.Fatalf("run 1: %+v", got)
	}
	if got := onlyCall(t, rec, 2); got.Reference != "second" {
		t.Fatalf("run 2: %+v", got)
	}
	cbs, err := rec.Callbacks(ctx, 2)
	if err != nil || len(cbs) != 1 || cbs[0].HTTPStatus != 500 || cbs[0].Outcome != report.Successful {
		t.Fatalf("callbacks %+v %v", cbs, err)
	}
	if cbs, _ := rec.Callbacks(ctx, 1); len(cbs) != 0 {
		t.Fatalf("run 1 callbacks %+v", cbs)
	}
	runs, err := rec.Runs(ctx)
	if err != nil || len(runs) != 2 || runs[0].ID != 2 || !runs[0].Started.Equal(c.Now()) {
		t.Fatalf("runs %+v %v", runs, err)
	}
	if latest, err := rec.Latest(ctx); err != nil || latest != 2 {
		t.Fatalf("latest %d %v", latest, err)
	}
	if _, err := rec.Calls(ctx, 9); err != report.ErrNoRun { //nolint:errorlint // returned as is
		t.Fatalf("unknown run: %v", err)
	}
}

func TestNilRecorderRecordsNothing(t *testing.T) {
	var rec *report.Recorder
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if h := rec.Wrap("mtn", "x", nil, inner); h == nil {
		t.Fatal("no handler")
	}
	if err := rec.Callback(context.Background(), report.Callback{}); err != nil {
		t.Fatal(err)
	}
}
