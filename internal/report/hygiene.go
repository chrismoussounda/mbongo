package report

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// How long each operator's tokens last.
var tokenLife = map[string]time.Duration{"mtn": time.Hour, "airtel": 180 * time.Second}

const (
	// earlyTokens is how many tokens asked for while the last was still young make a habit.
	earlyTokens = 3
	// quickPoll is the shortest wait between two status calls for one payment.
	quickPoll = 5 * time.Second
	// rateLimitWait is how long to wait after a 429; MTN's answer says one second.
	rateLimitWait = time.Second
	// slowCallback is how long a callback endpoint may take to answer.
	slowCallback = 3 * time.Second
)

func hygiene(_ context.Context, t *traffic, _ Ledger) ([]Finding, error) {
	var found []Finding
	for _, f := range []func(*traffic) (Finding, bool){tokenPerCall, pollingTooFast, rateLimitIgnored,
		callbacksFailing, callbacksSlow} {
		if finding, ok := f(t); ok {
			found = append(found, finding)
		}
	}
	return found, nil
}

// tokenPerCall finds a client asking for new tokens while the last one had most of its life left.
func tokenPerCall(t *traffic) (Finding, bool) {
	life := tokenLife[t.operator]
	var (
		last    *Call
		refused bool
		early   []Call
	)
	for i, c := range t.calls {
		switch {
		case c.Operation != "token":
			refused = refused || c.Status == 401
		case c.Status == 200 && c.Token != "":
			if last != nil && !refused && c.At.Sub(last.At) < life/2 {
				early = append(early, c)
			}
			last, refused = &t.calls[i], false
		}
	}
	if len(early) < earlyTokens {
		return Finding{}, false
	}
	return Finding{Check: "token_per_call", Group: Hygiene, Severity: Warning, Calls: ids(early...),
		Message: fmt.Sprintf("%d new tokens were asked for while the last one had more than half of its %s left. Keep a token until it nears its end, or until a call says it is no longer valid.",
			len(early), life)}, true
}

// pollingTooFast finds status calls for one payment closer together than quickPoll.
func pollingTooFast(t *traffic) (Finding, bool) {
	var (
		quick []Call
		refs  []string
	)
	for _, p := range t.order {
		n := 0
		for i := 1; i < len(p.statuses); i++ {
			if p.statuses[i].At.Sub(p.statuses[i-1].At) < quickPoll {
				quick = append(quick, p.statuses[i])
				n++
			}
		}
		if n >= 2 {
			refs = append(refs, p.reference)
		}
	}
	if len(refs) == 0 {
		return Finding{}, false
	}
	return Finding{Check: "polling_too_fast", Group: Hygiene, Severity: Warning, Calls: ids(quick...),
		Message: fmt.Sprintf("The status of %s was asked again and again less than %s apart. Wait 20 to 25 seconds before the first check, then longer between each.",
			list(refs), quickPoll)}, true
}

// rateLimitIgnored finds calls sent right after a 429.
func rateLimitIgnored(t *traffic) (Finding, bool) {
	var hurried []Call
	for i := 1; i < len(t.calls); i++ {
		prev, c := t.calls[i-1], t.calls[i]
		if prev.Status == 429 && c.At.Sub(prev.At) < rateLimitWait {
			hurried = append(hurried, c)
		}
	}
	if len(hurried) == 0 {
		return Finding{}, false
	}
	return Finding{Check: "rate_limit_ignored", Group: Hygiene, Severity: Warning, Calls: ids(hurried...),
		Message: fmt.Sprintf("%d calls came less than %s after a 429 Too Many Requests. Back off before trying again.",
			len(hurried), rateLimitWait)}, true
}

// callbacksFailing finds callbacks the client's endpoint did not take.
func callbacksFailing(t *traffic) (Finding, bool) {
	var (
		failed []string
		first  Callback
	)
	for _, cb := range t.callbacks {
		if !delivered(cb) {
			if len(failed) == 0 {
				first = cb
			}
			failed = append(failed, cb.Reference)
		}
	}
	if len(failed) == 0 {
		return Finding{}, false
	}
	why := fmt.Sprintf("HTTP %d", first.HTTPStatus)
	if first.Error != "" {
		why = first.Error
	}
	return Finding{Check: "callback_failing", Group: Hygiene, Severity: Error, Reference: first.Reference,
		Message: fmt.Sprintf("%d of %d callbacks were not taken by the endpoint (first, for %s: %s). A callback is sent once and never retried.",
			len(failed), len(t.callbacks), first.Reference, why)}, true
}

// callbacksSlow finds callbacks the endpoint took long to answer.
func callbacksSlow(t *traffic) (Finding, bool) {
	var (
		slow    int
		slowest Callback
	)
	for _, cb := range t.callbacks {
		if time.Duration(cb.Millis)*time.Millisecond > slowCallback {
			slow++
			if cb.Millis > slowest.Millis {
				slowest = cb
			}
		}
	}
	if slow == 0 {
		return Finding{}, false
	}
	return Finding{Check: "callback_slow", Group: Hygiene, Severity: Warning, Reference: slowest.Reference,
		Message: fmt.Sprintf("%d callbacks took more than %s to be answered (the slowest %s, for %s). Answer at once and do the work afterwards.",
			slow, slowCallback, (time.Duration(slowest.Millis) * time.Millisecond).Round(100*time.Millisecond),
			slowest.Reference)}, true
}

// list names up to three references.
func list(refs []string) string {
	if len(refs) > 3 {
		return strings.Join(refs[:3], ", ") + fmt.Sprintf(" and %d more", len(refs)-3)
	}
	return strings.Join(refs, ", ")
}
