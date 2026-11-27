// Package faults makes Mbongo fail on purpose, the ways real operators fail, when a test asks
// for it through a rule.
package faults

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/chrismoussounda/mbongo/internal/migrate"
)

// The faults a rule can ask for.
const (
	// AnswerLost does the work, then drops the connection instead of answering.
	AnswerLost = "answer_lost"
	// ErrorAfterMoney does the work, then answers 500.
	ErrorAfterMoney = "error_after_money"
	// Slow does the work, then answers after Delay.
	Slow = "slow"
	// RateLimited answers 429 without doing anything.
	RateLimited = "rate_limited"
	// Maintenance answers 503 without doing anything.
	Maintenance = "maintenance"
	// TokenRevoked answers as if the access token had been revoked.
	TokenRevoked = "token_revoked"
	// Stuck leaves the payment in progress for good.
	Stuck = "stuck"
	// Ambiguous moves the money but reports the outcome as unknown for Delay (Airtel's TA).
	Ambiguous = "ambiguous"
	// CallbackTwice, CallbackLate, CallbackNever and CallbackBeforeAnswer change the callback.
	CallbackTwice        = "callback_twice"
	CallbackLate         = "callback_late"
	CallbackNever        = "callback_never"
	CallbackBeforeAnswer = "callback_before_answer"
)

// Callback is the operation callback rules match.
const Callback = "callback"

var (
	ErrUnknownFault = errors.New("faults: unknown fault")
	ErrNotFound     = errors.New("faults: no such rule")
	ErrOperation    = errors.New("faults: callback faults go with the callback operation, and only they do")
)

var callbackFaults = map[string]bool{CallbackTwice: true, CallbackLate: true, CallbackNever: true,
	CallbackBeforeAnswer: true}

var known = map[string]bool{AnswerLost: true, ErrorAfterMoney: true, Slow: true, RateLimited: true,
	Maintenance: true, TokenRevoked: true, Stuck: true, Ambiguous: true, CallbackTwice: true,
	CallbackLate: true, CallbackNever: true, CallbackBeforeAnswer: true}

// Rule applies Fault to the operator's requests that match Operation and Phone (empty matches
// any), Times times, or until cleared when Times is 0.
type Rule struct {
	ID        int64         `json:"id"`
	Operator  string        `json:"operator"`
	Operation string        `json:"operation,omitempty"`
	Phone     string        `json:"phone,omitempty"`
	Fault     string        `json:"fault"`
	Delay     time.Duration `json:"-"`
	Times     int           `json:"times"`
}

func (r Rule) MarshalJSON() ([]byte, error) {
	type plain Rule
	return json.Marshal(struct {
		plain
		DelaySeconds float64 `json:"delaySeconds,omitempty"`
	}{plain(r), r.Delay.Seconds()})
}

const schema = `
CREATE TABLE fault_rules (
    id        INTEGER PRIMARY KEY,
    operator  TEXT NOT NULL,
    operation TEXT NOT NULL,
    phone     TEXT NOT NULL,
    fault     TEXT NOT NULL,
    delay_ms  INTEGER NOT NULL,
    remaining INTEGER NOT NULL
);
`

// Injector keeps the rules. A nil Injector never fails anything.
type Injector struct {
	db *sql.DB
}

func New(ctx context.Context, db *sql.DB) (*Injector, error) {
	if err := migrate.Run(ctx, db, "faults", []string{schema}); err != nil {
		return nil, err
	}
	return &Injector{db: db}, nil
}

// Add saves a rule. A callback fault always goes with the callback operation, so a rule for
// requests is never used up by a callback, nor the other way round.
func (in *Injector) Add(ctx context.Context, r Rule) (Rule, error) {
	if !known[r.Fault] {
		return r, ErrUnknownFault
	}
	if callbackFaults[r.Fault] && r.Operation == "" {
		r.Operation = Callback
	}
	if callbackFaults[r.Fault] != (r.Operation == Callback) {
		return r, ErrOperation
	}
	remaining := r.Times
	if remaining == 0 {
		remaining = -1
	}
	res, err := in.db.ExecContext(ctx, `INSERT INTO fault_rules (operator, operation, phone, fault, delay_ms, remaining)
		VALUES (?, ?, ?, ?, ?, ?)`, r.Operator, r.Operation, r.Phone, r.Fault, r.Delay.Milliseconds(), remaining)
	if err != nil {
		return r, err
	}
	r.ID, err = res.LastInsertId()
	return r, err
}

