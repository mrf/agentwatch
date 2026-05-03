# Migration from agent-racer

`agentwatch` is an API-first extraction of the session monitoring subsystem from `agent-racer`. Racer-specific concepts (lanes, positions, overtakes, gamification) remain in `agent-racer`.

This guide maps old types and patterns to their new equivalents.

## Package Mapping

| agent-racer path | agentwatch path | Notes |
|------------------|-----------------|-------|
| `backend/internal/session/` | `session/` | Slimmed down; racer fields removed |
| `backend/internal/monitor/monitor.go` | `monitor/` | Rewritten; same purpose, cleaner API |
| `backend/internal/ws/server.go` | `transport/wsapi/` | EventSink-driven, no racer event types |
| `backend/internal/ws/broadcast.go` | `transport/wsapi/broadcaster.go` | EventSink-driven |
| `backend/internal/session/privacy.go` | `session/privacy.go` | Ported; PID and TmuxTarget fields removed |
| `backend/internal/jsonl/` | `internal/jsonl/` | Preserved as-is (private) |
| `sources/claude/` | `sources/claude/` | Preserved parser logic; new option API |
| `sources/codex/` | `sources/codex/` | Preserved; new option API |
| `sources/gemini/` | `sources/gemini/` | Preserved; new option API |
| — | `source/` | New: defines the Source interface |
| — | `transport/httpapi/` | New: JSON HTTP adapter |

## Type Mapping

### SessionState

The public session state was slimmed significantly. All racer-specific and privacy-sensitive fields were removed.

**Unchanged fields** (same name and semantics on `session.SessionState`): `ID`, `Source`, `Slug`, `Activity`, `Model`, `WorkingDir`, `Branch`, `CurrentTool`, `ContextTokens`, `OutputTokens`, `MaxContextTokens`, `ContextUtilization`, `TokenEstimated`, `MessageCount`, `ToolCallCount`, `StartedAt`, `LastActivityAt`, `LastDataReceivedAt`, `CompletedAt`, `Subagents`.

**Removed fields:**

| agent-racer field | Reason |
|-------------------|--------|
| `Lane`, `Position`, `PositionDelta` | Racer-specific; assign lanes and compute positions in your sink |
| `Name` | Display concern; derive from `Slug` or `ID` in your consumer |
| `IsChurning`, `BurnRatePerMinute`, `CompactionCount` | Not in v1 core; compute in your sink if needed |
| `PID`, `TmuxTarget` | Host-process awareness; keep in your integration layer |
| `LastAssistantText`, `LogPath` | Privacy-sensitive; not in public state |

### Monitor

The old monitor mixed orchestration, racer state, process awareness, tmux integration, broadcasting, and token math. The new monitor is narrower.

| agent-racer pattern | agentwatch equivalent |
|--------------------|----------------------|
| `monitor.New(...)` with many positional args | `monitor.New(opts...)` with functional options |
| `monitor.GetSessions()` | `mon.Snapshot()` |
| `monitor.GetSession(id)` | `mon.Get(id)` |
| Hardcoded poll interval | `monitor.WithPollInterval(d)` |
| Hardcoded stale threshold | `monitor.WithStaleThreshold(d)` |
| Hardcoded health threshold | `monitor.WithHealthThreshold(n)` |
| Direct broadcast calls | Implement `monitor.EventSink` and pass via `monitor.WithSink` |
| Source construction inside monitor | Pass sources explicitly via `monitor.WithSources` |
| Global source registration | `source.Registry` or direct `WithSources` — no global init |

### WebSocket Server

The new WS server is a generic `monitor.EventSink` adapter with no racer-specific event types.

| agent-racer pattern | agentwatch equivalent |
|--------------------|----------------------|
| Racer-specific WS message types | `monitor.Event` wrapped as JSON — no separate message types |
| Auth baked in | Implement `wsapi.Authenticator` and pass via `wsapi.WithAuthenticator` |
| Hardcoded origin policy | `wsapi.WithAllowedOrigins(origins)` |
| Hardcoded rate limit | `wsapi.WithRateLimit(limit, burst, window)` |

### Privacy Filter

| agent-racer | agentwatch |
|-------------|------------|
| `session.Privacy{RedactPID: true}` | `session.Policy{...}` |
| `RedactPID` | **removed** — PID is not in public state |
| `RedactTmuxTarget` | **removed** — TmuxTarget is not in public state |
| `RedactWorkingDir` | `session.Policy{RedactWorkingDir: true}` |
| `RedactBranch` | `session.Policy{RedactBranch: true}` |
| `RedactModel` | `session.Policy{RedactModel: true}` |
| `RedactSessionID` | `session.Policy{RedactSessionID: true}` |
| `RedactSource` | `session.Policy{RedactSource: true}` |

## Migrating agent-racer to Use agentwatch

`agent-racer` should become a consumer of `agentwatch`, not contain it. Recommended migration path:

1. Add `agentwatch` as a dependency (`go get github.com/mrf/agentwatch`).
2. Implement `monitor.EventSink` in a new `backend/internal/racer/sink.go`. This sink receives `monitor.Event` and maps sessions to lanes, computes positions, emits overtake notifications, etc.
3. Wire the sink: `monitor.New(monitor.WithSources(srcs...), monitor.WithSink(racerSink))`.
4. Keep lane assignment, position computation, overtakes, gamification, tracks, replay, and frontend code in `agent-racer`.
5. Move racer-specific WS message types into `agent-racer` — they do not belong in `agentwatch`.
6. Remove the copy of monitor, session store, WS broadcaster, and source parsers from `agent-racer` once the agentwatch versions are proven equivalent.

## What Was Preserved vs Rewritten

### Preserved (logic copied, API adapted)

- JSONL complete-line reader (`internal/jsonl`)
- Claude parser and fixtures (`sources/claude/parser.go`)
- Store clone and lock discipline (`session.SessionState.Clone()`)
- End-marker validation logic (`sources/claude`)
- Health hysteresis (`monitor.Health`, `WithHealthThreshold`)
- Deadlock detection test patterns (race tests in monitor and session packages)
- Rate-limit behavior in WS server (`transport/wsapi/ratelimit.go`)
- Slow-client test patterns (`transport/wsapi`)
- Privacy filter tests (adapted for new field set)

### Rewritten (same purpose, new design)

- Monitor orchestration: source cursor, store, lifecycle table, event envelope
- Session state: slimmed, no racer fields
- WS server: EventSink-driven, generic event protocol
- WS broadcaster: decoupled from store and racer types
- Source construction: explicit options, no global init
- Config: functional options throughout
- Filesystem defaults: removed from library; examples and consumers choose paths
