# agentwatch — Library Extraction Plan

> **Status:** Draft, pre-implementation
> **Source repo:** `agent-racer` (this repo) — origin of the code being extracted
> **Target repo:** `agentwatch` (placeholder name) — new repo, fresh `go.mod`, no dependency on agent-racer
> **Branch in source:** `claude/backend-library-abstraction-UvaD5`
> **Decision:** Partial rewrite. Keep proven primitives (interfaces, concurrency patterns, validation, parsing). Rewrite the orchestration layer and slim the state model to remove racer-specific concepts.

This document is the seed for beads issues in the new repo. It is self-contained: an engineer with read access to agent-racer should be able to execute it without further context.

---

## 1. Library Purpose

`agentwatch` is a Go library for monitoring multiple local AI coding agents (Claude Code, OpenAI Codex CLI, Gemini CLI, and any custom source the consumer plugs in). It tails their JSONL session logs, aggregates session state, and exposes the result via:

1. A pull API (`monitor.Snapshot()`) for synchronous consumers
2. A push API (`monitor.EventSink`) for reactive consumers
3. Optional HTTP+WebSocket transport adapters for remote/web frontends

The library is **UI-agnostic and game-agnostic**. Concepts like racing lanes, leaderboards, achievements, and battle passes are explicitly out of scope; consumers layer those on top.

### Non-goals (v1)

- Cloud agent monitoring (only local JSONL-backed agents)
- Persistence beyond in-memory state (no DB, no replay file format)
- Auth providers (consumers supply their own `Authenticator` impl)
- Frontend embedding (transport adapters serve JSON only)
- Multi-tenant isolation

---

## 2. Module Layout

```
github.com/<org>/agentwatch/
├── go.mod                       # module github.com/<org>/agentwatch
├── README.md
├── LICENSE                      # MIT, matching agent-racer
├── source/                      # PUBLIC: plugin contract
│   ├── source.go                # Source interface, SessionHandle, SourceUpdate
│   ├── registry.go              # NEW: dynamic source registration
│   └── filewatch/               # NEW: shared file-discovery helper
│       ├── watcher.go
│       └── watcher_test.go
├── jsonl/                       # PUBLIC: parsing primitives
│   ├── parse.go
│   └── parse_test.go
├── session/                     # PUBLIC: state model + store
│   ├── state.go                 # slimmed SessionState
│   ├── store.go                 # Store + UpdateAndNotify
│   ├── lifecycle.go             # NEW: explicit state machine
│   └── tail.go                  # incremental file tailing
├── monitor/                     # PUBLIC: orchestration
│   ├── monitor.go               # rewritten poll loop, ~150 LOC target
│   ├── events.go                # NEW: EventSink interface + event types
│   ├── validate.go              # end-marker validation (copied)
│   └── options.go               # NEW: functional options
├── transport/                   # PUBLIC, opt-in
│   ├── httpapi/                 # REST handlers
│   │   ├── handler.go
│   │   └── handler_test.go
│   └── wsapi/                   # WebSocket protocol + broadcaster
│       ├── messages.go
│       ├── broadcaster.go
│       └── server.go
├── sources/                     # PUBLIC: reference implementations
│   ├── claude/
│   │   ├── source.go
│   │   └── parser.go
│   ├── codex/
│   ├── gemini/
│   └── mock/                    # for tests + demos
├── examples/
│   ├── stdout/                  # monitor → stdout
│   └── httpserver/              # monitor → HTTP+WS
└── docs/
    ├── architecture.md
    ├── source-implementation.md
    └── migration-from-agent-racer.md
```

**Critical:** No `internal/` directory. Every package is importable.

---

## 3. What to Copy Verbatim from agent-racer

All paths below are relative to the agent-racer repo root.

