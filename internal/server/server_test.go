package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chrismoussounda/mbongo/internal/server"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func newServer() http.Handler {
	return server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", pinger{})
}

func TestHealthFailsWhenStorageIsDown(t *testing.T) {
	h := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), "test", pinger{errors.New("gone")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHealthSaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != "test" {
		t.Errorf("body = %v", body)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nowhere", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHealthRefusesPost(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestPanicBecomes500(t *testing.T) {
	h := server.Recover(slog.New(slog.NewTextHandler(io.Discard, nil)))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAbortedHandlerStaysAborted(t *testing.T) {
	h := server.Recover(quiet())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // comparing the panic value
			t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestPanicAfterTheResponseStartedAborts(t *testing.T) {
	h := server.Recover(quiet())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("half"))
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // comparing the panic value
			t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
		}
		if rec.Body.String() != "half" {
			t.Fatalf("body = %q, a 500 was written after the response started", rec.Body.String())
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestHandlersCanStillFlush(t *testing.T) {
	h := server.Recover(quiet())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	}))
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !rec.flushed {
		t.Fatal("the real writer was never flushed")
	}
}
