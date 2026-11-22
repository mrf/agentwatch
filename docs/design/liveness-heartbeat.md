# Liveness during long tool calls (agentwatch-kjk)

Status: **proposal, awaiting Mark's review.** No implementation until signed off.

## 1. Problem

The stale sweep (`monitor/monitor.go`, "Sweep: detect stale active sessions") ends any
active session whose `LastDataReceivedAt` is older than `staleThreshold` (default 5m,
`monitor/options.go`). `LastDataReceivedAt` is set only in `applyUpdate`, which runs only
when `Parse` returns an update with a non-empty `SessionID`. A zero update persists the
cursor (cgy) and does nothing else.

Timeline for a Claude session running a 10-minute `go test` (defaults: poll 2s, stale 5m,
retention 30s):

| t | transcript | monitor |
|---|---|---|
| 0:00 | assistant record, `stop_reason:"tool_use"` | `updated`; Activity=Working, LastDataReceivedAt=0:00 |
| 0:02–5:00 | nothing written | zero updates |
| 5:00 | — | `stale`: active → terminal, CompletedAt=5:00 |
| 5:30 | — | `removed` |
| 10:00 | user record with `tool_result` | `resumed` from removed key |

Consumers see a working agent vanish for five minutes. Worse, the resume from a removed
key builds a fresh state from one batch's deltas while the cursor survived, so
`MessageCount`/`ToolCallCount` restart near zero (code reading; filed separately as a
finding, not reproduced here).

### What the transcript actually contains during the wait

The ticket assumed the batch holds progress/system/queue-operation lines. Measured on the
local corpus (last 400 top-level `~/.claude/projects/*/*.jsonl`, script in the session
scratchpad): of **487** tool calls lasting > 5m, **411 have no record at all** between
`tool_use` and `tool_result`; the other 76 contain one-off metadata bursts (`mode`,
`permission-mode`, `agent-name`, `worktree-state`, 8 `queue-operation`, 5 `attachment`).
**485 / 487 contain a silent interval > 300s**; p50 of the largest silent interval is
596s (the Bash 10-minute cap). The corpus contains **zero** `type:"progress"` records —
current Claude Code no longer writes them; the parser's progress path serves older
transcripts and fixtures.

So no option that keys on *records being written* fixes the common case. The only signal
present during the wait is state the monitor already holds: the session is `Working`
(the agent owes the next record). `stop_reason:"tool_use"` appears on 203,271 of 215,587
enriched assistant records, so Claude's `Working` is reliable at this point.

## 2. Options

| | Option | Rule 5 (opaque cursor) | Rule 6 (commit before events) | §4.5 table | Fixes measured case? |
|---|---|---|---|---|---|
| a | `Update.Heartbeat bool`: source sets it on advanced-but-unreportable batches; monitor bumps `LastDataReceivedAt`, no event | ok | ok | no new row | ≤ 2 / 487 |
| b | Sources emit `LastActivityAt`-only updates | ok | ok | no new row, but emits `updated` + delta per bookkeeping batch | ≤ 2 / 487 |
| c | Monitor-side "cursor advanced" via opaque `Equal`/`Advanced` method | bends it (monitor reasons about cursor progress), API change on `Source` | ok | no new row | ≤ 2 / 487 |
| d | Parser treats specific kinds (`queue-operation`, `attachment`, `system/turn_duration`) as data | ok | ok | no new row; emits events and may perturb Activity/counts | ≤ 2 / 487 |
| e | **Activity-aware stale threshold**: sweep uses a longer threshold for sessions whose stored Activity is `Working` | ok — no cursor involved | ok — sweep already commits under lock, emits after | same row ("no data past stale threshold"); threshold becomes per-activity | **yes** |

Notes:
- (a) is the cleanest of a–d and composes with `HasTokens`/`Reset` (it would be ignored
  whenever `SessionID` carries real data). It is still worth having only if some source is
  found to emit periodic unreportable records; none is today (below).
- (b) and (d) turn bookkeeping into `updated` events — the noise y9o removed from opencode.
  Codex/pi/cursor already behave like (b): any new line yields a `SessionID`-bearing
  update, so they have no "advanced but unreported" gap to close.
- (c) adds a method to a public interface for a signal that (a) carries without one.

**Recommendation: (e).** Defer (a) until a source demonstrably needs it.

## 3. Semantics of (e)

