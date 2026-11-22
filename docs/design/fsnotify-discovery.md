# Event-driven discovery via fsnotify, with polling fallback

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-yk6. Grounded in the tree at `de58e50` (after the n97 stale-mtime fix).

**Recommendation up front:** don't take the dependency yet. After n97, discovery is cheap.
The poll cost that remains is in `Parse` on handles that haven't changed, and fsnotify only
helps there if the monitor starts skipping Parse based on events. That is a larger and
riskier change than this ticket suggests. A stat-gated Parse fast path (slice 0 below,
no dependency) removes most of the cost. fsnotify is worth adding only if you want lower
latency than the poll interval gives. §2 has the numbers, and §6 Q1 is the decision.

## 0. Plan / ADR conflicts (read first)

- **Dependencies.** The plan has no general dependency policy. ADR 0006 §4 sets the precedent:
  it rejected gopsutil because "as a library, agentwatch should minimize its dependency
  footprint". fsnotify is smaller than gopsutil. v1.10.1 (published 2026-05-04) imports only
  `golang.org/x/sys`, which `go.mod` already carries as an indirect dependency through
  modernc sqlite. It would still be a new direct dependency for every consumer, including
  consumers that only use the mock source. Adding it needs your explicit yes (Q2).
- **Package visibility (CLAUDE.md, ADR 0002).** The watcher wrapper must be internal:
  either `internal/filewatch` or a new internal package. The hook through which the monitor
  learns "something changed" is a **public API addition** in `source`, because sources are
  public. That is a compatibility burden, so Q3 asks about it.
- **Design rule 4 (ctx on blocking).** Any `Watch`/notify method takes `context.Context`,
  and the watcher's goroutine exits when that context is cancelled.
- **Design rule 5 (opaque cursors).** The design never lets the watcher or the monitor look
  at a cursor. Events only change *when* `PollOnce` runs, or (slice 4 only) *which handles*
  get parsed. Cursor bookkeeping stays exactly as it is in `monitor/monitor.go` (the
  "Persist the returned cursor unconditionally" block).
- **Design rule 6 (commit before delivery, outside locks).** Unchanged. Every state change
  still goes through `PollOnce`, and the watcher never touches the store.
- **Plan §4 API.** `Run` currently loops on a ticker and nothing else. This design adds a
  second wake-up source, and `PollOnce` keeps its current semantics, so tests stay
  deterministic.

## 1. How discovery works today

| Source | Discover | Parse per handle, per poll |
|---|---|---|
| claude | `filewatch.Walker` over root, `*.jsonl`, skips `subagents/` | `jsonl.ReadFrom` (open, fstat, verify head hash) + `ReadDir` of `<id>/subagents/` + `ReadFrom` on each subagent file + end-marker drain |
| codex / pi / cursor | `filewatch.Walker`, jsonl patterns, optional `WithMaxAge` | `jsonl.ReadFrom` |
| antigravity | `filewatch.Walker`, `*.db`; window applied using the `-wal` mtime | stat db + wal fingerprint; full SQLite read only on change |
| opencode | one SQL query against a single `opencode.db` | one SQL query per handle (`queryParseData`) |

After n97, a warm `Walker.Discover` does one `Stat` per directory and one `Stat` per cached
file. It calls `ReadDir` only on directories whose mtime changed. `Monitor.Run` calls
`PollOnce` every `pollInterval` (2s by default), and `PollOnce` calls Discover, then Parse
on **every** returned handle.

## 2. Measurement: what polling costs after n97

Method: a throwaway test (not committed) in `sources/claude`, run read-only against the
real `~/.claude/projects`. That tree has 4,472 directories and 1,502 `*.jsonl` files, 323 of
them subagent transcripts. Hardware: WSL2 ext4, 8 vCPU.

| Discover window | Handles | Walker cold | Walker warm | Steady poll (Discover + Parse all, no new data) |
|---|---|---|---|---|
| none (as `examples/httpserver` configures it) | 1,179 | 96 ms | 22 ms | **433 ms** |
| 24h | 150 | 104 ms | 19 ms | 102 ms |
| 30m | 43 | 105 ms | 21 ms | 36 ms |

Reading the table:

- **Discovery is about 20 ms per poll**, which is 1% of one core at a 2s interval. Event-driven
  discovery can save at most that.
