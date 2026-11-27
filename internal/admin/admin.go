// Package admin is Mbongo's own API, for what an operator does by hand: issuing keys,
// opening wallets, and the payer's yes or no until the phone page exists.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/money"
	"github.com/chrismoussounda/mbongo/internal/mtn"
)

type handler struct {
	token  string
	books  *bank.Bank
	mtn    *mtn.Service
	logger *slog.Logger
}

// Routes does nothing without a token, so the admin API is off unless asked for.
func Routes(mux *http.ServeMux, token string, books *bank.Bank, m *mtn.Service, logger *slog.Logger) {
	if token == "" {
		return
	}
	h := &handler{token: token, books: books, mtn: m, logger: logger}
	mux.HandleFunc("POST /_mbongo/mtn/subscriptions", h.guard(h.subscription))
	mux.HandleFunc("POST /_mbongo/mtn/api-users", h.guard(h.apiUser))
	mux.HandleFunc("POST /_mbongo/customers", h.guard(h.customer))
	mux.HandleFunc("POST /_mbongo/mtn/{user}/fund", h.guard(h.fund))
	mux.HandleFunc("POST /_mbongo/mtn/{user}/requests/{ref}/approve", h.guard(h.decide(true)))
	mux.HandleFunc("POST /_mbongo/mtn/{user}/requests/{ref}/refuse", h.guard(h.decide(false)))
}

func (h *handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			reply(w, http.StatusUnauthorized, map[string]string{"error": "admin token required"})
			return
		}
		next(w, r)
	}
}

func (h *handler) subscription(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Product string `json:"product"`
		Partner string `json:"partner"`
	}
	if !read(w, r, &in) {
		return
	}
	key, err := h.mtn.CreateSubscription(r.Context(), in.Product, in.Partner)
	if errors.Is(err, mtn.ErrUnknownProduct) {
		h.refuse(w, "product must be collection or disbursement")
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, map[string]string{"subscriptionKey": key})
}

func (h *handler) apiUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SubscriptionKey      string `json:"subscriptionKey"`
		ProviderCallbackHost string `json:"providerCallbackHost"`
		TargetEnvironment    string `json:"targetEnvironment"`
	}
	if !read(w, r, &in) {
		return
	}
	id, key, err := h.mtn.CreateAPIUser(r.Context(), in.SubscriptionKey, in.ProviderCallbackHost, in.TargetEnvironment)
	switch {
	case errors.Is(err, mtn.ErrUnknownSubscription):
		h.refuse(w, "unknown subscriptionKey")
		return
	case errors.Is(err, mtn.ErrUnknownEnvironment):
		h.refuse(w, "targetEnvironment must be sandbox or mtncongo")
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, map[string]string{"apiUser": id, "apiKey": key})
}

func (h *handler) customer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Operator string `json:"operator"`
		Phone    string `json:"phone"`
		Currency string `json:"currency"`
		Balance  string `json:"balance"`
	}
	if !read(w, r, &in) {
		return
	}
	if in.Operator != string(bank.MTN) && in.Operator != string(bank.Airtel) {
		h.refuse(w, "operator must be mtn or airtel")
		return
	}
	balance := int64(0)
	if in.Balance != "" {
		var err error
		if balance, err = money.Parse(in.Balance, in.Currency); err != nil {
			h.refuse(w, "balance must be an amount in "+in.Currency+", such as 5000 or 50.00")
			return
		}
	}
	a, err := h.books.OpenCustomer(r.Context(), bank.Operator(in.Operator), in.Phone, in.Currency, balance)
	switch {
	case errors.Is(err, bank.ErrExists):
		reply(w, http.StatusConflict, map[string]string{"error": "this customer already exists"})
		return
	case errors.Is(err, bank.ErrInvalid):
		h.refuse(w, "phone is required")
		return
	case errors.Is(err, bank.ErrCurrency):
		h.refuse(w, "currency must be XAF, XOF or EUR")
		return
	case err != nil:
		h.internal(w, err)
		return
	}
	formatted, _ := money.Format(a.Balance, a.Currency)
	reply(w, http.StatusCreated, map[string]string{"operator": in.Operator, "phone": a.Name,
		"currency": a.Currency, "balance": formatted})
}

func (h *handler) fund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount string `json:"amount"`
	}
	if !read(w, r, &in) {
		return
	}
	a, err := h.mtn.Account(r.Context(), r.PathValue("user"))
	if errors.Is(err, mtn.ErrNotFound) {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such API user"})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	amount, err := money.Parse(in.Amount, a.Currency)
	if err != nil || amount == 0 {
		h.refuse(w, "amount must be a positive amount in "+a.Currency)
		return
	}
	if a, err = h.mtn.Fund(r.Context(), r.PathValue("user"), amount); err != nil {
		h.internal(w, err)
		return
	}
	balance, _ := money.Format(a.Balance, a.Currency)
	reply(w, http.StatusOK, map[string]string{"balance": balance, "currency": a.Currency})
}

func (h *handler) decide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ref := r.PathValue("user"), r.PathValue("ref")
		var err error
		if approve {
			err = h.mtn.Approve(r.Context(), user, ref)
		} else {
			err = h.mtn.Refuse(r.Context(), user, ref)
		}
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, mtn.ErrNotFound):
			reply(w, http.StatusNotFound, map[string]string{"error": "no such request"})
		case errors.Is(err, bank.ErrDecided):
			reply(w, http.StatusConflict, map[string]string{"error": "already decided"})
		default:
			h.internal(w, err)
		}
	}
}

func (h *handler) refuse(w http.ResponseWriter, message string) {
	reply(w, http.StatusBadRequest, map[string]string{"error": message})
}

func (h *handler) internal(w http.ResponseWriter, err error) {
	h.logger.Error("admin", "err", err)
	reply(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func read(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(into); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
