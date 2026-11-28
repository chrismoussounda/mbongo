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
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/money"
)

// operation is one of MTN's payment resources. They share validation, storage and status.
type operation struct {
	name    string
	product string
	kind    bank.Kind
	party   string
	// approval: whether the customer has to say yes, or MTN executes at once.
	approval bool
}

func operationNamed(name string) (operation, error) {
	for _, op := range []operation{opRequestToPay, opTransfer, opDeposit, opRefund} {
		if op.name == name {
			return op, nil
		}
	}
	return operation{}, fmt.Errorf("mtn: unknown operation %q", name)
}

var (
	opRequestToPay = operation{name: "requesttopay", product: collection, kind: bank.Collection, party: "payer",
		approval: true}
	opTransfer = operation{name: "transfer", product: disbursement, kind: bank.Payout, party: "payee"}
	opDeposit  = operation{name: "deposit", product: disbursement, kind: bank.Payout, party: "payee"}
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

// congoNumber is an MTN Congo line in the form the API takes: 242, then 06 and seven digits.
// Only such numbers become automatic customers; others stay unknown, as at MTN.
var congoNumber = regexp.MustCompile(`^24206[0-9]{7}$`)

func (s *Service) ensureCustomer(ctx context.Context, phone string) error {
	if !congoNumber.MatchString(phone) {
		return nil
	}
	return s.books.EnsureCustomer(ctx, bank.MTN, phone, currency(MTNCongo))
}

func (s *Service) create(op operation) func(http.ResponseWriter, *http.Request, apiUser) {
	return func(w http.ResponseWriter, r *http.Request, u apiUser) {
		req, amount, ok := s.readRequest(w, r, u, op)
		if !ok {
			return
		}
		var refundable int64
		if op == opRefund {
			if refundable, ok = s.checkRefund(w, r, u, &req); !ok {
				return
			}
		}
		switch err := s.start(r.Context(), u, op, &req, amount, refundable); {
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
		if req.TransactionID.Valid && (u.Environment == Sandbox || !op.approval) && !faults.Is(r.Context(), faults.Stuck) {
			s.execute(r.Context(), u, op, req)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// readRequest checks the headers and body MTN checks, and answers the error itself.
func (s *Service) readRequest(w http.ResponseWriter, r *http.Request, u apiUser, op operation) (request, int64, bool) {
	fail := func(status int, code, message string) (request, int64, bool) {
		writeError(w, status, code, message)
		return request{}, 0, false
	}
	ref := r.Header.Get("X-Reference-Id")
	if !isUUID(ref) {
		return fail(http.StatusBadRequest, "INVALID_REFERENCE_ID", "X-Reference-Id must be a UUID version 4.")
	}
	var p paymentBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&p); err != nil {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "The request body is not valid JSON.")
	}
	if p.Currency != currency(u.Environment) {
		return fail(http.StatusInternalServerError, "INVALID_CURRENCY", "Currency not supported.")
	}
	amount, err := money.Parse(p.Amount, p.Currency)
	if err != nil || amount == 0 {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "amount is not a valid positive amount for this currency.")
	}
	if utf8.RuneCountInString(p.PayerMessage) > maxMessage || utf8.RuneCountInString(p.PayeeNote) > maxMessage {
		return fail(http.StatusBadRequest, "VALIDATION_ERROR", "payerMessage and payeeNote are limited to 160 characters.")
	}
	callback := r.Header.Get("X-Callback-Url")
	if callback != "" {
		cu, err := url.Parse(callback)
		if err != nil || cu.Hostname() != u.CallbackHost {
			return fail(http.StatusBadRequest, "INVALID_CALLBACK_URL_HOST", "Callback URL host does not match the API user's providerCallbackHost.")
		}
	}
	req := request{APIUser: u.ID, ReferenceID: ref, Product: op.product, Operation: op.name,
		Amount: p.Amount, Currency: p.Currency, ExternalID: p.ExternalID, PayerMessage: p.PayerMessage,
		PayeeNote: p.PayeeNote, CallbackURL: callback}
	if op == opRefund {
		req.RefundOf = p.ReferenceIDToRefund
		return req, amount, true
	}
	customer := p.Payer
	if op.party == "payee" {
		customer = p.Payee
	}
	if customer.PartyIDType != "MSISDN" || !digitsOnly.MatchString(customer.PartyID) {
		w.WriteHeader(http.StatusBadRequest)
		return request{}, 0, false
	}
	req.PartyID = customer.PartyID
	return req, amount, true
}

var errOverRefund = errors.New("mtn: refund larger than what is left of the payment")

// checkRefund allows a refund of a successful collection only, and returns its amount.
// What is left to refund is checked when the refund is saved.
func (s *Service) checkRefund(w http.ResponseWriter, r *http.Request, u apiUser, req *request) (int64, bool) {
	ctx, of := r.Context(), req.RefundOf
	partner, err := s.store.partner(ctx, u.Subscription)
	if err != nil {
		s.internal(w, err)
		return 0, false
	}
	original, err := s.store.collectionToRefund(ctx, of, u.Environment, partner)
	if errors.Is(err, ErrNotFound) {
		transactionNotFound(w)
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
		transactionNotFound(w)
		return 0, false
	}
	req.PartyID = original.PartyID
	return paid, true
}

// transactionNotFound is MTN's answer to a refund of a payment that isn't there or wasn't paid.
func transactionNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusInternalServerError, "TRANSACTION_NOT_FOUND", "The transaction could not be found")
}

