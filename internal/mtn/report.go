package mtn

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/report"
)

// WithReport records the calls API users make and the callbacks they get.
func (s *Service) WithReport(rec *report.Recorder) *Service {
	s.journal = rec
	return s
}

// describe says which API user made a call and about which payment.
func (s *Service) describe(ctx context.Context, e report.Exchange) report.Call {
	r := e.Request
	var c report.Call
	switch {
	case strings.HasSuffix(r.URL.Path, "/token/"):
		c.Client, _, _ = r.BasicAuth()
		var answer struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(e.Answer, &answer) == nil {
			c.Token = report.Fingerprint(answer.AccessToken)
		}
		return c
	case strings.HasPrefix(r.URL.Path, "/v1_0/apiuser"):
		c.Client = r.PathValue("id")
		if c.Client == "" {
			c.Client = r.Header.Get("X-Reference-Id")
		}
		return c
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c.Token = report.Fingerprint(token)
	if user, _, _, err := s.store.token(ctx, token); err == nil {
		c.Client = user
	} else {
		c.Client = s.journal.ClientOf(ctx, "mtn", c.Token)
	}
	if ref := r.PathValue("ref"); ref != "" {
		c.Reference = ref
		var answer struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(e.Answer, &answer) == nil {
			c.Outcome = seenAs(answer.Status)
		}
		return c
	}
	c.Reference = r.Header.Get("X-Reference-Id")
	var p paymentBody
	if json.Unmarshal(e.Body, &p) != nil {
		return c
	}
	c.Order, c.RefundOf, c.Amount, c.Currency = p.ExternalID, p.ReferenceIDToRefund, p.Amount, p.Currency
	c.Phone = p.Payer.PartyID
	if c.Phone == "" {
		c.Phone = p.Payee.PartyID
	}
	return c
}

// seenAs is what an MTN status means to the client.
func seenAs(status string) string {
	switch status {
	case "PENDING":
		return report.Pending
	case "SUCCESSFUL":
		return report.Successful
	case "FAILED":
		return report.Failed
	}
	return ""
}