- **Parse on unchanged handles dominates**, at roughly 0.35 ms per handle. With no window,
  that comes to about 21% of a core at 2s. I have not instrumented how that time splits
  between open, head-hash verify, and the subagent `ReadDir`, so the split is unmeasured.
- Codex, pi and cursor have no local sessions from the last 24h, and there is no opencode DB
  locally, so those sources are unmeasured. Their Parse path is the same `jsonl.ReadFrom`
  without the subagent work, so they should cost less per handle than Claude.

Conclusion: fsnotify used only to trigger `PollOnce` saves roughly 1% CPU and cuts latency
from ≤2s to about the debounce window. To cut the 433 ms, Parse must be skipped for unchanged
handles. Two ways to do that:

- **(a) Stat fast path inside each source, no dependency (slice 0).** Each jsonl source
  compares the size and inode it already keeps in its own cursor against a fresh `Stat`.
  When they match, it returns the zero update without opening the file. This is
  source-internal, so rule 5 holds. Claude also has to stat its subagent files, because
  appends to them don't change the directory's mtime. That is still a few stats rather than
  opens and reads.
- **(b) Event-gated Parse (slice 4).** The monitor parses only the handles the watcher
  flagged. This requires that events are never missed, and a missed event silently ends a
  live session as stale. It needs a periodic full poll as a safety net.

(a) gets most of (b)'s CPU saving with none of its risk. The rest of this doc is the
fsnotify design, in case you want the latency improvement anyway.

## 3. Design (only if Q1 = yes)

### 3.1 Shape

```go
// package source — optional interface, detected by type assertion in Run.
type ChangeNotifier interface {
    // Watch blocks until ctx is done, calling changed() (non-blocking, coalesced)
    // whenever the source's on-disk state may have changed. A non-nil return
    // before ctx is done means "events unavailable"; the monitor keeps polling.
    Watch(ctx context.Context, changed func()) error
}
```

- `Run` starts one goroutine per notifying source. When any of them signals, the monitor
  debounces and runs a full `PollOnce`. The ticker stays, at the same interval or at a
  longer `WithFallbackPollInterval` (Q4). `PollOnce` itself is untouched.
- The source owns the mapping from paths to handles, so the monitor never interprets paths.
  Slice 4 would extend this to `changed(ids ...string)`.
- The watcher is created in `Watch`, never in `New`, so constructing a source still spawns
  no goroutines and opens no file descriptors.
- `internal/filewatch` gains a `Watcher` that wraps `fsnotify.Watcher` and sits behind a
  small interface (`Add`, `Remove`, `Events`, `Errors`, `Close`). Tests inject a fake.

### 3.2 Watch set per source

| Source | Watch | Filter |
|---|---|---|
| claude | every directory under root, including `subagents/`; plus `sessionEndDir` | `*.jsonl`, end-marker names |
| codex / pi / cursor | every directory under their walker roots | jsonl patterns |
| antigravity | root (flat) | `*.db`, `*.db-wal` |
| opencode | directory containing `opencode.db` | `opencode.db`, `opencode.db-wal`; ignore `-shm` |

The source watches directories, not individual files, as the fsnotify docs recommend. On
inotify, a directory watch also reports writes to the files inside it. For SQLite sources,
WAL appends are the signal, and they arrive as a burst on every streamed token, so the
debounce in 3.4 matters most there. `-shm` is ignored so that our own read-only readers
can't trigger a feedback loop. I have not verified that mmap'd shm writes are invisible to
inotify, so the filter is a precaution.

### 3.3 Per-OS limits (fsnotify v1.10.1 docs)