func (in *Injector) List(ctx context.Context) ([]Rule, error) {
	rows, err := in.db.QueryContext(ctx, `SELECT id, operator, operation, phone, fault, delay_ms, remaining
		FROM fault_rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	rules := []Rule{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// Remove deletes one rule, or all of them when id is 0.
func (in *Injector) Remove(ctx context.Context, id int64) error {
	if id == 0 {
		_, err := in.db.ExecContext(ctx, "DELETE FROM fault_rules")
		return err
	}
	res, err := in.db.ExecContext(ctx, "DELETE FROM fault_rules WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(err, ErrNotFound)
	}
	return nil
}

// Take uses up one application of the oldest rule matching the request, if any. A rule naming
// a phone or an operation goes before one that matches anything.
func (in *Injector) Take(ctx context.Context, operator, operation, phone string) (Rule, bool, error) {
	if in == nil {
		return Rule{}, false, nil
	}
	tx, err := in.db.BeginTx(ctx, nil)
	if err != nil {
		return Rule{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := scan(tx.QueryRowContext(ctx, `SELECT id, operator, operation, phone, fault, delay_ms, remaining
		FROM fault_rules
		WHERE operator = ? AND (operation = ? OR (operation = '' AND ? != 'callback'))
		AND phone IN ('', ?) AND remaining != 0
		ORDER BY (phone != '') + (operation != '') DESC, id LIMIT 1`, operator, operation, operation, phone))
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, err
	}
	if r.Times > 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE fault_rules SET remaining = remaining - 1 WHERE id = ?", r.ID); err != nil {
			return Rule{}, false, err
		}
		if r.Times == 1 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM fault_rules WHERE id = ?", r.ID); err != nil {
				return Rule{}, false, err
			}
		}
	}
	return r, true, tx.Commit()
}

type scanner interface{ Scan(dest ...any) error }

func scan(s scanner) (Rule, error) {
	var (
		r     Rule
		delay int64
	)
	err := s.Scan(&r.ID, &r.Operator, &r.Operation, &r.Phone, &r.Fault, &delay, &r.Times)
	if r.Times < 0 {
		r.Times = 0
	}
	r.Delay = time.Duration(delay) * time.Millisecond
	return r, err
}

type ruleKey struct{}

// FromContext returns the rule applied to the request being handled, if any.
func FromContext(ctx context.Context) (Rule, bool) {
	r, ok := ctx.Value(ruleKey{}).(Rule)
	return r, ok
}

// Is reports whether the request being handled carries the given fault.
func Is(ctx context.Context, fault string) bool {
	r, ok := FromContext(ctx)
	return ok && r.Fault == fault
}

// Answers writes an operator's own answers to the faults it fakes.
type Answers interface {
	RateLimited(w http.ResponseWriter)
	Maintenance(w http.ResponseWriter)
	TokenRevoked(w http.ResponseWriter)
	Internal(w http.ResponseWriter)
}

const maxBody = 64 << 10

// Wrap applies the matching rule to one of the operator's routes. The phone is read from the
// first 64 KiB of the body, wherever the operator puts it; the handler still gets all of it.
func (in *Injector) Wrap(operator, operation string, answers Answers, next http.Handler) http.Handler {
	if in == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peek, _ := io.ReadAll(io.LimitReader(r.Body, maxBody))
		r.Body = readCloser{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
		rule, found, err := in.Take(r.Context(), operator, operation, Phone(peek))
		if err != nil {
			answers.Internal(w)
			return
		}
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		switch rule.Fault {
		case RateLimited:
			answers.RateLimited(w)
			return
		case Maintenance:
			answers.Maintenance(w)
			return
		case TokenRevoked:
			answers.TokenRevoked(w)
			return
		}
		held := &heldResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(held, r.WithContext(context.WithValue(r.Context(), ruleKey{}, rule)))
		switch rule.Fault {
		case AnswerLost:
			panic(http.ErrAbortHandler)
		case ErrorAfterMoney:
			answers.Internal(w)
			return
		case Slow:
			select {
			case <-time.After(rule.Delay):
			case <-r.Context().Done():
				return
			}
		}
		held.copyTo(w)
	})
}

// Phone finds the customer's number in a request body: MTN's payer or payee partyId,
// Airtel's subscriber or payee msisdn.
func Phone(body []byte) string {
	var b struct {
		Payer struct {
			PartyID string `json:"partyId"`
		} `json:"payer"`
		Payee struct {
			PartyID string `json:"partyId"`
			MSISDN  string `json:"msisdn"`
		} `json:"payee"`
		Subscriber struct {
			MSISDN string `json:"msisdn"`
		} `json:"subscriber"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	for _, p := range []string{b.Payer.PartyID, b.Payee.PartyID, b.Payee.MSISDN, b.Subscriber.MSISDN} {
		if p != "" {
			return p
		}
	}
	return ""
}

// readCloser hands the handler the whole body again, the part read to find the phone first.
type readCloser struct {
	io.Reader
	io.Closer
}

// heldResponse keeps the handler's answer until the fault decides what the client gets.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header         { return h.header }
func (h *heldResponse) WriteHeader(status int)      { h.status = status }
func (h *heldResponse) Write(b []byte) (int, error) { return h.body.Write(b) }

func (h *heldResponse) copyTo(w http.ResponseWriter) {
	for k, v := range h.header {
		w.Header()[k] = v
	}
	w.WriteHeader(h.status)
	_, _ = w.Write(h.body.Bytes())
}
