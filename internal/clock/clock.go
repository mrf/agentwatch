// Package clock abstracts time for deterministic testing.
package clock

import "time"

// Clock abstracts time.Now for testing.
type Clock interface {
	Now() time.Time
}

// Wall returns a Clock backed by time.Now.
func Wall() Clock {
	return wallClock{}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Mock is a controllable Clock for tests.
type Mock struct {
	now time.Time
}

// NewMock returns a Mock set to the given time.
func NewMock(t time.Time) *Mock {
	return &Mock{now: t}
}

// Now returns the current mock time.
func (m *Mock) Now() time.Time { return m.now }

// Set sets the mock time.
func (m *Mock) Set(t time.Time) { m.now = t }

// Advance moves the mock time forward by d.
func (m *Mock) Advance(d time.Duration) { m.now = m.now.Add(d) }
