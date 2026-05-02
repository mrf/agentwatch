# agentwatch - Library Extraction Plan

> **Status:** Draft, pre-implementation
> **Target repo:** `agentwatch` (placeholder name) - new repo, fresh `go.mod`, no dependency on `agent-racer`
> **Source repo:** `agent-racer` - origin of code and fixtures being extracted
> **Source branch:** `claude/backend-library-abstraction-UvaD5`, but this must be resolved to an exact commit SHA before any copy work starts
> **Decision:** API-first partial rewrite. Preserve proven parser, validation, fixture, and concurrency knowledge. Rebuild orchestration, events, lifecycle, and transports around a smaller public contract.

This document is the seed for beads issues in the new repo. It should let an engineer with read access to `agent-racer` execute the work without extra context.

Important source-control note: during review on 2026-05-02, the local `agent-racer` checkout was on `main`, the named branch was not present locally, and the checkout had local modifications in `backend/cmd/server/main.go` and `backend/internal/ws/server.go`. Do not extract from an ambiguous or dirty source state. First fetch the intended branch and pin the source SHA in an ADR.

---

## 1. Library Purpose

`agentwatch` is a Go library for monitoring multiple local AI coding agents such as Claude Code, OpenAI Codex CLI, Gemini CLI, and custom sources supplied by consumers.

It observes local agent session artifacts, aggregates session state, and exposes the result through:

1. A pull API: `Monitor.Snapshot()` for synchronous consumers.
2. A push API: `monitor.EventSink` for reactive consumers.
3. Optional HTTP and WebSocket transport adapters for remote or web frontends.

The library is UI-agnostic and game-agnostic. Concepts like racing lanes, leaderboards, achievements, tracks, battle passes, and overtakes are out of scope. Consumers layer those on top.

### Non-goals for v1

- Cloud agent monitoring.
- Database persistence.
- Replay file format.
- Built-in auth providers.
- Frontend embedding.
- Multi-tenant isolation.
- Prompt, response, or tool-output preview fields in public state.

The public API should be boring, narrow, and durable. It should support `agent-racer`, dashboards, TUIs, local automation, replay recorders, and future agent sources without making those consumers depend on racer-specific assumptions.

---

## 2. Public Surface Policy

The original draft said "No `internal/` directory. Every package is importable." That is not the right default for a serious Go library.

The revised rule:

> Public packages are stable extension points. Everything else starts internal until a real consumer needs it.

Use public packages for contracts consumers intentionally depend on:

- `monitor` - constructing and running monitors, snapshots, event sinks.
- `source` - implementing custom sources.
- `session` - inspecting session state and lifecycle types.
- `sources/<name>` - reference source constructors.
- `transport/httpapi` and `transport/wsapi` - optional adapters.

Use `internal/` for implementation details:

- Parser internals and schema structs.
- End-marker parsing and validation.
- File discovery helpers until a concrete external source needs them.
- Event dispatch/fanout mechanics.
- Clock/test helpers.
- Merge and lifecycle implementation details.

Do not expose a package just because two public packages need to share code. Once a package is public, it becomes a compatibility burden.

---

## 3. Module Layout

Initial layout:

```text
github.com/<org>/agentwatch/
|-- go.mod
|-- README.md
|-- LICENSE
|-- monitor/                    # PUBLIC: orchestration API
|   |-- monitor.go
|   |-- events.go
|   |-- options.go
|   |-- health.go
|   `-- monitor_test.go
|-- source/                     # PUBLIC: custom source contract
|   |-- source.go
|   `-- registry.go
|-- session/                    # PUBLIC: state model
|   |-- state.go
|   |-- lifecycle.go
|   `-- privacy.go
|-- sources/                    # PUBLIC: reference source constructors
|   |-- claude/
|   |   |-- source.go
|   |   `-- parser_test.go
|   |-- codex/
|   |-- gemini/
|   `-- mock/
|-- transport/                  # PUBLIC: optional adapters
|   |-- httpapi/
|   |   |-- handler.go
|   |   `-- handler_test.go
|   `-- wsapi/
|       |-- messages.go
|       |-- broadcaster.go
|       `-- server.go
|-- internal/                   # PRIVATE: implementation only
|   |-- eventbus/
|   |-- filewatch/
|   |-- jsonl/
|   |-- clock/
|   `-- testutil/
|-- examples/
|   |-- stdout/
|   `-- httpserver/
`-- docs/
    |-- architecture.md
    |-- source-implementation.md
    |-- migration-from-agent-racer.md
    `-- decisions/
```

