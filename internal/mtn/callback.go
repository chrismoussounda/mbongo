package mtn

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/chrismoussounda/mbongo/internal/faults"
)

// notify sends at most one callback, in the background, unless a callback rule says
// otherwise: never, twice, late, or before the answer to the request that caused it.
func (s *Service) notify(req request) {
	if req.CallbackURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	rule, _, err := s.faults.Take(ctx, "mtn", faults.Callback, req.PartyID)
	cancel()
	if err != nil {
		s.logger.Error("callback rule", "err", err)
	}
	switch rule.Fault {
	case faults.CallbackNever:
	case faults.CallbackBeforeAnswer:
		s.deliver(req, 1)
	case faults.CallbackLate:
		s.books.Clock().AfterFunc(rule.Delay, func() { s.sends.Go(func() { s.deliver(req, 1) }) })
	case faults.CallbackTwice:
		s.sends.Go(func() { s.deliver(req, 2) })
	default:
		s.sends.Go(func() { s.deliver(req, 1) })
	}
}

// deliver sends the callback times times, if no other call has sent it.
func (s *Service) deliver(req request, times int) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, err := s.store.markNotified(ctx, req.APIUser, req.ReferenceID)
	if err != nil || !first {
		return
	}
	u, err := s.store.apiUser(ctx, req.APIUser)
	if err != nil {
		s.logger.Error("callback", "err", err)
		return
	}
	fresh, err := s.store.request(ctx, req.APIUser, req.ReferenceID)
	if err != nil {
		s.logger.Error("callback", "err", err)
		return
	}
	op, err := operationNamed(fresh.Operation)
	if err != nil {
		s.logger.Error("callback", "err", err)
		return
	}
	body, err := s.statusBody(ctx, u, op, fresh)
	if err != nil {
		s.logger.Error("callback", "err", err)
		return
	}
	raw, _ := json.Marshal(body)
	for range times {
		if err := s.send(ctx, req.CallbackURL, raw); err != nil {
			s.logger.Info("callback not delivered, not retried", "ref", req.ReferenceID, "err", err)
		}
	}
}

// Wait returns once callbacks already started are delivered or have failed. Late callbacks
// still waiting for their time are not waited for.
func (s *Service) Wait() { s.sends.Wait() }

// answers are MTN's own replies to the faults Mbongo fakes. The gateway's 429 is Azure API
// Management's; the other bodies are MTN's usual error shape.
type answers struct{}

func (answers) RateLimited(w http.ResponseWriter) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"statusCode": 429,
		"message": "Rate limit is exceeded. Try again in 1 seconds."})
}

func (answers) Maintenance(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "The service is temporarily unavailable.")
}

func (answers) TokenRevoked(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "Access token is invalid or expired.")
}

func (answers) Internal(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "INTERNAL_PROCESSING_ERROR", "An internal error occurred while processing.")
}
