package faults_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
	"github.com/chrismoussounda/mbongo/internal/faults"
)

func setup(t *testing.T) (*faults.Injector, context.Context) {
	t.Helper()
	ctx := context.Background()
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "f.db"), clock.NewManual(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = books.Close() })
	in, err := faults.New(ctx, books.DB())
	if err != nil {
		t.Fatal(err)
	}
	return in, ctx
}

func take(t *testing.T, in *faults.Injector, operator, operation, phone string) string {
	t.Helper()
	r, found, err := in.Take(context.Background(), operator, operation, phone)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return ""
	}
	return r.Fault
}

func TestRulesMatchAndRunOut(t *testing.T) {
	in, ctx := setup(t)
	if _, err := in.Add(ctx, faults.Rule{Operator: "mtn", Operation: "transfer", Phone: "111", Fault: faults.Slow, Times: 2}); err != nil {
		t.Fatal(err)
	}
	if got := take(t, in, "airtel", "transfer", "111"); got != "" {
		t.Fatalf("other operator: %q", got)
	}
	if got := take(t, in, "mtn", "deposit", "111"); got != "" {
		t.Fatalf("other operation: %q", got)
	}
	if got := take(t, in, "mtn", "transfer", "222"); got != "" {
		t.Fatalf("other phone: %q", got)
	}
	for i := range 2 {
		if got := take(t, in, "mtn", "transfer", "111"); got != faults.Slow {
			t.Fatalf("use %d: %q", i+1, got)
		}
	}
	if got := take(t, in, "mtn", "transfer", "111"); got != "" {
		t.Fatalf("after two uses: %q", got)
	}
	if rules, _ := in.List(ctx); len(rules) != 0 {
		t.Fatalf("spent rule still listed: %+v", rules)
	}
}

func TestNarrowerRuleWinsAndUnlimitedStays(t *testing.T) {
	in, ctx := setup(t)
	broad, _ := in.Add(ctx, faults.Rule{Operator: "mtn", Fault: faults.Maintenance, Times: 0})
	_, _ = in.Add(ctx, faults.Rule{Operator: "mtn", Phone: "111", Fault: faults.RateLimited, Times: 1})
	if got := take(t, in, "mtn", "requesttopay", "111"); got != faults.RateLimited {
		t.Fatalf("the rule for this phone should win: %q", got)
	}
	for range 3 {
		if got := take(t, in, "mtn", "requesttopay", "111"); got != faults.Maintenance {
			t.Fatalf("unlimited rule: %q", got)
		}
	}
	if err := in.Remove(ctx, broad.ID); err != nil {
		t.Fatal(err)
	}
	if got := take(t, in, "mtn", "requesttopay", "111"); got != "" {
		t.Fatalf("removed rule still applies: %q", got)
	}
	if err := in.Remove(ctx, broad.ID); err == nil {
		t.Fatal("removing twice should fail")
	}
}

func TestUnknownFaultAndClearAll(t *testing.T) {
	in, ctx := setup(t)
	if _, err := in.Add(ctx, faults.Rule{Operator: "mtn", Fault: "explode"}); err == nil {
		t.Fatal("unknown fault accepted")
	}
	_, _ = in.Add(ctx, faults.Rule{Operator: "mtn", Fault: faults.Slow})
	_, _ = in.Add(ctx, faults.Rule{Operator: "airtel", Fault: faults.Slow})
	if err := in.Remove(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if rules, _ := in.List(ctx); len(rules) != 0 {
		t.Fatalf("%+v", rules)
	}
	var none *faults.Injector
	if _, found, err := none.Take(ctx, "mtn", "", ""); found || err != nil {
		t.Fatal("a nil injector fails nothing")
	}
}

func TestPhoneIsFoundWhereEachOperatorPutsIt(t *testing.T) {
	for body, want := range map[string]string{
		`{"payer":{"partyIdType":"MSISDN","partyId":"46733123453"}}`:  "46733123453",
		`{"payee":{"partyIdType":"MSISDN","partyId":"242061234567"}}`: "242061234567",
		`{"payee":{"msisdn":"055000001"}}`:                            "055000001",
		`{"subscriber":{"msisdn":"055000002"}}`:                       "055000002",
		`not json`:                                                    "",
	} {
		if got := faults.Phone([]byte(body)); got != want {
			t.Errorf("%s: %q", body, got)
		}
	}
}

func TestCallbackRulesStayApart(t *testing.T) {
	in, ctx := setup(t)
	_, _ = in.Add(ctx, faults.Rule{Operator: "mtn", Fault: faults.Slow, Times: 1})
	if got := take(t, in, "mtn", faults.Callback, "111"); got != "" {
		t.Fatalf("a request rule was used by a callback: %q", got)
	}
	r, err := in.Add(ctx, faults.Rule{Operator: "mtn", Fault: faults.CallbackNever, Times: 1})
	if err != nil || r.Operation != faults.Callback {
		t.Fatalf("%+v %v", r, err)
	}
	if got := take(t, in, "mtn", "transfer", "111"); got != faults.Slow {
		t.Fatalf("the request rule: %q", got)
	}
	if got := take(t, in, "mtn", faults.Callback, "111"); got != faults.CallbackNever {
		t.Fatalf("the callback rule: %q", got)
	}
	if _, err := in.Add(ctx, faults.Rule{Operator: "mtn", Operation: faults.Callback, Fault: faults.Slow}); err == nil {
		t.Fatal("slow is not a callback fault")
	}
	if _, err := in.Add(ctx, faults.Rule{Operator: "mtn", Operation: "transfer", Fault: faults.CallbackTwice}); err == nil {
		t.Fatal("a callback fault on a request")
	}
}
