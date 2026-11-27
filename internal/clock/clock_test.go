package clock_test

import (
	"testing"
	"time"

	"github.com/chrismoussounda/mbongo/internal/clock"
)

var start = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

func TestManualStandsStillUntilMoved(t *testing.T) {
	c := clock.NewManual(start)
	if !c.Now().Equal(start) {
		t.Fatalf("now = %v", c.Now())
	}
	c.Advance(90 * time.Second)
	if want := start.Add(90 * time.Second); !c.Now().Equal(want) {
		t.Fatalf("now = %v, want %v", c.Now(), want)
	}
}

func TestManualRunsDueFuncsInOrder(t *testing.T) {
	c := clock.NewManual(start)
	var got []string
	c.AfterFunc(3*time.Minute, func() { got = append(got, "late") })
	c.AfterFunc(time.Minute, func() { got = append(got, "early") })
	c.AfterFunc(10*time.Minute, func() { got = append(got, "not yet") })

	c.Advance(5 * time.Minute)

	if len(got) != 2 || got[0] != "early" || got[1] != "late" {
		t.Fatalf("ran %v", got)
	}
}

func TestManualSeesTheTimeOfEachFunc(t *testing.T) {
	c := clock.NewManual(start)
	var at time.Time
	c.AfterFunc(time.Minute, func() { at = c.Now() })
	c.Advance(time.Hour)
	if want := start.Add(time.Minute); !at.Equal(want) {
		t.Fatalf("func saw %v, want %v", at, want)
	}
	if want := start.Add(time.Hour); !c.Now().Equal(want) {
		t.Fatalf("now = %v, want %v", c.Now(), want)
	}
}

func TestManualRunsFuncsScheduledDuringAdvance(t *testing.T) {
	c := clock.NewManual(start)
	ran := 0
	c.AfterFunc(time.Minute, func() {
		ran++
		c.AfterFunc(time.Minute, func() { ran++ })
	})
	c.Advance(3 * time.Minute)
	if ran != 2 {
		t.Fatalf("ran %d funcs, want 2", ran)
	}
}

func TestStoppedFuncNeverRuns(t *testing.T) {
	c := clock.NewManual(start)
	ran := false
	timer := c.AfterFunc(time.Minute, func() { ran = true })
	if !timer.Stop() {
		t.Fatal("Stop returned false for a pending func")
	}
	c.Advance(time.Hour)
	if ran {
		t.Fatal("a stopped func ran")
	}
	if timer.Stop() {
		t.Fatal("Stop returned true twice")
	}
}

func TestRealRunsAfterFunc(t *testing.T) {
	done := make(chan struct{})
	clock.Real{}.AfterFunc(time.Millisecond, func() { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("real AfterFunc never ran")
	}
}
