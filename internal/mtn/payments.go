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

// operation is one of MTN's payment resources. They share validation, storage and status.
type operation struct {
	name    string
	product string
	kind    bank.Kind
	party   string
	// messages: whether the status echoes payerMessage and payeeNote (sdk: not for transfers or refunds).
	messages bool
	// approval: whether the customer has to say yes, or MTN executes at once.
	approval bool
}

func operationNamed(name string) operation {
	for _, op := range []operation{opRequestToPay, opTransfer, opDeposit, opRefund} {
		if op.name == name {
			return op
		}
	}
	return opRequestToPay
}

var (
	opRequestToPay = operation{name: "requesttopay", product: collection, kind: bank.Collection, party: "payer",
		messages: true, approval: true}
	opTransfer = operation{name: "transfer", product: disbursement, kind: bank.Payout, party: "payee"}
	opDeposit  = operation{name: "deposit", product: disbursement, kind: bank.Payout, party: "payee", messages: true}
	opRefund   = operation{name: "refund", product: disbursement, kind: bank.Payout, party: "payee"}
)

type party struct {
	PartyIDType string `json:"partyIdType"`
	PartyID     string `json:"partyId"`
}

type paymentBody struct {
	Amount              string `json:"amount"`
	Currency            string `json:"currency"`
	ExternalID          string `json:"externalId"`
	Payer               party  `json:"payer"`
	Payee               party  `json:"payee"`
	PayerMessage        string `json:"payerMessage"`
	PayeeNote           string `json:"payeeNote"`
	ReferenceIDToRefund string `json:"referenceIdToRefund"`
}

var digitsOnly = regexp.MustCompile(`^[0-9]{6,15}$`)