// start saves the request together with its bank transaction, or alone, failed, when the
// customer has no wallet.
func (s *Service) start(ctx context.Context, u apiUser, op operation, req *request, amount, refundable int64) error {
	if u.Environment == Sandbox {
		if _, err := s.books.OpenCustomer(ctx, bank.MTN, req.PartyID, req.Currency, 0); err != nil &&
			!errors.Is(err, bank.ErrExists) {
			return err
		}
	} else if err := s.ensureCustomer(ctx, req.PartyID); err != nil {
		return err
	}
	window := approvalWindow
	switch {
	case faults.Is(ctx, faults.Stuck):
		window = forever
	case u.Environment == Sandbox && req.PartyID == numberPending:
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
func (s *Service) execute(ctx context.Context, u apiUser, op operation, req request) {
	id := req.TransactionID.String
	var err error
	switch {
	case req.PartyID == numberPending:
		return
	case req.PartyID == numberTimeout:
		_, err = s.books.Refuse(ctx, id, "expired")
	case req.PartyID == numberFails:
		_, err = s.books.Refuse(ctx, id, "internal_processing_error")
	case req.PartyID == numberRejects && op.approval:
		_, err = s.books.Refuse(ctx, id, bank.ReasonRefused)
	default:
		err = s.approve(ctx, u, op, id)
	}
	if err != nil {
		s.logger.Error("execute", "ref", req.ReferenceID, "err", err)
		return
	}
	s.notify(req)
}

// approve moves the money. A sandbox payer always has enough, so they are topped up first,
// whatever was paid to or from them before.
func (s *Service) approve(ctx context.Context, u apiUser, op operation, id string) error {
	if u.Environment == Sandbox && op.kind == bank.Collection {
		t, err := s.books.Transaction(ctx, id)
		if err != nil {
			return err
		}
		payer, err := s.books.Account(ctx, t.CustomerID)
		if err != nil {
			return err
		}
		if short := t.Amount - payer.Balance; short > 0 {
			if _, err := s.books.Fund(ctx, payer.ID, short); err != nil {
				return err
			}
		}
	}
	_, err := s.books.Approve(ctx, id)
	return err
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
	Amount                 string `json:"amount"`
	Currency               string `json:"currency"`
	FinancialTransactionID string `json:"financialTransactionId,omitempty"`
	ExternalID             string `json:"externalId"`
	Payer                  *party `json:"payer,omitempty"`
	Payee                  *party `json:"payee,omitempty"`
	PayerMessage           string `json:"payerMessage"`
	PayeeNote              string `json:"payeeNote"`
	Status                 string `json:"status"`
	Reason                 string `json:"reason,omitempty"`
}

func (s *Service) statusBody(ctx context.Context, u apiUser, op operation, req request) (paymentStatus, error) {
	body := paymentStatus{Amount: req.Amount, Currency: req.Currency, ExternalID: req.ExternalID,
		PayerMessage: req.PayerMessage, PayeeNote: req.PayeeNote, Status: "FAILED"}
	customer := &party{PartyIDType: "MSISDN", PartyID: req.PartyID}
	if op.party == "payer" {
		body.Payer = customer
	} else {
		body.Payee = customer
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
			body.FinancialTransactionID = financialID(t.ID)
		case bank.Expired:
			code = "EXPIRED"
		case bank.Failed:
			code = failureCode(t.Reason, u.Environment)
		}
	}
	if body.Status == "FAILED" {
		body.Reason = code
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

// balance is refused in the sandbox, as MTN's sandbox refuses it.
func (s *Service) balance(product string) func(http.ResponseWriter, *http.Request, apiUser) {
	return func(w http.ResponseWriter, r *http.Request, u apiUser) {
		if u.Environment == Sandbox && product == collection {
			writeError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "Requested resource was not found.")
			return
		}
		if u.Environment == Sandbox {
			writeError(w, http.StatusInternalServerError, "NOT_ALLOWED", "Authorization failed. Insufficient permissions.")
			return
		}
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
}

func (s *Service) accountHolder(w http.ResponseWriter, r *http.Request, u apiUser) {
	if strings.ToLower(r.PathValue("type")) != "msisdn" {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Only msisdn is supported.")
		return
	}
	if u.Environment == Sandbox {
		wrongEnvironment(w)
		return
	}
	if err := s.ensureCustomer(r.Context(), r.PathValue("id")); err != nil {
		s.internal(w, err)
		return
	}
	active, err := s.store.hasCustomer(r.Context(), r.PathValue("id"), currency(u.Environment))
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"result": active})
}

// financialID is the number MTN gives a payment once money moved.
func financialID(transactionID string) string {
	return fmt.Sprint(crc32.ChecksumIEEE([]byte(transactionID)))
}
