package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARecoveredPanicIsLoggedAsA500(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	panics := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := logRequests(logger, Recover(slog.New(slog.NewTextHandler(io.Discard, nil)))(panics))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if !strings.Contains(logs.String(), "status=500") {
		t.Fatalf("logs = %s", logs.String())
	}
}
