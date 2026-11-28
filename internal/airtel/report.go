package airtel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/money"
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
		if e.Status == http.StatusOK {
			c.Client, c.Token = in.ClientID, report.Fingerprint(answer.AccessToken)
		}
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
			Status      string `json:"status"`
			Transaction struct {
				Status        string `json:"status"`
				AirtelMoneyID string `json:"airtel_money_id"`
			} `json:"transaction"`
		} `json:"data"`
		Status struct {
			Success *bool `json:"success"`
		} `json:"status"`
	}
	_ = json.Unmarshal(e.Answer, &answer)
	status := answer.Data.Transaction.Status
	if status == "" {
		status = answer.Data.Status
	}
	if r.Method == http.MethodGet {
		c.Reference, c.Outcome = r.PathValue("id"), seenAs(status)
		return c
	}
	var p paymentBody
	if json.Unmarshal(e.Body, &p) != nil {
		return c
	}
	// The body's reference is the text the customer reads, not an order number, so Order stays empty.
	c.Reference, c.Amount = p.Transaction.ID, string(p.Transaction.Amount)
	c.Currency = r.Header.Get("X-Currency")
	c.Phone = p.Subscriber.MSISDN
	if c.Phone == "" {
		c.Phone = p.Payee.MSISDN
	}
	if strings.HasSuffix(r.URL.Path, "/refund") {
		// A refund is known by its own Airtel Money ID, the one its answer gives; without an
		// answer, by the newest refund of that payment.
		c.RefundOf, c.Reference = p.Transaction.AirtelMoneyID, answer.Data.Transaction.AirtelMoneyID
		if c.Reference == "" && (e.Status == 0 || e.Status >= http.StatusInternalServerError) {
			c.Reference = s.store.newestRefund(ctx, c.Client, c.RefundOf)
		}
	}
	if e.Status == http.StatusOK && answer.Status.Success != nil {
		c.Outcome = answered(r.URL.Path, *answer.Status.Success, status)
	}
	return c
}

// answered is what the answer to a new payment tells the app: a collection waits for the
// customer, cash-out, cash-in, payouts and refunds settle at once unless still in progress.
func answered(path string, success bool, status string) string {
	switch {
	case !success:
		return report.Refused
	case strings.HasPrefix(path, "/merchant/"), status == "TIP", status == "TA":
		return report.Pending
	}
	return report.Successful
}

// Outcome is what the app would learn about one of its payments if it asked now.
func (s *Service) Outcome(ctx context.Context, clientID, id, operation string) (string, error) {
	p, err := s.store.payment(ctx, clientID, id, operation)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	st, err := s.outcome(ctx, p)
	return seenAs(st.status), err
}

// RefundTarget is the payment an Airtel Money ID names, among the app's own.
func (s *Service) RefundTarget(ctx context.Context, clientID, moneyID string) (report.Target, bool, error) {
	p, err := s.store.paymentByMoneyID(ctx, clientID, moneyID, kindCollection, kindCashOut, kindCashIn,
		kindDisbursement, kindRefund)
	if errors.Is(err, ErrNotFound) {
		return report.Target{}, false, nil
	}
	if err != nil {
		return report.Target{}, false, err
	}
	amount, err := money.Format(p.Amount, currency)
	return report.Target{Reference: p.ID, Operation: p.Kind, Amount: amount, Currency: currency}, true, err
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

// ClientName is the app's name.
func (s *Service) ClientName(ctx context.Context, clientID string) (string, error) {
	app, err := s.store.app(ctx, clientID)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return app.Name, err
}
