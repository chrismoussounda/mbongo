package airtel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"
)

type callbackTransaction struct {
	ID            string `json:"id"`
	Message       string `json:"message"`
	StatusCode    string `json:"status_code"`
	AirtelMoneyID string `json:"airtel_money_id"`
}

// notify posts the outcome to the app's one callback URL, once, in the background. With a
// callback key, the body also carries hash: HMAC-SHA256 of the transaction object, in Base64.
func (s *Service) notify(p payment) {
	s.sends.Go(func() {
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
		if err := s.send(ctx, app.CallbackURL, raw); err != nil {
			s.logger.Info("callback not delivered, not retried", "id", p.ID, "err", err)
		}
	})
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