| Source path (agent-racer) | Destination (agentwatch) | Lines | Notes |
|---|---|---|---|
| `backend/internal/monitor/source.go` | `source/source.go` | 179 | **Add `context.Context`** to `Discover` and `Parse` signatures. Otherwise unchanged. This is the cleanest part of the codebase. |
| `backend/internal/jsonl/jsonl.go` | `jsonl/parse.go` | — | Domain-agnostic line reader / scanner. There is also a `backend/internal/monitor/jsonl.go` — investigate which is canonical (see §6). |
| `backend/internal/session/store.go` | `session/store.go` | 116 | **Keep the `UpdateAndNotify` lock-release-before-callback pattern verbatim.** This is a hard-won concurrency detail. |
| `backend/internal/session/tail.go` | `session/tail.go` | — | Incremental file tailing utility. Generic. |
| End-marker validation in `backend/internal/monitor/monitor.go` (~lines 89–132 — verify in source before lifting) | `monitor/validate.go` | ~45 | Bounds checks, regex, timestamp skew, path traversal. Already hardened. |
| WS message envelope types from `backend/internal/ws/protocol.go` | `transport/wsapi/messages.go` | — | Keep `MsgSnapshot`, `MsgDelta`, `MsgSourceHealth`, `MsgCompletion`. **Drop** `MsgOvertake`, `MsgAchievementUnlocked`, `MsgBattlePassProgress`, `MsgEquipped`. |
| `backend/internal/ws/ratelimit.go` | `transport/wsapi/ratelimit.go` | — | Connection rate limiting. Generic. |
| Deadlock detection test pattern from `backend/internal/monitor/monitor_test.go` (~lines 14–32) | `monitor/monitor_test.go` | ~20 | Pattern is gold; reuse for the new orchestration. |
| Activity enum and JSON marshalling from `backend/internal/session/state.go` lines 9–69 | `session/state.go` | 60 | The `Activity` type and its String/MarshalJSON/UnmarshalJSON implementations are clean. |
| `clone()` / `Clone()` deep-copy methods from `backend/internal/session/state.go` lines 125–148 | `session/state.go` | 24 | Deep-copy discipline that downstream code relies on. |
| Source-specific JSONL parser internals from `backend/internal/monitor/claude_source.go`, `codex_source.go`, `gemini_source.go` | `sources/<name>/parser.go` | — | **Keep the parsing knowledge** (which fields map to what, token accounting, model detection). **Discard the discovery boilerplate** — that becomes `filewatch.Watcher` (§5). |
| Source-specific test fixtures under `backend/internal/monitor/*_test.go` | `sources/<name>/parser_test.go` | — | Fixtures encode hard-won format knowledge. Migrate them. |
| `backend/internal/session/privacy.go` (if it does PII redaction) | `session/privacy.go` | — | Investigate first (§6); copy if generic. |

### Git provenance

When copying, preserve git history where possible:

```bash
# from the new repo:
git remote add agent-racer <agent-racer-url>
git fetch agent-racer claude/backend-library-abstraction-UvaD5
# then for each file, use `git show agent-racer/<branch>:<path>` to extract
# (do NOT merge — the package paths and structure are different)
```

A short commit message convention: `lift: copy <thing> from agent-racer@<sha>` so provenance is greppable.

---

## 4. What to Rewrite (Do Not Copy)

