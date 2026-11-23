# Writing a Custom Source

A `Source` feeds session data into the monitor. Implement the `source.Source` interface to add support for any agent whose session artifacts you can read.

## The Interface

```go
// source/source.go
type Source interface {
    Name() string
    Discover(ctx context.Context) ([]SessionHandle, error)
    Parse(ctx context.Context, h SessionHandle, c Cursor) (Update, Cursor, error)
}
```

### Name

Return a stable, lowercase identifier for the source. The monitor uses this as a key in health maps and event fields.

```go
func (s *MySource) Name() string { return "myagent" }
```

### Discover

Return the set of sessions currently visible to the source. The monitor calls `Discover` on every poll cycle. Sessions that disappear from `Discover` are not immediately removed — they go terminal via the stale threshold.

```go
func (s *MySource) Discover(ctx context.Context) ([]source.SessionHandle, error) {
    var handles []source.SessionHandle
    // ... walk files, query a database, scan processes ...
    handles = append(handles, source.SessionHandle{
        ID:         "unique-session-id",
        Path:       "/path/to/session/artifact",
        WorkingDir: "/home/user/project",
        StartedAt:  time.Now(),
        Source:     s.Name(),
    })
    return handles, nil
}
```

`SessionHandle` is a lightweight descriptor — it should be cheap to produce. Heavy parsing happens in `Parse`.

### Parse

Read new data for the session identified by `h`, starting from cursor `c`. Return the incremental update, the next cursor position, and any error.

If there is no new data, return a zero `Update` (one where `SessionID == ""`), the same cursor, and a nil error:

```go
func (s *MySource) Parse(
    ctx context.Context,
    h source.SessionHandle,
    c source.Cursor,
) (source.Update, source.Cursor, error) {

    offset, _ := strconv.ParseInt(string(c), 10, 64)

    data, newOffset, err := readNewBytes(h.Path, offset)
    if err != nil {
        return source.Update{}, c, err
    }
    if len(data) == 0 {
        return source.Update{}, c, nil  // no new data
    }

    update := source.Update{
        SessionID:          h.ID,
        Activity:           session.ActivityWorking,
        MessageCountDelta:  1,
        LastActivityAt:     time.Now(),
    }

    nextCursor := source.Cursor(strconv.FormatInt(newOffset, 10))
    return update, nextCursor, nil
}
```

## Cursor Contract

`source.Cursor` is an opaque string. The monitor stores it and passes it back on the next `Parse` call, but never inspects it. You can encode anything — a byte offset, a file mtime, a line number, a JSON object, a hash:

```go
// byte offset
cursor = source.Cursor("1024")

// mtime in nanoseconds
cursor = source.Cursor(strconv.FormatInt(mtime.UnixNano(), 10))

// composite state as JSON
type myCursor struct {
    Offset   int64  `json:"offset"`
    SeenHash string `json:"seenHash"`
}
b, _ := json.Marshal(myCursor{Offset: 1024, SeenHash: "abc123"})
cursor = source.Cursor(b)
```

A zero cursor (`""`) means no prior position — the source should start from the beginning of the session.

## Update Fields

`Update` carries incremental data. Most fields are optional — only set the ones your source actually knows.

```go
type Update struct {
    SessionID string           // required: identifies which session this update belongs to

    Activity  session.Activity // "idle" | "working" | "waiting" | "terminal"
    Slug      string           // human-readable name for the session
    Model     string           // model identifier, e.g. "claude-sonnet-4-6"
    WorkingDir string          // current working directory
    Branch    string           // current git branch

    HasTokens        bool      // true when this update carries token counts
    ContextTokens    int       // absolute token counts (not deltas)
    OutputTokens     int
    MaxContextTokens int
    TokenEstimated   bool

    MessageCountDelta  int     // delta since last Parse call (monitor accumulates)
    ToolCallCountDelta int
    CurrentTool        string

    StartedAt      time.Time
    LastActivityAt time.Time

    Subagents []session.SubagentState

    Terminal  bool      // set true when the session has ended
    EndReason string    // human-readable reason for termination
    EndedAt   time.Time // when the session ended (defaults to now if zero)
}
```

