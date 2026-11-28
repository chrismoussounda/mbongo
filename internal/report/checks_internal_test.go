package report

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
)

type fakeLedger struct {
	outcomes map[string]string
	targets  map[string]Target
}

func (f fakeLedger) Outcome(_ context.Context, _, ref, _ string) (string, error) {
	return f.outcomes[ref], nil
}

func (f fakeLedger) RefundTarget(_ context.Context, _, of string) (Target, bool, error) {
	t, ok := f.targets[of]
	return t, ok, nil
}

// script plays one client's traffic, a second apart unless told otherwise.
type script struct {
	t      *testing.T
	rec    *Recorder
	clock  *clock.Manual
	ledger fakeLedger
}

func newScript(t *testing.T) *script {
	t.Helper()
	ctx := context.Background()
	c := clock.NewManual(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "c.db"), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = books.Close() })
	rec, err := New(ctx, books.DB(), c)
	if err != nil {
		t.Fatal(err)
	}
	return &script{t: t, rec: rec, clock: c,
		ledger: fakeLedger{outcomes: map[string]string{}, targets: map[string]Target{}}}
}

func (s *script) wait(d time.Duration) { s.clock.Advance(d) }

// call records c as user u1's MTN call, answered 202 for a creation and 200 otherwise.
func (s *script) call(c Call) {
	s.t.Helper()
	if c.Operator == "" {
		c.Operator = "mtn"
	}
	if c.Client == "" {
		c.Client = "u1"
	}
	if c.Status == 0 && c.Outcome != "lost" {
		c.Status = 200
		if creates(c.Operation) {
			c.Status = 202
		}
	}
	if c.Outcome == "lost" {
		c.Outcome = ""
	}
	if c.Currency == "" && c.Amount != "" {
		c.Currency = "XAF"
	}
	c.At = s.clock.Now()
	if err := s.rec.addCall(context.Background(), c); err != nil {
		s.t.Fatal(err)
	}
	s.clock.Advance(time.Second)
}

func (s *script) pay(ref, order, phone, amount string) {
	s.call(Call{Operation: "requesttopay", Reference: ref, Order: order, Phone: phone, Amount: amount})
}

func (s *script) status(ref, outcome string) {
	s.call(Call{Operation: "status", Reference: ref, Outcome: outcome})
}

func (s *script) callback(ref, outcome string, httpStatus int) {
	s.t.Helper()
	if err := s.rec.Callback(context.Background(), Callback{Operator: "mtn", Client: "u1", Reference: ref,
		Outcome: outcome, HTTPStatus: httpStatus, At: s.clock.Now()}); err != nil {
		s.t.Fatal(err)
	}
	s.clock.Advance(time.Second)
}

func (s *script) ends(ref, outcome string) { s.ledger.outcomes[ref] = outcome }

func (s *script) report() ClientReport {
	s.t.Helper()
	reports, err := s.rec.Build(context.Background(), 1, map[string]Ledger{"mtn": s.ledger, "airtel": s.ledger})
	if err != nil {
		s.t.Fatal(err)
	}
	if len(reports) != 1 {
		s.t.Fatalf("reports %+v", reports)
	}
	return reports[0]
}

// found are the findings of one check.
func (s *script) found(check string) []Finding {
	s.t.Helper()
	var out []Finding
	for _, f := range s.report().Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func (s *script) expect(check, severity, ref string) Finding {
	s.t.Helper()
	got := s.found(check)
	if len(got) != 1 || got[0].Severity != severity || got[0].Reference != ref || got[0].Message == "" {
		s.t.Fatalf("%s: %+v (all: %+v)", check, got, s.report().Findings)
	}
	return got[0]
}

func (s *script) none(check string) {
	s.t.Helper()
	if got := s.found(check); len(got) != 0 {
		s.t.Fatalf("%s: %+v", check, got)
	}
}

func TestCleanTrafficHasNoFindings(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "token", Token: "t1"})
	s.pay("r1", "o-1", "242061234567", "1000")
	s.wait(20 * time.Second)
	s.status("r1", Pending)
	s.callback("r1", Successful, 200)
	s.status("r1", Successful)
	s.ends("r1", Successful)
	r := s.report()
	if len(r.Findings) != 0 || r.Errors != 0 || r.Warnings != 0 || r.Calls != 4 || r.Callbacks != 1 || r.Client != "u1" {
		t.Fatalf("%+v", r)
	}
}