| agent-racer artifact | Why rewrite | Replacement strategy |
|---|---|---|
| `backend/internal/monitor/monitor.go` (1449 lines) | Monolithic. `pollSource` ~198 lines with deep nesting; `poll` ~176 lines mixing discovery, lifecycle, position computation, broadcasting. | Three small methods: `discover()`, `tick()`, `prune()`. Lifecycle transitions live in `session.Lifecycle` state machine. Target: ≤ 150 LOC for `monitor.go`. |
| `SessionState` in `backend/internal/session/state.go` (31 fields) | God struct mixing domain, wire protocol, racer UI state. | Slim to ~15 generic fields (§5). Strip `Lane`, `Position`, `PositionDelta`. |
| `backend/internal/ws/server.go` (866 lines) | Constructor imports `gamification`, `tracks`, `replay`. Transport reaches into game logic. | New `wsapi.NewServer(monitor, opts...)` with zero domain imports. Auth becomes an interface. |
| `backend/internal/ws/broadcast.go` | Tightly coupled to monitor's broadcast queue and racer event types. | New `wsapi.Broadcaster` driven by the generic `monitor.EventSink`. |
| Hardcoded `buildSources` in `backend/cmd/server/main.go` | Sources are statically registered; consumers can't add their own. | `source.Registry` with `Register(name, factory)` and `Get(name)`. Built-in sources self-register via `init()` in `sources/<name>/`. |
| Magic constants scattered through monitor.go (`maxEndMarkerFileSize`, `burnRateWindow`, `maxTokenSnapshots`, etc.) | Hardcoded; impossible to tune per consumer. | `monitor.Config` struct with sensible defaults; functional options to override. |
| Codex / Gemini `Discover()` boilerplate (~150 LOC each, near-identical) | Duplicated logic for walking a root, matching session IDs, tracking mtimes. | `filewatch.Watcher` composes the discovery; sources only declare root + ID pattern + per-file parser (§5). |
| XDG path defaults in `backend/internal/config/` | Library shouldn't assume any filesystem layout. | All paths via constructor options. The *example app* uses XDG; the library doesn't. |

---

## 5. Net-New Components

### 5.1 `source.Source` (refined contract)

```go
package source

type Source interface {
    Name() string
    Discover(ctx context.Context) ([]SessionHandle, error)
    Parse(ctx context.Context, h SessionHandle, offset int64) (SourceUpdate, int64, error)
}

type SessionHandle struct {
    ID         string
    Path       string
    WorkingDir string
    StartedAt  time.Time
    KnownSlugs map[string]string // for incremental parsing continuity
}

type SourceUpdate struct {
    Activity     session.Activity
    Model        string
    TokensUsed   int
    MaxContext   int
    CurrentTool  string
    Subagents    []session.SubagentState
    Terminal     bool
    EndReason    string
    // ... only generic fields; no Lane/Position
}
```

**Differences from agent-racer:**
- `context.Context` added to both methods (cancellation, timeouts).
- No `Close()`; sources are stateless or own their lifecycle.

### 5.2 `source.Registry`

```go
package source

type Factory func(opts ...Option) (Source, error)

type Registry struct { /* ... */ }

func (r *Registry) Register(name string, f Factory)
func (r *Registry) Get(name string) (Factory, bool)
func (r *Registry) Names() []string

var Default = NewRegistry()
```

Built-in sources self-register:
```go
// sources/claude/source.go
func init() { source.Default.Register("claude", New) }
```

### 5.3 `source/filewatch.Watcher`

Captures the discovery boilerplate that codex_source.go and gemini_source.go duplicate today.

```go
package filewatch

type Watcher struct {
    Roots          []string                       // e.g., ~/.claude/projects, $CODEX_HOME/sessions
    SessionPattern *regexp.Regexp                 // extracts session ID from filename
    Filter         func(path string, info fs.FileInfo) bool // optional skip predicate
    ModifiedSince  time.Duration                  // ignore stale files
}

func (w *Watcher) Discover(ctx context.Context) ([]source.SessionHandle, error)
```

Each source becomes ~30 LOC instead of ~150.

### 5.4 `session.SessionState` (slimmed)

```go
package session

type SessionState struct {
    // Identity
    ID         string    `json:"id"`
    Source     string    `json:"source"`
    Slug       string    `json:"slug,omitempty"`

    // Status
    Activity   Activity  `json:"activity"`
    Lifecycle  Lifecycle `json:"lifecycle"` // NEW

    // Token accounting
    TokensUsed         int     `json:"tokensUsed"`
    TokenEstimated     bool    `json:"tokenEstimated"`
    MaxContextTokens   int     `json:"maxContextTokens"`
    ContextUtilization float64 `json:"contextUtilization"`

    // Context
    Model       string `json:"model"`
    WorkingDir  string `json:"workingDir"`
    Branch      string `json:"branch,omitempty"`
    CurrentTool string `json:"currentTool,omitempty"`

    // Activity counters
    MessageCount  int `json:"messageCount"`
    ToolCallCount int `json:"toolCallCount"`

    // Timing
    StartedAt          time.Time  `json:"startedAt"`
    LastActivityAt     time.Time  `json:"lastActivityAt"`
    LastDataReceivedAt time.Time  `json:"lastDataReceivedAt"`
    CompletedAt        *time.Time `json:"completedAt,omitempty"`

    // Composition
    Subagents []SubagentState `json:"subagents,omitempty"`

    // Internal
    LogPath string `json:"-"`
}
```

