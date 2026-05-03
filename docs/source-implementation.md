# Writing a Custom Source

A `Source` feeds session data into the monitor. Implement the `source.Source` interface to add support for any agent whose session artifacts you can read.

## The Interface

```go
// source/source.go
type Source interface {
    Name() string
    Discover(ctx context.Context) ([]SessionHandle, error)
    Parse(ctx context.Context, h SessionHandle, c Cursor) (SourceUpdate, Cursor, error)
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

If there is no new data, return a zero `SourceUpdate` (one where `SessionID == ""`), the same cursor, and a nil error:

```go
func (s *MySource) Parse(
    ctx context.Context,
    h source.SessionHandle,
    c source.Cursor,
) (source.SourceUpdate, source.Cursor, error) {

    offset, _ := strconv.ParseInt(string(c), 10, 64)

    data, newOffset, err := readNewBytes(h.Path, offset)
    if err != nil {
        return source.SourceUpdate{}, c, err
    }
    if len(data) == 0 {
        return source.SourceUpdate{}, c, nil  // no new data
    }

    update := source.SourceUpdate{
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

## SourceUpdate Fields

`SourceUpdate` carries incremental data. Most fields are optional — only set the ones your source actually knows.

```go
type SourceUpdate struct {
    SessionID string           // required: identifies which session this update belongs to

    Activity  session.Activity // "idle" | "working" | "waiting" | "terminal"
    Slug      string           // human-readable name for the session
    Model     string           // model identifier, e.g. "claude-sonnet-4-6"
    WorkingDir string          // current working directory
    Branch    string           // current git branch

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

**Delta vs absolute fields:** token counts (`ContextTokens`, `OutputTokens`, `MaxContextTokens`) are absolute — pass the current total. Message and tool call counts are deltas — pass the number of new events since the last `Parse` call; the monitor adds them to the running total.

## Signaling Termination

Set `Terminal: true` when the session has ended. The monitor transitions the session to `LifecycleTerminal` and emits an `EventTerminal` lifecycle event. After the configured retention window the session is removed.

```go
update := source.SourceUpdate{
    SessionID: h.ID,
    Terminal:  true,
    EndReason: "session completed",
    EndedAt:   completedAt,
}
```

## Handling Errors

Return an error from `Discover` or `Parse` to signal a failure. The monitor logs it, records a health failure against the source, and continues polling. Your source is responsible for resetting its own internal state on the next successful call.

Do not return an error when there is simply no new data — return a zero `SourceUpdate` and the same cursor instead.

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
| `sources/gemini` | `mtime_ns,msg_count,tool_count` | Rewrite-on-save JSON |
