package monitor

import (
	"context"
	"time"

	"github.com/mrf/agentwatch/session"
)

// EventType names the kind of monitor event.
type EventType string

const (
	// EventSnapshot carries the full current state of all sessions.
	EventSnapshot EventType = "snapshot"
	// EventDelta carries only sessions that changed since the last event.
	EventDelta EventType = "delta"
	// EventLifecycle carries a single session lifecycle transition.
	EventLifecycle EventType = "lifecycle"
	// EventHealth carries an updated health record for a source.
	EventHealth EventType = "health"
)

// Event is the single envelope type delivered to every EventSink.
// Seq is monotonically increasing per monitor instance.
// Events are delivered after state commit and outside store locks.
type Event struct {
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
	Type EventType `json:"type"`

	Sessions  []session.SessionState  `json:"sessions,omitempty"`
	Updates   []session.SessionState  `json:"updates,omitempty"`
	Removed   []string                `json:"removed,omitempty"`
	Lifecycle *session.LifecycleEvent `json:"lifecycle,omitempty"`
	Health    *Health                 `json:"health,omitempty"`
}

// EventSink receives monitor events. HandleEvent must return quickly; long-running
// sinks should wrap themselves in an async queue.
type EventSink interface {
	HandleEvent(ctx context.Context, ev Event) error
}

// EventSinkFunc is a function adapter for EventSink.
type EventSinkFunc func(ctx context.Context, ev Event) error

// HandleEvent implements EventSink.
func (f EventSinkFunc) HandleEvent(ctx context.Context, ev Event) error {
	return f(ctx, ev)
}
