package bank_test

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
)

func TestApprovingTwiceAtOnceMovesMoneyOnce(t *testing.T) {
	f := setup(t)
	c := f.customer("242061234567", 10_000)
	f.merchant("shop", 0)
	tx, _ := f.request(bank.Collection, "ref-1", 2_500)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = f.bank.Approve(f.ctx, tx.ID) })
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, bank.ErrDecided):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || f.balance(c) != 7_500 {
		t.Fatalf("%d approvals went through, customer has %d", ok, f.balance(c))
	}
	f.check()
}

func TestBooksBalanceAfterRandomWork(t *testing.T) {
	f := setup(t)
	rng := rand.New(rand.NewPCG(1, 2))
	phones := []string{"242061111111", "242062222222", "242063333333"}
	for i, p := range phones {
		f.customer(p, int64(i)*5_000)
	}
	for _, m := range []string{"shop", "taxi"} {
		f.merchant(m, 3_000)
	}
	var pending []string
	for i := range 400 {
		switch rng.IntN(4) {
		case 0, 1:
			kind := bank.Collection
			if rng.IntN(2) == 0 {
				kind = bank.Payout
			}
			tx, err := f.bank.Request(f.ctx, bank.Request{
				Kind: kind, Operator: bank.MTN, Merchant: []string{"shop", "taxi"}[rng.IntN(2)],
				Phone: phones[rng.IntN(len(phones))], Amount: 1 + rng.Int64N(4_000), Currency: "XAF",
				PartnerRef: fmt.Sprint("ref-", i), Window: time.Duration(1+rng.IntN(5)) * time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			pending = append(pending, tx.ID)
		case 2:
			if len(pending) > 0 {
				id := pending[rng.IntN(len(pending))]
				var err error
				if rng.IntN(3) == 0 {
					_, err = f.bank.Refuse(f.ctx, id, bank.ReasonRefused)
				} else {
					_, err = f.bank.Approve(f.ctx, id)
				}
				if err != nil && !errors.Is(err, bank.ErrDecided) {
					t.Fatal(err)
				}
			}
		case 3:
			f.clock.Advance(time.Duration(rng.IntN(90)) * time.Second)
		}
	}
	f.check()
}

func TestCheckFindsBrokenBooks(t *testing.T) {
	cases := map[string]string{
		"balance":  "UPDATE accounts SET balance = balance + 1 WHERE kind = 'customer'",
		"movement": "UPDATE entries SET amount = amount + 1 WHERE transaction_id IS NOT NULL AND amount > 0",
		"total":    "UPDATE entries SET amount = amount * 2 WHERE transaction_id IS NULL",
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.customer("242061234567", 10_000)
			f.merchant("shop", 0)
			tx, _ := f.request(bank.Collection, "ref-1", 2_500)
			_, _ = f.bank.Approve(f.ctx, tx.ID)
			_ = f.bank.Close()

			db, err := sql.Open("sqlite", "file:"+f.path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(f.ctx, tamper); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()

			f.bank = f.open()
			if err := f.bank.Check(f.ctx); err == nil {
				t.Fatal("Check found nothing")
			}
		})
	}
}
