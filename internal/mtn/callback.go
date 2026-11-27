package mtn

import (
	"context"
	"encoding/json"
	"time"
)

// notify sends at most one callback, in the background.
func (s *Service) notify(req request) {
	if req.CallbackURL == "" {
		return
	}
	s.sends.Go(func() {
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
		body, err := s.statusBody(ctx, u, fresh)
		if err != nil {
			s.logger.Error("callback", "err", err)
			return
		}
		raw, _ := json.Marshal(body)
		if err := s.Send(ctx, req.CallbackURL, raw); err != nil {
			s.logger.Info("callback not delivered, not retried", "ref", req.ReferenceID, "err", err)
		}
	})
}

// Wait returns once callbacks already started are delivered or have failed.
func (s *Service) Wait() { s.sends.Wait() }
