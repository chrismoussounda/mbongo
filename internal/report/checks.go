package report

import (
	"context"
	"time"
)

// Severities: an error loses or double-moves money, or leaves it unaccounted for; a warning is
// a practice that will cause one sooner or later.
const (
	Error   = "error"
	Warning = "warning"
)

// Groups of checks.
const (
	DoubleMoney = "double-money"
	Trust       = "trust"
	Refunds     = "refunds"
	Hygiene     = "api-hygiene"
)

// Refused is a creation the operator turned down at once: no payment exists.
const Refused = "refused"

// Finding is one mistake found in a client's traffic.
type Finding struct {
	Check     string  `json:"check"`
	Group     string  `json:"group"`
	Severity  string  `json:"severity"`
	Reference string  `json:"reference,omitempty"`
	Message   string  `json:"message"`
	Calls     []int64 `json:"calls,omitempty"`
}

// Target is the payment a refund points at.
type Target struct {
	Reference string
	Operation string
	Amount    string
	Currency  string
}

// Ledger is what an operator knows about a client's payments that the calls don't show.
type Ledger interface {
	// Outcome is what the client would learn about one of its payments if it asked now, or ""
	// when there is no such payment.
	Outcome(ctx context.Context, client, reference, operation string) (string, error)
	// RefundTarget finds the payment a refund points at by what the client sent; found is
	// false when there is none.
	RefundTarget(ctx context.Context, client, refundOf string) (t Target, found bool, err error)
}

// ClientReport is what the checks found in one client's traffic during a run.
type ClientReport struct {
	Run       int64     `json:"run"`
	Operator  string    `json:"operator"`
	Client    string    `json:"client"`
	Calls     int       `json:"calls"`
	Callbacks int       `json:"callbacks"`
	Errors    int       `json:"errors"`
	Warnings  int       `json:"warnings"`
	Findings  []Finding `json:"findings"`
}

// Operations that start a payment, by what they do with the client's money.
var (
	collections = map[string]bool{"requesttopay": true, "collection": true, "cashout": true}
	payouts     = map[string]bool{"transfer": true, "deposit": true, "cashin": true, "disbursement": true}
)

func creates(op string) bool { return collections[op] || payouts[op] || op == "refund" }

func direction(op string) string {
	switch {
	case collections[op]:
		return "collection"
	case payouts[op]:
		return "payout"
	}
	return op
}

// payment is one payment a client started, with what it learned about it and how it ended.
type payment struct {
	key       string
	reference string
	operation string
	creations []Call
	statuses  []Call
	callbacks []Callback
	final     string
}

func (p *payment) first() Call { return p.creations[0] }

// learned is what the client knew of the payment at a time: the last outcome a status call or
// a delivered callback told it.
func (p *payment) learned(at time.Time) string {
	var (
		known string
		when  time.Time
	)
	for _, c := range p.statuses {
		if c.Status == 200 && c.Outcome != "" && !c.At.After(at) && !c.At.Before(when) {
			known, when = c.Outcome, c.At
		}
	}
	for _, cb := range p.callbacks {
		if delivered(cb) && !cb.At.After(at) && !cb.At.Before(when) {
			known, when = cb.Outcome, cb.At
		}
	}
	return known
}

func delivered(cb Callback) bool {
	return cb.Error == "" && cb.HTTPStatus >= 200 && cb.HTTPStatus < 300
}

// family keeps apart the payments an operator numbers separately: Airtel's disbursements
// and refunds have ids of their own.
func family(operator string, c Call) string {
	if operator != "airtel" {
		return ""
	}
	switch {
	case c.Operation == "disbursement" || (c.Operation == "status" && len(c.Path) > len(disbursementPath) &&
		c.Path[:len(disbursementPath)] == disbursementPath):
		return "disbursement"
	case c.Operation == "refund":
		return "refund"
	}
	return "payment"
}

const disbursementPath = "/standard/v1/disbursements/"

// traffic is one client's calls and callbacks in a run, sorted out by payment.
type traffic struct {
	operator, client string
	calls            []Call
	callbacks        []Callback
	payments         map[string]*payment
	order            []*payment
}

func keyOf(operator string, c Call) string { return family(operator, c) + "|" + c.Reference }

// Build checks each client's traffic in a run, clients in the order they first called.
func (rec *Recorder) Build(ctx context.Context, run int64, ledgers map[string]Ledger) ([]ClientReport, error) {
	calls, err := rec.Calls(ctx, run)
	if err != nil {
		return nil, err
	}
	cbs, err := rec.Callbacks(ctx, run)
	if err != nil {
		return nil, err
	}
	var (
		all   []*traffic
		byKey = map[string]*traffic{}
	)
	get := func(operator, client string) *traffic {
		k := operator + "|" + client
		if t, ok := byKey[k]; ok {
			return t
		}
		t := &traffic{operator: operator, client: client, payments: map[string]*payment{}}
		byKey[k] = t
		all = append(all, t)
		return t
	}
	for _, c := range calls {
		if c.Client != "" {
			get(c.Operator, c.Client).calls = append(get(c.Operator, c.Client).calls, c)
		}
	}
	for _, cb := range cbs {
		t := get(cb.Operator, cb.Client)
		t.callbacks = append(t.callbacks, cb)
	}
	reports := make([]ClientReport, 0, len(all))
	for _, t := range all {
		if err := t.sort(ctx, ledgers[t.operator]); err != nil {
			return nil, err
		}
		r := ClientReport{Run: run, Operator: t.operator, Client: t.client, Calls: len(t.calls),
			Callbacks: len(t.callbacks), Findings: []Finding{}}
		for _, check := range checks {
			found, err := check(ctx, t, ledgers[t.operator])
			if err != nil {
				return nil, err
			}
			r.Findings = append(r.Findings, found...)
		}
		for _, f := range r.Findings {
			if f.Severity == Error {
				r.Errors++
			} else {
				r.Warnings++
			}
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// sort groups the calls and callbacks by payment and asks the ledger how each one ended.
func (t *traffic) sort(ctx context.Context, ledger Ledger) error {
	for _, c := range t.calls {
		if c.Reference == "" || (!creates(c.Operation) && c.Operation != "status") {
			continue
		}
		k := keyOf(t.operator, c)
		p, ok := t.payments[k]
		if !ok {
			if !creates(c.Operation) {
				continue
			}
			p = &payment{key: k, reference: c.Reference, operation: c.Operation}
			t.payments[k] = p
			t.order = append(t.order, p)
		}
		if creates(c.Operation) {
			p.creations = append(p.creations, c)
		} else {
			p.statuses = append(p.statuses, c)
		}
	}
	for _, cb := range t.callbacks {
		for _, p := range t.order {
			if p.reference == cb.Reference {
				p.callbacks = append(p.callbacks, cb)
				break
			}
		}
	}
	if ledger == nil {
		return nil
	}
	for _, p := range t.order {
		final, err := ledger.Outcome(ctx, t.client, p.reference, p.operation)
		if err != nil {
			return err
		}
		p.final = final
	}
	return nil
}

type check func(ctx context.Context, t *traffic, ledger Ledger) ([]Finding, error)

var checks = []check{doubleMoney}

func ids(calls ...Call) []int64 {
	out := make([]int64, len(calls))
	for i, c := range calls {
		out[i] = c.ID
	}
	return out
}