- New option `WithWorkingStaleThreshold(d time.Duration)`, default **30m** (open
  question 1). Validation in `New`: negative → `ErrInvalidOption`; `0` means "use
  `staleThreshold`" (old behaviour). If `staleThreshold == 0` (stale disabled), stale stays
  disabled regardless.
- Sweep: for each active session, `limit := staleThreshold`; if
  `s.Activity == session.ActivityWorking && workingStaleThreshold > staleThreshold`,
  `limit = workingStaleThreshold`. Stale iff `LastDataReceivedAt` older than `now - limit`.
  The `stale` event, `Reason`, `CompletedAt` and retention are unchanged; optionally the
  reason names the threshold used ("…past working stale threshold").
- **Which sessions:** active + `Working` only. `Waiting`/`Idle` keep 5m: there the human
  owes the next record, and going stale after 5m of no input is the existing design.
  Terminal sessions are not swept, as today.
- **No event when the longer threshold "saves" a session**: nothing changed in state, so
  there is nothing to deliver; emitting would recreate (b)'s noise.
- **Reset / HasTokens:** untouched — (e) reads stored Activity, never update fields. A
  Reset update refreshes `LastDataReceivedAt` like any update.
- **Terminal:** a `Terminal=true` update still wins immediately; (e) only governs silence.
  Claude's SessionEnd markers still end clean exits promptly.
- **Subagent-completion sweep:** independent; unchanged.
- **Cost:** a crashed or `kill -9`'d agent that was `Working` lingers up to 30m instead
  of 5m. Pre-existing misclassification makes this slightly wider: Claude labels the
  post-interrupt `[Request interrupted by user…]` user record as `Working` (12 occurrences
  in the corpus), so an interrupted-then-abandoned session also gets 30m.

## 4. Which sources are affected

(e) is monitor-only; no source changes. Its accuracy depends on each source reporting
`Working` while a tool runs:

- **claude** — `stop_reason:"tool_use"` → Working (`sources/claude/parser.go`); a user
  `tool_result` keeps it Working until `end_turn` → Waiting. Verified on corpus counts above.
- **codex** — function/command/tool items set Working; `task_complete` → Waiting.
- **pi** — `toolResult`/`bashExecution`/assistant tool stop → Working.
- **cursor** — user/assistant role mapping; tool-wait semantics unverified.
- **opencode** — `inProgress` (assistant reply without completion) → Working.
- **antigravity** — not reviewed for this doc.

For (a), had it been chosen: claude would set it for batches whose only records are
`queue-operation`, `attachment`, `system/*` other than `compact_boundary`, or self-progress;
codex/pi/cursor have no gap; opencode's `changed=false` is a DB-diff result with no
"advanced" notion, so it would never set it.

## 5. Test plan (tests first, all under `-race`)

`monitor` (mock source + fake clock):
1. `Working` session, silent 10m, working threshold 30m → still active, no events.
2. Same, silent 31m → `stale` event, terminal, then `removed` after retention.
3. `Waiting` session silent 5m+1s → `stale` (regression pin for the short path).
4. Activity flips Working → Waiting via an update; the 5m clock then applies from that
   update's `LastDataReceivedAt`.
5. `WithWorkingStaleThreshold(0)` → 5m for all (old behaviour); `WithStaleThreshold(0)` →
   never stale even when Working.
6. `WithWorkingStaleThreshold(-1)` → `ErrInvalidOption`; threshold below `staleThreshold`
   → treated as `staleThreshold` (or rejected — open question 2).
7. Race test: concurrent `PollOnce` + `Snapshot` across the sweep with mixed activities.

`sources/claude` fixture: a batch ending in an assistant `tool_use` record yields
`Activity == Working`; a following batch of only bookkeeping records yields a zero update
(pins that (e), not the source, carries liveness).

## 6. Open questions for Mark

1. Default working threshold: 30m? Long `go test -race` runs and 10-minute Bash caps argue
   ≥ 15m; crash detection argues shorter.
2. Working threshold below the base threshold: clamp or reject in `New`?
3. Keep (a) `Heartbeat` on the shelf, or add it now for forward compatibility? This doc
   says shelf: no source needs it today and an unused public field is a compatibility cost.
4. Should the §4.5 row read "no data past the stale threshold for its activity"? This doc
   treats it as a clarification, not a new transition — confirm.
5. Longer term, a per-source liveness probe (process alive, as antigravity's removed procmap did, de58e50)
   would replace guessing from silence. In scope later, or never (rule 3 keeps PIDs
   internal either way)?