Package visibility can be promoted later. It should not be promoted speculatively.

---

## 4. Core API Decisions

### 4.1 Source Contract

The source API must not assume append-only byte offsets. Current Gemini parsing already treats mtime as a fake offset because Gemini rewrites JSON files. Future sources may be stream-backed, process-backed, DB-backed, or snapshot-only.

Use an opaque cursor:

```go
package source

type Cursor string

type Source interface {
    Name() string
    Discover(ctx context.Context) ([]SessionHandle, error)
    Parse(ctx context.Context, h SessionHandle, c Cursor) (SourceUpdate, Cursor, error)
}

type SessionHandle struct {
    ID         string
    Path       string
    WorkingDir string
    StartedAt  time.Time
    Source     string
}

type SourceUpdate struct {
    SessionID string
    Slug      string

    Activity session.Activity
    Model    string

    ContextTokens    int
    OutputTokens     int
    MaxContextTokens int
    TokenEstimated   bool

    MessageCountDelta  int
    ToolCallCountDelta int
    CurrentTool        string

    WorkingDir string
    Branch     string

    StartedAt      time.Time
    LastActivityAt time.Time

    Subagents []session.SubagentState

    Terminal  bool
    EndReason string
    EndedAt   time.Time
}
```

Rules:

- The monitor treats `Cursor` as opaque. It stores and returns it to the source but never parses or compares it.
- File-tail sources may encode byte offsets as decimal strings.
- Rewrite-on-save sources may encode mtimes, hashes, or source-specific JSON.
- Source-specific continuity, such as Claude subagent parent maps, belongs inside the source or cursor. It should not leak into `SessionHandle`.
- `context.Context` is required on blocking methods.
- `Close()` is not part of the core interface. If a source owns resources, cancellation through `Run(ctx)` should be enough. A later optional `io.Closer` check can be added if a real source needs it.

### 4.2 Registry

Avoid global `init()` registration as the main path. Import side effects are convenient in examples but weak as a library foundation.

Preferred explicit registration:

```go
package source

type Factory func() (Source, error)

type Registry struct { /* ... */ }

func NewRegistry() *Registry
func (r *Registry) Register(name string, f Factory) error
func (r *Registry) Get(name string) (Factory, bool)
func (r *Registry) Names() []string
```

Built-in sources should expose constructors and registration helpers:

```go
package claude

func New(opts ...Option) (source.Source, error)
func Register(r *source.Registry, opts ...Option) error
```

Examples may provide a convenience registry, but core behavior should be explicit.

### 4.3 Monitor API

```go
package monitor

func New(opts ...Option) (*Monitor, error)

func (m *Monitor) Run(ctx context.Context) error
func (m *Monitor) PollOnce(ctx context.Context) error
func (m *Monitor) Snapshot() []session.SessionState
func (m *Monitor) Health() []Health

func WithSources(sources ...source.Source) Option
func WithPollInterval(d time.Duration) Option
func WithSink(sink EventSink) Option
func WithLogger(l *slog.Logger) Option
func WithClock(c Clock) Option
func WithHealthThreshold(n int) Option
func WithCompletionRetention(d time.Duration) Option
```

Rules:

- `New` returns `(*Monitor, error)` so invalid options fail early.
- No `WithStateDir` in v1. Persistence is out of scope.
- No XDG path assumptions in `monitor`.
- State commits happen before event delivery.
- Store locks must be released before calling sinks.
- `PollOnce` is a first-class test and integration tool.

### 4.4 Session State

