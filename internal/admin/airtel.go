package admin

import (
	"errors"
	"net/http"

	"github.com/chrismoussounda/mbongo/internal/airtel"
	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/money"
)

func (h *handler) airtelApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		CallbackURL string `json:"callbackUrl"`
		CallbackKey string `json:"callbackKey"`
		PIN         string `json:"pin"`
	}
	if !read(w, r, &in) {
		return
	}
	if in.Name == "" {
		h.refuse(w, "name is required")
		return
	}
	app, err := h.airtel.CreateApp(r.Context(), airtel.App{Name: in.Name, CallbackURL: in.CallbackURL,
		CallbackKey: in.CallbackKey, PIN: in.PIN})
	if err != nil {
		h.internal(w, err)
		return
	}
	reply(w, http.StatusCreated, map[string]string{"clientId": app.ClientID, "clientSecret": app.ClientSecret,
		"pin": app.PIN})
}

func (h *handler) airtelFund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Amount string `json:"amount"`
	}
	if !read(w, r, &in) {
		return
	}
	amount, err := money.Parse(in.Amount, "XAF")
	if err != nil || amount == 0 {
		h.refuse(w, "amount must be a positive amount in XAF")
		return
	}
	a, err := h.airtel.Fund(r.Context(), r.PathValue("client"), amount)
	if errors.Is(err, bank.ErrNotFound) {
		reply(w, http.StatusNotFound, map[string]string{"error": "no such app"})
		return
	}
	if err != nil {
		h.internal(w, err)
		return
	}
	balance, _ := money.Format(a.Balance, a.Currency)
	reply(w, http.StatusOK, map[string]string{"balance": balance, "currency": a.Currency})
}

func (h *handler) airtelDecide(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		client, id := r.PathValue("client"), r.PathValue("id")
		var err error
		if approve {
			err = h.airtel.Approve(r.Context(), client, id)
		} else {
			err = h.airtel.Refuse(r.Context(), client, id)
		}
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, airtel.ErrNotFound):
			reply(w, http.StatusNotFound, map[string]string{"error": "no such collection"})
		case errors.Is(err, airtel.ErrDecided):
			reply(w, http.StatusConflict, map[string]string{"error": "already decided"})
		default:
			h.internal(w, err)
		}
	}
}
