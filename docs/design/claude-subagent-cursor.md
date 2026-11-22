# Bounding the Claude cursor's `Subagents` map (agentwatch-mqj)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-mqj. Grounded in the tree at `ac29852`.

**Recommendation up front: close agentwatch-mqj without changing the cursor.** The map does
grow linearly, at about 420 bytes per subagent. At the subagent counts seen on this machine
that is tens of KB, and JSON is only 15–25% of Parse's cost. The rest is per-file I/O, and
compaction does nothing for it. The two cheap compaction schemes (tombstone, cap+evict) are
incorrect, and I reproduced both failures. The only safe scheme saves roughly a third of each
entry and isn't worth its own format change. The real per-poll cost is the stat fast path
that `fsnotify-discovery.md` §2(a) already proposes, and that doc is where it belongs.

## 0. Plan / ADR conflicts

Under the recommendation there are none: no code, plan, ADR or public-package edit. If Mark
picks option B (§4) instead:

- **ADR 0003 / plan §4.1 (opaque cursor):** no edit. The format change stays inside
  `sources/claude`, and old cursors still decode because the dropped fields are simply
  ignored (`decodeCursor` in `source.go`), so no `Reset` is forced.
- **CLAUDE.md design rule 3 (no text in public state):** unaffected. `subagentEntry` is
  `jsonl.Position` plus the already-public `session.SubagentState`, as audited in
  `persistence.md` §2.
- **Plan §4.5 transition table:** unaffected. Subagent activity is not session lifecycle.

## 1. What reads `claudeCursor.Subagents` today

All in `sources/claude`:

| Reader | Needs from a **completed** entry |
|---|---|
| `readSubagentFiles` (`subagents.go`): `cc.Subagents[id]` → `jsonl.ReadFrom(…, entry.Position, …)` on **every** `agent-*.jsonl` in the directory, every poll | `Position`. Without it the file is re-read from 0. `State.ParentID` too, or the `.meta.json` sidecar is re-read every poll |
| `accumulateSubagent` (`subagents.go`): counts are added, `StartedAt` keeps the earliest value, terminal stays terminal | `MessageCount`, `ToolCallCount`, `StartedAt`, `Activity`, but only if the entry is touched again |
| `knownParents` (`subagents.go`) → `parseLinesWithParents` → `checkSubagentCompletion` (`parser.go`) | `ParentID`. Only matters before completion: a parent `tool_result` is consumed once and the parent `Position` moves past it |
| `pruneParents` (`subagents.go`, from agentwatch-fyv) | `Activity == terminal`. If the entry is missing, the zero value isn't terminal, so the `Parents` link is never pruned |
| `subagentStates(touched)` → `Update.Subagents` (`source.go` `Parse`) | the full cumulative `State`, because that is what gets emitted |

On the monitor side (`monitor.go`), a non-Reset update goes through `mergeSubagents`, then
`overlaySubagent`. **Only non-zero incoming fields overwrite**, and subagents missing from the
update are kept. A `Reset` update replaces the list (`mergeSubagents(nil, u.Subagents)`).

**The reset and re-read path doesn't depend on completed entries.** A rewritten parent or an
undecodable cursor sets `cc = claudeCursor{}` (`Parse`), so every subagent file is re-read
from 0 and every entry is rebuilt. When a single subagent's own file is rewritten,
`readSubagentFiles` zeroes that entry's counts and re-reads it. Compaction can only cause
trouble on the **non-reset** path, when an entry that was compacted is touched again.

## 2. Measurement

I wrote a throwaway test in `sources/claude` (not committed). It builds a synthetic session in
the `testdata/subagent_layout` shape: N `agent-<17 chars>.jsonl` files of 3 records each,
`.meta.json` sidecars, and a parent with N `Agent` tool_uses, all completed by `tool_result`.
It then ran 50 polls, each appending one parent line ("dirty") and then doing a no-op Parse
("idle"). WSL2 ext4.

| N subagents | cursor bytes | dirty Parse | idle Parse | cursor decode+encode |
|---:|---:|---:|---:|---:|
| 0 | 87 | 0.12 ms | 0.11 ms | 0.003 ms |
| 10 | 4,515 | 0.93 ms | 1.0 ms | 0.26 ms |
| 100 | 44,235 | 10.4 ms | 9.1 ms | 1.6 ms |
| 300 | 132,534 | 27.0 ms | 27.4 ms | 7.1 ms |
| 1000 | 441,497 | 84 ms | 81 ms | 20 ms |

Breakdown at N=1000: decode 13.8 ms, encode 7.2 ms, and `readSubagentFiles` 61 ms (open,
fstat and a ≤4 KiB head-hash read per file: `maxHeadLen`, `internal/jsonl/position.go`). At
N=100 the same split is 1.0, 0.5 and 8.7 ms.

A sample entry, 420 B:
`{"offset":1150,"dev":…,"ino":…,"headLen":1150,"headSum":…,"state":{"id":"a…","parentId":"toolu_…","activity":"terminal","currentTool":"Read","startedAt":…,"lastActivityAt":…,"model":"claude-sonnet-4-6","contextTokens":50,"outputTokens":6,"messageCount":3,"toolCallCount":1,"completedAt":…}}`.
The ID also appears a second time, as the map key.

**How big is N in practice?** `fsnotify-discovery.md` §2 counted the real `~/.claude/projects`
on this machine: **323 subagent transcripts across 1,179 sessions in total**. So no session can
exceed N=323 (about 136 KB of cursor), and the typical session is far lower. I did not measure
the per-session distribution, because reading outside the repo would stall on a permission
prompt (UNVERIFIED; open question 3).

What the numbers show:

1. Growth is exactly linear, at about 420 B per subagent, and has no other driver.
2. Per poll, JSON is 15–25% of Parse cost. The other 75–85% is touching every subagent file,
   and no cursor design removes that.
