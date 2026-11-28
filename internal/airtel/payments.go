package airtel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/faults"
	"github.com/chrismoussounda/mbongo/internal/money"
)

const (
	kindCollection   = "collection"
	kindCashOut      = "cashout"
	kindCashIn       = "cashin"
	kindDisbursement = "disbursement"
	kindRefund       = "refund"
)

// codes are one product's DP response codes. Where a product has no code for a case, the
// nearest one is borrowed; docs/airtel.md lists which.
type codes struct {
	prefix                                                                    string
	success, inProcess, noFunds, refused, expired, notFound, badNumber, dupID string
}

func (c codes) code(n string) string { return c.prefix + "000010" + n }

var (
	collectionCodes   = codes{"DP008", "01", "06", "07", "08", "24", "25", "10", "05"}
	cashCodes         = codes{"DP010", "01", "06", "07", "13", "14", "05", "12", "11"}
	disbursementCodes = codes{"DP009", "01", "06", "07", "13", "05", "15", "12", "11"}
)

func codesFor(kind string) codes {
	switch kind {
	case kindCashIn, kindCashOut:
		return cashCodes
	case kindDisbursement:
		return disbursementCodes
	}
	return collectionCodes
}

// congoNumber is an Airtel Congo line as the API takes it: 05 or 04 and seven digits, no 242.
// Only such numbers become automatic customers; others stay unknown, as at Airtel.
var congoNumber = regexp.MustCompile(`^0[45][0-9]{7}$`)

func (s *Service) ensureCustomer(ctx context.Context, msisdn string) error {
	if !congoNumber.MatchString(msisdn) {
		return nil
	}
	return s.books.EnsureCustomer(ctx, bank.Airtel, msisdn, currency)
}

// amount takes a JSON number or a string, since clients send both.
type amount string

