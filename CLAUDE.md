# agentwatch

Go library for monitoring local AI coding agents. Extracted from `agent-racer` — see `agentwatch-plan.md` for the full design and provenance.

## Build Gate

Every change must pass before committing:

```bash
go vet ./...
go test ./...
go test -race ./...
```

All three pass or it doesn't ship. No exceptions.

## Strict TDD

Tests are mandatory, not aspirational. Write tests first.

1. **Write a failing test** that captures the behavior you're about to implement.
2. **Write the minimal code** to make it pass.
3. **Run the full gate** (`go vet && go test ./... && go test -race ./...`).
4. Do not close a beads issue until its tests pass under `-race`.

Concurrency-sensitive packages (monitor, eventbus, store) require race and deadlock coverage. A function without a test is unfinished work.

## agent-racer Provenance

This library is an API-first partial rewrite of `agent-racer`. The plan document (`agentwatch-plan.md` §6) is the extraction manifest.

**Before writing anything from scratch, check the manifest.** If the plan says "preserve" for a component, lift the proven behavior and tests from agent-racer — don't reinvent them. Adapt to the new API shape, but keep the battle-tested logic.

Specifically:
- **Preserve** — JSONL complete-line reader, Claude parser fixtures, store clone/lock discipline, end-marker validation, health hysteresis, deadlock test patterns, rate-limit behavior, slow-client test patterns, privacy filter tests.
- **Rewrite** — monitor orchestration, session state (slim, no racer fields), WebSocket protocol (wrap `monitor.Event`, no racer message types), source construction (explicit registry, no global init), config/filesystem defaults (no XDG in library).

Copy commits should include provenance: `lift: preserve <thing> from agent-racer@<sha>`

When porting, reference the pinned SHA in `docs/decisions/0001-source-provenance.md`. Do not extract from an unpinned or dirty source state.

## Package Visibility

Public packages are stable extension points. Everything else starts `internal/`.

- **Public**: `monitor`, `source`, `session`, `sources/*`, `transport/httpapi`, `transport/wsapi`
- **Internal**: `internal/jsonl`, `internal/filewatch`, `internal/eventbus`, `internal/clock`, `internal/testutil`

Do not promote an internal package speculatively. Once public, it's a compatibility burden.

## Design Rules

These are hard constraints from the plan (§12). Violating them requires updating the plan first.

1. No game/UI concepts in core packages (lanes, leaderboards, overtakes — those live in consumers).
2. No filesystem path defaults in `monitor`. Consumers and examples choose paths.
3. No prompt text, assistant text, tool output, raw log path, PID, or tmux target in public v1 state.
4. `context.Context` on every blocking method.
5. Source cursors are opaque to the monitor. Never parse or compare a `Cursor` outside its source.
6. State commits happen before event delivery and outside store locks.
7. Functional options for multi-argument constructors.
8. Traditional `for i := 0; i < n; i++` loops; no range-over-integer.
9. No global source registration as the primary API.
10. Health error strings must be sanitized before leaving the process — no absolute paths or panic details through transports.

## Lifecycle Transitions

The transition table in the plan (§4.5) is a gate. If you need a new transition, update the table in the plan before implementing it.

## Code Style

- `go vet` and `golangci-lint run` clean.
- JSON tags on all public structs.
- Deep-clone methods on types that cross goroutine boundaries.
- Sinks must return quickly. Long-running sinks wrap themselves in an async queue.
- HTTP examples bind to `127.0.0.1` by default.
- WebSocket origin defaults: same-origin and localhost only.
