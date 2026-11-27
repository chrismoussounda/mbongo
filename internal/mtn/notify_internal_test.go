package mtn

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/bank"
	"github.com/chrismoussounda/mbongo/internal/clock"
)

func TestACallbackIsSentOnlyOnce(t *testing.T) {
	ctx := context.Background()
	books, err := bank.Open(ctx, filepath.Join(t.TempDir(), "m.db"), clock.NewManual(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = books.Close() }()
	var sent atomic.Int32
	s, err := New(ctx, books, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context, string, []byte) error { sent.Add(1); return nil })
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := s.CreateSubscription(ctx, collection)
	user, _, _ := s.CreateAPIUser(ctx, sub, "shop.example", MTNCongo)
	req := request{APIUser: user, ReferenceID: "r", Product: collection, PartyID: "242061234567",
		Amount: "1", Currency: "XAF", CallbackURL: "https://shop.example/cb", Failure: "PAYER_NOT_FOUND"}
	if err := s.store.insertRequest(ctx, req); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		s.notify(req)
	}
	s.Wait()
	if n := sent.Load(); n != 1 {
		t.Fatalf("sent %d callbacks", n)
	}
}
