// Package clock provides a Clock interface and implementations for production
// and deterministic test use.
package clock

import (
	"sync"
	"time"
)

// Clock provides time operations. Inject RealClock in production and FakeClock
// in tests that need deterministic timing.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker is a time source that fires at regular intervals.
type Ticker interface {
	// C returns the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop prevents the ticker from firing. It does not drain C.
	Stop()
}

// RealClock is a Clock backed by the system clock.
// Its zero value is ready to use.
type RealClock struct{}

// New returns a Clock backed by the system clock.
func New() Clock {
	return RealClock{}
}

// Wall returns a Clock backed by the system clock. Alias for New.
func Wall() Clock {
	return RealClock{}
}

// Now returns the current system time.
func (RealClock) Now() time.Time {
	return time.Now()
}

// NewTicker returns a Ticker that fires at intervals of d using the system clock.
func (RealClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{t: time.NewTicker(d)}
}

type realTicker struct {
	t *time.Ticker
}

func (r *realTicker) C() <-chan time.Time {
	return r.t.C
}

func (r *realTicker) Stop() {
	r.t.Stop()
}

// Mock is an alias for FakeClock for convenience.
type Mock = FakeClock

// NewMock is an alias for NewFake for convenience.
func NewMock(initial time.Time) *FakeClock {
	return NewFake(initial)
}

// FakeClock is a Clock with controllable time for deterministic tests.
// Time advances only when Advance is called explicitly.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

// NewFake returns a FakeClock initialized to initial.
func NewFake(initial time.Time) *FakeClock {
	return &FakeClock{now: initial}
}

// Now returns the current fake time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTicker returns a Ticker that fires when Advance moves time past multiples
// of d relative to the time NewTicker was called.
func (f *FakeClock) NewTicker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{
		d:    d,
		next: f.now.Add(d),
		ch:   make(chan time.Time, 1),
	}
	f.tickers = append(f.tickers, t)
	return t
}

// Advance moves the clock forward by d and fires any tickers whose interval
// has elapsed. Tickers with a full channel drop the excess tick (same behavior
// as the real time.Ticker under load).
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now
	tickers := make([]*fakeTicker, len(f.tickers))
	copy(tickers, f.tickers)
	f.mu.Unlock()

	for i := 0; i < len(tickers); i++ {
		tickers[i].fire(now)
	}
}

type fakeTicker struct {
	mu      sync.Mutex
	d       time.Duration
	next    time.Time
	ch      chan time.Time
	stopped bool
}

func (t *fakeTicker) C() <-chan time.Time {
	return t.ch
}

func (t *fakeTicker) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
}

// fire sends tick times to ch for each elapsed period up to now.
// Excess ticks are dropped if ch is full (capacity 1).
func (t *fakeTicker) fire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	for !t.next.After(now) {
		select {
		case t.ch <- t.next:
		default:
			// Channel full; drop this tick, same as time.Ticker under load.
		}
		t.next = t.next.Add(t.d)
	}
}