```go
package session

type SessionState struct {
    ID     string `json:"id"`
    Source string `json:"source"`
    Slug   string `json:"slug,omitempty"`

    Activity  Activity       `json:"activity"`
    Lifecycle LifecycleState `json:"lifecycle"`

    ContextTokens      int     `json:"contextTokens"`
    OutputTokens       int     `json:"outputTokens,omitempty"`
    TokenEstimated     bool    `json:"tokenEstimated"`
    MaxContextTokens   int     `json:"maxContextTokens"`
    ContextUtilization float64 `json:"contextUtilization"`

    Model       string `json:"model"`
    WorkingDir  string `json:"workingDir"`
    Branch      string `json:"branch,omitempty"`
    CurrentTool string `json:"currentTool,omitempty"`

    MessageCount  int `json:"messageCount"`
    ToolCallCount int `json:"toolCallCount"`

    StartedAt          time.Time  `json:"startedAt"`
    LastActivityAt     time.Time  `json:"lastActivityAt"`
    LastDataReceivedAt time.Time  `json:"lastDataReceivedAt"`
    CompletedAt        *time.Time `json:"completedAt,omitempty"`

    Subagents []SubagentState `json:"subagents,omitempty"`
}
```

Removed from public state:

- `Lane`, `Position`, `PositionDelta` - racer-specific.
- `Name` - display concern; consumers derive it.
- `IsChurning`, `BurnRatePerMinute`, `CompactionCount` - useful candidates, but not core until a non-racer consumer needs them.
- `PID`, `TmuxTarget` - host-process awareness. Put in an integration package or consumer sink, not v1 core.
- `LastAssistantText` - privacy-sensitive.
- `LogPath` - sensitive local path and implementation detail.

### 4.5 Lifecycle Model

`Resumed` is an event, not a durable state.

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

Transition rules:

| Current condition | Trigger | State after | Event |
|---|---|---|---|
| Untracked | source discovers handle and parse is not stale | active | discovered |
| Active | parse returns new data | active | updated |
| Active | source update has `Terminal=true` | terminal | terminal |
| Active | no data past stale threshold | terminal | stale |
| Terminal | source returns new data after terminal | active | resumed |
| Terminal | retention window expires | removed from store | removed |
| Removed key | no new data | no store entry | none |
| Removed key | new data appears | active | resumed |

This table is a gate. If implementation needs another transition, update the table first.

### 4.6 Event Model

Prefer one event envelope over multiple callback methods.

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
    Seq uint64    `json:"seq"`
    At  time.Time `json:"at"`
    Type EventType `json:"type"`

    Sessions []session.SessionState `json:"sessions,omitempty"`
    Updates  []session.SessionState `json:"updates,omitempty"`
    Removed  []string               `json:"removed,omitempty"`

    Lifecycle *session.LifecycleEvent `json:"lifecycle,omitempty"`
    Health    *Health                 `json:"health,omitempty"`
}

type EventSink interface {
    HandleEvent(ctx context.Context, ev Event) error
}
```

Delivery contract:

- `Seq` is monotonically increasing per monitor instance.
- Events are delivered after state commit and outside store locks.
- A sink must return quickly. Long-running sinks should wrap themselves in an async queue.
- Default `MultiSink` delivers sequentially and returns a joined error.
- Sink errors are logged and surfaced through monitor health. They do not roll back committed state.
- `Removed` events are part of the core event model. Transports should not invent their own removal semantics.
- A replay recorder should be implementable as an `EventSink` without library changes.

An optional `monitor.AsyncSink` can be added once a concrete transport needs a shared bounded-queue implementation. Its overflow policy must be explicit: block, drop-new, or drop-old.

### 4.7 Health Model

Health types belong in `monitor`, not `transport/wsapi`.

```go
package monitor

type HealthStatus string

const (
    HealthHealthy  HealthStatus = "healthy"
    HealthDegraded HealthStatus = "degraded"
    HealthFailed   HealthStatus = "failed"
)

