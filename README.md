# agentwatch

[![Go Report Card](https://goreportcard.com/badge/github.com/mrf/agentwatch)](https://goreportcard.com/report/github.com/mrf/agentwatch)
[![Go Reference](https://pkg.go.dev/badge/github.com/mrf/agentwatch.svg)](https://pkg.go.dev/github.com/mrf/agentwatch)

A Go library for monitoring multiple local AI coding agents — Claude Code, OpenAI Codex CLI, Gemini CLI, and anything you can write a `Source` for.

## Overview

`agentwatch` observes local agent session artifacts, aggregates session state, and exposes the result through:

- A **pull API**: `Monitor.Snapshot()` for synchronous consumers.
- A **push API**: `monitor.EventSink` for reactive consumers.
- Optional **HTTP and WebSocket transport adapters** for remote or web frontends.

The library is UI-agnostic. Concepts like racing lanes, leaderboards, and achievements are out of scope — consumers layer those on top.

## Status

Pre-release. API is not yet stable.

## Installation

```
go get github.com/mrf/agentwatch
```

Requires Go 1.25+.

## Quickstart

### 1. Create a source

Sources tell the monitor where to find agent sessions. The built-in Claude source scans a Claude projects directory:

```go
import "github.com/mrf/agentwatch/sources/claude"

src, err := claude.New(claude.WithRoot("/home/user/.claude/projects"))
if err != nil {
    log.Fatal(err)
}
```

### 2. Create a monitor

```go
import "github.com/mrf/agentwatch/monitor"

mon, err := monitor.New(
    monitor.WithSources(src),
    monitor.WithPollInterval(2 * time.Second),
)
if err != nil {
    log.Fatal(err)
}
```

### 3. Poll once and read the snapshot

```go
ctx := context.Background()
if err := mon.PollOnce(ctx); err != nil {
    log.Fatal(err)
}

for _, s := range mon.Snapshot() {
    fmt.Printf("%s  %s  %s\n", s.ID, s.Activity, s.Model)
}
```

### 4. Or run continuously with an event sink

```go
sink := monitor.EventSinkFunc(func(ctx context.Context, ev monitor.Event) error {
    fmt.Printf("event: %s  sessions: %d\n", ev.Type, len(ev.Updates))
    return nil
})

mon, err := monitor.New(
    monitor.WithSources(src),
    monitor.WithSink(sink),
    monitor.WithPollInterval(2 * time.Second),
)
if err != nil {
    log.Fatal(err)
}

if err := mon.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
    log.Fatal(err)
}
```

## API Overview

### monitor package

`Monitor` is the central type. Create one with `monitor.New(opts...)`.

| Method | Description |
|--------|-------------|
| `PollOnce(ctx)` | Run one discover+parse cycle across all sources |
| `Run(ctx)` | Poll on the configured interval until ctx is canceled |
| `Snapshot()` | Return a copy of all currently tracked sessions |
| `Get(id)` | Return a single session by ID |
| `Health()` | Return per-source health state |
| `Sources()` | Return configured source names |

**Options:**

| Option | Default | Description |
|--------|---------|-------------|
| `WithSources(srcs...)` | required | Session data providers |
| `WithSink(sink)` | nil | Event receiver |
| `WithPollInterval(d)` | — | Interval for `Run` |
| `WithLogger(l)` | discard | `*slog.Logger` |
| `WithStaleThreshold(d)` | 5 min | Time before a silent session goes terminal |
| `WithCompletionRetention(d)` | 30 s | How long terminal sessions stay visible |
| `WithHealthThreshold(n)` | 3 | Consecutive failures before status degrades |

### session package

`SessionState` is the public snapshot of a monitored session:

```go
type SessionState struct {
    ID                 string
    Source             string
    Slug               string
    Activity           Activity        // "idle" | "working" | "waiting" | "terminal"
    Lifecycle          LifecycleState  // "active" | "terminal"
    ContextTokens      int
    OutputTokens       int
    MaxContextTokens   int
    ContextUtilization float64
    TokenEstimated     bool
    Model              string
    WorkingDir         string
    Branch             string
    CurrentTool        string
    MessageCount       int
    ToolCallCount      int
    StartedAt          time.Time
    LastActivityAt     time.Time
    LastDataReceivedAt time.Time
    CompletedAt        *time.Time
    Subagents          []SubagentState
}
```

`session.Policy` lets consumers redact sensitive fields before forwarding state:

```go
policy := session.Policy{RedactWorkingDir: true, RedactBranch: true}
safe := policy.Apply(state)
```

### source package

The `Source` interface is the extension point for new agent types. See [docs/source-implementation.md](docs/source-implementation.md) for the full guide.

### transport/httpapi

HTTP adapter exposing monitor state as JSON:

```go
handler, err := httpapi.NewHandler(mon)
// GET /sessions     — list all sessions
// GET /sessions/{id} — single session
// GET /healthz      — source health
// GET /sources      — configured source names
```

### transport/wsapi

WebSocket adapter that implements `monitor.EventSink` and pushes events to connected clients:

```go
wsSrv, err := wsapi.NewServer(mon)
// GET /ws — WebSocket upgrade, then receives monitor.Event as JSON
```

## Built-in Sources

| Package | Agent | Discovery |
|---------|-------|-----------|
| `sources/claude` | Claude Code | `*.jsonl` files under a projects directory |
| `sources/codex` | OpenAI Codex CLI | `sessions/YYYY/MM/DD/rollout-*.jsonl` under CODEX_HOME |
| `sources/gemini` | Gemini CLI | `checkpoint.json` files one level below the root |
| `sources/mock` | Test double | Programmatic control via `QueueHandles`, `QueueParseResult` |

## Examples

- [`examples/stdout`](examples/stdout/main.go) — print events as JSON. Usage: `go run ./examples/stdout -dir ~/.claude/projects`
- [`examples/httpserver`](examples/httpserver/main.go) — HTTP + WebSocket server. Usage: `go run ./examples/httpserver -dir ~/.claude/projects`

## Architecture

See [docs/architecture.md](docs/architecture.md) for a package map, data flow diagram, and extension point reference.

## Migrating from agent-racer

See [docs/migration-from-agent-racer.md](docs/migration-from-agent-racer.md) for a type-by-type mapping.

## License

MIT