**Removed from agent-racer's SessionState:**
- `Lane` — racer-specific. Consumer maintains lane assignment in their `EventSink`.
- `Position`, `PositionDelta` — racer-specific. Same.
- `Name` — display logic; consumer derives from `Slug`/`WorkingDir`.
- `IsChurning`, `BurnRatePerMinute`, `CompactionCount` — racer-derived metrics. **Decision needed (file as beads issue):** are these generally useful or racer-specific? Default: leave out of v1, add via consumer wrapper if needed.
- `TmuxTarget`, `PID` — host-process awareness; useful but couples library to OS process model. **Decision needed:** include behind a `monitor.WithProcessAwareness()` option, or leave to consumer.
- `LastAssistantText` — preview text; potentially privacy-sensitive. Leave out of v1.

### 5.5 `session.Lifecycle` (explicit state machine)

```go
package session

type Lifecycle int

const (
    LifecycleNew Lifecycle = iota
    LifecycleActive
    LifecycleResumed
    LifecycleTerminal
)

type LifecycleEvent struct {
    From, To Lifecycle
    SessionID string
    At        time.Time
    Reason    string
}
```

Replaces the implicit lifecycle scattered across `pollSource`'s `tracked`/`removedKeys`/store-state in agent-racer.

### 5.6 `monitor.EventSink`

```go
package monitor

type EventSink interface {
    OnUpdate(updates []session.SessionState)
    OnLifecycle(ev session.LifecycleEvent)
    OnHealth(source string, h Health)
}

// MultiSink fans out to multiple sinks.
type MultiSink []EventSink
```

**This is the extension point that makes racing, gamification, and any other domain logic possible without polluting the library.** Agent-racer's lane/position/overtake/achievement code becomes a `racer.Sink` that lives in the agent-racer repo.

### 5.7 `monitor.New` (functional options)

```go
package monitor

func New(opts ...Option) *Monitor

// Options
func WithSources(sources ...source.Source) Option
func WithPollInterval(d time.Duration) Option
func WithSink(sink EventSink) Option
func WithStateDir(path string) Option           // optional state persistence dir
func WithLogger(l *slog.Logger) Option
func WithClock(c Clock) Option                  // for tests
func WithMaxEndMarkerSize(bytes int) Option     // tunable from formerly-magic constants
```

No XDG assumptions. No global state. Test-friendly via `WithClock`.

### 5.8 `transport/wsapi.NewServer`

```go
package wsapi

func NewServer(mon *monitor.Monitor, opts ...Option) *Server

func WithAuthenticator(a Authenticator) Option   // pluggable; default = no auth
func WithAllowedOrigins(origins []string) Option
func WithMaxConnections(n int) Option
func WithHeartbeat(d time.Duration) Option

type Authenticator interface {
    Authenticate(r *http.Request) (identity string, ok bool)
}
```

Zero imports of game-logic packages. Subscribes to `monitor` via an internal `EventSink` adapter.

### 5.9 `transport/httpapi.Handler`

```go
package httpapi

func NewHandler(mon *monitor.Monitor, opts ...Option) http.Handler
```

Routes:
- `GET /sessions` — list
- `GET /sessions/{id}` — detail
- `GET /healthz` — liveness
- `GET /sources` — registered sources + health

No racer routes (`/tracks`, `/achievements`, `/equip`, etc.) — those live in agent-racer.

---

## 6. Files in agent-racer to Investigate Before Copying

These need a quick read to decide copy/rewrite/skip:

| Path | Question |
|---|---|
| `backend/internal/jsonl/jsonl.go` vs `backend/internal/monitor/jsonl.go` | Two parsers exist. Which is canonical? Are they different by design or accidental duplication? Pick one. |
| `backend/internal/session/privacy.go` | Generic PII redaction → copy. Racer-specific filter → skip. |
| `backend/internal/session/teams.go` | What is "teams"? If it's racer-specific multi-session grouping, skip. If it's generic session tagging, consider for v2. |
| `backend/internal/session/event.go` | Event types. May overlap with new `monitor.EventSink`. Reconcile. |
| `backend/internal/monitor/process.go` and `process_test.go` | Process detection. Useful? Racer-specific? Lean toward including behind an option (§5.4). |
| `backend/internal/monitor/tmux.go`, `tmux_linux.go`, `tmux_other.go` | Tmux integration for surfacing the running shell. Useful for any TUI/dashboard, but Linux-only and host-coupled. Decide: include as a separate `agentwatch/integrations/tmux` sub-package, or leave in agent-racer. |
| `backend/internal/monitor/health.go` | Source health tracking. Almost certainly generic — copy. |
| `backend/internal/ws/broadcast_*_test.go` | Broadcaster test patterns (slow client, conn limit, write pump). Lift the test approaches even if rewriting the broadcaster itself. |

File a beads issue per row in the new repo.

---

## 7. Build Phases

Each phase becomes one or more beads issues in the new repo. Phases are sequential; later phases depend on earlier ones.

### Phase 1 — Primitives (target: 1 week)

**Goal:** copy the library-grade parts and prove they still work in isolation.

- [ ] Initialize new repo, `go.mod`, CI skeleton (lint + test on PR)
- [ ] Copy `source/source.go` (with context added)
- [ ] Copy `jsonl/parse.go` (resolve duplicate-jsonl question first)
- [ ] Copy `session/store.go` with concurrency tests
- [ ] Copy `session/tail.go` with tests
- [ ] Copy validation logic into `monitor/validate.go` with tests
- [ ] Verify all copied tests pass

**Exit criteria:** `go test ./...` green; deadlock-detection test runs.

### Phase 2 — Source Plumbing (target: 1 week)

**Goal:** unify discovery, port the three reference sources.

- [ ] Build `source/registry.go`
- [ ] Build `source/filewatch/watcher.go` with tests
- [ ] Port `sources/claude/` using filewatch (target: ≤ 80 LOC excl. tests)
- [ ] Port `sources/codex/`
- [ ] Port `sources/gemini/`
- [ ] Build `sources/mock/` for downstream tests/demos
- [ ] Migrate fixture files

**Exit criteria:** each source's existing parser tests pass against ported implementation. Discovery boilerplate is in `filewatch` only, not duplicated.

### Phase 3 — Orchestration (target: 1 week)

**Goal:** the rewritten monitor loop with explicit lifecycle and event sink.

- [ ] Build `session/state.go` (slim) and `session/lifecycle.go`
- [ ] Build `monitor/events.go` (`EventSink`, `Health`, event types)
- [ ] Build `monitor/options.go` (functional options)
- [ ] Build `monitor/monitor.go` — rewritten poll loop
- [ ] Port deadlock-detection test
- [ ] Add new tests: lifecycle transitions, sink fan-out, source-health tracking
- [ ] Build `examples/stdout/` to validate end-to-end

**Exit criteria:** `monitor.go` ≤ 150 LOC. `examples/stdout` runs against a real `~/.claude/projects/` directory and prints session updates.

### Phase 4 — Transport Adapters (target: 1 week)

**Goal:** optional HTTP and WS layers consumers can drop in.

- [ ] Build `transport/wsapi/messages.go` (snapshot/delta envelope)
- [ ] Build `transport/wsapi/broadcaster.go` (subscribes to `monitor.EventSink`)
- [ ] Build `transport/wsapi/server.go` with pluggable `Authenticator`
- [ ] Port slow-client / conn-limit / write-pump test patterns
- [ ] Build `transport/httpapi/handler.go`
- [ ] Build `examples/httpserver/` — full minimal server, < 100 LOC of glue

