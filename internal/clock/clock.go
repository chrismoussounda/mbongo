// Package clock lets Mbongo run on real time or on a clock that only moves when told,
// so a five-minute approval window can be skipped in a test or a demo.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is the part of the time package Mbongo uses.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer cancels a func scheduled with AfterFunc.
type Timer interface {
	// Stop reports whether it stopped the func before it ran.
	Stop() bool
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

func (Real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Manual moves only when Advance is called.
type Manual struct {
	mu      sync.Mutex
	now     time.Time
	pending []*manualTimer
	seq     int
}

type manualTimer struct {
	clock *Manual
	at    time.Time
	seq   int
	f     func()
	done  bool
}

func NewManual(start time.Time) *Manual { return &Manual{now: start.UTC()} }

func (m *Manual) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *Manual) AfterFunc(d time.Duration, f func()) Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	t := &manualTimer{clock: m, at: m.now.Add(d), seq: m.seq, f: f}
	m.pending = append(m.pending, t)
	return t
}

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

// Advance moves the clock forward by d, running each func that falls due at its own time,
// including funcs scheduled by those funcs. Funcs run without the lock held.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	end := m.now.Add(d)
	m.mu.Unlock()
	for {
		m.mu.Lock()
		next := m.nextDue(end)
		if next == nil {
			m.now = end
			m.mu.Unlock()
			return
		}
		next.done = true
		if next.at.After(m.now) {
			m.now = next.at
		}
		m.mu.Unlock()
		next.f()
	}
}

func (m *Manual) nextDue(end time.Time) *manualTimer {
	live := m.pending[:0]
	for _, t := range m.pending {
		if !t.done {
			live = append(live, t)
		}
	}
	m.pending = live
	sort.Slice(live, func(i, j int) bool {
		if live[i].at.Equal(live[j].at) {
			return live[i].seq < live[j].seq
		}
		return live[i].at.Before(live[j].at)
	})
	if len(live) == 0 || live[0].at.After(end) {
		return nil
	}
	return live[0]
}
