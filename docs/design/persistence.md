# Optional persistence of cursors and counters across restarts (agentwatch-3xm)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-3xm. Grounded in the tree at `80d3cef`.

**Recommendation up front:** the monitor does no I/O. Add a byte-level checkpoint API,
`(*Monitor).Checkpoint(ctx) ([]byte, error)` and `WithRestore([]byte) Option`. It saves
each session's cursor and state together, never one without the other. The consumer
chooses the path and when to write (CLAUDE.md rule 2). The atomic-file helper lives in
`examples/` and the future `cmd/agentwatch`, not in a new public package.

Two prerequisites: every source must answer an unreadable cursor with `Reset` (§5.1), and
the cursor/removed maps must be bounded (agentwatch-fyv, §5.4).

## 0. Plan / ADR conflicts (read first)

Signing off means the first implementation commit makes these edits. Code comes after.

1. **Plan §4.3 says "No `WithStateDir` in v1. Persistence is out of scope."** This proposal
   contradicts the second sentence. Proposed text: "No `WithStateDir`. The monitor does no
   persistence I/O. Consumers may save `Checkpoint` bytes and pass them back with
   `WithRestore`." §1's non-goal "Database persistence" still holds.
2. **Plan §4.5 transition table (a gate).** Add a row:
   `Untracked | restored from checkpoint | saved lifecycle | none`. Restoring emits no
   event (§3.3).
3. **ADR 0003 (opaque cursor).** Amend it with two source obligations. (a) A cursor may be
   written to disk by the consumer, so it must never contain CLAUDE.md rule 3 data (prompt
   or assistant text, tool output, raw log path, PID, tmux target). (b) A cursor the source
   cannot decode, including an older format of its own, must give a `Reset` update with
   absolute totals, never an error that repeats forever and never deltas counted against
   zero (§5.1). The same text goes in `docs/source-implementation.md` next to the existing
   "cursor you return is persisted" paragraph.
4. **Plan §4.6 / `Seq`.** No edit. `Seq` stays "monotonically increasing per monitor
   instance" and is not saved (open question 4).
5. **Public package list (CLAUDE.md, ADR 0002).** No edit under the recommendation. If Mark
   wants a shipped file store instead (open question 2), add `persist/filestate` (name to be
   decided) to both lists.
6. **Plan §12.12 "easier to expand than retract".** The checkpoint is an opaque `[]byte` on
   purpose. No exported struct, no exported cursor field, so the format can change without
   an API break.

Rules this design keeps: CLAUDE.md rule 2 (the monitor has no path), rule 4 (`Checkpoint`
takes a `ctx`), rule 5 (cursors are stored as given and never compared), rule 6 (restore
emits nothing and checkpointing does no I/O under `mu`), rule 7 (`WithRestore` is a
functional option).

## 1. What a restart loses today (measured)

A throwaway test (`monitor` package, internal, not committed) used the real Claude source,
`sources/claude/testdata/tool_session.jsonl` and an end-marker directory. Results:

| Scenario | Observed |
|---|---|
| A: discover, then end with a marker | `discovered` seq=1, `terminal` seq=3 reason `exit`; msgs=2 tools=1; marker deleted from disk |
| B: new monitor + source on the same files (a plain restart) | `discovered` seq=1, **lifecycle `active`**, msgs=2 tools=1 |
| C: new monitor with only A's cursor restored, then one user line appended | first poll: **session not tracked, no events**; after the append: `discovered`, **msgs=1 tools=0** (true total is 3/1), `StartedAt` 11:05 instead of 11:00 |
| D: marker already on disk before the first poll | `discovered`, lifecycle `active`, **marker deleted**. The terminal signal is lost |

What these show:

- **Counters are not lost on a plain restart.** Every source re-reads from the zero cursor
  (`monitor.go` `PollOnce`: `cursor := m.cursors[cKey]` is empty in a fresh map), so the
  whole-history counts are rebuilt (row B). The cost is the re-read. The two shipped
  examples set no discover window (`examples/httpserver/main.go`, `examples/stdout/main.go`
  call `claude.New(claude.WithRoot(...))` only), so the first poll on this machine reads
  **1,183 transcripts, 3.2 GB**. With a 24h window it reads 154 files, 272 MB, the largest
  20.8 MB (sizes from `find ~/.claude/projects -name '*.jsonl'`, subagents excluded). Wall
  time was not measured.
- **Terminal knowledge is lost.** It comes from three places:
  1. Claude end markers. `drainEndMarkers` calls `os.Remove` after consuming each marker,
     so the next process cannot see it.
  2. The stale sweep and the subagent-completion sweep, which exist only in memory.
  3. Retention removal: the `m.removed` set is in memory only.

  After a restart, every session still inside the discover window comes back as
  `discovered`/`active` (row B). It shows as active until `staleThreshold` (default 5m),
  then lingers for `completionRetention` before it goes. Consumers that record events (a
  replay sink) see duplicate `discovered` events for sessions they already saw end.