func TestAnOrderPaidTwice(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.status("r1", Successful)
	s.pay("r2", "o-1", "242061234567", "1000")
	s.status("r2", Successful)
	s.ends("r1", Successful)
	s.ends("r2", Successful)
	f := s.expect("order_paid_twice", Error, "r2")
	if len(f.Calls) != 2 {
		t.Fatalf("%+v", f)
	}
}

func TestAnOrderRetriedAfterAFailureIsFine(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.status("r1", Failed)
	s.pay("r2", "o-1", "242061234567", "1000")
	s.status("r2", Successful)
	s.ends("r1", Failed)
	s.ends("r2", Successful)
	s.none("order_paid_twice")
	s.none("lost_answer_new_reference")
}

func TestTwoEqualPayoutsCloseTogether(t *testing.T) {
	s := newScript(t)
	for _, ref := range []string{"p1", "p2"} {
		s.call(Call{Operation: "transfer", Reference: ref, Phone: "242061234567", Amount: "5000"})
		s.status(ref, Successful)
		s.ends(ref, Successful)
		s.wait(2 * time.Minute)
	}
	s.expect("payout_twice", Warning, "p2")
}

func TestEqualPayoutsFarApartAreFine(t *testing.T) {
	s := newScript(t)
	for _, ref := range []string{"p1", "p2"} {
		s.call(Call{Operation: "transfer", Reference: ref, Phone: "242061234567", Amount: "5000"})
		s.status(ref, Successful)
		s.ends(ref, Successful)
		s.wait(20 * time.Minute)
	}
	s.none("payout_twice")
}

func TestALostAnswerRetriedWithANewReference(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Outcome: "lost"})
	s.pay("r2", "o-1", "242061234567", "1000")
	s.expect("lost_answer_new_reference", Error, "r2")
}

func TestAServerErrorRetriedWithANewReference(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "transfer", Reference: "p1", Phone: "242061234567", Amount: "5000", Status: 500})
	s.call(Call{Operation: "transfer", Reference: "p2", Phone: "242061234567", Amount: "5000"})
	f := s.expect("lost_answer_new_reference", Error, "p2")
	if len(f.Calls) != 2 {
		t.Fatalf("%+v", f)
	}
}

func TestALostAnswerDoneRight(t *testing.T) {
	t.Run("same reference", func(t *testing.T) {
		s := newScript(t)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Outcome: "lost"})
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
		s.pay("r2", "o-2", "242061234567", "1000")
		s.none("lost_answer_new_reference")
	})
	t.Run("status first", func(t *testing.T) {
		s := newScript(t)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Outcome: "lost"})
		s.call(Call{Operation: "status", Reference: "r1", Status: 404})
		s.pay("r2", "o-1", "242061234567", "1000")
		s.none("lost_answer_new_reference")
	})
	t.Run("another order", func(t *testing.T) {
		s := newScript(t)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Outcome: "lost"})
		s.pay("r2", "o-2", "242061234567", "1000")
		s.none("lost_answer_new_reference")
	})
}

func TestAFailedPaymentRetriedWithItsReference(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.status("r1", Failed)
	s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
	s.expect("failed_retried_same_reference", Warning, "r1")
}

func TestClientsAreReportedApart(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.call(Call{Client: "u2", Operation: "requesttopay", Reference: "r2", Order: "o-1"})
	s.call(Call{Operator: "airtel", Client: "u1", Operation: "collection", Reference: "r3", Order: "o-1"})
	s.ends("r1", Successful)
	s.ends("r2", Successful)
	s.ends("r3", Successful)
	reports, err := s.rec.Build(context.Background(), 1, map[string]Ledger{"mtn": s.ledger, "airtel": s.ledger})
	if err != nil || len(reports) != 3 {
		t.Fatalf("%+v %v", reports, err)
	}
	for _, r := range reports {
		for _, f := range r.Findings {
			if f.Check == "order_paid_twice" {
				t.Fatalf("%s %s: %+v", r.Operator, r.Client, f)
			}
		}
	}
}

func TestEqualPayoutsToTwoNumbersAreFine(t *testing.T) {
	s := newScript(t)
	for ref, phone := range map[string]string{"p1": "242061234567", "p2": "242067654321"} {
		s.call(Call{Operation: "transfer", Reference: ref, Phone: phone, Amount: "5000"})
		s.ends(ref, Successful)
	}
	s.none("payout_twice")
}