type Health struct {
    Source           string
    Status           HealthStatus
    DiscoverFailures int
    ParseFailures    int
    LastError        string
    UpdatedAt        time.Time
}

```

Health error strings must be sanitized before leaving the process. Absolute paths and panic details should not be exposed through transports.

---

## 5. Security and Privacy

This library observes sensitive local development artifacts. Treat privacy as a design constraint, not a feature bolted onto transports.

Rules:

1. Public session state must not include prompt text, assistant text, tool output, raw JSONL paths, PIDs, or tmux targets in v1.
2. `WorkingDir`, `Branch`, model names, session IDs, and source names are still potentially sensitive. Keep `session.PrivacyFilter`.
3. Health errors exposed over HTTP/WS must be sanitized. Replace absolute paths with `<path>` and reduce panic details to `internal error`.
4. HTTP examples bind to `127.0.0.1` by default.
5. WebSocket origin defaults should allow same-origin and localhost development, not arbitrary remote origins.
6. Auth is adapter-level. Core monitor has no auth concept.
7. `LogPath` can exist in private tracked metadata but not in JSON output.

The existing `backend/internal/session/privacy.go` in `agent-racer` is mostly generic, but it references fields that v1 removes (`PID`, `TmuxTarget`). Copy the idea and tests, not the file verbatim.

---

## 6. What to Preserve from agent-racer

All source paths are relative to the `agent-racer` repo root. Pin a source SHA before using any of these.

| Source path | Preserve | Destination |
|---|---|---|
| `backend/internal/monitor/source.go` | Interface intent and source comments, not the exact `offset int64` API | `source/source.go` |
| `backend/internal/jsonl/jsonl.go` | Complete-line iteration, file-size cap, line-size cap | `internal/jsonl/reader.go` |
| `backend/internal/monitor/jsonl.go` | Claude parsing knowledge, subagent parsing behavior, path decode logic if still needed | `sources/claude` private parser files |
| `backend/internal/session/store.go` | Clone discipline and lock-release-before-callback pattern | `monitor` or private store package |
| `backend/internal/session/tail.go` | Incremental tailing behavior if still needed by sources | `internal/jsonl` or source-private code |
| End-marker validation in `backend/internal/monitor/monitor.go` | Bounds checks, timestamp skew handling, path traversal checks | `sources/claude` private marker parser |
| `backend/internal/monitor/health.go` | Hysteresis idea and tests | `monitor/health.go`, without ws imports |
| `backend/internal/session/state.go` | Activity enum, JSON marshal/unmarshal, deep-copy pattern | `session/state.go` |
| Source parser tests and fixtures | Format knowledge | `sources/<name>/parser_test.go` |
| `backend/internal/ws/ratelimit.go` | Generic connection rate-limit behavior | `transport/wsapi/ratelimit.go` |
| `backend/internal/ws/broadcast_*_test.go` | Slow-client, conn-limit, write-pump test approaches | new wsapi tests |
| Deadlock detection tests from monitor/store tests | Concurrency risk coverage | monitor/store tests |

Copy commit messages should include provenance:

```text
lift: preserve <thing> from agent-racer@<sha>
```

Do not merge the source branch into the new repo. Use `git show <sha>:<path>` or an equivalent pinned extraction.

---

## 7. What to Rewrite

| agent-racer artifact | Why rewrite | Replacement |
|---|---|---|
| `backend/internal/monitor/monitor.go` | Monolithic orchestration mixed with racer state, process awareness, tmux, broadcasting, token math, and Claude end markers | New monitor built around source cursor, store, lifecycle table, and event envelope |
| `SessionState` | Mixes generic state with racer UI, process, tmux, and preview fields | Slim public state in `session` |
| `session.Store` file as-is | Currently assigns `Lane` | Preserve clone/concurrency behavior, remove racer fields |
| `backend/internal/ws/server.go` | Imports app concerns and game logic | `transport/wsapi` adapter driven only by monitor events |
| `backend/internal/ws/broadcast.go` | Couples broadcaster to store and racer event types | EventSink-driven broadcaster |
| Hardcoded source construction in app server | Consumers need explicit source choice | Explicit registry and source constructors |
| Magic constants in monitor | Hard to tune and test | `monitor.Config` via functional options |
| Codex/Gemini discovery boilerplate | Duplicated walking/filtering logic | Private `internal/filewatch` initially |
| XDG defaults in library path | Library should not own local app policy | Examples and consumers choose paths |
| Process/tmux awareness | Host-coupled, optional, privacy-sensitive | Later `integrations/process` or consumer-side sink |

Line-count targets such as "monitor.go <= 150 LOC" are smell tests, not gates. The actual gates are package boundaries, lifecycle clarity, tests, and consumer proof.

---

## 8. Source-Specific Packaging

### Claude

Claude-specific pieces stay under `sources/claude` or source-private internals:

- Claude JSONL schema structs.
- Claude progress/subagent parsing.
- Claude project path encoding/decoding.
- Claude session-end marker parsing and validation.

The core monitor should not know that Claude writes hook marker files. It should only see `SourceUpdate{Terminal: true, EndReason: ...}`.

Claude-specific options include:

```go
func WithRoot(path string) Option
func WithDiscoverWindow(d time.Duration) Option
func WithSessionEndDir(path string) Option
func WithMaxEndMarkerSize(bytes int) Option
```

### Codex

Codex-specific pieces stay under `sources/codex`:

- CODEX_HOME resolution only if passed as an option or documented constructor default.
- Rollout filename/session ID parsing.
- Old and new rollout envelope support.
- Token/model extraction.

The public constructor should accept explicit roots:

```go
func WithRoot(path string) Option
func WithDiscoverWindow(d time.Duration) Option
```

If the constructor uses environment defaults, document them as source package behavior, not monitor behavior.

### Gemini

Gemini-specific pieces stay under `sources/gemini`:

- Rewrite-on-save parsing.
- Hash-to-working-dir lookup.
- Any process scanning needed to map hashes.

Because Gemini currently needs process inspection for path mapping, keep that logic source-private in v1. Do not introduce public process awareness until another consumer needs it.

---

## 9. Transport Adapters

### HTTP

```go
package httpapi

