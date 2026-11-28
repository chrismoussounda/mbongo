// Package airtel answers like Airtel Money Congo's Open API. docs/airtel.md says where each
// behaviour comes from.
package airtel

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/report"
)

const (
	tokenTTL       = 180 * time.Second
	approvalWindow = 5 * time.Minute
	country        = "CG"
	currency       = "XAF"
	maxBody        = 64 << 10
	defaultPIN     = "1234"
	// forever is the window of a payment a rule keeps stuck.
	forever = 100 * 365 * 24 * time.Hour
	// ambiguousFor is how long a payment stays TA when the rule gives no delay; the docs ask
	// to wait three minutes before asking for a status.
	ambiguousFor = 3 * time.Minute
)

var ErrDecided = errors.New("airtel: only a pending collection can be approved or refused")

type Service struct {
	store   *store
	books   *bank.Bank
	logger  *slog.Logger
	send    Sender
	sends   sync.WaitGroup
	keyMu   sync.Mutex
	faults  *faults.Injector
	journal *report.Recorder
}

// Sender delivers a callback and returns the HTTP status the receiver answered.
type Sender func(ctx context.Context, url string, body []byte) (int, error)

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
	return func(ctx context.Context, u string, body []byte) (int, error) {
		// The URL is the one set for the app through the admin API, as on Airtel's portal.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body)) //nolint:gosec // G704, see above
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req) //nolint:gosec // G704, see above
		if err != nil {
			return 0, err
		}
		return res.StatusCode, res.Body.Close()
	}
}

// WithFaults lets rules make the service fail on purpose.
func (s *Service) WithFaults(in *faults.Injector) *Service {
	s.faults = in
	return s
}

// Routes registers Airtel's paths. Each one can be made to fail by a rule naming its
// operation: token, keys, collection, refund, status, cashout, cashin, disbursement, balance
// or user.
func (s *Service) Routes(mux *http.ServeMux) {
	handle := func(pattern, operation string, h http.Handler) {
		mux.Handle(pattern, s.journal.Wrap("airtel", operation, s.describe, s.faults.Wrap("airtel", operation, answers{}, h)))
	}
	handle("POST /auth/oauth2/token", "token", http.HandlerFunc(s.issueToken))
	routes := []struct {
		pattern, operation string
		h                  func(http.ResponseWriter, *http.Request, App)
	}{
		{"GET /v1/rsa/encryption-keys", "keys", s.encryptionKeys},
		{"POST /merchant/v1/payments/{$}", kindCollection, s.collect},
		{"POST /standard/v1/payments/refund", kindRefund, s.refund},
		{"GET /standard/v1/payments/{id}", "status", s.enquiry(kindCollection, kindCashOut, kindCashIn)},
		{"POST /standard/v1/cashout/{$}", kindCashOut, s.cashOut},
		{"POST /standard/v1/cashin/{$}", kindCashIn, s.cashIn},
		{"POST /standard/v1/disbursements/{$}", kindDisbursement, s.disburse},
		{"GET /standard/v1/disbursements/{id}", "status", s.enquiry(kindDisbursement)},
		{"GET /standard/v1/users/balance", "balance", s.balance},
		{"GET /standard/v1/users/{msisdn}", "user", s.user},
	}
	for _, rt := range routes {
		handle(rt.pattern, rt.operation, s.authorized(rt.h))
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

const waitingCollections = `SELECT p.transaction_id, a.name, p.reference
	FROM airtel_payments p
	JOIN transactions t ON t.id = p.transaction_id
	JOIN airtel_apps a ON a.client_id = p.client_id
	WHERE p.kind = 'collection' AND t.status = 'pending' AND t.expires_at > ?`

// Prompts are the collections waiting for the payer's yes, oldest first.
func (s *Service) Prompts(ctx context.Context, msisdn string) ([]bank.Prompt, error) {
	return s.books.PromptsFrom(ctx, waitingCollections+" AND p.msisdn = ? ORDER BY t.created_at, t.id",
		s.books.Clock().Now().UTC().Format(bank.TimeLayout), msisdn)
}

// Waiting is every number's prompts, oldest first.
func (s *Service) Waiting(ctx context.Context) ([]bank.Prompt, error) {
	return s.books.PromptsFrom(ctx, waitingCollections+" ORDER BY t.created_at, t.id",
		s.books.Clock().Now().UTC().Format(bank.TimeLayout))
}

// Answer is the payer's yes or no to a collection, given by its bank transaction; it also
// sends the callback.
func (s *Service) Answer(ctx context.Context, transactionID string, approve bool) error {
	var clientID, id string
	err := s.store.db.QueryRowContext(ctx, `SELECT client_id, id FROM airtel_payments
		WHERE transaction_id = ? AND kind = 'collection'`, transactionID).Scan(&clientID, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return bank.ErrNotFound
	}
	if err != nil {
		return err
	}
	if approve {
		err = s.Approve(ctx, clientID, id)
	} else {
		err = s.Refuse(ctx, clientID, id)
	}
	if errors.Is(err, ErrDecided) {
		return bank.ErrDecided
	}
	return err
}

// Describe is what the customer's phone shows about a transaction: the app on the other side
// and its Airtel Money ID.
func (s *Service) Describe(ctx context.Context, t bank.Transaction) (counterparty, ref string, err error) {
	err = s.store.db.QueryRowContext(ctx, `SELECT a.name, p.airtel_money_id FROM airtel_payments p
		JOIN airtel_apps a ON a.client_id = p.client_id
		WHERE p.transaction_id = ?`, t.ID).Scan(&counterparty, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", bank.ErrNotFound
	}
	return counterparty, ref, err
}
