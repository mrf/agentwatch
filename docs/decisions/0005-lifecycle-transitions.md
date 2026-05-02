# ADR 0005: Session Lifecycle Transitions

**Status:** Accepted

## Context

`agent-racer` uses a `Resumed` state alongside `Active` and `Terminal`. Treating
`Resumed` as a durable state complicates the store because the monitor must
decide when to transition out of it and what "resumed" means for downstream
consumers like the leaderboard.

`agentwatch` needs a lifecycle model that:

1. Is simple enough to implement correctly without special-casing.
2. Expresses all meaningful transitions observable from log artifacts.
3. Separates durable state (stored in the session record) from transient events
   (delivered to sinks once and discarded).
4. Can be expressed as a complete transition table that serves as an
   implementation gate.

## Decision

There are two durable lifecycle states: `active` and `terminal`. `Resumed` is
an event, not a state.

```go
package session

type LifecycleState string

const (
    LifecycleActive   LifecycleState = "active"
    LifecycleTerminal LifecycleState = "terminal"
)

type LifecycleEventType string

const (
    EventDiscovered LifecycleEventType = "discovered"
    EventUpdated    LifecycleEventType = "updated"
    EventResumed    LifecycleEventType = "resumed"
    EventTerminal   LifecycleEventType = "terminal"
    EventStale      LifecycleEventType = "stale"
    EventRemoved    LifecycleEventType = "removed"
)

type LifecycleEvent struct {
    Type      LifecycleEventType
    SessionID string
    Source    string
    From      LifecycleState
    To        LifecycleState
    At        time.Time
    Reason    string
}
```

### Transition table

This table is a gate. Any implementation must satisfy every row. A new
transition requires updating this table before implementation begins.

| Current condition | Trigger | State after | Event emitted |
|---|---|---|---|
| Untracked | Source discovers handle; parse result is not stale | `active` | `discovered` |
| `active` | Parse returns new data | `active` | `updated` |
| `active` | `SourceUpdate.Terminal == true` | `terminal` | `terminal` |
| `active` | No new data past stale threshold | `terminal` | `stale` |
| `terminal` | Source returns new data after terminal | `active` | `resumed` |
| `terminal` | Retention window expires | Removed from store | `removed` |
| Removed key | No new data | No store entry | none |
| Removed key | New data appears | `active` | `resumed` |

### Rules

- **`Resumed` is an event, not a state.** After a resume, the session is
  `active`. The `LifecycleEvent.From` field records the prior durable state
  for consumers that need to distinguish a fresh discovery from a resumption.
- **Stale detection** uses wall-clock time from `session.LastDataReceivedAt`.
  The stale threshold is a monitor configuration option, not a constant.
- **Retention** controls how long a terminal session remains in the store after
  reaching the `terminal` state. Removal does not emit a `terminal` event —
  it emits `removed`.
- **State commits** happen before the corresponding `LifecycleEvent` is
  delivered to sinks. Sinks observe the new state, not the old state.
- **`LifecycleEvent.Reason`** carries a human-readable explanation for
  `terminal` (from `SourceUpdate.EndReason`) and `stale` (timeout duration).
  It is empty for `discovered`, `updated`, and `resumed`.

## Consequences

- The store holds only two runtime states per session, which simplifies
  serialization, snapshotting, and store locking.
- Consumers that need to distinguish "new session" from "resumed session" use
  the event type (`discovered` vs. `resumed`), not the durable state.
- The transition table is normative. If an implementation produces a transition
  not in the table, it is a bug.
- Adding `Stale` as a third durable state would require a table update and a
  minor version bump. The current model avoids that by collapsing stale into
  `terminal` with a `stale` event.