**Exit criteria:** `examples/httpserver` runs; a `curl` to `/sessions` and a `wscat` to `/ws` both work; auth interface is exercised by an example token authenticator.

### Phase 5 — agent-racer Migration (target: 1–2 weeks)

**Goal:** prove the abstraction by making agent-racer a consumer.

- [ ] Add `agentwatch` as a dependency in agent-racer's `backend/go.mod` (replace directive during dev)
- [ ] Delete agent-racer's `backend/internal/monitor/source.go`, `jsonl/`, `session/store.go`, etc. — replace with imports
- [ ] Build `agent-racer/backend/internal/racer/sink.go` implementing `monitor.EventSink`. Owns:
  - Lane assignment
  - Position computation
  - Overtake event detection
  - Gamification fan-out (achievements, battle pass, XP)
- [ ] Move racer-specific WS message types to agent-racer's own broadcaster, layered on top of `wsapi`
- [ ] Keep `gamification/`, `tracks/`, `replay/` in agent-racer
- [ ] Verify full `make ci` (backend + frontend) passes
- [ ] Verify the frontend works end-to-end in the browser

**Exit criteria:** agent-racer is a thin consumer of `agentwatch`. All racer-specific code lives in agent-racer. If any concept can't be expressed via `EventSink`, the library design has a flaw — pause and revise before publishing.

### Phase 6 — Publish (target: 0.5 week)

- [ ] Tag `v0.1.0`
- [ ] Write `README.md` with quickstart
- [ ] Write `docs/architecture.md`, `docs/source-implementation.md`, `docs/migration-from-agent-racer.md`
- [ ] Decide name (Pitwall is taken; Agentry is workable; explore Paddock / Manifold / others — see §10)
- [ ] Pick license (MIT, matching agent-racer)
- [ ] Submit to `pkg.go.dev`

---

## 8. Validation Gates

Apply at the end of each phase:

```bash
go test ./...
golangci-lint run
go vet ./...
go test -race ./...   # critical for store and broadcaster
```

The deadlock-detection test pattern from agent-racer's `monitor_test.go` lines 14–32 must be ported and pass for every concurrency-sensitive package.

---

## 9. Hard Design Rules

These are non-negotiable; any PR that violates them should be blocked.

