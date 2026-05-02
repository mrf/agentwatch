# ADR 0003: Opaque Source Cursor

**Status:** Accepted

## Context

The original `agent-racer` source abstraction used a `int64` byte offset to
track position within a log file. That works for append-only files, but it
breaks for sources that do not follow append-only semantics:

- **Gemini** rewrites its JSON session file in place. There is no meaningful
  byte offset; mtime is used as a proxy cursor.
- Future sources may be stream-backed, process-backed, DB-backed, or
  snapshot-only. None of these map to byte offsets.

If the monitor interprets cursor values — comparing them, storing them as
integers, deciding how to advance them — it becomes coupled to the internal
mechanics of each source type. That coupling breaks whenever a new source type
requires a different cursor representation.

## Decision

The `source.Cursor` type is an opaque string. The monitor stores it and returns
it to the source, but never parses, compares, or interprets it.

```go
package source

type Cursor string

type Source interface {
    Name() string
    Discover(ctx context.Context) ([]SessionHandle, error)
    Parse(ctx context.Context, h SessionHandle, c Cursor) (SourceUpdate, Cursor, error)
}
```

### Encoding conventions (source-internal, not a library contract)

Individual source implementations choose their own cursor encoding:

- File-tail sources may encode byte offsets as decimal strings: `"4096"`.
- Rewrite-on-save sources may encode mtimes, content hashes, or JSON blobs.
- Snapshot sources may encode a logical sequence number or a content hash.

These conventions are private to each source package. The monitor does not
validate, parse, or compare cursor values across sources.

### Zero value

An empty `Cursor` (`""`) signals "no prior position." Sources must handle the
zero value by starting from their natural beginning (e.g., offset 0, epoch
mtime, first known sequence number).

### Continuity state

Source-specific continuity state — such as Claude subagent parent maps — lives
inside the source implementation or encoded within the cursor. It must not leak
into `SessionHandle` or any other monitor-visible type.

### Context requirement

`Parse` takes a `context.Context`. Sources must respect cancellation.

## Consequences

- The monitor is decoupled from source storage mechanics. Adding a new source
  type requires no changes to monitor internals.
- Sources own their cursor format and can change it freely without coordination,
  as long as they handle the zero-value case.
- Debugging is slightly harder — an opaque string gives less information than a
  typed offset. Source implementations should log their decoded cursor values
  internally.
- Cross-source cursor comparison is impossible by design. The monitor must not
  attempt it.
