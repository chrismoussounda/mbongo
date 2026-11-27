package mtn_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

const (
	ref2 = "6c8a4f5b-2d3e-4f90-8b1c-2d3e4f5a6b7c"
	ref3 = "7d9b5a6c-3e4f-4a01-9c2d-3e4f5a6b7c8d"
)

func send(phone, amount, currency string) map[string]any {
	return map[string]any{"amount": amount, "currency": currency, "externalId": "payout-1",
		"payee":        map[string]string{"partyIdType": "MSISDN", "partyId": phone},
		"payerMessage": "Salaire", "payeeNote": "Novembre"}
}

func (m *merchant) post(op, ref string, body any) answer {
	return m.e.call(http.MethodPost, "/disbursement/v1_0/"+op, m.headers(map[string]string{"X-Reference-Id": ref}), body)
}

func (m *merchant) get(path string) answer {
	return m.e.call(http.MethodGet, path, m.headers(nil), nil)
}

func TestSandboxTransferPaysAtOnce(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	if a := d.post("transfer", ref1, send("46733123453", "12.50", "EUR")); a.status != 202 {
		t.Fatalf("transfer: %d %v", a.status, a.body)
	}
	s := d.get("/disbursement/v1_0/transfer/" + ref1)
	want := "amount,currency,externalId,financialTransactionId,payee,status"
	if s.body["status"] != "SUCCESSFUL" || keys(s.body) != want {
		t.Fatalf("status %v, fields %s", s.body["status"], keys(s.body))
	}
	if payee, _ := s.body["payee"].(map[string]any); payee["partyId"] != "46733123453" {
		t.Fatalf("payee %v", s.body["payee"])
	}
	payee, err := e.books.FindAccount(e.ctx, bank.MTN, bank.CustomerAccount, "46733123453", "EUR")
	if err != nil || payee.Balance != 1250 {
		t.Fatalf("payee wallet %+v, %v", payee, err)
	}
	if err := e.books.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestATransferIgnoresAReferenceToRefund(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	d := e.user("disbursement", "sandbox")
	_ = c.requestToPay(ref1, pay("46733123453", "20", "EUR"), nil)
	body := send("46733123453", "50", "EUR")
	body["referenceIdToRefund"] = ref1
	if a := d.post("transfer", ref2, body); a.status != 202 {
		t.Fatalf("transfer: %d %v", a.status, a.body)
	}
	if a := d.post("refund", ref3, refund(ref1, "20")); a.status != 202 {
		t.Fatalf("the transfer counted as a refund: %d %v", a.status, a.body)
	}
}

func TestDepositEchoesTheMessages(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	_ = d.post("deposit", ref1, send("46733123453", "5", "EUR"))
	s := d.get("/disbursement/v1_0/deposit/" + ref1)
	if s.body["status"] != "SUCCESSFUL" || s.body["payerMessage"] != "Salaire" || s.body["payeeNote"] != "Novembre" {
		t.Fatalf("%v", s.body)
	}
}

func TestEachOperationHasItsOwnPath(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	_ = d.post("transfer", ref1, send("46733123453", "5", "EUR"))
	for _, path := range []string{"/disbursement/v1_0/deposit/", "/disbursement/v1_0/refund/"} {
		if a := d.get(path + ref1); a.status != 404 {
			t.Errorf("%s: %d", path, a.status)
		}
	}
	if a := e.merchant("sandbox").get("/collection/v1_0/requesttopay/" + ref1); a.status != 404 {
		t.Errorf("another user's transfer as a collection: %d", a.status)
	}
}

func TestProductsKeepTheirOwnKeys(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	viaCollection := *c
	viaCollection.product = "disbursement"
	if a := viaCollection.token(); a.status != 401 {
		t.Fatalf("collection key on the disbursement token: %d", a.status)
	}
	if a := c.post("transfer", ref1, send("46733123453", "5", "EUR")); a.status != 401 {
		t.Fatalf("collection token on a transfer: %d", a.status)
	}
}

func TestSandboxPayoutTestNumbers(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	_ = d.post("transfer", ref1, send("46733123450", "5", "EUR"))
	_ = d.post("transfer", ref2, send("46733123451", "5", "EUR"))
	_ = d.post("transfer", ref3, send("46733123454", "5", "EUR"))
	if s := d.get("/disbursement/v1_0/transfer/" + ref1); s.body["status"] != "FAILED" {
		t.Errorf("failing number: %v", s.body)
	}
	if s := d.get("/disbursement/v1_0/transfer/" + ref2); s.body["status"] != "SUCCESSFUL" {
		t.Errorf("a payee can't reject a transfer: %v", s.body)
	}
	if s := d.get("/disbursement/v1_0/transfer/" + ref3); s.body["status"] != "PENDING" {
		t.Errorf("pending number: %v", s.body)
	}
}

func TestCongoTransferNeedsFundsAndAPayee(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "mtncongo")
	_, _ = e.books.OpenCustomer(e.ctx, bank.MTN, "242061234567", "XAF", 0)

	_ = d.post("transfer", ref1, send("242061234567", "3000", "XAF"))
	if s := d.get("/disbursement/v1_0/transfer/" + ref1); s.body["status"] != "FAILED" ||
		s.body["reason"] != "LOW_BALANCE_OR_PAYEE_LIMIT_REACHED_OR_NOT_ALLOWED" {
		t.Fatalf("unfunded: %v", s.body)
	}
	if _, err := e.svc.Fund(e.ctx, d.user, 10_000); err != nil {
		t.Fatal(err)
	}
	_ = d.post("transfer", ref2, send("242061234567", "3000", "XAF"))
	if s := d.get("/disbursement/v1_0/transfer/" + ref2); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("funded: %v", s.body)
	}
	if b := d.get("/disbursement/v1_0/account/balance"); b.body["availableBalance"] != "7000" {
		t.Fatalf("balance %v", b.body)
	}
	_ = d.post("transfer", ref3, send("242069999999", "100", "XAF"))
	if s := d.get("/disbursement/v1_0/transfer/" + ref3); s.body["reason"] != "PAYEE_NOT_FOUND" {
		t.Fatalf("unknown payee: %v", s.body)
	}
}