- **Restoring the cursor alone is worse than restoring nothing** (row C). Sources return
  deltas relative to the cursor, so a cursor without the matching state gives undercounts,
  a wrong `StartedAt`, and a session that stays invisible until the agent writes again.
  Cursor and state must be saved and restored as one unit.
- Row D is a separate existing bug, not caused by persistence. An `Untracked` session whose
  first update has `Terminal=true` goes through the `!existed` branch of `PollOnce`, which
  never reads `update.Terminal`, and the marker is already deleted. A finding has been filed
  for it. It matches open question 4 in `process-liveness.md` ("Untracked + `Terminal=true`
  → terminal" row). Persistence makes it rarer but does not fix it.
- Also reset: `Seq` (allowed by §4.6; `wsapi` already sends a fresh snapshot on every
  connect, `transport/wsapi/server.go` "There is no resume point"), health hysteresis, and
  `SkippedLines`, which today is recounted by the re-read.

## 2. What to persist, and what never to persist

Monitor state (`monitor.go` `Monitor` struct): `store`, `cursors`, `removed`,
`contentStart`, `health`, `seq`.

| Item | Persist? | Why |
|---|---|---|
| `cursors[source:id]` for sessions in `store` or `removed` | **yes**, verbatim | stored opaque, never parsed (rule 5) |
| `store[id]` (`session.SessionState`, deep clone) | **yes**, with its cursor | needed for correct deltas (row C). It is already the public, rule-3-clean state |
| `contentStart[id]` | yes (bool per session) | `mergeStartedAt` needs it, or a restored content start is overwritten |
| `removed[id]` | yes, bounded (§5.4) | without it, removed sessions come back as `discovered` |
| cursors with no store/removed entry (zero-update-only handles) | **no** | restoring them is row C. Dropping them re-reads from 0, which is today's safe behaviour, and bounds the file |
| `health`, `seq` | no | runtime only. Document that `SkippedLines` counts since process start once restore is used |
| anything inside a source (`ClaudeSource.pendingTerminals`, `lastMarkerScan`, opencode's DB handle) | no | source-private. ADR 0003 already says continuity state belongs in the cursor |

**Privacy audit of current cursor contents (rule 3).** None of the six sources encode text
or paths:

- `sources/claude`: `claudeCursor` = `jsonl.Position` + `Parents` (tool_use IDs) +
  `Subagents` (`subagentEntry` = `Position` + `session.SubagentState`, which is public).
- `sources/codex`, `sources/pi`, `sources/cursor`: `jsonl.Position` only.
- `sources/opencode` (`encodeCursor` in `parser.go`) and `sources/antigravity`: integers
  only.

`jsonl.Position` holds an offset, dev/ino, and an FNV-64a hash of the first ≤4 KiB of the
transcript. That hash is computed from prompt-bearing bytes, but it is a 64-bit fingerprint
of 4 KiB and cannot practically be reversed. Accepted, and listed as open question 6. The
helper writes files with mode 0600. Transports never serve checkpoint bytes.

## 3. API and semantics

```go
// Checkpoint returns an encoded snapshot of session state and cursors that
// WithRestore accepts. It waits for an in-flight poll, so it never captures a
// cursor whose batch has not been committed. Do not call it from a sink.
func (m *Monitor) Checkpoint(ctx context.Context) ([]byte, error)

// WithRestore seeds a new Monitor from Checkpoint output. New returns an error
// wrapping ErrInvalidCheckpoint if data does not decode or has an unknown version.
func WithRestore(data []byte) Option
```

### 3.1 Consistency with the poll loop

`PollOnce` writes the cursor (the "Persist the returned cursor unconditionally" block) and
commits state (the later `m.mu.Lock()` … `m.store[h.ID] = state` block) in **two separate
critical sections**. A checkpoint that took only `mu` could see the new cursor with the old
state, and restoring it would lose that batch's deltas (row C on a small scale). So
`Checkpoint` acquires `pollSem`, the same way `PollOnce` does including `ctx` cancellation,
then takes `mu`, deep-clones, releases both, and encodes outside every lock. Cost: it waits
for at most one poll.

A sink that calls `Checkpoint` deadlocks until its `ctx` ends, because the sink runs while
`PollOnce` holds `pollSem`. This is documented, and a test pins it as ctx-bounded rather
than permanent.

Alternative considered: merge the two critical sections so cursor and state commit
together. It touches the hottest code path and still needs `pollSem` for the sweeps, so
it was rejected.

### 3.2 Encoding and versions

Versioned JSON: `{"version":1,"savedAt":…,"sessions":[{"source","id","cursor","state",
"contentStart","removed"}]}`. The type is unexported, and JSON tags exist only for the
on-disk format.

- Unknown `version`, or data that does not decode, makes `New` fail with
  `ErrInvalidCheckpoint`. The consumer then starts without a restore.
- A record whose `source` is not configured is dropped and logged.
- A record that has state without a cursor, or a cursor without state or `removed`, is
  dropped whole. That case only arises from tampering.
- Cursor formats belong to the sources. Each source handles its own legacy formats (as
  `jsonl.ParsePosition` already accepts bare offsets) or resets (§5.1).

### 3.3 Restore behaviour

- `New` fills `store`, `cursors`, `removed` and `contentStart`. No event is emitted.
  `Snapshot`/`SnapshotWithSeq` show the restored sessions immediately with `Seq` 0, and a
  `wsapi` client that connects gets them in its first snapshot. Whether to emit an
  `EventSnapshot` to the sink after restore is open question 3.
- The first poll runs the normal paths with no special cases:
  - Restored sessions with new data get `updated` (not `discovered`), with counts =
    saved + delta.
  - A rotated or truncated file gives `Reset`, so counts are replaced. This comes from
    `jsonl.ReadFrom`'s size/dev/ino/head-hash check and the Claude
    `decodeCursor`/`rewritten` path.
  - The stale sweep compares the saved `LastDataReceivedAt` (wall clock) against now. A
    session that was silent through the downtime goes `stale` on the first poll, which is
    correct and announced. One that wrote during the downtime is parsed first and is not
    stale. `liveness-heartbeat.md`'s per-activity threshold works on the restored
    `Activity` unchanged.
  - Terminal sessions past retention are `removed` on the first poll.
  - A removed key with new data takes the existing re-read-from-`""` resume path.
- An end marker written during the downtime is consumed against a session that now
  exists, so it produces `terminal`. This fixes row D for every session that was known
  before the restart.
- If inodes change across reboots on some filesystem, every restored `Position` fails
  `matches`, which gives `Reset` and a full re-read. That is today's cost with correct
  counts, so the worst case is no worse than now.

## 4. Atomic writes and corruption (helper, not library)

The helper lives in `examples/internal/statefile` and later in `cmd/agentwatch`, behind an
explicit `-state <path>` flag with no default (rule 2). XDG paths are decided by the binary
(`cmd-binary.md`).

- Write: `os.CreateTemp` in the target's directory with mode 0600, write, `f.Sync()`,
  close, `os.Rename`, then fsync the directory.
- Read: cap the size (proposed 64 MiB). If the file is missing, start fresh. If it is
  oversized or undecodable, or `ErrInvalidCheckpoint` is returned, log the error, rename
  the file to `<path>.corrupt`, and start fresh.
- When to write: every `-state-interval` (default 30s) and on clean shutdown, after `Run`
  returns. A crash loses at most one interval. A Claude marker consumed inside that window
  is still lost, which is the same as today.
- Two processes sharing one path: last writer wins, and this is documented as unsupported.
  Advisory locking is open question 5.

## 5. Prerequisites and interactions

### 5.1 Source conformance: unreadable cursor → `Reset` (blocking)

Under persistence, an unreadable cursor can be reached through file corruption or a source
upgrade. Today:

- **Comply:**
  - claude: `decodeCursor` error leads to `reset = true`.
  - codex, pi, cursor: `jsonl.ReadFromCursor`, which re-reads with `reset` when the
    cursor does not parse.
- **opencode: does not comply.** `Parse` returns `decodeCursor`'s error and keeps the same
  cursor `c`, so the session fails on every poll and never recovers.
- **antigravity: does not comply.** `decodeCursor` returns zeros on a malformed cursor and
  there is no `Reset`. The deltas then equal the full totals and are **added to the
  restored counts, doubling them**. A database that shrinks is also clamped to delta 0
  with no `Reset`.

Both need a fix and a conformance test before restore is safe to enable for them.

### 5.2 `process-liveness.md`

That doc covers sessions that died while the monitor was down and were never known. This
doc covers sessions that were known. They complement each other. Its open question 4 and
the row-D finding are the same gap.

### 5.3 `fsnotify-discovery.md`, `needs-attention.md`, `cmd-binary.md`

- fsnotify: no conflict. It never touches cursors, and the first poll is a full `Discover`
  either way.
- needs-attention: any new `SessionState` field is saved automatically, but adding one
  changes the encoding. Bump `version` only for semantic changes; JSON already ignores
  unknown fields and zero-fills missing ones.
- cmd-binary: it lists persistence as a non-goal (§"Non-goals"). This doc would make
  persistence a later slice behind `-state`.

### 5.4 Bounded growth (agentwatch-fyv)

`cursors` is never deleted from, and `removed` is deleted from only on resume
(`delete(m.removed, …)` in `PollOnce` is the only delete). The only other deletes in
`monitor.go` are `store` and `contentStart` in the retention sweep. So both maps grow for
the life of the process, and a checkpoint would carry that growth across restarts. Saving
only store and removed entries (§2) removes the cursor-only part. The `removed` set still
needs a horizon: store a `removedAt` time and drop entries older than it, both in memory
and in the checkpoint.

**UNVERIFIED:** I could not read agentwatch-fyv's text from this worktree (no `br`).
Whatever bound fyv picks should apply here unchanged. If fyv does not land first, slice 3
adds the horizon itself. Dropping a removed entry only means the file is later seen again
as `discovered`, which is today's behaviour.

## 6. Test plan (strict TDD; tests before each slice)

- **Round trip:** poll, `Checkpoint`, `New(WithRestore)`. `Snapshot` deep-equals the
  original, no sink events are emitted at restore, and mutating the snapshot does not touch
  the restored monitor.
- **Row C inverted (real Claude source + fixture):** restore, append a line, poll. Expect
  `updated`, msgs=3 tools=1, `StartedAt` = 11:00.
- **Row B inverted:** marker consumed and terminal, then checkpoint and restore. The
  session stays terminal and no `discovered` is emitted.
- **Reset after restore:** truncate or replace the file while down. Expect a `Reset`
  update with counts replaced.
- **Sweeps on first poll:** a restored active session with an old `LastDataReceivedAt` goes
  `stale`; the same session with new data does not. A terminal session past retention is
  `removed`. A removed key with data is `resumed` with whole-history counts.
- **Rejects:** unknown version or garbage bytes give `ErrInvalidCheckpoint` from `New`
  (`errors.Is`). An unconfigured source's record is dropped. Half records are dropped.
  Cursor-only handles are absent from the output.
- **Consistency under race (`-race`):** a mock source whose cursor `"N"` means N messages
  consumed. `Run` polls continuously while a goroutine checkpoints in a loop. In every
  decoded checkpoint, each session's `MessageCount` equals its cursor's N. This test fails
  if `Checkpoint` takes only `mu` (§3.1).
- **Deadlock:** `Checkpoint` with a canceled or timed-out ctx returns `ctx.Err()` while a
  poll is blocked in a slow sink. `Checkpoint` called from a sink returns when its ctx
  deadline passes; the test bounds this with a `context.WithTimeout` deadline.
- **Source conformance:** a table test across all sources: a garbage cursor gives `Reset`
  with absolute totals and no error. It fails today for opencode and antigravity (§5.1).
- **Helper:** atomic replace (no partial file after a simulated failed write), mode 0600,
  oversize and corrupt files renamed to `.corrupt` with a fresh start, missing file means a
  fresh start.
- **Golden:** commit a v1 checkpoint fixture so later format changes must decode it or bump
  `version`.

## 7. Implementation slices

1. **Docs:** the §0 edits to the plan, ADR 0003 and `source-implementation.md`. No code.
2. **Source conformance:** opencode and antigravity reset on unreadable cursors, plus the
   all-source table test. This is useful even without persistence.
3. **Bounded removed set:** `removedAt` plus a horizon. Fold into agentwatch-fyv if it
   covers this.
4. **`Checkpoint(ctx)`**, with the round-trip-encode, consistency/race and deadlock tests.
5. **`WithRestore`**, with the restore, first-poll and reject tests, and the §4.5 row.
6. **Helper + `examples/httpserver -state`**, later `cmd/agentwatch`, with the helper tests.

## 8. Open questions for Mark

1. **Proceed at all?** A plain restart already rebuilds counts (row B). The gains are
   ended sessions staying ended, no duplicate `discovered` events, and a much smaller
   startup read (3.2 GB here, unwindowed). Is that worth the new public API?
2. **Opaque bytes and an examples-only helper (recommended), or a shipped public
   `persist/...` file store?** The second adds a public package to CLAUDE.md and ADR 0002.
3. Should restore emit an `EventSnapshot` to the sink so event-only consumers learn the
   restored state, or emit nothing and rely on `Snapshot()` (recommended)?
4. Should `Seq` be saved so it stays monotonic across restarts (useful for replay
   recorders), or stay per instance (recommended; matches §4.6)?
5. Is single-writer-per-path a documented limitation, or should the helper take an
   advisory lock?
6. Is the `jsonl.Position` head hash (FNV-64a of ≤4 KiB of transcript) acceptable on disk,
   or should the ADR 0003 amendment forbid content-derived hashes in cursors?
7. Default `-state-interval` of 30s?
