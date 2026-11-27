// Package mtn answers like MTN MoMo's Open API. docs/mtn-collection.md says where each
// behaviour comes from.
package mtn

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/money"
)

const (
	collection = "collection"
	tokenTTL   = time.Hour
	// approvalWindow is how long a customer has to approve (docs).
	approvalWindow = 5 * time.Minute
	maxMessage     = 160
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
		return "", fmt.Errorf("mtn: unknown product %q", product)
	}
	return s.store.addSubscription(ctx, product)
}

// CreateAPIUser makes a user with its key at once, as MTN's partner portal does for production.
func (s *Service) CreateAPIUser(ctx context.Context, subscription, callbackHost, environment string) (id, key string, err error) {
	if environment != Sandbox && environment != MTNCongo {
		return "", "", fmt.Errorf("mtn: unknown environment %q", environment)
	}
	if _, err := s.store.subscriptionProduct(ctx, subscription); err != nil {
		return "", "", fmt.Errorf("mtn: unknown subscription key: %w", err)
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

func (s *Service) createAPIUser(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Ocp-Apim-Subscription-Key")
	if _, err := s.store.subscriptionProduct(r.Context(), key); err != nil {
		apimDenied(w)
		return
	}
	id := r.Header.Get("X-Reference-Id")
	if !isUUID(id) {
		writeError(w, http.StatusBadRequest, "INVALID_REFERENCE_ID", "X-Reference-Id must be a UUID version 4.")
		return
	}
	var body struct {
		ProviderCallbackHost string `json:"providerCallbackHost"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ProviderCallbackHost == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "providerCallbackHost is required.")
		return
	}
	if _, err := s.store.apiUser(r.Context(), id); err == nil {
		writeError(w, http.StatusConflict, "RESOURCE_ALREADY_EXIST", "Duplicated reference id. Creation of resource failed.")
		return
	}
	if err := s.store.addAPIUser(r.Context(), apiUser{ID: id, Subscription: key,
		CallbackHost: body.ProviderCallbackHost, Environment: Sandbox}); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Service) sandboxUser(w http.ResponseWriter, r *http.Request) (apiUser, bool) {
	key := r.Header.Get("Ocp-Apim-Subscription-Key")
	if _, err := s.store.subscriptionProduct(r.Context(), key); err != nil {
		apimDenied(w)
		return apiUser{}, false
	}
	u, err := s.store.apiUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) || (err == nil && (u.Subscription != key || u.Environment != Sandbox)) {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "Requested resource was not found.")
		return apiUser{}, false
	}
	if err != nil {
		s.internal(w, err)
		return apiUser{}, false
	}
	return u, true
}

func (s *Service) getAPIUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.sandboxUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"providerCallbackHost": u.CallbackHost, "targetEnvironment": u.Environment})
}

func (s *Service) createAPIKey(w http.ResponseWriter, r *http.Request) {
	u, ok := s.sandboxUser(w, r)
	if !ok {
		return
	}
	key := randomHex()
	if err := s.store.setAPIKey(r.Context(), u.ID, key); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"apiKey": key})
}

func (s *Service) issueToken(product string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Ocp-Apim-Subscription-Key")
		if p, err := s.store.subscriptionProduct(r.Context(), key); err != nil || p != product {
			apimDenied(w)
			return
		}
		id, secret, ok := r.BasicAuth()
		u, err := s.store.apiUser(r.Context(), id)
		if !ok || err != nil || u.Subscription != key || u.APIKey == "" || u.APIKey != secret {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login_failed"})
			return
		}
		if r.Header.Get("X-Target-Environment") != u.Environment {
			wrongEnvironment(w)
			return
		}
		token, err := s.store.addToken(r.Context(), u.ID, product, s.books.Clock().Now().Add(tokenTTL))
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": token, "token_type": "access_token", "expires_in": int(tokenTTL.Seconds()),
		})
	}
}

func (s *Service) authorized(product string, next func(http.ResponseWriter, *http.Request, apiUser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Ocp-Apim-Subscription-Key")
		if p, err := s.store.subscriptionProduct(r.Context(), key); err != nil || p != product {
			apimDenied(w)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "Bearer token is missing.")
			return
		}
		user, tokenProduct, expires, err := s.store.token(r.Context(), token)
		if err != nil || tokenProduct != product || !s.books.Clock().Now().Before(expires) {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "Access token is invalid or expired.")
			return
		}
		u, err := s.store.apiUser(r.Context(), user)
		if err != nil {
			s.internal(w, err)
			return
		}
		if u.Subscription != key {
			apimDenied(w)
			return
		}
		if r.Header.Get("X-Target-Environment") != u.Environment {
			wrongEnvironment(w)
			return
		}
		next(w, r, u)
	}
}

type party struct {
	PartyIDType string `json:"partyIdType"`
	PartyID     string `json:"partyId"`
}

type payment struct {
	Amount       string `json:"amount"`
	Currency     string `json:"currency"`
	ExternalID   string `json:"externalId"`
	Payer        party  `json:"payer"`
	PayerMessage string `json:"payerMessage"`
	PayeeNote    string `json:"payeeNote"`
}

var digitsOnly = regexp.MustCompile(`^[0-9]{6,15}$`)

func (s *Service) requestToPay(w http.ResponseWriter, r *http.Request, u apiUser) {
	ctx := r.Context()
	ref := r.Header.Get("X-Reference-Id")
	if !isUUID(ref) {
		writeError(w, http.StatusBadRequest, "INVALID_REFERENCE_ID", "X-Reference-Id must be a UUID version 4.")
		return
	}
	var p payment
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "The request body is not valid JSON.")
		return
	}
	if p.Currency != currency(u.Environment) {
		writeError(w, http.StatusBadRequest, "INVALID_CURRENCY", "Currency not supported.")
		return
	}
	amount, err := money.Parse(p.Amount, p.Currency)
	if err != nil || amount == 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "amount is not a valid positive amount for this currency.")
		return
	}
	if p.Payer.PartyIDType != "MSISDN" || !digitsOnly.MatchString(p.Payer.PartyID) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "payer must be an MSISDN in international format.")
		return
	}
	if utf8.RuneCountInString(p.PayerMessage) > maxMessage || utf8.RuneCountInString(p.PayeeNote) > maxMessage {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "payerMessage and payeeNote are limited to 160 characters.")
		return
	}
	callback := r.Header.Get("X-Callback-Url")
	if callback != "" {
		cu, err := url.Parse(callback)
		if err != nil || cu.Hostname() != u.CallbackHost {
			writeError(w, http.StatusBadRequest, "INVALID_CALLBACK_URL_HOST", "Callback URL host does not match the API user's providerCallbackHost.")
			return
		}
	}

	req := request{APIUser: u.ID, ReferenceID: ref, Product: collection, PartyID: p.Payer.PartyID,
		Amount: p.Amount, Currency: p.Currency, ExternalID: p.ExternalID, PayerMessage: p.PayerMessage,
		PayeeNote: p.PayeeNote, CallbackURL: callback}
	if err := s.store.insertRequest(ctx, req); err != nil {
		if errors.Is(err, errDuplicate) {
			writeError(w, http.StatusConflict, "RESOURCE_ALREADY_EXIST", "Duplicated reference id. Creation of resource failed.")
			return
		}
		s.internal(w, err)
		return
	}

	if err := s.start(ctx, u, req, amount); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// start opens the bank transaction behind an accepted request and, in the sandbox,
// plays the outcome the test numbers promise.
func (s *Service) start(ctx context.Context, u apiUser, req request, amount int64) error {
	sandbox := u.Environment == Sandbox
	if sandbox {
		if _, err := s.books.OpenCustomer(ctx, bank.MTN, req.PartyID, req.Currency, 1_000_000_000_00); err != nil &&
			!errors.Is(err, bank.ErrExists) {
			return err
		}
	}
	window := approvalWindow
	if sandbox && req.PartyID == numberPending {
		window = 24 * time.Hour
	}
	t, err := s.books.Request(ctx, bank.Request{Kind: bank.Collection, Operator: bank.MTN, Merchant: u.ID,
		Phone: req.PartyID, Amount: amount, Currency: req.Currency, PartnerRef: req.ReferenceID,
		ExternalID: req.ExternalID, Window: window})
	switch {
	case errors.Is(err, bank.ErrNoCustomer):
		return s.store.setFailure(ctx, u.ID, req.ReferenceID, "PAYER_NOT_FOUND")
	case err != nil:
		return err
	}
	if err := s.store.setTransaction(ctx, u.ID, req.ReferenceID, t.ID); err != nil {
		return err
	}
	req.TransactionID = sql.NullString{String: t.ID, Valid: true}
	if !sandbox {
		return nil
	}
	switch req.PartyID {
	case numberTimeout, numberPending:
		return nil
	case numberFails:
		_, err = s.books.Refuse(ctx, t.ID, "internal_processing_error")
	case numberRejects:
		_, err = s.books.Refuse(ctx, t.ID, bank.ReasonRefused)
	default:
		_, err = s.books.Approve(ctx, t.ID)
	}
	if err != nil {
		return err
	}
	s.notify(req)
	return nil
}

func (s *Service) requestToPayStatus(w http.ResponseWriter, r *http.Request, u apiUser) {
	req, err := s.store.request(r.Context(), u.ID, r.PathValue("ref"))
	if errors.Is(err, ErrNotFound) || (err == nil && req.Product != collection) {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "Requested resource was not found.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	body, err := s.statusBody(r.Context(), u, req)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Service) statusBody(ctx context.Context, u apiUser, req request) (map[string]any, error) {
	body := map[string]any{
		"amount":       req.Amount,
		"currency":     req.Currency,
		"externalId":   req.ExternalID,
		"payer":        party{PartyIDType: "MSISDN", PartyID: req.PartyID},
		"payerMessage": req.PayerMessage,
		"payeeNote":    req.PayeeNote,
	}
	status, reason := "FAILED", req.Failure
	if req.TransactionID.Valid {
		t, err := s.books.Transaction(ctx, req.TransactionID.String)
		if err != nil {
			return nil, err
		}
		switch t.Status {
		case bank.Pending:
			status = "PENDING"
		case bank.Successful:
			status = "SUCCESSFUL"
			body["financialTransactionId"] = fmt.Sprint(crc32.ChecksumIEEE([]byte(t.ID)))
		case bank.Expired:
			reason = "EXPIRED"
		case bank.Failed:
			reason = failureCode(t.Reason, u.Environment)
		}
	}
	if status == "FAILED" {
		if u.Environment == Sandbox {
			body["reason"] = map[string]string{"code": reason, "message": reasonMessage(reason)}
		} else {
			body["reason"] = reason
		}
	}
	body["status"] = status
	return body, nil
}

func failureCode(r bank.Reason, environment string) string {
	switch r {
	case bank.ReasonNotEnoughFunds:
		if environment == MTNCongo {
			return "LOW_BALANCE_OR_PAYEE_LIMIT_REACHED_OR_NOT_ALLOWED"
		}
		return "NOT_ENOUGH_FUNDS"
	case bank.ReasonRefused:
		return "APPROVAL_REJECTED"
	default:
		return strings.ToUpper(string(r))
	}
}

func reasonMessage(code string) string {
	switch code {
	case "NOT_ENOUGH_FUNDS":
		return "The payer does not have enough funds."
	case "APPROVAL_REJECTED":
		return "The payer rejected the request."
	case "EXPIRED":
		return "The payer did not approve in time."
	case "PAYER_NOT_FOUND":
		return "The payer was not found."
	default:
		return "The transaction could not be processed."
	}
}

func (s *Service) balance(w http.ResponseWriter, r *http.Request, u apiUser) {
	a, err := s.store.merchant(r.Context(), u.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	amount, err := money.Format(a.Balance, a.Currency)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"availableBalance": amount, "currency": a.Currency})
}

func (s *Service) accountHolder(w http.ResponseWriter, r *http.Request, u apiUser) {
	if strings.ToLower(r.PathValue("type")) != "msisdn" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Only msisdn is supported.")
		return
	}
	active := u.Environment == Sandbox
	if !active {
		var err error
		active, err = s.store.hasCustomer(r.Context(), r.PathValue("id"), currency(u.Environment))
		if err != nil {
			s.internal(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"result": active})
}

// notify sends at most one callback, in the background.
func (s *Service) notify(req request) {
	if req.CallbackURL == "" {
		return
	}
	s.sends.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		first, err := s.store.markNotified(ctx, req.APIUser, req.ReferenceID)
		if err != nil || !first {
			return
		}
		u, err := s.store.apiUser(ctx, req.APIUser)
		if err != nil {
			s.logger.Error("callback", "err", err)
			return
		}
		fresh, err := s.store.request(ctx, req.APIUser, req.ReferenceID)
		if err != nil {
			s.logger.Error("callback", "err", err)
			return
		}
		body, err := s.statusBody(ctx, u, fresh)
		if err != nil {
			s.logger.Error("callback", "err", err)
			return
		}
		raw, _ := json.Marshal(body)
		if err := s.Send(ctx, req.CallbackURL, raw); err != nil {
			s.logger.Info("callback not delivered, not retried", "ref", req.ReferenceID, "err", err)
		}
	})
}

// Wait returns once callbacks already started are delivered or have failed.
func (s *Service) Wait() { s.sends.Wait() }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// apimDenied copies the gateway's answer to a missing or wrong subscription key (assumed).
func apimDenied(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"statusCode": 401,
		"message": "Access denied due to invalid subscription key. Make sure to provide a valid key for an active subscription."})
}

func wrongEnvironment(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "NOT_ALLOWED_TARGET_ENVIRONMENT", "Access to target environment is forbidden.")
}

func (s *Service) internal(w http.ResponseWriter, err error) {
	s.logger.Error("mtn", "err", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL_PROCESSING_ERROR", "An internal error occurred while processing.")
}

var uuidV4 = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidV4.MatchString(s) }

func newUUID() string {
	h := []byte(randomHex())
	h[12] = '4'
	h[16] = "89ab"[h[16]%4]
	s := string(h)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