- **No recursive watching on any OS** (fsnotify issue #18). Every directory has to be
  `Add`ed. When a directory-create event arrives, the source adds a watch and then rescans
  that directory, to catch files created before the watch existed. The local Claude tree
  would need about 4,500 watches.
- **Linux inotify.** Each Watcher is one instance (`max_user_instances`, 128 here, shared
  with every other inotify user on the account). Each directory is one watch
  (`max_user_watches`, 524,288 here; defaults vary by distro and RAM). Exceeding a limit
  returns ENOSPC or EMFILE. Queue overflow (`max_queued_events`, 16,384 here) arrives as
  `ErrEventOverflow`.
- **macOS / BSD: kqueue, not FSEvents.** fsnotify opens one fd per watched file *and*
  directory. The local Claude tree would need about 10,000 fds, which exceeds typical
  per-process limits. Recommendation: on darwin, watch only the roots and the project
  directories of in-window sessions, or don't watch at all (Q5).
- **Windows: ReadDirectoryChangesW.** Recursive mode is not exposed through fsnotify's API,
  so watches are added per directory, as on Linux. Buffer overflow (64K default,
  `WithBufferSize`) arrives as `ErrEventOverflow`.
- **Network and special filesystems** (NFS, SMB, FUSE): fsnotify's docs say events
  "generally don't work". A WSL user pointing a root at `/mnt/c` most likely falls in this
  category. That is my inference; the docs don't mention 9p or drvfs, and I haven't tested
  it. A watcher can't detect this case, which is one reason the ticker never goes away.

### 3.4 Coalescing, overflow, fallback

- **Debounce:** trailing-edge, 100 ms by default, capped at a 500 ms maximum delay so that
  a continuous WAL stream still yields a poll at least every 500 ms. If a poll is already
  running when a signal arrives, one more poll is queued, never several. This uses
  `internal/clock` so tests can drive it.
- **Overflow (`ErrEventOverflow`):** trigger an immediate full `PollOnce`, which is already
  a full rescan, and keep watching.
- **Add failure (ENOSPC, EMFILE, permission):** stop adding watches for that source, close
  its watcher, and return from `Watch` with an error. The monitor logs it once and falls
  back to plain ticker polling for that source. A partial watch set is never trusted.
  Whether health should report this is Q6; health `LastError` is a fixed category set
  (design rule 10).
- **Missing root:** keep polling until the root appears, then start watching.
- **Directory removed:** fsnotify removes the watch itself; the source drops its own record
  of the directory.

## 4. Test plan (strict TDD, race and deadlock coverage)

Each test is written first and watched failing.

1. `Run` with a mock `ChangeNotifier`: a signal triggers `PollOnce` before the ticker fires
   (fake clock). A burst of N signals produces one poll. A signal during a poll queues
   exactly one more poll.
2. When `Watch` returns an error, `Run` keeps polling on the ticker and logs once.
   Cancelling ctx makes `Run` and every `Watch` goroutine return. Each `Watch` returning is
   asserted directly through a done channel in the mock. The deadlock test pattern is
   lifted from agent-racer.
3. A slow sink plus a signal storm doesn't deadlock and keeps poll calls bounded.
4. Unit tests for `filewatch.Watcher` with an injected fake event stream: overflow triggers
   a rescan callback; directory-create adds a watch and rescans; an Add error tears the
   watcher down; events are filtered by pattern.
5. Real-filesystem integration test (Linux, `-race`): append to a jsonl, create a project
   directory and then a file in it immediately (the watch race), write a SQLite WAL. Each
   produces a poll within the max delay.
6. Per source: the watch set matches the walker's directory set, including `subagents/`
   for Claude and db/wal names for antigravity and opencode.
7. Slice 4 only: with a dropped event, the safety-net full poll still delivers the update
   before `staleThreshold`.

## 5. Slicing

0. **No dependency (recommended now, separate ticket):** add the stat fast path to the
   jsonl sources' Parse and re-measure the §2 table. Consider giving `examples/httpserver` a
   discover window.
1. `source.ChangeNotifier`, plus `Run` wake-ups and debounce using a mock source. Still no
   dependency.
2. `internal/filewatch.Watcher` on fsnotify; this slice adds the dependency. Linux first,
   then Windows. darwin per Q5.
3. Wire up claude, then codex, pi and cursor, then antigravity and opencode.
4. Optional: event-gated Parse with a safety-net poll, only if slice 0 leaves a measurable
   cost.

## 6. Open questions for Mark

1. **Do you want lower latency than `pollInterval` gives?** If not, the recommendation is to
   do slice 0 and close yk6 without fsnotify.
2. Do you accept `github.com/fsnotify/fsnotify` as a direct dependency? Its only import is
   x/sys, which is already indirect.
3. Should `source.ChangeNotifier` be a new public optional interface? Alternative: keep
   everything source-internal and expose only a monitor-side `WithWake(<-chan struct{})`.
4. Should events be opt-in (`WithFSEvents`) or on by default? And when events are active,
   should the ticker stay at `pollInterval` or slow to a fallback interval (e.g. 30s)?
5. darwin: accept the kqueue fd cost, use a narrowed watch set, or poll only?
6. Should a degraded watcher appear in `Health`? That would need a new fixed `LastError`
   category or a new boolean field, either of which is a public API change.
