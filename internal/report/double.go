package report

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// payoutWindow is how close two equal payouts to one number must be to look like one paid twice.
const payoutWindow = 10 * time.Minute

func doubleMoney(_ context.Context, t *traffic, _ Ledger) ([]Finding, error) {
	var found []Finding
	found = append(found, paidTwice(t)...)
	found = append(found, lostAnswerRetried(t)...)
	found = append(found, failedRetriedWithSameReference(t)...)
	return found, nil
}

// paidTwice finds an order paid more than once, and equal payouts to one number close together.
func paidTwice(t *traffic) []Finding {
	var found []Finding
	byOrder := map[string][]*payment{}
	var orders []string
	for _, p := range t.order {
		if p.final != Successful || p.operation == "refund" || p.first().Order == "" {
			continue
		}
		k := direction(p.operation) + "|" + p.first().Order
		if byOrder[k] == nil {
			orders = append(orders, k)
		}
		byOrder[k] = append(byOrder[k], p)
	}
	flagged := map[*payment]bool{}
	for _, k := range orders {
		ps := byOrder[k]
		if len(ps) < 2 {
			continue
		}
		refs := make([]string, len(ps))
		var calls []Call
		for i, p := range ps {
			refs[i], flagged[p] = p.reference, true
			calls = append(calls, p.first())
		}
		found = append(found, Finding{Check: "order_paid_twice", Group: DoubleMoney, Severity: Error,
			Reference: ps[1].reference, Calls: ids(calls...),
			Message: fmt.Sprintf("Order %q went through %d times as a %s (references %s): the money moved more than once.",
				ps[0].first().Order, len(ps), direction(ps[0].operation), strings.Join(refs, ", "))})
	}
	var paid []*payment
	for _, p := range t.order {
		if payouts[p.operation] && p.final == Successful && !flagged[p] {
			paid = append(paid, p)
		}
	}
	for i, a := range paid {
		for _, b := range paid[i+1:] {
			ca, cb := a.first(), b.first()
			if flagged[b] || ca.Phone != cb.Phone || ca.Amount != cb.Amount || cb.At.Sub(ca.At) > payoutWindow {
				continue
			}
			flagged[b] = true
			found = append(found, Finding{Check: "payout_twice", Group: DoubleMoney, Severity: Warning,
				Reference: b.reference, Calls: ids(ca, cb),
				Message: fmt.Sprintf("%s %s was paid out twice to %s within %s (references %s and %s). If it was one payout, the second is a double payment.",
					cb.Amount, cb.Currency, cb.Phone, cb.At.Sub(ca.At).Round(time.Second), a.reference, b.reference)})
		}
	}
	return found
}

// unanswered is a creation whose result the client never got: no answer, or a server error
// that may come after the money moved.
func unanswered(c Call) bool { return creates(c.Operation) && (c.Status == 0 || c.Status >= 500) }

// sameAttempt tells whether two creations are for the same thing: the same order, or with no
// order, the same number and amount; refunds, the same payment.
func sameAttempt(a, b Call) bool {
	if direction(a.Operation) != direction(b.Operation) {
		return false
	}
	if a.Operation == "refund" {
		return a.RefundOf == b.RefundOf
	}
	if a.Order != "" || b.Order != "" {
		return a.Order == b.Order
	}
	return a.Phone == b.Phone && a.Amount == b.Amount
}

// lostAnswerRetried finds a creation without an answer followed by the same attempt under a new
// reference, with no status call in between to see whether the first went through.
func lostAnswerRetried(t *traffic) []Finding {
	var found []Finding
	for i, lost := range t.calls {
		if !unanswered(lost) || lost.Reference == "" {
			continue
		}
		checked := false
		for _, next := range t.calls[i+1:] {
			if next.Operation == "status" && next.Reference == lost.Reference {
				checked = true
			}
			if !creates(next.Operation) || !sameAttempt(lost, next) {
				continue
			}
			if next.Reference == lost.Reference || checked {
				break
			}
			answer := "no answer"
			if lost.Status != 0 {
				answer = fmt.Sprintf("HTTP %d", lost.Status)
			}
			found = append(found, Finding{Check: "lost_answer_new_reference", Group: DoubleMoney, Severity: Error,
				Reference: next.Reference, Calls: ids(lost, next),
				Message: fmt.Sprintf("The %s %s got %s, then was sent again with a new reference %s without asking for the status of the first. The first may have gone through: retry with the same reference, or ask its status first.",
					lost.Operation, lost.Reference, answer, next.Reference)})
			break
		}
	}
	return found
}

// failedRetriedWithSameReference finds a payment started again under a reference the client
// already knew had failed; the operator turns it down as a duplicate.
func failedRetriedWithSameReference(t *traffic) []Finding {
	var found []Finding
	for _, p := range t.order {
		for _, again := range p.creations[1:] {
			if p.learned(again.At) != Failed {
				continue
			}
			group := DoubleMoney
			if p.operation == "refund" {
				group = Refunds
			}
			found = append(found, Finding{Check: "failed_retried_same_reference", Group: group, Severity: Warning,
				Reference: p.reference, Calls: ids(p.first(), again),
				Message: fmt.Sprintf("The %s %s had failed and was sent again with the same reference. A new attempt needs a new reference; the operator refuses the old one as a duplicate.",
					p.operation, p.reference)})
			break
		}
	}
	return found
}
