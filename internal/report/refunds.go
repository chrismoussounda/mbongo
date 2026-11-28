package report

import (
	"context"
	"fmt"

	"github.com/chrismoussounda/mbongo/internal/money"
)

// refunds checks each refund against the payment it points at and the refunds sent before it.
func refunds(ctx context.Context, t *traffic, ledger Ledger) ([]Finding, error) {
	if ledger == nil {
		return nil, nil
	}
	var found []Finding
	earlier := map[string][]*payment{}
	for _, c := range t.calls {
		if c.Operation != "refund" || c.RefundOf == "" {
			continue
		}
		p, known := t.payments[keyOf(t.operator, c)]
		if known && p.first().ID != c.ID {
			// The same refund sent again; the operator refuses it as a duplicate.
			continue
		}
		target, ok, err := ledger.RefundTarget(ctx, t.client, c.RefundOf)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		switch {
		case target.Operation == "refund":
			found = append(found, Finding{Check: "refund_of_refund", Group: Refunds, Severity: Error,
				Reference: c.Reference, Calls: ids(c),
				Message: fmt.Sprintf("A refund was asked for %s, which is itself a refund. Only a payment taken from a customer can be refunded.", c.RefundOf)})
		case payouts[target.Operation]:
			found = append(found, Finding{Check: "refund_of_payout", Group: Refunds, Severity: Error,
				Reference: c.Reference, Calls: ids(c),
				Message: fmt.Sprintf("A refund was asked for %s, a %s. Only a payment taken from a customer can be refunded.", c.RefundOf, target.Operation)})
		default:
			found = append(found, againstEarlier(c, target, earlier[c.RefundOf])...)
		}
		if known && c.Reference != "" && p.final != "" {
			earlier[c.RefundOf] = append(earlier[c.RefundOf], p)
		}
	}
	return found, nil
}

// againstEarlier finds a refund sent while an earlier one for the same payment was still
// pending, and one that asks back more than the payment had left.
func againstEarlier(c Call, target Target, before []*payment) []Finding {
	var found []Finding
	taken := int64(0)
	for _, p := range before {
		known := p.learned(c.At)
		if known != Successful && known != Failed {
			found = append(found, Finding{Check: "refund_while_pending", Group: Refunds, Severity: Error,
				Reference: c.Reference, Calls: ids(p.first(), c),
				Message: fmt.Sprintf("A refund of %s was sent while the refund %s of it was still pending. Wait for the first one's outcome: both may go through.",
					c.RefundOf, p.reference)})
			break
		}
	}
	for _, p := range before {
		if p.final == Successful || p.final == Pending {
			taken += amountOf(p.first().Amount, target)
		}
	}
	paid, err := money.Parse(target.Amount, target.Currency)
	if err != nil {
		return found
	}
	if asked := amountOf(c.Amount, target); taken+asked > paid {
		found = append(found, Finding{Check: "over_refund", Group: Refunds, Severity: Error,
			Reference: c.Reference, Calls: ids(c),
			Message: fmt.Sprintf("A refund of %s %s was asked for %s, which was %s %s with %s already refunded or on its way back.",
				show(asked, target.Currency), target.Currency, c.RefundOf, target.Amount, target.Currency,
				show(taken, target.Currency))})
	}
	return found
}

// amountOf is a refund's amount in the smallest unit; a refund without one gives back the whole payment.
func amountOf(amount string, target Target) int64 {
	if amount == "" {
		amount = target.Amount
	}
	n, err := money.Parse(amount, target.Currency)
	if err != nil {
		return 0
	}
	return n
}

func show(minor int64, currency string) string {
	s, _ := money.Format(minor, currency)
	return s
}