func refund(of, amount string) map[string]any {
	return map[string]any{"amount": amount, "currency": "EUR", "externalId": "refund-1",
		"payerMessage": "Remboursement", "payeeNote": "", "referenceIdToRefund": of}
}

func TestRefundGoesBackToThePayer(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	d := e.user("disbursement", "sandbox")
	_ = c.requestToPay(ref1, pay("46733123453", "20", "EUR"), nil)

	if a := d.post("refund", ref2, refund(ref1, "15")); a.status != 202 {
		t.Fatalf("refund: %d %v", a.status, a.body)
	}
	s := d.get("/disbursement/v1_0/refund/" + ref2)
	payee, _ := s.body["payee"].(map[string]any)
	if s.body["status"] != "SUCCESSFUL" || payee["partyId"] != "46733123453" || s.body["payerMessage"] != nil {
		t.Fatalf("%v", s.body)
	}
	if a := d.post("refund", ref3, refund(ref1, "5.01")); a.status != 400 || a.body["code"] != "NOT_ALLOWED" {
		t.Fatalf("refund past the payment: %d %v", a.status, a.body)
	}
	if a := d.post("refund", ref3, refund(ref1, "5")); a.status != 202 {
		t.Fatalf("the rest of the payment: %d %v", a.status, a.body)
	}
	if err := e.books.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRefundNeedsASuccessfulPayment(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	d := e.user("disbursement", "sandbox")
	_ = c.requestToPay(ref1, pay("46733123451", "20", "EUR"), nil)

	if a := d.post("refund", ref2, refund(ref1, "1")); a.status != 400 || a.body["code"] != "NOT_ALLOWED" {
		t.Fatalf("refund of a rejected payment: %d %v", a.status, a.body)
	}
	if a := d.post("refund", ref2, refund(ref3, "1")); a.status != 404 {
		t.Fatalf("refund of nothing: %d %v", a.status, a.body)
	}
	_ = d.post("transfer", ref3, send("46733123453", "20", "EUR"))
	if a := d.post("refund", ref2, refund(ref3, "1")); a.status != 404 {
		t.Fatalf("refund of a transfer: %d %v", a.status, a.body)
	}
	stranger := e.partnerUser("another shop", "disbursement", "sandbox")
	if a := stranger.post("refund", ref2, refund(ref1, "1")); a.status != 404 {
		t.Fatalf("refund of another partner's payment: %d %v", a.status, a.body)
	}
	congo := e.user("disbursement", "mtncongo")
	if a := congo.post("refund", ref2, map[string]any{"amount": "1", "currency": "XAF", "referenceIdToRefund": ref1}); a.status != 404 {
		t.Fatalf("refund across environments: %d %v", a.status, a.body)
	}
}

func TestRefundsAtOnceNeverExceedThePayment(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	d := e.user("disbursement", "sandbox")
	_ = c.requestToPay(ref1, pay("46733123453", "20", "EUR"), nil)

	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Go(func() {
			statuses[i] = d.post("refund", fmt.Sprintf("00000000-0000-4000-8000-%012d", i), refund(ref1, "5")).status
		})
	}
	wg.Wait()
	accepted := 0
	for _, s := range statuses {
		if s == 202 {
			accepted++
		}
	}
	if accepted != 4 {
		t.Fatalf("%d refunds of 5 accepted on a payment of 20: %v", accepted, statuses)
	}
}