3. The idle Parse costs as much as the dirty one, because `decodeCursor` and
   `readSubagentFiles` run on every call. Only the encode is skipped when nothing is dirty.

## 3. Failure modes I reproduced (throwaway test)

- **Cap+evict, where a completed file-backed entry is deleted while its file still exists.**
  On the next Parse the file is re-read from 0 and the subagent is emitted again as
  `activity=waiting`, `completedAt=zero`. The cursor stores it as `waiting`. Because the parent
  `tool_result` has already been consumed, **it never becomes terminal again**. The monitor's
  overlay replaces `terminal` with `waiting`. Eviction also leaves a progress-tracked
  subagent's `Parents` link unpruned (see `pruneParents` in §1), which brings back the growth
  agentwatch-fyv removed.
- **Tombstone, which keeps `Position`, `ParentID` and terminal but drops counts and times.** It
  is invisible while the file is static (zero subagents emitted). After one more line is
  appended to the completed file, it emits `msgs=1` and `startedAt=10:09` where the true
  values are 4 and 10:00. The overlay overwrites non-zero fields, so **the monitor's count
  regresses from 3 to 1 and `StartedAt` moves later**. Whether Claude Code ever appends to a
  finished subagent transcript, for example on an agent resume, is UNVERIFIED (open question 2).

## 4. Options

| | Scheme | Saves | Correct? |
|---|---|---|---|
| **A** | Do nothing (recommended) | — | yes |
| B | Slim completed entries: keep `Position`, `ID`, `ParentID`, `Activity`, `StartedAt`, `MessageCount`, `ToolCallCount` and `CompletedAt`. Drop `Slug`, `Model`, `CurrentTool`, `LastActivityAt` and the token counts. Omit `State.ID`, which duplicates the key, and refill it on decode | about 130 of 420 B (estimate, not measured) | yes. A re-touched slim entry emits zeros for the dropped fields, and `overlaySubagent` ignores zeros. Monotone fields stay correct |
| C | Tombstone (§3) | about 60% | **no**, if a completed file can grow |
| D | Cap+evict (§3) | bounded | **no**, for file-backed subagents. It resurrects them and undoes fyv |
| E | Stop storing completed entries entirely and gate the file loop on a stat check | everything | needs `Position` to decide whether to skip, so it reduces to B or C |

B makes the cursor about 30% smaller but leaves it O(N), and it leaves the 75–85% I/O share
alone. The per-poll fix is §2(a) of `fsnotify-discovery.md`: compare the size and inode
already in `entry.Position` against a `Stat` and skip `ReadFrom` when they match. That doc
already says Claude must stat its subagent files. **Its slice 0 should state explicitly that
terminal subagent entries are kept and stat-gated, never evicted**, so §3 doesn't come back by
another route. That is a one-line addition to that doc, not part of this one.

## 5. Interactions

- **agentwatch-fyv (b4cf87c).** `pruneParents` is correct only while every subagent in
  `Parents` is still in `Subagents`. A and B keep that true. D breaks it.
- **`persistence.md`.** Checkpoints store the cursor verbatim, with a 64 MiB read cap (§4 of
  that doc). The worst case on this machine is about 136 KB for one session, and the total of
  all subagent entries is also bounded by 323 × 420 B. That is negligible. Neither A nor B
  forces a `Reset` on restore.
- **Monitor-side state grows at the same rate, and it's public.** `session.State.Subagents`
  keeps every subagent ever seen (`mergeSubagents` keeps the ones an update omits). It is
  cloned and sent through the transports with the session's state. Compacting the cursor
  doesn't bound it, and bounding it would change public behaviour. I haven't measured it, and
  it is out of scope here. It is logged as a finding for the parent to triage.
- **`ws-resume-filters.md`** open question 6 (`session=<id>` matching subagents) is unaffected.

## 6. Test plan (only if B is chosen)

Write these tests first, each one red before the code.

1. Round-trip: after a completed subagent is slimmed, `decodeCursor` restores `State.ID` from
   the key, and a cursor in today's format still decodes with no `Reset`.
2. A slim entry whose file grows: the emitted counts are the old total plus the delta,
   `StartedAt` stays the earliest, `Activity` stays terminal, and after the monitor merge
   (`monitor` package) every field matches an unslimmed run.
3. No resurrection: a completed file-backed subagent stays terminal across 20 idle polls.
   This test is worth adding under A too, as a guard against future eviction.
4. Size bound: cursor bytes ≤ base + N × (slim size) for N=100.
5. Concurrency: no new shared state. `Parse` mutates a cursor decoded per call, so the
   existing `-race` run of `sources/claude` is enough. There is no deadlock surface.

## 7. Slicing

Under A:
- (a) Close agentwatch-mqj, citing §2.
- (b) Optional, test-only: add §6 test 3 as a regression guard.
- (c) Add the "terminal entries are stat-gated, never evicted" line to
  `fsnotify-discovery.md` slice 0.

Under B: a single ticket covering §6 tests 1–4 and the slimming in `putSubagent` and
`decodeCursor`.

## Open questions for Mark

1. Do you accept A (close as negligible), or do you want B anyway, for a smaller persisted
   checkpoint?
2. Does Claude Code ever append to `agent-<id>.jsonl` after the parent's `tool_result`, for
   example on an agent resume? If it never does, C becomes safe, but I would still not
   recommend it: it saves little and fails silently.
3. Can you run this, so the per-session distribution backs up §2?
   `for d in ~/.claude/projects/*/*/subagents; do ls "$d"/agent-*.jsonl 2>/dev/null | wc -l; done | sort -n | tail -5`
4. Should monitor-side `State.Subagents` growth (§5) get its own design ticket?