func (s *Service) create(op operation) func(http.ResponseWriter, *http.Request, apiUser) {
	return func(w http.ResponseWriter, r *http.Request, u apiUser) {
		ctx := r.Context()
		ref := r.Header.Get("X-Reference-Id")
		if !isUUID(ref) {
			writeError(w, http.StatusBadRequest, "INVALID_REFERENCE_ID", "X-Reference-Id must be a UUID version 4.")
			return
		}
		var p paymentBody
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&p); err != nil {
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
		req := request{APIUser: u.ID, ReferenceID: ref, Product: op.product, Operation: op.name,
			Amount: p.Amount, Currency: p.Currency, ExternalID: p.ExternalID, PayerMessage: p.PayerMessage,
			PayeeNote: p.PayeeNote, CallbackURL: callback}

		var refundable int64
		if op == opRefund {
			var ok bool
			if refundable, ok = s.checkRefund(w, r, u, p.ReferenceIDToRefund, &req); !ok {
				return
			}
		} else {
			customer := p.Payer
			if op.party == "payee" {
				customer = p.Payee
			}
			if customer.PartyIDType != "MSISDN" || !digitsOnly.MatchString(customer.PartyID) {
				writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", op.party+" must be an MSISDN in international format.")
				return
			}
			req.PartyID = customer.PartyID
		}

		switch err := s.start(ctx, u, op, &req, amount, refundable); {
		case errors.Is(err, errDuplicate):
			writeError(w, http.StatusConflict, "RESOURCE_ALREADY_EXIST", "Duplicated reference id. Creation of resource failed.")
			return
		case errors.Is(err, errOverRefund):
			writeError(w, http.StatusBadRequest, "NOT_ALLOWED", "Only a successful payment can be refunded, up to its amount.")
			return
		case err != nil:
			s.internal(w, err)
			return
		}
		if req.TransactionID.Valid && (u.Environment == Sandbox || !op.approval) {
			s.execute(ctx, op, req)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

var errOverRefund = errors.New("mtn: refund larger than what is left of the payment")

// checkRefund allows a refund of a successful collection only, and returns its amount.
// What is left to refund is checked when the refund is saved (assumed rules).
func (s *Service) checkRefund(w http.ResponseWriter, r *http.Request, u apiUser, of string, req *request) (int64, bool) {
	ctx := r.Context()
	partner, err := s.store.partner(ctx, u.Subscription)
	if err != nil {
		s.internal(w, err)
		return 0, false
	}
	original, err := s.store.collectionToRefund(ctx, of, u.Environment, partner)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "referenceIdToRefund was not found.")
		return 0, false
	}
	if err != nil {
		s.internal(w, err)
		return 0, false
	}
	status, err := s.statusBody(ctx, u, opRequestToPay, original)
	if err != nil {
		s.internal(w, err)
		return 0, false
	}
	paid, err := money.Parse(original.Amount, original.Currency)
	if err != nil {
		s.internal(w, err)
		return 0, false
	}
	if status.Status != "SUCCESSFUL" {
		writeError(w, http.StatusBadRequest, "NOT_ALLOWED", "Only a successful payment can be refunded, up to its amount.")
		return 0, false
	}
	req.PartyID, req.RefundOf = original.PartyID, of
	return paid, true
}

// start saves the request together with its bank transaction, or alone, failed, when the
// customer has no wallet.
func (s *Service) start(ctx context.Context, u apiUser, op operation, req *request, amount, refundable int64) error {
	if u.Environment == Sandbox {
		opening := int64(0)
		if op.kind == bank.Collection {
			opening = sandboxFloat
		}
		if _, err := s.books.OpenCustomer(ctx, bank.MTN, req.PartyID, req.Currency, opening); err != nil &&
			!errors.Is(err, bank.ErrExists) {
			return err
		}
	}
	window := approvalWindow
	if u.Environment == Sandbox && req.PartyID == numberPending {
		window = 24 * time.Hour
	}
	_, err := s.books.RequestWith(ctx, bank.Request{Kind: op.kind, Operator: bank.MTN, Merchant: u.ID,
		Phone: req.PartyID, Amount: amount, Currency: req.Currency, PartnerRef: req.ReferenceID,
		ExternalID: req.ExternalID, Window: window}, func(tx *sql.Tx, t bank.Transaction) error {
		req.TransactionID = sql.NullString{String: t.ID, Valid: true}
		if req.RefundOf != "" {
			refunded, err := refunded(ctx, tx, req.RefundOf)
			if err != nil {
				return err
			}
			if refunded+amount > refundable {
				return errOverRefund
			}
		}
		return insertRequest(ctx, tx, *req)
	})
	switch {
	case errors.Is(err, bank.ErrDuplicate):
		return errDuplicate
	case errors.Is(err, bank.ErrNoCustomer):
		req.TransactionID = sql.NullString{}
		req.Failure = strings.ToUpper(op.party) + "_NOT_FOUND"
		err = s.store.insertRequest(ctx, *req)
		if isUnique(err) {
			return errDuplicate
		}
		return err
	}
	return err
}

// execute decides at once: in the sandbox the way MTN's test numbers do, and for payouts
// the way MTN runs them without asking anyone. A failure here leaves the payment pending
// until it expires, as a glitch at MTN would.
func (s *Service) execute(ctx context.Context, op operation, req request) {
	id := req.TransactionID.String
	var err error
	switch {
	case req.PartyID == numberTimeout || req.PartyID == numberPending:
		return
	case req.PartyID == numberFails:
		_, err = s.books.Refuse(ctx, id, "internal_processing_error")
	case req.PartyID == numberRejects && op.approval:
		_, err = s.books.Refuse(ctx, id, bank.ReasonRefused)
	default:
		_, err = s.books.Approve(ctx, id)
	}
	if err != nil {
		s.logger.Error("execute", "ref", req.ReferenceID, "err", err)
		return
	}
	s.notify(req)
}

func (s *Service) status(op operation) func(http.ResponseWriter, *http.Request, apiUser) {
	return func(w http.ResponseWriter, r *http.Request, u apiUser) {
		req, err := s.store.request(r.Context(), u.ID, r.PathValue("ref"))
		if errors.Is(err, ErrNotFound) || (err == nil && req.Operation != op.name) {
			writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "Requested resource was not found.")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		body, err := s.statusBody(r.Context(), u, op, req)
		if err != nil {
			s.internal(w, err)
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

type paymentStatus struct {
	Amount                 string  `json:"amount"`
	Currency               string  `json:"currency"`
	FinancialTransactionID string  `json:"financialTransactionId,omitempty"`
	ExternalID             string  `json:"externalId"`
	Payer                  *party  `json:"payer,omitempty"`
	Payee                  *party  `json:"payee,omitempty"`
	PayerMessage           *string `json:"payerMessage,omitempty"`
	PayeeNote              *string `json:"payeeNote,omitempty"`
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

func (s *Service) statusBody(ctx context.Context, u apiUser, op operation, req request) (paymentStatus, error) {
	body := paymentStatus{Amount: req.Amount, Currency: req.Currency, ExternalID: req.ExternalID, Status: "FAILED"}
	customer := &party{PartyIDType: "MSISDN", PartyID: req.PartyID}
	if op.party == "payer" {
		body.Payer = customer
	} else {
		body.Payee = customer
	}
	if op.messages {
		body.PayerMessage, body.PayeeNote = &req.PayerMessage, &req.PayeeNote
	}
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
	case "PAYEE_NOT_FOUND":
		return "The payee was not found."
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