func TestSandboxPayerAlwaysHasEnough(t *testing.T) {
	e := setup(t)
	d := e.user("disbursement", "sandbox")
	c := e.merchant("sandbox")
	_ = d.post("transfer", ref1, send("46733123453", "5", "EUR"))
	_ = c.requestToPay(ref2, pay("46733123453", "25", "EUR"), nil)
	if s := c.status(ref2); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("a collection from a number paid before: %v", s.body)
	}
	_ = c.requestToPay(ref3, pay("46733123454", "40", "EUR"), nil)
	if err := e.svc.Approve(e.ctx, c.user, ref3); err != nil {
		t.Fatal(err)
	}
	if s := c.status(ref3); s.body["status"] != "SUCCESSFUL" {
		t.Fatalf("a pending sandbox payment approved later: %v", s.body)
	}
	if err := e.books.Check(e.ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPayoutsSendTheirCallback(t *testing.T) {
	e := setup(t)
	c := e.merchant("sandbox")
	d := e.user("disbursement", "sandbox")
	_ = c.requestToPay(ref1, pay("46733123453", "20", "EUR"), nil)
	h := map[string]string{"X-Callback-Url": "https://shop.example/payouts"}
	_ = e.call(http.MethodPost, "/disbursement/v1_0/transfer", d.headers(map[string]string{"X-Reference-Id": ref2,
		"X-Callback-Url": h["X-Callback-Url"]}), send("46733123453", "5", "EUR"))
	_ = e.call(http.MethodPost, "/disbursement/v1_0/refund", d.headers(map[string]string{"X-Reference-Id": ref3,
		"X-Callback-Url": h["X-Callback-Url"]}), refund(ref1, "5"))

	got := e.waitCallbacks(2)
	if len(got) != 2 {
		t.Fatalf("callbacks: %+v", got)
	}
	for _, cb := range got {
		payee, _ := cb.body["payee"].(map[string]any)
		if cb.url != h["X-Callback-Url"] || cb.body["status"] != "SUCCESSFUL" || payee["partyId"] != "46733123453" {
			t.Errorf("callback %+v", cb)
		}
	}
}
