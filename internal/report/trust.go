package report

import (
	"context"
	"fmt"
	"time"
)

func trust(_ context.Context, t *traffic, _ Ledger) ([]Finding, error) {
	var found []Finding
	for _, p := range t.order {
		if f, ok := trustedCallback(p); ok {
			found = append(found, f)
		}
		if f, ok := unfollowed(p); ok {
			found = append(found, f)
		}
	}
	return found, nil
}

// trustedCallback finds a success the client heard of only from a callback, without asking the
// operator whether it is true.
func trustedCallback(p *payment) (Finding, bool) {
	var heard *Callback
	for i, cb := range p.callbacks {
		if delivered(cb) && cb.Outcome == Successful {
			heard = &p.callbacks[i]
			break
		}
	}
	if heard == nil {
		return Finding{}, false
	}
	for _, c := range p.statuses {
		if c.Status == 200 && (c.Outcome == Successful || !c.At.Before(heard.At)) {
			return Finding{}, false
		}
	}
	return Finding{Check: "callback_trusted", Group: Trust, Severity: Warning, Reference: p.reference,
		Calls: ids(p.first()),
		Message: fmt.Sprintf("The callback said the %s %s succeeded and no status call followed. Anyone can post to a callback URL: ask for the status before handing anything over.",
			p.operation, p.reference)}, true
}

// unfollowed finds a payment that ended without the client learning how: money taken or paid
// that its books may never show, or a failure an order still waits on.
func unfollowed(p *payment) (Finding, bool) {
	if p.final != Successful && p.final != Failed {
		return Finding{}, false
	}
	if known := p.learned(farFuture); known == p.final {
		return Finding{}, false
	}
	how := "never asked for its status, and no callback reached it"
	if last, ok := lastStatus(p); ok {
		how = fmt.Sprintf("stopped asking after %s, when it was still pending", last.At.Sub(p.first().At).Round(time.Second))
	}
	if p.final == Successful {
		return Finding{Check: "money_unconfirmed", Group: Trust, Severity: Error, Reference: p.reference,
			Calls: ids(p.first()),
			Message: fmt.Sprintf("The %s %s succeeded, but the client %s. The money moved and the client's books may not show it.",
				p.operation, p.reference, how)}, true
	}
	return Finding{Check: "failure_unseen", Group: Trust, Severity: Warning, Reference: p.reference,
		Calls: ids(p.first()),
		Message: fmt.Sprintf("The %s %s failed, but the client %s. Whatever waits on it may wait for good.",
			p.operation, p.reference, how)}, true
}

var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

func lastStatus(p *payment) (Call, bool) {
	for i := len(p.statuses) - 1; i >= 0; i-- {
		if p.statuses[i].Status == 200 {
			return p.statuses[i], true
		}
	}
	return Call{}, false
}
