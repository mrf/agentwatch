# ADR 0002: Package Visibility Policy

**Status:** Accepted

## Context

The original draft of the `agentwatch` plan said "No `internal/` directory.
Every package is importable." That choice trades long-term API stability for
short-term convenience. A Go library with everything public must treat every
exported type, function, and field as part of its compatibility contract the
moment it is tagged.

`agentwatch` is an extraction of `agent-racer` internals. Many implementation
details — JSONL parsing schemas, file discovery helpers, event dispatch
mechanics, clock utilities — are useful internally but should not lock in
external consumers. Exposing them prematurely creates maintenance debt before
the library has any real consumers.

## Decision

Public packages are stable extension points. Everything else starts `internal/`
until a real consumer demonstrates a need.

### Public packages

These packages form the v1 API surface. Breaking changes require a major
version bump.

| Package | Purpose |
|---|---|
| `monitor` | Constructing and running monitors; `Snapshot`, `Health`, `EventSink` |
| `source` | Implementing custom sources; `Source` interface, `Cursor`, `Registry` |
| `session` | Session state, lifecycle types, activity enum, privacy filter |
| `sources/claude` | Reference Claude source constructor |
| `sources/codex` | Reference Codex source constructor |
| `sources/gemini` | Reference Gemini source constructor |
| `sources/mock` | Deterministic test source |
| `transport/httpapi` | Optional HTTP adapter |
| `transport/wsapi` | Optional WebSocket adapter |

### Internal packages

These packages are implementation details. Their APIs may change between minor
versions without notice.

| Package | Purpose |
|---|---|
| `internal/jsonl` | Complete-line JSONL reader and file-size caps |
| `internal/filewatch` | Shared file discovery helpers for sources |
| `internal/eventbus` | Event fanout and dispatch mechanics |
| `internal/clock` | Clock abstraction for deterministic tests |
| `internal/testutil` | Shared test helpers |

### Promotion rule

A package may be moved from `internal/` to public only when:

1. A concrete consumer (not a hypothetical one) requires the import.
2. The API has been reviewed for naming and stability.
3. The package has test coverage adequate for a public contract.

Do not promote a package because two public packages share its types. Use type
aliases, duplication, or restructuring instead. Premature promotion is
irreversible without a major version bump.

## Consequences

- The initial module surface is small and easy to document.
- Breaking changes during early development affect only the library itself, not
  downstream consumers.
- Some code duplication across packages is acceptable — it is preferable to
  premature abstraction.
- When a new source type (e.g., a process-backed or DB-backed source) needs
  discovery helpers, `internal/filewatch` can be promoted at that time with a
  reviewed API.
- `go doc` output is cleaner: consumers see only the contracts they depend on.
