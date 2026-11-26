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
	return logRequests(logger, Recover(logger)(mux))
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

// Recover turns a panic in a handler into a 500. If the response had already started,
// or the handler aborted on purpose, it aborts the connection instead of sending a
// half-written body that looks complete.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := asRecorder(w)
			defer func() {
				err := recover()
				if err == nil {
					return
				}
				if err == http.ErrAbortHandler { //nolint:errorlint // recover returns the value itself
					panic(err)
				}
				logger.Error("panic", "err", err, "method", r.Method, "path", r.URL.Path)
				if rec.wrote {
					panic(http.ErrAbortHandler)
				}
				http.Error(rec, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// recorder remembers the status and whether anything was sent.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func asRecorder(w http.ResponseWriter) *recorder {
	if r, ok := w.(*recorder); ok {
		return r
	}
	return &recorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer, to flush for instance.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := asRecorder(w)
		defer func() {
			logger.Info("request", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
	})
}
