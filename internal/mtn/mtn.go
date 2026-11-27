// Package mtn answers like MTN MoMo's Open API. docs/mtn.md says where each
// behaviour comes from.
package mtn

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/faults"
)

const (
	collection   = "collection"
	disbursement = "disbursement"
	tokenTTL     = time.Hour
	// approvalWindow is how long a customer has to approve.
	approvalWindow = 5 * time.Minute
	maxMessage     = 160
	maxBody        = 64 << 10
	// forever is the window of a payment a rule keeps stuck.
	forever = 100 * 365 * 24 * time.Hour
	// sandboxFloat is what a sandbox payout account starts with.
	sandboxFloat = 1_000_000_000_00
)

// Sandbox test numbers.
const (
	numberFails   = "46733123450"
	numberRejects = "46733123451"
	numberTimeout = "46733123452"
	numberPending = "46733123454"
)

type Service struct {
	store  *store
	books  *bank.Bank
	logger *slog.Logger
	send   Sender
	sends  sync.WaitGroup
	faults *faults.Injector
}

type Sender func(ctx context.Context, url string, body []byte) error

// New sends callbacks with send, or by HTTP PUT when send is nil.
func New(ctx context.Context, books *bank.Bank, logger *slog.Logger, send Sender) (*Service, error) {
	st, err := newStore(ctx, books)
	if err != nil {
		return nil, err
	}
	if send == nil {
		send = put(&http.Client{Timeout: 10 * time.Second})
	}
	return &Service{store: st, books: books, logger: logger, send: send}, nil
}

func (s *Service) PurgeTokens(ctx context.Context) error {
	_, err := s.store.purgeTokens(ctx, s.books.Clock().Now())
	return err
}

func put(client *http.Client) Sender {
	return func(ctx context.Context, u string, body []byte) error {
		// Calling a URL chosen by the client is the point of a callback; readRequest already
		// limits it to the host the API user registered, as MTN does.
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body)) //nolint:gosec // G704, see above

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

// WithFaults lets rules make the service fail on purpose.
func (s *Service) WithFaults(in *faults.Injector) *Service {
	s.faults = in
	return s
}

// Routes registers MTN's paths. Each one can be made to fail by a rule naming its operation:
// apiuser, token, balance, accountholder, a payment operation, or status.
func (s *Service) Routes(mux *http.ServeMux) {
	handle := func(pattern, operation string, h http.HandlerFunc) {
		mux.Handle(pattern, s.faults.Wrap("mtn", operation, answers{}, h))
	}
	handle("POST /v1_0/apiuser", "apiuser", s.createAPIUser)
	handle("GET /v1_0/apiuser/{id}", "apiuser", s.getAPIUser)
	handle("POST /v1_0/apiuser/{id}/apikey", "apiuser", s.createAPIKey)
	for _, product := range []string{collection, disbursement} {
		handle("POST /"+product+"/token/", "token", s.issueToken(product))
		handle("GET /"+product+"/v1_0/account/balance", "balance", s.authorized(product, s.balance(product)))
		handle("GET /"+product+"/v1_0/accountholder/{type}/{id}/active", "accountholder", s.authorized(product, s.accountHolder))
	}
	for _, op := range []operation{opRequestToPay, opTransfer, opDeposit, opRefund} {
		handle("POST /"+op.product+"/v1_0/"+op.name, op.name, s.authorized(op.product, s.create(op)))
		handle("GET /"+op.product+"/v1_0/"+op.name+"/{ref}", "status", s.authorized(op.product, s.status(op)))
	}
}

// CreateSubscription issues a subscription key, as MTN's portal does. Subscriptions with the
// same partner belong together, so payouts can refund that partner's collections; an empty
// partner makes the subscription a partner of its own.
func (s *Service) CreateSubscription(ctx context.Context, product, partner string) (string, error) {
	if product != collection && product != disbursement {
		return "", ErrUnknownProduct
	}
	return s.store.addSubscription(ctx, product, partner)
}

// CreateAPIUser makes a user with its key at once, as MTN's partner portal does for production.
func (s *Service) CreateAPIUser(ctx context.Context, subscription, callbackHost, environment string) (id, key string, err error) {
	if environment != Sandbox && environment != MTNCongo {
		return "", "", ErrUnknownEnvironment
	}
	if _, err := s.store.subscriptionProduct(ctx, subscription); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", ErrUnknownSubscription
		}
		return "", "", err
	}
	id, key = newUUID(), randomHex()
	err = s.store.addAPIUser(ctx, apiUser{ID: id, Subscription: subscription, CallbackHost: callbackHost,
		Environment: environment, APIKey: key})
	return id, key, err
}

// Account is where an API user's collections land and its payouts are paid from.
func (s *Service) Account(ctx context.Context, user string) (bank.Account, error) {
	return s.store.merchant(ctx, user)
}

func (s *Service) Fund(ctx context.Context, user string, amount int64) (bank.Account, error) {
	a, err := s.store.merchant(ctx, user)
	if err != nil {
		return a, err
	}
	return s.books.Fund(ctx, a.ID, amount)
}

// Approve also sends the callback.
func (s *Service) Approve(ctx context.Context, user, ref string) error {
	return s.decide(ctx, user, ref, func(u apiUser, op operation, id string) error {
		return s.approve(ctx, u, op, id)
	})
}

// Refuse also sends the callback.
func (s *Service) Refuse(ctx context.Context, user, ref string) error {
	return s.decide(ctx, user, ref, func(_ apiUser, _ operation, id string) error {
		_, err := s.books.Refuse(ctx, id, bank.ReasonRefused)
		return err
	})
}

func (s *Service) decide(ctx context.Context, user, ref string, f func(apiUser, operation, string) error) error {
	r, err := s.store.request(ctx, user, ref)
	if err != nil {
		return err
	}
	if !r.TransactionID.Valid {
		return bank.ErrDecided
	}
	u, err := s.store.apiUser(ctx, user)
	if err != nil {
		return err
	}
	op, err := operationNamed(r.Operation)
	if err != nil {
		return err
	}
	if err := f(u, op, r.TransactionID.String); err != nil {
		return err
	}
	s.notify(r)
	return nil
}