func TestOnlyWhatReachedTheClientCountsAsLearned(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.status("r1", Pending)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
		s.none("failed_retried_same_reference")
	})
	t.Run("callback refused by the client", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.callback("r1", Failed, 500)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
		s.none("failed_retried_same_reference")
	})
	t.Run("callback delivered", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.callback("r1", Failed, 204)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
		s.expect("failed_retried_same_reference", Warning, "r1")
	})
	t.Run("learned only later", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.call(Call{Operation: "requesttopay", Reference: "r1", Order: "o-1", Status: 409})
		s.status("r1", Failed)
		s.none("failed_retried_same_reference")
	})
}

func TestCallsFromNoKnownClientAreLeftOut(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	if err := s.rec.addCall(context.Background(), Call{Operator: "mtn", Operation: "token", Status: 401,
		At: s.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := s.report(); r.Calls != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestACallbackTrustedWithoutAStatusCall(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.status("r1", Pending)
	s.callback("r1", Successful, 200)
	s.ends("r1", Successful)
	s.expect("callback_trusted", Warning, "r1")
	s.none("money_unconfirmed")
}

func TestACallbackCheckedOrAlreadyKnownIsFine(t *testing.T) {
	t.Run("checked after", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.callback("r1", Successful, 200)
		s.status("r1", Successful)
		s.ends("r1", Successful)
		s.none("callback_trusted")
	})
	t.Run("known before", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.status("r1", Successful)
		s.callback("r1", Successful, 200)
		s.ends("r1", Successful)
		s.none("callback_trusted")
	})
	t.Run("a failure", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.callback("r1", Failed, 200)
		s.ends("r1", Failed)
		s.none("callback_trusted")
		s.none("failure_unseen")
	})
	t.Run("not delivered", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.callback("r1", Successful, 502)
		s.ends("r1", Successful)
		s.none("callback_trusted")
		s.expect("money_unconfirmed", Error, "r1")
	})
}

func TestMoneyTakenNeverConfirmed(t *testing.T) {
	t.Run("never asked", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.ends("r1", Successful)
		f := s.expect("money_unconfirmed", Error, "r1")
		if f.Group != Trust {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("gave up while pending", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.status("r1", Pending)
		s.wait(time.Minute)
		s.status("r1", Pending)
		s.ends("r1", Successful)
		f := s.expect("money_unconfirmed", Error, "r1")
		if want := "stopped asking after 1m2s"; !contains(f.Message, want) {
			t.Fatalf("%q lacks %q", f.Message, want)
		}
	})
	t.Run("still pending", func(t *testing.T) {
		s := newScript(t)
		s.pay("r1", "o-1", "242061234567", "1000")
		s.ends("r1", Pending)
		s.none("money_unconfirmed")
		s.none("failure_unseen")
	})
	t.Run("refused at once", func(t *testing.T) {
		s := newScript(t)
		s.call(Call{Operation: "requesttopay", Reference: "r1", Status: 400})
		s.none("money_unconfirmed")
		s.none("failure_unseen")
	})
}

func TestAFailureNeverSeen(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "transfer", Reference: "p1", Phone: "242061234567", Amount: "5000"})
	s.ends("p1", Failed)
	s.expect("failure_unseen", Warning, "p1")
	s.none("money_unconfirmed")
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestACheckAfterTheCallbackCountsWhateverItSaw(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.callback("r1", Successful, 200)
	s.status("r1", Pending)
	s.ends("r1", Successful)
	s.none("callback_trusted")
}

func TestARefusedStatusCallIsNoCheck(t *testing.T) {
	s := newScript(t)
	s.pay("r1", "o-1", "242061234567", "1000")
	s.call(Call{Operation: "status", Reference: "r1", Status: 401})
	s.ends("r1", Successful)
	if f := s.expect("money_unconfirmed", Error, "r1"); !contains(f.Message, "never asked") {
		t.Fatalf("%q", f.Message)
	}
}

func (s *script) refund(ref, of, amount string) {
	s.call(Call{Operation: "refund", Reference: ref, RefundOf: of, Amount: amount})
}

// paid makes r1 a collection of 1000 XAF the client saw succeed.
func (s *script) paid() {
	s.pay("r1", "o-1", "242061234567", "1000")
	s.status("r1", Successful)
	s.ends("r1", Successful)
	s.ledger.targets["r1"] = Target{Reference: "r1", Operation: "requesttopay", Amount: "1000", Currency: "XAF"}
}

