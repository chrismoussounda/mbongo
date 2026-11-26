// Package server holds Mbongo's HTTP routes and the middleware around them.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Pinger reports whether storage answers.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the whole HTTP handler.
func New(logger *slog.Logger, version string, store Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health(logger, version, store))
	return Recover(logger)(logRequests(logger, mux))
}

func health(logger *slog.Logger, version string, store Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			logger.Error("storage unreachable", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "storage unreachable", "version": version})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Recover turns a panic in a handler into a 500 instead of a dropped connection.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic", "err", err, "method", r.Method, "path", r.URL.Path)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}
