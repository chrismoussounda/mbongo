package airtel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/report"
)

type callbackTransaction struct {
	ID            string `json:"id"`
	Message       string `json:"message"`
	StatusCode    string `json:"status_code"`
	AirtelMoneyID string `json:"airtel_money_id"`
}

// notify posts the outcome to the app's one callback URL, once, in the background. With a
// callback key, the body also carries hash: HMAC-SHA256 of the transaction object, in Base64.
// A callback rule can make it never come, come twice, late, or before the answer; an
// ambiguous payment's callback waits until its outcome shows.
func (s *Service) notify(p payment) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	rule, _, err := s.faults.Take(ctx, "airtel", faults.Callback, p.MSISDN)
	cancel()
	if err != nil {
		s.logger.Error("callback rule", "err", err)
	}
	var wait time.Duration
	if rule.Fault == faults.CallbackLate {
		wait = rule.Delay
	}
	if until, err := time.Parse(bank.TimeLayout, p.AmbiguousUntil); err == nil {
		wait += until.Sub(s.books.Clock().Now())
	}
	times := 1
	if rule.Fault == faults.CallbackTwice {
		times = 2
	}
	switch {
	case rule.Fault == faults.CallbackNever:
	case wait > 0:
		s.books.Clock().AfterFunc(wait, func() { s.sends.Go(func() { s.deliver(p, times, rule.Fault) }) })
	case rule.Fault == faults.CallbackBeforeAnswer:
		s.deliver(p, times, rule.Fault)
	default:
		s.sends.Go(func() { s.deliver(p, times, rule.Fault) })
	}
}

// deliver posts the callback times times, if the payment has an outcome and no other call
// has sent it, and records each sending with the fault that shaped it.
func (s *Service) deliver(p payment, times int, fault string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	app, err := s.store.app(ctx, p.ClientID)
	if err != nil || app.CallbackURL == "" {
		return
	}
	st, err := s.outcome(ctx, p)
	if err != nil || (st.status != "TS" && st.status != "TF") {
		return
	}
	first, err := s.store.markNotified(ctx, p.ClientID, p.ID)
	if err != nil || !first {
		return
	}
	tx := callbackTransaction{ID: p.ID, Message: callbackMessage(p, app, st), StatusCode: st.status,
		AirtelMoneyID: p.AirtelMoneyID}
	body := map[string]any{"transaction": tx}
	if app.CallbackKey != "" {
		body["hash"] = callbackHash(app.CallbackKey, tx)
	}
	raw, _ := json.Marshal(body)
	for range times {
		start := s.books.Clock().Now()
		status, err := s.send(ctx, app.CallbackURL, raw)
		cb := report.Callback{Operator: "airtel", Client: p.ClientID, Operation: p.Kind, Reference: p.ID, Outcome: seenAs(st.status),
			HTTPStatus: status, Fault: fault, At: start, Millis: s.books.Clock().Now().Sub(start).Milliseconds()}
		if err != nil {
			cb.Error = err.Error()
			s.logger.Info("callback not delivered, not retried", "id", p.ID, "err", err)
		}
		if err := s.journal.Callback(ctx, cb); err != nil {
			s.logger.Error("report", "err", err)
		}
	}
}

// answers are Airtel's replies to the faults Mbongo fakes, in its usual envelope.
type answers struct{}

func (answers) RateLimited(w http.ResponseWriter) {
	refuse(w, http.StatusTooManyRequests, "429", "Too Many Requests")
}

func (answers) Maintenance(w http.ResponseWriter) {
	refuse(w, http.StatusServiceUnavailable, "503", "Service Unavailable")
}

func (answers) TokenRevoked(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "invalid_token", "error_description": "The access token is invalid or expired"})
}

func (answers) Internal(w http.ResponseWriter) {
	refuse(w, http.StatusInternalServerError, "ESB000001", "Internal error")
}

func callbackHash(key string, tx callbackTransaction) string {
	raw, _ := json.Marshal(tx)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(raw)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
