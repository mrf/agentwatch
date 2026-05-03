# Architecture

## Package Map

```
github.com/mrf/agentwatch/
│
├── monitor/          PUBLIC  — orchestration: poll, state store, event delivery
├── session/          PUBLIC  — state model: SessionState, Activity, LifecycleState, Policy
├── source/           PUBLIC  — source contract: Source interface, SessionHandle, SourceUpdate, Cursor
│
├── sources/
│   ├── claude/       PUBLIC  — Claude Code source (JSONL session files)
│   ├── codex/        PUBLIC  — OpenAI Codex CLI source (rollout JSONL files)
│   ├── gemini/       PUBLIC  — Gemini CLI source (checkpoint.json files)
│   └── mock/         PUBLIC  — deterministic test double
│
├── transport/
│   ├── httpapi/      PUBLIC  — JSON HTTP adapter (GET /sessions, /healthz, /sources)
│   └── wsapi/        PUBLIC  — WebSocket adapter (push events to browser clients)
│
└── internal/
    ├── jsonl/                — complete-line JSONL reader with size cap
    ├── filewatch/            — shared file discovery helpers
    ├── eventbus/             — event fanout mechanics
    ├── clock/                — clock abstraction for deterministic tests
    └── testutil/             — shared test helpers
```

**Public** packages are stable extension points. Breaking changes require a version bump.  
**Internal** packages are implementation details; their APIs can change freely.

## Data Flow

```
Sources (Discover + Parse)
        │
        │  SessionHandle + SourceUpdate
        ▼
   Monitor.PollOnce
        │
        ├── state store (map[id]SessionState, guarded by sync.Mutex)
        │       • applyUpdate merges SourceUpdate → SessionState
        │       • lifecycle transitions fire before event delivery
        │       • stale sweep: active sessions silent > threshold → terminal
        │       • retention sweep: terminal sessions past window → removed
        │
        └── EventSink.HandleEvent (called outside store locks)
                │
                ├── EventDelta    — updated + removed session IDs
                ├── EventLifecycle — single lifecycle transition
                └── EventHealth   — source health status change

   Monitor.Snapshot()  →  []SessionState  (pull path, any time)
```

### Sequencing guarantees

1. State is committed to the store before any event is delivered.
2. Events are delivered outside all store locks.
3. `PollOnce` is not safe to call concurrently; `Run` serialises calls internally.
4. `Snapshot` and `Get` acquire the store lock briefly for a deep copy and are safe to call from any goroutine.

## Key Types

### monitor.Event

The event envelope delivered to every `EventSink`:

```go
type Event struct {
    Seq      uint64                   // monotonically increasing
    At       time.Time
    Type     EventType                // snapshot | delta | lifecycle | health
    Sessions []session.SessionState   // full snapshot (EventSnapshot only)
    Updates  []session.SessionState   // changed sessions (EventDelta)
    Removed  []string                 // removed session IDs (EventDelta)
    Lifecycle *session.LifecycleEvent // single transition (EventLifecycle)
    Health   *Health                  // source health change (EventHealth)
}
```

### session.LifecycleEvent

Records a single state machine transition:

```go
type LifecycleEvent struct {
    Type      LifecycleEventType  // discovered | updated | terminal | stale | resumed | removed
    SessionID string
    Source    string
    From      LifecycleState
    To        LifecycleState
    At        time.Time
    Reason    string
}
```

### Lifecycle State Machine

```
                ┌─────────────┐
   discover     │             │   new data
──────────────▶ │   active    │◀──────────────┐
                │             │               │ (resumed)
                └──────┬──────┘               │
                       │                      │
          Terminal=true │ or stale threshold   │
                       ▼                      │
                ┌─────────────┐               │
                │  terminal   │───────────────┘
                │             │
                └──────┬──────┘
                       │ retention window expires
                       ▼
                   (removed)
```

| Trigger | From | To | Event type |
|---------|------|----|------------|
| First parse result | — | active | `discovered` |
| New data | active | active | `updated` |
| `SourceUpdate.Terminal = true` | active | terminal | `terminal` |
| No data past stale threshold | active | terminal | `stale` |
| New data after terminal | terminal | active | `resumed` |
| Retention window expires | terminal | — | `removed` |

## Extension Points

### Adding a new agent source

Implement `source.Source` and pass it to `monitor.WithSources`. No registration required. See [source-implementation.md](source-implementation.md).

### Adding a new transport

Implement `monitor.EventSink` and wire it as `monitor.WithSink`. The wsapi and httpapi transports are ordinary consumers of the same interface.

For the common case of needing the monitor to forward events to a transport that was constructed after the monitor, use a relay (see `examples/httpserver/main.go` for the `sinkRelay` pattern that breaks the circular dependency).

### Privacy filtering

Apply `session.Policy` before forwarding state to a transport or external sink:

```go
policy := session.Policy{
    RedactWorkingDir: true,
    RedactBranch:     true,
    RedactModel:      true,
    RedactSessionID:  true,
    RedactSource:     true,
}
safe := policy.Apply(state)
```

Health error strings are sanitized internally (absolute paths replaced with `<path>`) before they leave the monitor.

### Multiple sinks

Use `monitor.MultiSink` to deliver events to several sinks sequentially:

```go
sink := monitor.MultiSink{sinkA, sinkB}
mon, err := monitor.New(monitor.WithSources(src), monitor.WithSink(sink))
```

### Source Registry

`source.Registry` maps names to factory functions. It is an optional organizational tool — pass sources directly to `WithSources` if you do not need dynamic lookup:

```go
reg := source.NewRegistry()
claude.Register(reg, claude.WithRoot(dir))
codex.Register(reg, codex.WithRoot(codexHome))

// later
factory, ok := reg.Get("claude")
src, err := factory()
```

## Design Constraints

Hard rules from the project plan. Changing any of these requires updating the plan first.

1. No game or UI concepts in core packages (`Lane`, `Position`, `Overtake` live in consumers).
2. No filesystem path defaults in `monitor`. Consumers and examples choose paths.
3. No prompt text, assistant text, tool output, raw log path, PID, or tmux target in public v1 state.
4. `context.Context` on every blocking method.
5. Source cursors are opaque to the monitor — never parsed or compared outside their source.
6. State commits happen before event delivery and outside store locks.
7. Functional options for multi-argument constructors.
8. Traditional `for i := 0; i < n; i++` loops; no range-over-integer.
9. No global source registration as the primary API.
10. Health error strings must be sanitized before leaving the process.