1. **No `internal/` directory.** Every package is importable.
2. **No game/UI concepts in core packages.** No `Lane`, `Position`, `Overtake`, `Achievement`, `Track`, etc. anywhere under `monitor/`, `session/`, `source/`, `transport/`, `jsonl/`.
3. **No filesystem-path defaults baked into the library.** All paths via options.
4. **No global state** except `source.Default` registry (and even that is opt-in).
5. **`context.Context` on every blocking method.**
6. **Functional options** for all multi-arg constructors.
7. **Traditional `for i := 0; i < n; i++` loops** — no range-over-integer (matching agent-racer's CLAUDE.md rule for compatibility).
8. **Slim public API.** If a type doesn't need to be exported, don't export it. Easier to expand than retract.

---

## 10. Open Questions

File these as beads issues before implementation starts:

1. **Library name.** `Pitwall` is taken (heavy F1 sim-racing collisions, plus a security platform). `Agentry` is workable but conflicts with SAP's legacy product and `agentry.io` (AI receptionist). Candidates to evaluate next: `Paddock`, `Manifold`, `Switchboard`, `Atrium`, coined names.
2. **Process awareness scope.** Is `PID` / `TmuxTarget` core-library territory or an integration sub-package?
3. **Burn-rate / churn / compaction-count metrics.** Generally useful or racer-specific? If keep, they belong in `monitor.SessionState` with derivation logic in `monitor`.
4. **Replay file format.** Out of v1; but design `EventSink` so a replay recorder can be implemented as a sink without library changes.
5. **JSONL parser canonicalization.** Two exist in agent-racer (`internal/jsonl/jsonl.go` and `internal/monitor/jsonl.go`). Determine which is authoritative before copying.
6. **`Authenticator` interface shape.** Does it return identity (multi-tenant ready) or just bool? Lean toward returning identity for forward-compat; document that the v1 reference impl ignores it.
7. **Tmux integration packaging.** Separate `agentwatch/integrations/tmux` module, baked into core, or leave in agent-racer?

---

## 11. Quick-Reference: Decision Matrix

| Component in agent-racer | Decision | Destination |
|---|---|---|
| `monitor.Source` interface | Copy | `source/source.go` |
| `jsonl/jsonl.go` (one of two) | Copy after canonicalization | `jsonl/parse.go` |
| `session.Store` | Copy concurrency pattern | `session/store.go` |
| `session.SessionState` | Rewrite (slimmed) | `session/state.go` |
| `session.Activity` enum | Copy | `session/state.go` |
| `session.tail.go` | Copy | `session/tail.go` |
| `session.privacy.go` | Investigate | `session/privacy.go` (maybe) |
| `session.teams.go` | Likely skip (racer-specific?) | — |
| `session.event.go` | Reconcile with new EventSink | — |
| End-marker validation | Copy | `monitor/validate.go` |
| `monitor.Monitor` (poll loop) | Rewrite | `monitor/monitor.go` |
| `monitor.health.go` | Copy | `monitor/health.go` |
| `monitor.process.go` | Investigate; behind option | `monitor/process.go` (opt-in) |
| `monitor.tmux*.go` | Investigate; integration sub-package | `integrations/tmux/` (maybe) |
| `claude_source.go` discovery | Rewrite via filewatch | `sources/claude/source.go` |
| `claude_source.go` parser | Copy | `sources/claude/parser.go` |
| `codex_source.go` (same) | Same | `sources/codex/` |
| `gemini_source.go` (same) | Same | `sources/gemini/` |
| `ws/protocol.go` | Copy generic msgs only | `transport/wsapi/messages.go` |
| `ws/broadcast.go` | Rewrite | `transport/wsapi/broadcaster.go` |
| `ws/server.go` | Rewrite | `transport/wsapi/server.go` |
| `ws/ratelimit.go` | Copy | `transport/wsapi/ratelimit.go` |
| `ws/broadcast_*_test.go` patterns | Lift patterns | new tests |
| `cmd/server/main.go` | Rewrite as `examples/httpserver` | `examples/httpserver/main.go` |
| `gamification/*` | Stays in agent-racer | — |
| `tracks/*` | Stays in agent-racer | — |
| `replay/*` | Stays in agent-racer (v1) | — |
| `frontend/*` | Stays in agent-racer | — |
| `config/*` | Rewrite per-consumer | — |

---

## 12. First Three Beads Issues to File in the New Repo

1. **`bd-1`: Bootstrap repo and CI**
   Initialize `go.mod`, set up GitHub Actions for `go test`, `go vet`, `golangci-lint`, `go test -race`. License + minimal README. **No code yet.**

2. **`bd-2`: Resolve JSONL parser duplication (Open Question §10.5)**
   Read both `backend/internal/jsonl/jsonl.go` and `backend/internal/monitor/jsonl.go` in agent-racer. Document the difference. Decide which is canonical. Output: short ADR in `docs/decisions/`.

3. **`bd-3`: Phase 1 — copy primitives**
   Execute Phase 1 of §7. Single PR; passes `go test -race ./...`.

All later phases are filed once Phase 1 lands.

---

## Appendix A — Why "partial rewrite" not "extract"

Surveys of the agent-racer backend (this branch, prior turns) found:

- **Library-quality:** `Source` interface, JSONL parsing, `Store.UpdateAndNotify` concurrency pattern, end-marker validation, deadlock-detection tests.
- **App-quality:** 1449-line `monitor.go` mixing 5–6 concerns; 31-field `SessionState` god struct with racer fields; 866-line `ws/server.go` importing game-logic packages; duplicated discovery boilerplate across three sources; magic constants scattered without a central `Config`.

Refactor cost ~60–80h (and you still carry racer DNA in every seam). Rewrite-using-as-reference cost ~40–60h with a smaller, sharper end product. The interfaces are right; the implementation is opinionated for one app. Keep the contracts, rebuild the flesh.
