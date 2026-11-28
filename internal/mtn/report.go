package mtn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
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
		if e.Status != http.StatusOK {
			return c
		}
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

// Outcome is what the API user would learn about one of its payments if it asked now.
func (s *Service) Outcome(ctx context.Context, user, ref, _ string) (string, error) {
	r, err := s.store.request(ctx, user, ref)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	u, err := s.store.apiUser(ctx, user)
	if err != nil {
		return "", err
	}
	op, err := operationNamed(r.Operation)
	if err != nil {
		return "", err
	}
	body, err := s.statusBody(ctx, u, op, r)
	return seenAs(body.Status), err
}

// RefundTarget is the payment a reference names among the partner's API users, as a refund
// may name a collection another of them took.
func (s *Service) RefundTarget(ctx context.Context, user, ref string) (report.Target, bool, error) {
	var t report.Target
	err := s.store.db.QueryRowContext(ctx, `SELECT r.reference_id, r.operation, r.amount, r.currency
		FROM mtn_requests r
		JOIN mtn_api_users u ON u.id = r.api_user
		JOIN mtn_subscriptions s ON s.key = u.subscription
		JOIN mtn_api_users me ON me.id = ?
		JOIN mtn_subscriptions mine ON mine.key = me.subscription
		WHERE r.reference_id = ? AND s.partner = mine.partner AND u.environment = me.environment
		ORDER BY r.api_user = me.id DESC LIMIT 1`, user, ref).Scan(&t.Reference, &t.Operation, &t.Amount, &t.Currency)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// ClientName is the API user's partner and product, as the partner portal shows them.
func (s *Service) ClientName(ctx context.Context, user string) (string, error) {
	var partner, key, product string
	err := s.store.db.QueryRowContext(ctx, `SELECT s.partner, s.key, s.product FROM mtn_api_users u
		JOIN mtn_subscriptions s ON s.key = u.subscription WHERE u.id = ?`, user).Scan(&partner, &key, &product)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if partner == key {
		return product, nil
	}
	return partner + ", " + product, nil
}
