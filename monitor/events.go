package monitor

import (
	"context"
	"errors"
	"time"

	"github.com/mrf/agentwatch/session"
)

// EventType names the kind of event delivered to sinks.
type EventType string

const (
	// EventSnapshot carries the full set of currently tracked sessions.
	EventSnapshot EventType = "snapshot"
	// EventDelta carries only the sessions that changed since the last event.
	EventDelta EventType = "delta"
	// EventLifecycle signals a session state transition.
	EventLifecycle EventType = "lifecycle"
	// EventHealth signals a source health change.
	EventHealth EventType = "health"
)

// Event is the single envelope delivered to EventSink implementations.
type Event struct {
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
	Type EventType `json:"type"`

	Sessions []session.SessionState  `json:"sessions,omitempty"`
	Updates  []session.SessionState  `json:"updates,omitempty"`
	Removed  []string                `json:"removed,omitempty"`

	Lifecycle *session.LifecycleEvent `json:"lifecycle,omitempty"`
	Health    *Health                 `json:"health,omitempty"`
}

// EventSink receives events from a Monitor.
type EventSink interface {
	HandleEvent(ctx context.Context, ev Event) error
}

// MultiSink delivers events to multiple sinks sequentially.
// It returns a joined error from all failing sinks.
type MultiSink struct {
	sinks []EventSink
}

// NewMultiSink returns a MultiSink that delivers to the given sinks in order.
func NewMultiSink(sinks ...EventSink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

// HandleEvent implements EventSink. It delivers ev to each wrapped sink
// in order and returns a joined error.
func (ms *MultiSink) HandleEvent(ctx context.Context, ev Event) error {
	var errs []error
	for i := 0; i < len(ms.sinks); i++ {
		if err := ms.sinks[i].HandleEvent(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