func TestRefundsDoneRight(t *testing.T) {
	s := newScript(t)
	s.paid()
	s.refund("f1", "r1", "400")
	s.status("f1", Successful)
	s.ends("f1", Successful)
	s.refund("f2", "r1", "600")
	s.status("f2", Successful)
	s.ends("f2", Successful)
	if r := s.report(); len(r.Findings) != 0 {
		t.Fatalf("%+v", r.Findings)
	}
}

func TestARefundOfARefund(t *testing.T) {
	s := newScript(t)
	s.paid()
	s.refund("f1", "r1", "1000")
	s.status("f1", Successful)
	s.ends("f1", Successful)
	s.ledger.targets["f1"] = Target{Reference: "f1", Operation: "refund", Amount: "1000", Currency: "XAF"}
	s.call(Call{Operation: "refund", Reference: "f2", RefundOf: "f1", Amount: "1000", Status: 404})
	s.expect("refund_of_refund", Error, "f2")
	s.none("over_refund")
}

func TestARefundOfAPayout(t *testing.T) {
	s := newScript(t)
	s.call(Call{Operation: "transfer", Reference: "p1", Phone: "242061234567", Amount: "5000"})
	s.status("p1", Successful)
	s.ends("p1", Successful)
	s.ledger.targets["p1"] = Target{Reference: "p1", Operation: "transfer", Amount: "5000", Currency: "XAF"}
	s.call(Call{Operation: "refund", Reference: "f1", RefundOf: "p1", Amount: "5000", Status: 404})
	s.expect("refund_of_payout", Error, "f1")
}

func TestRefundingMoreThanWasPaid(t *testing.T) {
	t.Run("at once", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.call(Call{Operation: "refund", Reference: "f1", RefundOf: "r1", Amount: "1500", Status: 500})
		if f := s.expect("over_refund", Error, "f1"); !contains(f.Message, "1500 XAF") {
			t.Fatalf("%q", f.Message)
		}
	})
	t.Run("in two", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.refund("f1", "r1", "700")
		s.status("f1", Successful)
		s.ends("f1", Successful)
		s.refund("f2", "r1", "700")
		s.expect("over_refund", Error, "f2")
	})
	t.Run("after a failed one", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.refund("f1", "r1", "700")
		s.status("f1", Failed)
		s.ends("f1", Failed)
		s.refund("f2", "r1", "700")
		s.ends("f2", Successful)
		s.status("f2", Successful)
		s.none("over_refund")
		s.none("refund_while_pending")
	})
	t.Run("one still on its way back", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.refund("f1", "r1", "700")
		s.ends("f1", Pending)
		s.refund("f2", "r1", "700")
		s.expect("over_refund", Error, "f2")
	})
	t.Run("one resent with its reference", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.refund("f1", "r1", "700")
		s.status("f1", Successful)
		s.ends("f1", Successful)
		s.call(Call{Operation: "refund", Reference: "f1", RefundOf: "r1", Amount: "700", Status: 409})
		s.refund("f2", "r1", "300")
		s.none("over_refund")
	})
	t.Run("whole payment twice", func(t *testing.T) {
		s := newScript(t)
		s.paid()
		s.call(Call{Operator: "mtn", Operation: "refund", Reference: "f1", RefundOf: "r1"})
		s.status("f1", Successful)
		s.ends("f1", Successful)
		s.call(Call{Operator: "mtn", Operation: "refund", Reference: "f2", RefundOf: "r1"})
		s.expect("over_refund", Error, "f2")
	})
}

func TestASecondRefundWhileTheFirstIsPending(t *testing.T) {
	s := newScript(t)
	s.paid()
	s.refund("f1", "r1", "300")
	s.status("f1", Pending)
	s.ends("f1", Pending)
	s.refund("f2", "r1", "300")
	s.ends("f2", Successful)
	f := s.expect("refund_while_pending", Error, "f2")
	if len(f.Calls) != 2 {
		t.Fatalf("%+v", f)
	}
	s.none("over_refund")
}

func TestARefusedRefundIsNoEarlierRefund(t *testing.T) {
	s := newScript(t)
	s.paid()
	s.call(Call{Operation: "refund", Reference: "f1", RefundOf: "r1", Amount: "1000", Status: 400})
	s.refund("f2", "r1", "1000")
	s.status("f2", Successful)
	s.ends("f2", Successful)
	s.none("refund_while_pending")
	s.none("over_refund")
}
