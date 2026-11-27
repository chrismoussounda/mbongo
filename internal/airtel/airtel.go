// Package airtel answers like Airtel Money Congo's Open API. docs/airtel.md says where each
// behaviour comes from.
package airtel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

const (
	tokenTTL       = 180 * time.Second
	approvalWindow = 5 * time.Minute
	country        = "CG"
	currency       = "XAF"
	maxBody        = 64 << 10
	defaultPIN     = "1234"
)

var ErrDecided = errors.New("airtel: only a pending collection can be approved or refused")

type Service struct {
	store  *store
	books  *bank.Bank
	logger *slog.Logger
	send   Sender
	sends  sync.WaitGroup
	keyMu  sync.Mutex
}

type Sender func(ctx context.Context, url string, body []byte) error

// App is what Airtel's portal gives a partner. A CallbackKey turns on the callback hash.
type App struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Name         string `json:"name"`
	CallbackURL  string `json:"callback_url"`
	CallbackKey  string `json:"callback_key,omitempty"`
	PIN          string `json:"pin"`
}

// New sends callbacks with send, or by HTTP POST when send is nil.
func New(ctx context.Context, books *bank.Bank, logger *slog.Logger, send Sender) (*Service, error) {
	st, err := newStore(ctx, books)
	if err != nil {
		return nil, err
	}
	if send == nil {
		send = post(&http.Client{Timeout: 10 * time.Second})
	}
	return &Service{store: st, books: books, logger: logger, send: send}, nil
}

func post(client *http.Client) Sender {
	return func(ctx context.Context, u string, body []byte) error {
		// The URL is the one set for the app through the admin API, as on Airtel's portal.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body)) //nolint:gosec // G704, see above
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req) //nolint:gosec // G704, see above
		if err != nil {
			return err
		}
		return res.Body.Close()
	}
}

func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/oauth2/token", s.issueToken)
	routes := map[string]func(http.ResponseWriter, *http.Request, App){
		"GET /v1/rsa/encryption-keys":         s.encryptionKeys,
		"POST /merchant/v1/payments/{$}":      s.collect,
		"POST /standard/v1/payments/refund":   s.refund,
		"GET /standard/v1/payments/{id}":      s.enquiry(kindCollection, kindCashOut, kindCashIn),
		"POST /standard/v1/cashout/{$}":       s.cashOut,
		"POST /standard/v1/cashin/{$}":        s.cashIn,
		"POST /standard/v1/disbursements/{$}": s.disburse,
		"GET /standard/v1/disbursements/{id}": s.enquiry(kindDisbursement),
		"GET /standard/v1/users/balance":      s.balance,
		"GET /standard/v1/users/{msisdn}":     s.user,
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, s.authorized(h))
	}
}

// CreateApp registers an application with its merchant wallet, as Airtel's portal does.
// An empty pin means 1234.
func (s *Service) CreateApp(ctx context.Context, app App) (App, error) {
	app.ClientID, app.ClientSecret = newUUID(), newUUID()
	if app.PIN == "" {
		app.PIN = defaultPIN
	}
	return app, s.store.addApp(ctx, app)
}

func (s *Service) Account(ctx context.Context, clientID string) (bank.Account, error) {
	return s.books.FindAccount(ctx, bank.Airtel, bank.MerchantAccount, clientID, currency)
}

func (s *Service) Fund(ctx context.Context, clientID string, amount int64) (bank.Account, error) {
	a, err := s.Account(ctx, clientID)
	if err != nil {
		return a, err
	}
	return s.books.Fund(ctx, a.ID, amount)
}

// Approve is the customer's yes to a collection; it also sends the callback.
func (s *Service) Approve(ctx context.Context, clientID, id string) error {
	return s.decide(ctx, clientID, id, func(txID string) error {
		_, err := s.books.Approve(ctx, txID)
		return err
	})
}

func (s *Service) Refuse(ctx context.Context, clientID, id string) error {
	return s.decide(ctx, clientID, id, func(txID string) error {
		_, err := s.books.Refuse(ctx, txID, bank.ReasonRefused)
		return err
	})
}

func (s *Service) decide(ctx context.Context, clientID, id string, f func(string) error) error {
	p, err := s.store.payment(ctx, clientID, id, kindCollection)
	if err != nil {
		return err
	}
	if err := f(p.TransactionID); errors.Is(err, bank.ErrDecided) {
		return ErrDecided
	} else if err != nil {
		return err
	}
	s.notify(p)
	return nil
}

func (s *Service) PurgeTokens(ctx context.Context) error {
	return s.store.purgeTokens(ctx, s.books.Clock().Now())
}

// Wait returns once callbacks already started are delivered or have failed.
func (s *Service) Wait() { s.sends.Wait() }