func (a *amount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = amount(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*a = amount(n.String())
	return nil
}

type party struct {
	MSISDN string `json:"msisdn"`
}

type paymentBody struct {
	Reference   string `json:"reference"`
	Subscriber  party  `json:"subscriber"`
	Payee       party  `json:"payee"`
	PIN         string `json:"pin"`
	Transaction struct {
		Amount        amount `json:"amount"`
		ID            string `json:"id"`
		AirtelMoneyID string `json:"airtel_money_id"`
	} `json:"transaction"`
}

// read decodes the body, or answers ROUTER003 for a missing or broken one.
func read(w http.ResponseWriter, r *http.Request, in any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(in); err != nil {
		refuse(w, http.StatusBadRequest, "ROUTER003", "Missing header/body params")
		return false
	}
	return true
}

// start reads a payment and records it, returning the bank transaction still pending.
func (s *Service) start(w http.ResponseWriter, r *http.Request, app App, kind string, in paymentBody) (bank.Transaction, payment, bool) {
	c := codesFor(kind)
	msisdn := in.Subscriber.MSISDN
	if kind == kindDisbursement {
		msisdn = in.Payee.MSISDN
	}
	value, err := money.Parse(string(in.Transaction.Amount), currency)
	if msisdn == "" || in.Transaction.ID == "" || in.Reference == "" || in.Transaction.Amount == "" {
		refuse(w, http.StatusBadRequest, "ROUTER003", "Missing header/body params")
		return bank.Transaction{}, payment{}, false
	}
	if err != nil || value <= 0 {
		fail(w, c.code("04"), "Invalid Amount", map[string]any{})
		return bank.Transaction{}, payment{}, false
	}
	p := payment{ClientID: app.ClientID, ID: in.Transaction.ID, Kind: kind, MSISDN: msisdn, Amount: value,
		Reference: in.Reference}
	if err := s.ensureCustomer(r.Context(), msisdn); err != nil {
		s.internal(w, err)
		return bank.Transaction{}, payment{}, false
	}
	t, err := s.record(r.Context(), &p, bank.Request{Kind: bankKind(kind), Operator: bank.Airtel,
		Merchant: app.ClientID, Phone: msisdn, Amount: value, Currency: currency, ExternalID: in.Transaction.ID,
		Window: approvalWindow}, nil)
	switch {
	case errors.Is(err, bank.ErrNoCustomer):
		fail(w, c.code(c.badNumber), "Invalid mobile number", map[string]any{})
	case errors.Is(err, errDuplicate):
		fail(w, c.code(c.dupID), "Transaction with similar information already exists", map[string]any{})
	case err != nil:
		s.internal(w, err)
	default:
		return t, p, true
	}
	return bank.Transaction{}, payment{}, false
}

// record saves the bank transaction and Airtel's record of it together, under a fresh
// airtel_money_id, drawing another if that one is taken. check runs first, in the same
// database transaction.
func (s *Service) record(ctx context.Context, p *payment, req bank.Request, check func(*sql.Tx) error) (bank.Transaction, error) {
	if rule, ok := faults.FromContext(ctx); ok {
		switch rule.Fault {
		case faults.Stuck:
			req.Window = forever
		case faults.Ambiguous:
			delay := rule.Delay
			if delay == 0 {
				delay = ambiguousFor
			}
			p.AmbiguousUntil = s.books.Clock().Now().Add(delay).UTC().Format(bank.TimeLayout)
		}
	}
	for {
		p.AirtelMoneyID = s.moneyID()
		if p.Kind == kindRefund {
			p.ID = p.AirtelMoneyID
		}
		req.PartnerRef = p.AirtelMoneyID
		t, err := s.books.RequestWith(ctx, req, func(tx *sql.Tx, t bank.Transaction) error {
			if check != nil {
				if err := check(tx); err != nil {
					return err
				}
			}
			p.TransactionID = t.ID
			return insertPayment(ctx, tx, *p)
		})
		if !errors.Is(err, errMoneyIDUsed) && !errors.Is(err, bank.ErrDuplicate) {
			return t, err
		}
	}
}

func bankKind(kind string) bank.Kind {
	if kind == kindCollection || kind == kindCashOut {
		return bank.Collection
	}
	return bank.Payout
}

// collect asks the customer to approve, as a USSD push does.
func (s *Service) collect(w http.ResponseWriter, r *http.Request, app App) {
	var in paymentBody
	if !read(w, r, &in) {
		return
	}
	if _, _, started := s.start(w, r, app, kindCollection, in); !started {
		return
	}
	ok(w, collectionCodes.code(collectionCodes.inProcess), "SUCCESS",
		map[string]any{"transaction": map[string]string{"id": in.Transaction.ID, "status": "SUCCESS"}})
}

// cashOut takes the money from the customer at once: the docs answer success straight away
// and describe no prompt.
func (s *Service) cashOut(w http.ResponseWriter, r *http.Request, app App) {
	var in paymentBody
	if !read(w, r, &in) {
		return
	}
	t, p, started := s.start(w, r, app, kindCashOut, in)
	if !started {
		return
	}
	t, done := s.execute(w, r, t)
	if !done {
		return
	}
	s.notify(p)
	if st, unsettled := s.unsettled(r.Context(), p, t); unsettled {
		ok(w, st.code, "SUCCESS", map[string]string{"id": p.ID, "status": st.status})
		return
	}
	if t.Status != bank.Successful {
		fail(w, cashCodes.code(cashCodes.noFunds), "Insufficient Funds", map[string]string{"id": p.ID, "status": "FAILED"})
		return
	}
	ok(w, cashCodes.code(cashCodes.success), "SUCCESS", map[string]string{"id": p.ID, "status": "SUCCESS"})
}

func (s *Service) cashIn(w http.ResponseWriter, r *http.Request, app App) {
	var in paymentBody
	if !read(w, r, &in) || !s.checkPIN(w, r, app, in.PIN) {
		return
	}
	t, p, started := s.start(w, r, app, kindCashIn, in)
	if !started {
		return
	}
	t, done := s.execute(w, r, t)
	if !done {
		return
	}
	if st, unsettled := s.unsettled(r.Context(), p, t); unsettled {
		ok(w, st.code, "SUCCESS", map[string]any{"message": st.message, "status": st.status,
			"transaction": map[string]string{"id": p.ID}})
		return
	}
	if t.Status != bank.Successful {
		fail(w, cashCodes.code(cashCodes.noFunds), "Insufficient Funds", map[string]any{"message": "Insufficient Funds",
			"status": "FAILED", "transaction": map[string]string{"id": p.ID}})
		return
	}
	// Airtel's own spelling.
	ok(w, cashCodes.code(cashCodes.success), "SUCCESS", map[string]any{"message": "Transaction Successfull", //nolint:misspell
		"status": "SUCCESS", "transaction": map[string]string{"airtel_money_id": p.AirtelMoneyID, "id": p.ID}})
}

func (s *Service) disburse(w http.ResponseWriter, r *http.Request, app App) {
	var in paymentBody
	if !read(w, r, &in) || !s.checkPIN(w, r, app, in.PIN) {
		return
	}
	t, p, started := s.start(w, r, app, kindDisbursement, in)
	if !started {
		return
	}
	t, done := s.execute(w, r, t)
	if !done {
		return
	}
	c := disbursementCodes
	if st, unsettled := s.unsettled(r.Context(), p, t); unsettled {
		ok(w, st.code, "SUCCESS", map[string]any{"transaction": map[string]string{"id": p.ID, "status": st.status}})
		return
	}
	if t.Status != bank.Successful {
		fail(w, c.code(c.noFunds), "Insufficient Funds",
			map[string]any{"transaction": map[string]string{"id": p.ID, "status": "TF"}})
		return
	}
	merchant, err := s.books.Account(r.Context(), t.MerchantID)
	if err != nil {
		s.internal(w, err)
		return
	}
	ok(w, c.code(c.success), fmt.Sprintf("Trans.ID : %s. You have sent XAF %s to %s. Your available balance is XAF %s.",
		p.AirtelMoneyID, formatXAF(p.Amount, false), p.MSISDN, formatXAF(merchant.Balance, false)),
		map[string]any{"transaction": map[string]string{"reference_id": fmt.Sprintf("%010d", rand.Int64N(1e10)), //nolint:gosec // G404: an id
			"airtel_money_id": p.AirtelMoneyID, "id": p.ID, "status": "TS"}})
}

// refund pays a successful collection back in full, once.
func (s *Service) refund(w http.ResponseWriter, r *http.Request, app App) {
	var in paymentBody
	if !read(w, r, &in) {
		return
	}
	c := collectionCodes
	if in.Transaction.AirtelMoneyID == "" {
		refuse(w, http.StatusBadRequest, "ROUTER003", "Missing header/body params")
		return
	}
	original, err := s.store.paymentByMoneyID(r.Context(), app.ClientID, in.Transaction.AirtelMoneyID, kindCollection)
	if errors.Is(err, ErrNotFound) {
		fail(w, c.code(c.notFound), "Transaction Not Found", map[string]any{})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	paid, err := s.books.Transaction(r.Context(), original.TransactionID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if paid.Status != bank.Successful {
		fail(w, c.code(c.refused), "Refused", map[string]any{})
		return
	}
	p := payment{ClientID: app.ClientID, Kind: kindRefund, MSISDN: original.MSISDN, Amount: original.Amount,
		Reference: original.Reference, RefundOf: original.AirtelMoneyID}
	t, err := s.record(r.Context(), &p, bank.Request{Kind: bank.Payout, Operator: bank.Airtel,
		Merchant: app.ClientID, Phone: p.MSISDN, Amount: p.Amount, Currency: currency, ExternalID: original.ID},
		func(tx *sql.Tx) error {
			done, err := refunded(r.Context(), tx, app.ClientID, original.AirtelMoneyID)
			if err == nil && done {
				return errRefunded
			}
			return err
		})
	if errors.Is(err, errRefunded) {
		fail(w, c.code(c.refused), "Transaction already refunded", map[string]any{})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	t, done := s.execute(w, r, t)
	if !done {
		return
	}
	if st, unsettled := s.unsettled(r.Context(), p, t); unsettled {
		ok(w, st.code, "SUCCESS", map[string]any{"transaction": map[string]string{"airtel_money_id": p.AirtelMoneyID, "status": st.status}})
		return
	}
	if t.Status != bank.Successful {
		fail(w, c.code(c.noFunds), "Not enough balance", map[string]any{})
		return
	}
	ok(w, c.code(c.success), "SUCCESS",
		map[string]any{"transaction": map[string]string{"airtel_money_id": p.AirtelMoneyID, "status": "SUCCESS"}})
}

// execute settles a transaction nobody has to approve. A short payer fails it, which the
// caller reads from the returned status.
func (s *Service) execute(w http.ResponseWriter, r *http.Request, t bank.Transaction) (bank.Transaction, bool) {
	if faults.Is(r.Context(), faults.Stuck) {
		return t, true
	}
	t, err := s.books.Approve(r.Context(), t.ID)
	if err != nil {
		s.internal(w, err)
		return t, false
	}
	return t, true
}

// enquiry answers the status of one of the app's transactions of the given kinds.
func (s *Service) enquiry(kinds ...string) func(http.ResponseWriter, *http.Request, App) {
	return func(w http.ResponseWriter, r *http.Request, app App) {
		p, err := s.store.payment(r.Context(), app.ClientID, r.PathValue("id"), kinds...)
		c := codesFor(kinds[0])
		if errors.Is(err, ErrNotFound) {
			fail(w, c.code(c.notFound), "Transaction Not Found", map[string]any{})
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		st, err := s.outcome(r.Context(), p)
		if err != nil {
			s.internal(w, err)
			return
		}
		tx := map[string]string{"id": p.ID, "message": st.message, "status": st.status}
		if st.status == "TS" {
			tx["airtel_money_id"] = p.AirtelMoneyID
		}
		ok(w, st.code, "SUCCESS", map[string]any{"transaction": tx})
	}
}

type outcome struct {
	status, code, message string
}

// unsettled reports the outcome of a payment that has none yet to give: stuck in progress,
// or ambiguous for a while.
func (s *Service) unsettled(ctx context.Context, p payment, t bank.Transaction) (outcome, bool) {
	st, err := s.outcome(ctx, p)
	if err != nil || (st.status != "TIP" && st.status != "TA") {
		return outcome{}, false
	}
	return st, t.Status == bank.Pending || st.status == "TA"
}

func (s *Service) outcome(ctx context.Context, p payment) (outcome, error) {
	t, err := s.books.Transaction(ctx, p.TransactionID)
	if err != nil {
		return outcome{}, err
	}
	c := codesFor(p.Kind)
	if p.AmbiguousUntil != "" && s.books.Clock().Now().UTC().Format(bank.TimeLayout) < p.AmbiguousUntil {
		return outcome{"TA", c.code("00"), "Transaction is ambiguous"}, nil
	}
	switch t.Status {
	case bank.Successful:
		return outcome{"TS", c.code(c.success), "success"}, nil
	case bank.Pending:
		return outcome{"TIP", c.code(c.inProcess), "Transaction in pending state"}, nil
	case bank.Expired:
		return outcome{"TE", c.code(c.expired), "Transaction Timed Out"}, nil
	}
	if t.Reason == bank.ReasonNotEnoughFunds {
		return outcome{"TF", c.code(c.noFunds), "Not enough balance"}, nil
	}
	return outcome{"TF", c.code(c.refused), "The transaction was refused"}, nil
}

func (s *Service) balance(w http.ResponseWriter, r *http.Request, app App) {
	a, err := s.Account(r.Context(), app.ClientID)
	if err != nil {
		s.internal(w, err)
		return
	}
	ok(w, "DP02100000001", "SUCCESS", map[string]string{
		"balance": formatXAF(a.Balance, true), "currency": currency, "account_status": "Active"})
}

// user answers Airtel's KYC lookup with placeholder names, since Mbongo keeps none.
func (s *Service) user(w http.ResponseWriter, r *http.Request, _ App) {
	msisdn := r.PathValue("msisdn")
	if err := s.ensureCustomer(r.Context(), msisdn); err != nil {
		s.internal(w, err)
		return
	}
	_, err := s.books.FindAccount(r.Context(), bank.Airtel, bank.CustomerAccount, msisdn, currency)
	if errors.Is(err, bank.ErrNotFound) {
		fail(w, "", "User not found", map[string]any{})
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	ok(w, "", "SUCCESS", map[string]any{"first_name": "Mbongo", "last_name": "Client", "grade": "SUBS",
		"is_barred": false, "is_pin_set": true, "msisdn": msisdn, "registration": map[string]string{"status": "SUBS"}})
}

// formatXAF writes "37,600", or "37,600.00" as the balance shows it.
func formatXAF(v int64, cents bool) string {
	digits := strconv.FormatInt(v, 10)
	var b bytes.Buffer
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 && digits[i-1] != '-' {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if cents {
		b.WriteString(".00")
	}
	return b.String()
}

// moneyID looks like the docs' example "MP210603.1234.L06941".
func (s *Service) moneyID() string {
	now := s.books.Clock().Now().UTC()
	return fmt.Sprintf("MP%s.%s.%c%05d", now.Format("060102"), now.Format("1504"),
		'A'+rune(rand.IntN(26)), rand.IntN(100000)) //nolint:gosec // G404: an id, not a secret
}

func callbackMessage(p payment, app App, st outcome) string {
	if st.status == "TS" {
		return fmt.Sprintf("Paid XAF %s to %s Charge XAF 0, Trans ID %s.", formatXAF(p.Amount, false),
			strings.ToUpper(app.Name), p.AirtelMoneyID)
	}
	return st.message
}