func NewHandler(mon *monitor.Monitor, opts ...Option) (http.Handler, error)
```

Routes:

- `GET /sessions`
- `GET /sessions/{id}`
- `GET /healthz`
- `GET /sources`

No racer routes. No auth provider built in.

### WebSocket

```go
package wsapi

func NewServer(mon *monitor.Monitor, opts ...Option) (*Server, error)

func WithAuthenticator(a Authenticator) Option
func WithAllowedOrigins(origins []string) Option
func WithMaxConnections(n int) Option
func WithHeartbeat(d time.Duration) Option

type Identity struct {
    ID string
}

type Authenticator interface {
    Authenticate(r *http.Request) (Identity, bool)
}
```

The WebSocket protocol should wrap `monitor.Event`. It should not define separate racer-era message types. If `agent-racer` needs overtake or achievement events, those live in `agent-racer`.

---

## 10. Build Phases

### Phase 0 - Pin Source and Freeze API Decisions

Goal: remove ambiguity before code starts.

- [ ] Fetch the intended `agent-racer` branch.
- [ ] Pin exact source commit SHA in `docs/decisions/0001-source-provenance.md`.
- [ ] Document local dirty-file status if any source files are used.
- [ ] Write ADR: public/internal package policy.
- [ ] Write ADR: source cursor model.
- [ ] Write ADR: monitor event envelope and sink delivery contract.
- [ ] Write ADR: lifecycle transition table.
- [ ] Decide module path and placeholder name.

Exit criteria: the first implementation issue has a pinned source SHA and agreed public contracts.

### Phase 1 - Vertical Slice

Goal: prove the abstraction before porting everything.

- [ ] Initialize repo, `go.mod`, license, minimal README.
- [ ] Implement `session.Activity`, slim `SessionState`, clone methods.
- [ ] Implement `source.Source` with opaque cursor.
- [ ] Implement a tiny private store with lock-release-before-callback behavior.
- [ ] Implement `monitor.New`, `PollOnce`, `Snapshot`, and one event sink path.
- [ ] Port one source enough to parse real data. Prefer Claude because it exercises subagents and end markers.
- [ ] Build `sources/mock` for deterministic monitor tests.
- [ ] Build `examples/stdout`.
- [ ] Spike a minimal `agent-racer` adapter implementing `monitor.EventSink`.

Exit criteria:

- `go test ./...` and `go test -race ./...` pass.
- `examples/stdout` prints real session updates.
- The `agent-racer` spike can express lane assignment, position calculation, and completion handling without changing core API.
- If the spike exposes an API flaw, revise before moving on.

### Phase 2 - Preserve Primitives and Fixtures

Goal: move proven low-level behavior into the new shape.

- [ ] Preserve generic JSONL complete-line reader as `internal/jsonl`.
- [ ] Preserve Claude parser behavior and fixtures under `sources/claude`.
- [ ] Preserve store concurrency tests.
- [ ] Preserve end-marker validation behavior under `sources/claude`.
- [ ] Preserve source health hysteresis tests without ws imports.
- [ ] Preserve privacy filter tests adjusted for the slim state model.
- [ ] Add ADR for JSONL parser canonicalization.

Exit criteria: copied/preserved tests pass against the new API, and no Claude schema types leak into public `jsonl`.

### Phase 3 - Source Plumbing

Goal: port reference sources without widening core.

- [ ] Build `internal/filewatch` for shared discovery.
- [ ] Port `sources/claude`.
- [ ] Port `sources/codex`.
- [ ] Port `sources/gemini`.
- [ ] Add explicit constructors and `Register(r)` helpers.
- [ ] Migrate fixtures.
- [ ] Add source package docs.

Exit criteria: parser and discovery tests pass for all three sources. The monitor still depends only on the public `source.Source` contract.

### Phase 4 - Full Orchestration

Goal: complete monitor behavior around lifecycle, health, and removals.

- [ ] Implement lifecycle transitions exactly as specified in the table.
- [ ] Implement source health and sink health.
- [ ] Implement event sequencing.
- [ ] Implement removal/retention behavior.
- [ ] Implement `Run(ctx)` with ticker recreation if config changes are supported.
- [ ] Add tests for discovery, update, terminal, stale, removal, and resume.
- [ ] Add deadlock/race tests.

Exit criteria: monitor behavior is covered by tests and event ordering is deterministic.

### Phase 5 - Transport Adapters

Goal: optional HTTP and WS adapters that consume the generic monitor.

- [ ] Build `transport/httpapi`.
- [ ] Build `transport/wsapi` around `monitor.Event`.
- [ ] Port rate-limit behavior.
- [ ] Port slow-client, conn-limit, and write-pump test patterns.
- [ ] Build `examples/httpserver`.
- [ ] Add privacy and origin tests.

Exit criteria:

- `examples/httpserver` runs locally.
- `curl /sessions` returns sanitized state.
- A WebSocket client receives snapshot, delta, health, lifecycle, and removal events.
- No transport imports app-specific packages.

### Phase 6 - agent-racer Migration

Goal: prove the abstraction in the original app.

- [ ] Add `agentwatch` as an `agent-racer` dependency with a local replace during development.
- [ ] Replace extracted internals with imports.
- [ ] Build `agent-racer/backend/internal/racer/sink.go` implementing `monitor.EventSink`.
- [ ] Keep lane assignment, position computation, overtakes, gamification, tracks, replay, and frontend code in `agent-racer`.
- [ ] Move racer-specific WS messages into `agent-racer`.
- [ ] Run full backend and frontend gates.
- [ ] Verify the frontend end-to-end in browser.

Exit criteria: `agent-racer` is a consumer, not a source of hidden requirements. Any missing concept must be solved through generic events or explicitly left app-side.

### Phase 7 - Publish

- [ ] Tag `v0.1.0`.
- [ ] Write README quickstart.
- [ ] Write architecture docs.
- [ ] Write source implementation guide.
- [ ] Write migration guide.
- [ ] Decide final name.
- [ ] Submit to `pkg.go.dev`.

---

## 11. Validation Gates

Run at the end of each phase:

```bash
go test ./...
go test -race ./...
go vet ./...
golangci-lint run
```

Concurrency-sensitive packages need race coverage. Deadlock-detection patterns from `agent-racer` should be preserved.

Additional gates:

- Public API review before v0.1.0.
- `go doc` review for every public package.
- Privacy review for every JSON response and WS payload.
- `agent-racer` vertical-slice proof before all-source porting.

---

## 12. Hard Design Rules

1. Public packages are stable extension points; implementation details start under `internal/`.
2. No game/UI concepts in core packages.
3. No filesystem path defaults in `monitor`.
4. No prompt text, assistant text, tool output, raw log path, PID, or tmux target in public v1 state.
5. `context.Context` on every blocking method.
6. Source cursors are opaque to the monitor.
7. `Resumed` is an event, not a durable lifecycle state.
8. State commits happen before event delivery and outside store locks.
9. Functional options for multi-argument constructors.
10. Traditional `for i := 0; i < n; i++` loops; no range-over-integer.
11. No global source registration as the primary API.
12. Easier to expand than retract: do not export types speculatively.

---

## 13. Open Questions

File these as beads before implementation starts:

1. **Library name.** `agentwatch` is a placeholder. Evaluate conflicts and package readability.
2. **Cursor encoding.** Is `type Cursor string` enough, or should it be `[]byte`/struct for source-specific JSON? Default: string.
3. **Privacy defaults.** Should HTTP/WS adapters apply a default privacy filter or only expose what monitor state already omits?
4. **Process/tmux integration.** Leave to consumers in v1, or create later `integrations/process` and `integrations/tmux` packages?
5. **Burn-rate/churn/compaction metrics.** Useful enough for core, or app-side derived metrics? Default: app-side until proven otherwise.
6. **Replay format.** Out of v1, but event envelope should be replay-friendly.
7. **Authenticator shape.** Current lean: `(Identity, bool)`. Revisit if errors need to be distinguished from denial.
8. **Public filewatch helper.** Keep `internal/filewatch` in v1; promote only if custom source authors need it.

---

## 14. First Beads Issues

1. **`bd-1`: Pin source provenance**
   Fetch the intended `agent-racer` branch, pin the source SHA, and write `docs/decisions/0001-source-provenance.md`.

2. **`bd-2`: Lock public API ADRs**
   Write ADRs for package visibility, source cursor, event envelope, sink delivery, and lifecycle transitions.

3. **`bd-3`: Bootstrap repo**
   Initialize `go.mod`, license, README, CI for `go test`, `go test -race`, `go vet`, and lint.

4. **`bd-4`: Build vertical slice**
   Implement slim session state, source cursor contract, monitor `PollOnce`, mock source, one real source slice, stdout example, and tests.

5. **`bd-5`: Spike agent-racer consumer**
   Implement a temporary `agent-racer` sink against the vertical slice and document any API gaps before porting more code.

All later source-porting and transport work depends on these.

---

## Appendix A - Why Partial Rewrite

Review of `agent-racer` shows two categories:

Library-grade knowledge:

- Source abstraction intent.
- JSONL complete-line parsing behavior.
- Source-specific parser fixtures.
- Store clone discipline.
- Lock-release-before-callback concurrency pattern.
- End-marker validation checks.
- Health hysteresis tests.
- Deadlock/race test patterns.

App-grade implementation:

- A large monitor that mixes discovery, lifecycle, token math, process awareness, tmux, session-end markers, store updates, and broadcasting.
- Public session state that includes racer UI fields and privacy-sensitive host fields.
- WebSocket code that imports game logic.
- Duplicated source discovery boilerplate.
- Config and filesystem defaults that belong in an app, not a library.

The work is not "copy a library out." It is "use a working app as evidence while designing a reusable library." Contracts first, extraction second.
