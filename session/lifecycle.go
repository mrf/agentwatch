package session

import "time"

// LifecycleState is the durable state of a session — either active or terminal.
// "Resumed" is an event, not a durable state.
type LifecycleState string

const (
	// LifecycleActive means the session is being tracked and may receive updates.
	LifecycleActive LifecycleState = "active"
	// LifecycleTerminal means the session has ended and will be retained briefly
	// before removal.
	LifecycleTerminal LifecycleState = "terminal"
)

// LifecycleEventType names the kind of transition that occurred.
type LifecycleEventType string

const (
	// EventDiscovered fires when a session is first tracked.
	EventDiscovered LifecycleEventType = "discovered"
	// EventUpdated fires when new data arrives for an active session.
	EventUpdated LifecycleEventType = "updated"
	// EventResumed fires when a terminal session receives new data.
	EventResumed LifecycleEventType = "resumed"
	// EventTerminal fires when a session transitions to terminal state.
	EventTerminal LifecycleEventType = "terminal"
	// EventStale fires when no data is received past the stale threshold.
	EventStale LifecycleEventType = "stale"
	// EventRemoved fires when a terminal session's retention window expires
	// and it is removed from the store.
	EventRemoved LifecycleEventType = "removed"
)

// LifecycleEvent records a single state transition for a session.
type LifecycleEvent struct {
	Type      LifecycleEventType `json:"type"`
	SessionID string             `json:"sessionId"`
	Source    string             `json:"source"`
	From      LifecycleState     `json:"from"`
	To        LifecycleState     `json:"to"`
	At        time.Time          `json:"at"`
	Reason    string             `json:"reason,omitempty"`
}
