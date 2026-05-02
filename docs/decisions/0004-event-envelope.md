# ADR 0004: Event Envelope and Sink Delivery Contract

**Status:** Accepted

## Context

`agent-racer` uses multiple callback methods and message types on its WebSocket
broadcaster. Those types carry racer-specific fields (lanes, leaderboard
positions, achievements) and are defined in a transport package that imports
game logic. This coupling makes it impossible to use the broadcaster without
the game layer.

`agentwatch` needs an event model that:

1. Allows reactive consumers (WebSocket adapters, replay recorders, custom sinks)
   to receive all monitor output.
2. Does not reference racer, UI, or game concepts.
3. Is serialisable to JSON without a separate translation step.
4. Supports a replay recorder implementable as a sink with no library changes.

The alternative — multiple callback methods on an interface — would require
every sink to implement all methods, even the ones it ignores, and would make
adding new event types a breaking interface change.

## Decision

All monitor output is delivered through a single event envelope.

```go
package monitor

type EventType string

const (
    EventSnapshot  EventType = "snapshot"
    EventDelta     EventType = "delta"
    EventLifecycle EventType = "lifecycle"
    EventHealth    EventType = "health"
)

type Event struct {
    Seq  uint64    `json:"seq"`
    At   time.Time `json:"at"`
    Type EventType `json:"type"`

    Sessions  []session.SessionState   `json:"sessions,omitempty"`
    Updates   []session.SessionState   `json:"updates,omitempty"`
    Removed   []string                 `json:"removed,omitempty"`

    Lifecycle *session.LifecycleEvent  `json:"lifecycle,omitempty"`
    Health    *Health                  `json:"health,omitempty"`
}

type EventSink interface {
    HandleEvent(ctx context.Context, ev Event) error
}
```

### Event semantics

| Type | When emitted | Populated fields |
|---|---|---|
| `snapshot` | After first poll and on full-state requests | `Sessions` (all active sessions) |
| `delta` | On any session state change | `Updates` (changed sessions), `Removed` (session IDs leaving the store) |
| `lifecycle` | On lifecycle transition | `Lifecycle` |
| `health` | On source health change | `Health` |

### Delivery contract

1. `Seq` is monotonically increasing per monitor instance, starting at 1.
   Consumers may use gaps to detect dropped events.
2. Events are delivered **after** state is committed to the store and
   **outside** store locks. A sink must not call back into the monitor during
   `HandleEvent`.
3. A sink must return quickly. Long-running sinks (e.g., a WebSocket
   broadcaster) must wrap themselves in an async queue internally.
4. The default `MultiSink` delivers to all registered sinks sequentially and
   returns a joined error if any sink fails.
5. Sink errors are logged and surfaced through monitor health. They do **not**
   roll back committed state.
6. `Removed` events are part of the core event model. Transports must not
   invent separate removal semantics.

### AsyncSink

An optional `monitor.AsyncSink` wrapper may be added once a concrete transport
needs a shared bounded-queue implementation. Its overflow policy must be
explicit: block, drop-new, or drop-old. The policy is chosen at construction
time; it does not change at runtime.

## Consequences

- Adding a new event type (e.g., `compaction`) requires adding a constant and
  populating a new field on `Event`. It does not change the `EventSink`
  interface.
- Sinks that do not care about a given event type ignore the fields they do not
  need.
- The envelope is JSON-serialisable, so a replay recorder can write events to
  disk with no translation layer.
- The WebSocket transport wraps `monitor.Event` directly, which means the wire
  format is the monitor format. Racer-specific message types stay in
  `agent-racer`.
- Sequential delivery through `MultiSink` means a slow or failing sink
  increases event latency for subsequent sinks. Sinks that cannot return
  quickly must use `AsyncSink`.
