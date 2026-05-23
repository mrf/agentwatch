# ADR 0006: Process and Tmux Awareness API

**Status:** Proposed (v0.2 scope)

## Context

Agent-racer's integration surfaced two OS-level features that are generally
useful for consumers monitoring local AI agents:

1. **Process discovery** — scanning running agent processes to get PID, CPU
   activity state, and TCP connection counts. Agent-racer uses this to detect
   live sessions that have not yet written JSONL (pre-first-message), and to
   supplement file-based activity detection with OS-level liveness.

2. **Tmux resolution** — mapping agent PIDs to tmux pane targets via process
   tree walking. Agent-racer uses this so its UI can display which tmux pane
   holds each agent session, enabling click-to-focus.

Both features are currently implemented as post-processing on top of
`EventSink` in agent-racer. They are explicitly excluded from v0.1 core state
by the plan (§4.4: "PID, TmuxTarget — host-process awareness. Put in an
integration package or consumer sink, not v1 core") and by hard rule §12.4
("No prompt text, assistant text, tool output, raw log path, PID, or tmux
target in public v1 state").

The Gemini source already contains a precedent: `sources/gemini/procmap.go`
implements /proc-based process scanning to map hash-named session directories
to working directories. This is source-private and Linux-only.

### Design constraints from the plan

- No PID or TmuxTarget in `session.SessionState` (§4.4, §12.4).
- No game/UI concepts in core packages (§12.1).
- No global registration as the primary API (§12.11).
- Public packages are stable extension points; implementation starts internal
  until a real consumer demonstrates need (§2, ADR 0002).
- `context.Context` on every blocking method (§12.5).
- Functional options for multi-argument constructors (§12.9).
- Health error strings must be sanitized (§12.10).

### Key questions this ADR resolves

1. Whether gopsutil should be a direct or optional dependency.
2. Cross-platform strategy for process and tmux features.
3. How consumers attach process data to sessions without coupling core state
   to PID/TmuxTarget fields.
4. What stays in agent-racer vs. moves to agentwatch.

## Decision

### 1. Package structure

Introduce two new **public** packages under an `integrations/` directory:

```text
integrations/
├── process/        # Process discovery and liveness
│   ├── process.go
│   ├── scanner.go
│   ├── scanner_linux.go
│   ├── scanner_stub.go
│   └── process_test.go
└── tmux/           # Tmux pane resolution
    ├── tmux.go
    ├── resolver.go
    ├── resolver_linux.go
    ├── resolver_stub.go
    └── tmux_test.go
```

These are public because agent-racer is a concrete consumer today, and any
dashboard or TUI that monitors local agents has the same need. The packages
are opt-in imports — consumers who don't need them pay no dependency cost.

The `integrations/` prefix signals "useful but OS-coupled" without polluting
the top-level package namespace. It also provides a natural home for future
OS-level integrations (e.g., `integrations/systemd`, `integrations/docker`).

### 2. API surface

#### `integrations/process`

```go
package process

import (
    "context"
    "time"
)

// Info holds OS-level metadata for a discovered agent process.
type Info struct {
    PID           int       `json:"pid"`
    Name          string    `json:"name"`
    Cmdline       []string  `json:"cmdline,omitempty"`
    WorkingDir    string    `json:"workingDir,omitempty"`
    CPUActive     bool      `json:"cpuActive"`
    TCPConnCount  int       `json:"tcpConnCount"`
    ParentPID     int       `json:"parentPid,omitempty"`
    StartedAt     time.Time `json:"startedAt,omitempty"`
}

// Matcher identifies whether a running process belongs to a known agent
// type. Consumers register matchers for each agent they want to discover.
type Matcher func(pid int, name string, cmdline []string) (agentType string, match bool)

// Scanner discovers running agent processes on the local host.
type Scanner struct { /* unexported fields */ }

// NewScanner creates a process scanner with the given options.
func NewScanner(opts ...Option) (*Scanner, error)

// Scan returns Info for all currently running processes that match any
// registered Matcher. It is safe to call concurrently.
func (s *Scanner) Scan(ctx context.Context) ([]Info, error)

// Option configures a Scanner.
type Option func(*config)

// WithMatchers registers one or more process matchers.
func WithMatchers(ms ...Matcher) Option

// WithProcRoot overrides the /proc path for testing.
// Defaults to "/proc" on Linux.
func WithProcRoot(path string) Option

// ClaudeMatcher returns a Matcher that identifies Claude Code processes.
func ClaudeMatcher() Matcher

// CodexMatcher returns a Matcher that identifies OpenAI Codex CLI processes.
func CodexMatcher() Matcher

// GeminiMatcher returns a Matcher that identifies Gemini CLI processes.
func GeminiMatcher() Matcher
```

#### `integrations/tmux`

```go
package tmux

import (
    "context"
)

// Pane identifies a tmux pane that hosts an agent process.
type Pane struct {
    SessionName string `json:"sessionName"`
    WindowIndex int    `json:"windowIndex"`
    WindowName  string `json:"windowName,omitempty"`
    PaneIndex   int    `json:"paneIndex"`
    Target      string `json:"target"` // e.g. "main:2.0"
}

// Resolver maps PIDs to the tmux panes that contain them.
type Resolver struct { /* unexported fields */ }

// NewResolver creates a tmux resolver.
func NewResolver(opts ...Option) (*Resolver, error)

// Resolve returns the Pane for the given PID by walking the process tree
// upward until it finds a process owned by a tmux server. Returns
// (zero Pane, false) if the PID is not inside a tmux session or tmux is
// not available.
func (r *Resolver) Resolve(ctx context.Context, pid int) (Pane, bool)

// ResolveAll maps a batch of PIDs to panes. PIDs that cannot be resolved
// are omitted from the result.
func (r *Resolver) ResolveAll(ctx context.Context, pids []int) map[int]Pane

// Option configures a Resolver.
type Option func(*config)

// WithTmuxBinary overrides the path to the tmux binary.
// Defaults to "tmux" (found via PATH).
func WithTmuxBinary(path string) Option
```

### 3. Integration pattern: the enrichment sink

Process and tmux data flows to consumers through a **decorator EventSink**
that enriches monitor events with process metadata, rather than adding fields
to `session.SessionState`. This preserves the plan's hard rule against PID
and TmuxTarget in core state.

```go
package process

import (
    "context"

    "github.com/mrf/agentwatch/integrations/tmux"
    "github.com/mrf/agentwatch/monitor"
    "github.com/mrf/agentwatch/session"
)

// SessionProcess associates a session with its process-level metadata.
type SessionProcess struct {
    SessionID  string    `json:"sessionId"`
    Process    *Info     `json:"process,omitempty"`
    TmuxPane   *tmux.Pane `json:"tmuxPane,omitempty"`
}

// EnrichedEvent wraps a monitor.Event with process-level metadata for
// each session mentioned in the event.
type EnrichedEvent struct {
    monitor.Event
    Processes []SessionProcess `json:"processes,omitempty"`
}

// EnrichmentSink wraps an inner sink and enriches events with process
// and (optionally) tmux metadata before forwarding.
type EnrichmentSink struct { /* unexported fields */ }

// NewEnrichmentSink creates a sink that enriches events before forwarding
// to inner. The scanner is required; the tmux resolver is optional.
func NewEnrichmentSink(inner monitor.EventSink, scanner *Scanner, opts ...EnrichmentOption) *EnrichmentSink

// HandleEvent implements monitor.EventSink.
func (e *EnrichmentSink) HandleEvent(ctx context.Context, ev monitor.Event) error

// EnrichmentOption configures an EnrichmentSink.
type EnrichmentOption func(*enrichmentConfig)

// WithTmuxResolver enables tmux pane resolution for enriched events.
func WithTmuxResolver(r *tmux.Resolver) EnrichmentOption

// WithMatchStrategy controls how sessions are matched to processes.
// Default: match by working directory.
func WithMatchStrategy(s MatchStrategy) EnrichmentOption

// MatchStrategy determines how a session.SessionState is matched to a
// running process.
type MatchStrategy int

const (
    // MatchByWorkingDir matches sessions to processes by comparing
    // SessionState.WorkingDir to Info.WorkingDir.
    MatchByWorkingDir MatchStrategy = iota

    // MatchBySessionID matches sessions to processes by looking for the
    // session ID in the process command line arguments.
    MatchBySessionID
)
```

#### Consumer wiring example

```go
// In agent-racer or any consumer:
scanner, _ := process.NewScanner(
    process.WithMatchers(
        process.ClaudeMatcher(),
        process.CodexMatcher(),
    ),
)
tmuxResolver, _ := tmux.NewResolver()

enrichedSink := process.NewEnrichmentSink(
    myAppSink,
    scanner,
    process.WithTmuxResolver(tmuxResolver),
)

mon, _ := monitor.New(
    monitor.WithSources(claudeSource, codexSource),
    monitor.WithSink(enrichedSink),
)
```

The consumer's `myAppSink` receives `monitor.Event` (the interface contract).
The `EnrichmentSink` calls `scanner.Scan()` on each event, matches processes
to sessions, optionally resolves tmux panes, and forwards an `EnrichedEvent`
to the inner sink. The inner sink can type-assert to `EnrichedEvent` if it
wants the process data, or ignore it and use the base `Event` fields.

#### Alternative considered: monitor option

A `monitor.WithProcessAwareness()` option was considered. This was rejected
because:

- It would couple the monitor to OS-level dependencies.
- It would require adding process fields to `session.SessionState` or a
  parallel state map inside the monitor, violating §4.4 and §12.4.
- The sink pattern is more composable: consumers can choose which events to
  enrich, rate-limit scanning, or cache results.

#### Alternative considered: standalone poller

A separate `process.Poller` that runs independently and exposes a
`Snapshot()` method (similar to the monitor) was considered. This would
require consumers to correlate two independent data sources. The enrichment
sink is simpler because it delivers correlated data in a single event stream.

The standalone poller may still be useful as a building block for consumers
who want process data outside the event flow — the `Scanner` type serves
this role directly.

### 4. Dependency strategy

**No gopsutil dependency.** Use `/proc` directly on Linux (the only platform
where these features are practical today).

Rationale:

- The Gemini source (`sources/gemini/procmap.go`) already demonstrates that
  `/proc`-based scanning is sufficient and straightforward for the use cases
  here (PID, cmdline, cwd, process tree).
- gopsutil is a large dependency tree (~15 transitive deps) that would
  inflate `go.sum` for all consumers, even those who don't use process
  features. As a library, agentwatch should minimize its dependency footprint.
- The process inspection needed here is narrow: enumerate PIDs, read cmdline,
  read cwd, read stat for CPU state, read net/tcp for connection counts, and
  walk parent PID chains. This is ~200 lines of `/proc` parsing, well within
  the scope of a focused implementation.
- CPU "active" detection only needs coarse liveness (has the process consumed
  CPU recently?), not precise utilization metrics. Reading `/proc/<pid>/stat`
  for utime+stime deltas between scans is sufficient.

If a future platform (macOS, Windows) needs support and `/proc` is not
available, a platform-specific implementation using `sysctl` or WMI can be
added behind build tags without changing the public API. gopsutil can be
reconsidered at that point as a platform abstraction, but it is not justified
for Linux-only v0.2.

### 5. Cross-platform plan

| Platform | Process scanning | Tmux resolution |
|----------|-----------------|-----------------|
| Linux    | Full support via `/proc` | Full support via `tmux list-panes` + `/proc` tree walk |
| macOS    | Stub (returns empty) | Stub (returns empty) |
| Windows  | Stub (returns empty) | Stub (returns empty) |

Build tags control compilation:

```go
// scanner_linux.go
//go:build linux

// scanner_stub.go
//go:build !linux
```

Stub files return `nil, nil` (no processes found) rather than an error. This
allows consumers to unconditionally wire up process enrichment without
platform-gating their code. The enrichment sink simply produces no process
data on unsupported platforms.

macOS support is the most likely future addition. It would use `sysctl` /
`libproc` for process enumeration and `proc_pidpath` for executable paths.
The tmux resolver would work on macOS once process scanning is available,
since tmux itself is cross-platform and `tmux list-panes` works the same
way. This can be a separate ADR when a macOS consumer materializes.

### 6. What stays in agent-racer vs. moves to agentwatch

| Capability | Location | Rationale |
|-----------|----------|-----------|
| Process scanning (PID enumeration, cmdline, cwd, CPU, TCP) | **agentwatch** `integrations/process` | Generic; any local agent monitor needs this |
| Process matchers for claude/codex/gemini | **agentwatch** `integrations/process` | Source-aligned; belong with the reference sources |
| Tmux pane resolution (PID → tmux target) | **agentwatch** `integrations/tmux` | Generic; any tmux-based agent workflow needs this |
| Enrichment sink (decorate events with process + tmux data) | **agentwatch** `integrations/process` | The glue between monitor events and OS data |
| PID/TmuxTarget fields on racer session state | **agent-racer** consumer sink | Racer-specific state derived from enriched events |
| Click-to-focus UI behavior | **agent-racer** frontend | UI concern |
| Pre-first-message session detection via process scan | **agent-racer** consumer logic | Application-level policy about when to show sessions |

### 7. Gemini procmap consolidation

The existing `sources/gemini/procmap.go` performs process scanning for a
source-specific purpose (hash → working directory mapping). In v0.2, this
should be refactored to use `integrations/process.Scanner` internally,
eliminating the duplicated `/proc` parsing. The `Scanner` would be an
optional dependency of the Gemini source, passed via a `WithScanner` option.
If no scanner is provided, the Gemini source falls back to its current
self-contained `/proc` scan, preserving backward compatibility.

### 8. Privacy considerations

Process-level data is more sensitive than file-based session data:

- **PIDs** are ephemeral but can be correlated with system logs.
- **Command lines** may contain flags, paths, or tokens.
- **Working directories** are already covered by `session.Policy.RedactWorkingDir`.

The enrichment sink should respect a configurable policy. By default,
enriched events should:

- Include PID (needed for tmux resolution and consumer correlation).
- Include agent type from matcher (e.g., "claude") but not raw cmdline.
- Include tmux target string (e.g., "main:2.0") but not full tmux session
  metadata.

A `WithFullCmdline(true)` enrichment option can enable cmdline inclusion for
debugging use cases. Cmdline is omitted by default.

### 9. Performance considerations

Process scanning reads `/proc` for every running process on the host.
On a developer machine with ~500 processes, this takes <10ms. The
enrichment sink should:

- **Cache scan results** for a configurable TTL (default: same as monitor
  poll interval) to avoid redundant `/proc` walks within a single poll
  cycle.
- **Scan lazily**: only scan when an event contains session updates, not on
  health-only or lifecycle-only events.
- **Batch tmux resolution**: `ResolveAll` amortizes the cost of `tmux
  list-panes` (one subprocess call) across all PIDs in a batch.

## Consequences

- The `integrations/` package tree is a new public surface area. Its API
  stability follows the same rules as other public packages (ADR 0002).
- Consumers who import `integrations/process` or `integrations/tmux` get
  Linux-only functionality. This is clearly documented and the stubs ensure
  no build failures on other platforms.
- The monitor core remains free of OS-level coupling. Process awareness is
  entirely opt-in through sink composition.
- `go.mod` gains no new external dependencies for process/tmux support.
- Agent-racer can migrate its process/tmux code to agentwatch imports,
  reducing its internal implementation burden.
- The Gemini source's `procmap.go` can be simplified in a follow-up but is
  not blocked by this ADR.
- Future OS platform support requires only new build-tagged files, not API
  changes.
