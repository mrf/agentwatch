# ADR 0001: Source Provenance

**Status:** Template stub — not complete until source SHA is pinned

## Context

`agentwatch` is a partial rewrite of `agent-racer`. Proven behavior — parser
fixtures, concurrency patterns, end-marker validation, health hysteresis — is
being preserved by lifting code from a specific commit in the `agent-racer`
repository. To make that lift reproducible and auditable, the exact source
commit SHA must be pinned before any copy work begins.

During review on 2026-05-02, the local `agent-racer` checkout was on `main`.
The intended source branch (`claude/backend-library-abstraction-UvaD5`) was
not present locally, and the checkout had local modifications in
`backend/cmd/server/main.go` and `backend/internal/ws/server.go`. Extracting
from an ambiguous or dirty state would make the provenance unverifiable.

## Decision

**TODO: Fill in after fetching the branch and pinning the SHA.**

Steps required to complete this ADR:

1. Fetch the intended branch from `agent-racer`:
   ```
   git fetch origin claude/backend-library-abstraction-UvaD5
   ```
2. Verify the working tree is clean at the target ref.
3. Record the exact commit SHA below.
4. Record any source files that had local modifications and whether they affect
   extracted code.

| Field | Value |
|---|---|
| Source repo | `agent-racer` |
| Source branch | `claude/backend-library-abstraction-UvaD5` |
| Pinned SHA | **PENDING** |
| Dirty files at extraction time | **PENDING** |
| Extraction date | **PENDING** |

All copy commits in `agentwatch` must include:

```
lift: preserve <thing> from agent-racer@<sha>
```

## Consequences

- No extraction work should begin until this ADR has a pinned SHA.
- If the branch is not fetchable or the SHA is ambiguous, the extraction task
  is blocked until the source is resolved.
- Once pinned, the SHA is immutable — subsequent commits in `agent-racer` do
  not change what was extracted.
