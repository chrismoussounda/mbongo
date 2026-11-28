package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/chrismoussounda/mbongo/internal/report"
)

func (h *handler) ledgers() map[string]report.Ledger {
	return map[string]report.Ledger{"mtn": h.mtn, "airtel": h.airtel}
}

// report answers 200 whatever it finds, so a test reads the counts instead of the status.
func (h *handler) report(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var run int64
	if raw := q.Get("run"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			h.refuse(w, "run must be a run number")
			return
		}
		run = n
	}
	s, err := h.journal.Summarize(r.Context(), run, h.ledgers(), q.Get("operator"), q.Get("client"))
	if errors.Is(err, report.ErrNoRun) {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such run"})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusOK, s)
}

func (h *handler) newRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.journal.NewRun(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, run)
}

func (h *handler) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := h.journal.Runs(r.Context())
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusOK, runs)
}