**Delta vs absolute fields:** token counts (`ContextTokens`, `OutputTokens`, `MaxContextTokens`) are absolute — pass the current total, and set `HasTokens` when the update carries them. An update with `HasTokens` false and zero counts leaves the session's previous counts untouched, so a batch without a usage record does not reset them; `MaxContextTokens` likewise only applies when non-zero. Message and tool call counts are deltas — pass the number of new events since the last `Parse` call; the monitor adds them to the running total.

## Signaling Termination

Set `Terminal: true` when the session has ended. The monitor transitions the session to `LifecycleTerminal` and emits an `EventTerminal` lifecycle event. After the configured retention window the session is removed.

```go
update := source.Update{
    SessionID: h.ID,
    Terminal:  true,
    EndReason: "session completed",
    EndedAt:   completedAt,
}
```

## Handling Errors

Return an error from `Discover` or `Parse` to signal a failure. The monitor logs it, records a health failure against the source, and continues polling. Your source is responsible for resetting its own internal state on the next successful call.

Do not return an error when there is simply no new data — return a zero `Update` and the same cursor instead.

The cursor you return is persisted **even when you also return an error** or a zero `Update`. The monitor never inspects or compares it, so the cursor is how you choose between the two behaviors:

- **Skip the input** (an unparseable file, a batch holding only records you do not report): return the *advanced* cursor. The monitor will not re-read it. For an error, the failure is counted once against source health, and health recovers on the next clean poll.
- **Retry the input** (a transient read failure): return the cursor you were *given* (`c`) with the error.

A zero `Update` never creates a session or refreshes `LastDataReceivedAt`; only a non-zero update counts as data for stale detection.

## Releasing Resources

`Close` is not part of `source.Source`. A source that owns a resource (a database handle, say) may additionally implement `io.Closer`. When `Monitor.Run` returns, it calls `Close` once on each such source, in registration order. A `Close` error is logged and does not affect `Run`'s result or source health.

`PollOnce` never closes sources. Consumers that call only `PollOnce` (or use a source directly) must close any `io.Closer` source themselves.

`Run` closes sources only after any external `PollOnce` still in flight has finished (bounded by that poll's context), so a source's `Close` never overlaps a monitor-driven `Discover` or `Parse`. Keep `Close` safe to call concurrently anyway, since direct callers can race it. The `opencode` source reopens its handle lazily on the next `Discover` or `Parse` after `Close`; a `PollOnce` started after `Run` has returned therefore reopens it, and that caller must close the source again.

## Using the Source Registry

`source.Registry` maps names to factory functions for dynamic source lookup. It is optional — pass sources directly to `monitor.WithSources` if you do not need it.

```go
reg := source.NewRegistry()
err := reg.Register("myagent", func() (source.Source, error) {
    return myagent.New(myagent.WithRoot("/path/to/sessions"))
})

// later
factory, ok := reg.Get("myagent")
if ok {
    src, err := factory()
    // ...
}
```

Convention: provide a `Register` function in your source package so callers can add your source to any registry without importing its internals:

```go
// in your package:
func Register(r *source.Registry, opts ...Option) error {
    return r.Register("myagent", func() (source.Source, error) {
        return New(opts...)
    })
}
```

## Testing Your Source

Use `sources/mock` to test code that consumes a `source.Source`, and write direct unit tests for your source implementation using the `internal/testutil` helpers if needed.

A minimal source test should verify:

1. `Discover` returns the expected handles when artifacts are present.
2. `Parse` returns a zero update when there is nothing new (same cursor).
3. `Parse` returns the correct delta when new data appears.
4. `Parse` sets `Terminal: true` at the right time.
5. Cursors are treated as opaque — parsing the same artifact twice with the original cursor returns the same result.

## Reference Implementations

The built-in sources are concrete examples:

| Source | Cursor type | Format |
|--------|-------------|--------|
| `sources/claude` | JSON `{offset, subagentParents}` | Append-only JSONL |
| `sources/codex` | Decimal byte offset | Append-only JSONL |
| `sources/antigravity` | `mtime_ns,msg_count,tool_count` | Rewrite-on-save JSON |
