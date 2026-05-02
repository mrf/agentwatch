# agentwatch Execution Wave Plan

> Generated 2026-05-02. Reflects beads dependency graph.
> Orchestrator uses `bd ready` for live scheduling — this doc is for human reference and context recovery.

## Decisions

- **Module path:** `github.com/mrf/agentwatch`
- **Library name:** `agentwatch`
- **Source SHA:** agent-racer@5c193229048c47181fcf37cc375fe7edb0438d48

## Waves

### Wave 0 — Human decisions (CLOSED)

| Issue | Title | Assignee | Status |
|-------|-------|----------|--------|
| agentwatch-j9w | Decide module path and library name | mrf | CLOSED |
| agentwatch-d0g | Pin agent-racer source provenance SHA | mrf | CLOSED |

### Wave 1 — Bootstrap (1 agent)

Unblocks after: Wave 0

| Issue | Title | Directory | Priority |
|-------|-------|-----------|----------|
| agentwatch-3j1 | Bootstrap repo: go.mod, license, directory skeleton | entire repo | P0 |

### Wave 2 — Foundation (3+ agents, zero conflict risk)

Unblocks after: Wave 1 (bootstrap)

| Issue | Title | Directory | Priority |
|-------|-------|-----------|----------|
| agentwatch-hpy | session package: SessionState, Activity, lifecycle types | session/ | P1 |
| agentwatch-g50 | internal/jsonl: complete-line reader with size caps | internal/jsonl/ | P1 |
| agentwatch-ecu | internal/clock and internal/testutil | internal/clock/, internal/testutil/ | P2 |
| agentwatch-qms | Write API design ADRs | docs/decisions/ | P2 |
| agentwatch-z6l | internal/eventbus: event fanout | internal/eventbus/ | P2 |
| agentwatch-kbj | internal/filewatch: shared file discovery | internal/filewatch/ | P2 |

**6 items, all in separate directories. Maximum parallelism opportunity.**

### Wave 3 — Interfaces (2-3 agents)

Unblocks after: session types (agentwatch-hpy)

| Issue | Title | Directory | Priority | Also needs |
|-------|-------|-----------|----------|------------|
| agentwatch-uxu | source package: Source interface, Cursor, Registry | source/ | P1 | — |
| agentwatch-4o6 | session/privacy: PrivacyFilter | session/ | P2 | — |

### Wave 4 — Core implementations (3-5 agents, zero conflict risk)

Unblocks after: source interface (agentwatch-uxu)

| Issue | Title | Directory | Priority | Also needs |
|-------|-------|-----------|----------|------------|
| agentwatch-rk0 | sources/mock: deterministic test source | sources/mock/ | P1 | — |
| agentwatch-35y | monitor core: New, PollOnce, Snapshot, events | monitor/ | P1 | session |
| agentwatch-1wn | sources/claude: vertical-slice parser | sources/claude/ | P1 | jsonl |
| agentwatch-i3e | sources/codex: full implementation | sources/codex/ | P1 | filewatch |
| agentwatch-ryi | sources/gemini: full implementation | sources/gemini/ | P1 | filewatch |

**Critical path: agentwatch-35y (monitor core). Everything downstream depends on it.**

### Wave 5 — Monitor + examples (3-4 agents)

Unblocks after: monitor core (agentwatch-35y)

| Issue | Title | Directory | Priority | Also needs |
|-------|-------|-----------|----------|------------|
| agentwatch-8uc | monitor health: Health model with hysteresis | monitor/ | P1 | — |
| agentwatch-g9c | monitor lifecycle: full transitions | monitor/ | P1 | clock |
| agentwatch-q8e | examples/stdout: console monitor example | examples/stdout/ | P2 | claude slice |
| agentwatch-zmv | Spike: agent-racer consumer EventSink | examples/ or docs/ | P2 | claude slice, **mrf only** |

**Warning: health + lifecycle both touch monitor/. Schedule one after the other, not parallel.**

### Wave 6 — Monitor Run (1 agent, critical path)

Unblocks after: lifecycle (agentwatch-g9c) + health (agentwatch-8uc)

| Issue | Title | Directory | Priority |
|-------|-------|-----------|----------|
| agentwatch-995 | monitor Run, ticker, removal/retention | monitor/ | P1 |

### Wave 7 — Transport + tests (3-4 agents, zero conflict risk)

Unblocks after: monitor Run (agentwatch-995)

| Issue | Title | Directory | Priority | Also needs |
|-------|-------|-----------|----------|------------|
| agentwatch-nb5 | monitor concurrency + race/deadlock tests | monitor/ | P1 | — |
| agentwatch-bv0 | transport/httpapi: HTTP handler | transport/httpapi/ | P2 | — |
| agentwatch-tgy | transport/wsapi: WebSocket server + broadcaster | transport/wsapi/ | P2 | — |
| agentwatch-0z9 | sources/claude: full implementation | sources/claude/ | P1 | filewatch, source SHA |

### Wave 8 — Integration (2 agents)

Unblocks after: both transports

| Issue | Title | Directory | Priority |
|-------|-------|-----------|----------|
| agentwatch-j5o | examples/httpserver: HTTP+WS example server | examples/httpserver/ | P3 |
| agentwatch-gty | Documentation: README, architecture, source guide | docs/, README.md | P3 |

### Wave 9 — Publish (human)

Unblocks after: everything

| Issue | Title | Assignee | Priority |
|-------|-------|----------|----------|
| agentwatch-kmb | Tag v0.1.0 and publish to pkg.go.dev | mrf | P3 |

## Critical Path

```
Bootstrap → session → source → monitor core → lifecycle + health → Run → transport → docs → v0.1.0
```

Shortest path through the project is 9 sequential steps. Parallelism helps by clearing non-critical-path work (internal utils, sources, examples) alongside the critical path so they're ready when needed.

## Merge Conflict Map

| Package | Safe with | Conflict risk with |
|---------|-----------|-------------------|
| session/ | everything else | session/privacy (same dir, schedule sequentially) |
| source/ | everything except session during initial build | — |
| monitor/ | everything outside monitor/ | other monitor/ issues (health vs lifecycle) |
| sources/claude/ | sources/codex/, sources/gemini/, all non-source | — |
| sources/codex/ | sources/claude/, sources/gemini/ | — |
| sources/gemini/ | sources/claude/, sources/codex/ | — |
| transport/httpapi/ | transport/wsapi/ | — |
| transport/wsapi/ | transport/httpapi/ | — |
| internal/* | each subdir is independent | — |
| examples/* | everything | — |
| docs/ | everything | — |
