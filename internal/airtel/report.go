package airtel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/report"
)

// WithReport records the calls apps make and the callbacks they get.
func (s *Service) WithReport(rec *report.Recorder) *Service {
	s.journal = rec
	return s
}

// describe says which app made a call and about which payment.
func (s *Service) describe(ctx context.Context, e report.Exchange) report.Call {
	r := e.Request
	var c report.Call
	if r.URL.Path == "/auth/oauth2/token" {
		var in struct {
			ClientID string `json:"client_id"`
		}
		var answer struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.Unmarshal(e.Body, &in)
		_ = json.Unmarshal(e.Answer, &answer)
		c.Client, c.Token = in.ClientID, report.Fingerprint(answer.AccessToken)
		return c
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c.Token = report.Fingerprint(token)
	if client, err := s.store.tokenOwner(ctx, token, s.books.Clock().Now()); err == nil {
		c.Client = client
	} else {
		c.Client = s.journal.ClientOf(ctx, "airtel", c.Token)
	}
	var answer struct {
		Data struct {
			Transaction struct {
				Status string `json:"status"`
			} `json:"transaction"`
		} `json:"data"`
	}
	if json.Unmarshal(e.Answer, &answer) == nil {
		c.Outcome = seenAs(answer.Data.Transaction.Status)
	}
	if r.Method == http.MethodGet {
		c.Reference = r.PathValue("id")
		return c
	}
	var p paymentBody
	if json.Unmarshal(e.Body, &p) != nil {
		return c
	}
	c.Reference, c.Order, c.Amount = p.Transaction.ID, p.Reference, string(p.Transaction.Amount)
	c.Currency = r.Header.Get("X-Currency")
	c.Phone = p.Subscriber.MSISDN
	if c.Phone == "" {
		c.Phone = p.Payee.MSISDN
	}
	if strings.HasSuffix(r.URL.Path, "/refund") {
		c.RefundOf = p.Transaction.AirtelMoneyID
	}
	return c
}

// seenAs reads Airtel's status codes; TA, the unknown outcome, is still pending to the app.
func seenAs(status string) string {
	switch status {
	case "TIP", "TA":
		return report.Pending
	case "TS":
		return report.Successful
	case "TF", "TE":
		return report.Failed
	}
	return ""
}
