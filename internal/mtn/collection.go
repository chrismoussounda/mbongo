package mtn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/money"
)

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

type paymentStatus struct {
	Amount                 string  `json:"amount"`
	Currency               string  `json:"currency"`
	FinancialTransactionID string  `json:"financialTransactionId,omitempty"`
	ExternalID             string  `json:"externalId"`
	Payer                  party   `json:"payer"`
	PayerMessage           string  `json:"payerMessage"`
	PayeeNote              string  `json:"payeeNote"`
	Status                 string  `json:"status"`
	Reason                 *reason `json:"reason,omitempty"`
}

// reason is an object in the sandbox but a bare code in Congo (sdk).
type reason struct {
	Code string
	Bare bool
}

func (r reason) MarshalJSON() ([]byte, error) {
	if r.Bare {
		return json.Marshal(r.Code)
	}
	return json.Marshal(map[string]string{"code": r.Code, "message": reasonMessage(r.Code)})
}

func (s *Service) statusBody(ctx context.Context, u apiUser, req request) (paymentStatus, error) {
	body := paymentStatus{Amount: req.Amount, Currency: req.Currency, ExternalID: req.ExternalID,
		Payer: party{PartyIDType: "MSISDN", PartyID: req.PartyID}, PayerMessage: req.PayerMessage,
		PayeeNote: req.PayeeNote, Status: "FAILED"}
	code := req.Failure
	if req.TransactionID.Valid {
		t, err := s.books.Transaction(ctx, req.TransactionID.String)
		if err != nil {
			return body, err
		}
		switch t.Status {
		case bank.Pending:
			body.Status = "PENDING"
		case bank.Successful:
			body.Status = "SUCCESSFUL"
			body.FinancialTransactionID = fmt.Sprint(crc32.ChecksumIEEE([]byte(t.ID)))
		case bank.Expired:
			code = "EXPIRED"
		case bank.Failed:
			code = failureCode(t.Reason, u.Environment)
		}
	}
	if body.Status == "FAILED" {
		body.Reason = &reason{Code: code, Bare: u.Environment != Sandbox}
	}
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
