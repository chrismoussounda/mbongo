package janitor_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/janitor"
)

func TestRunKeepsGoingAfterAFailureAndStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var good, failures atomic.Int32
	done := make(chan struct{})
	go func() {
		janitor.Run(ctx, time.Millisecond, func(error) { failures.Add(1) },
			func(context.Context) error { return errors.New("boom") },
			func(context.Context) error { good.Add(1); return nil })
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for good.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatal("jobs did not run repeatedly")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if failures.Load() < 3 {
		t.Fatalf("failures reported %d times", failures.Load())
	}
}
