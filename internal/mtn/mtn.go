// Package mtn answers like MTN MoMo's Open API. docs/mtn-collection.md says where each
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
)

const (
	collection = "collection"
	tokenTTL   = time.Hour
	// approvalWindow is how long a customer has to approve (docs).
	approvalWindow = 5 * time.Minute
	maxMessage     = 160
	maxBody        = 64 << 10
)

// Sandbox test numbers (sdk).
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
	sends  sync.WaitGroup
	// Send delivers a callback. Tests replace it.
	Send func(ctx context.Context, url string, body []byte) error
}

func New(ctx context.Context, books *bank.Bank, logger *slog.Logger) (*Service, error) {
	st, err := newStore(ctx, books)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return &Service{store: st, books: books, logger: logger, Send: func(ctx context.Context, u string, body []byte) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		return res.Body.Close()
	}}, nil
}

func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1_0/apiuser", s.createAPIUser)
	mux.HandleFunc("GET /v1_0/apiuser/{id}", s.getAPIUser)
	mux.HandleFunc("POST /v1_0/apiuser/{id}/apikey", s.createAPIKey)
	mux.HandleFunc("POST /collection/token/", s.issueToken(collection))
	mux.HandleFunc("POST /collection/v1_0/requesttopay", s.authorized(collection, s.requestToPay))
	mux.HandleFunc("GET /collection/v1_0/requesttopay/{ref}", s.authorized(collection, s.requestToPayStatus))
	mux.HandleFunc("GET /collection/v1_0/account/balance", s.authorized(collection, s.balance))
	mux.HandleFunc("GET /collection/v1_0/accountholder/{type}/{id}/active", s.authorized(collection, s.accountHolder))
}

// CreateSubscription issues a subscription key, as MTN's portal does.
func (s *Service) CreateSubscription(ctx context.Context, product string) (string, error) {
	if product != collection {
		return "", ErrUnknownProduct
	}
	return s.store.addSubscription(ctx, product)
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

// Approve also sends the callback.
func (s *Service) Approve(ctx context.Context, user, ref string) error {
	return s.decide(ctx, user, ref, func(id string) error {
		_, err := s.books.Approve(ctx, id)
		return err
	})
}

// Refuse also sends the callback.
func (s *Service) Refuse(ctx context.Context, user, ref string) error {
	return s.decide(ctx, user, ref, func(id string) error {
		_, err := s.books.Refuse(ctx, id, bank.ReasonRefused)
		return err
	})
}

func (s *Service) decide(ctx context.Context, user, ref string, f func(id string) error) error {
	r, err := s.store.request(ctx, user, ref)
	if err != nil {
		return err
	}
	if !r.TransactionID.Valid {
		return bank.ErrDecided
	}
	if err := f(r.TransactionID.String); err != nil {
		return err
	}
	s.notify(r)
	return nil
}
