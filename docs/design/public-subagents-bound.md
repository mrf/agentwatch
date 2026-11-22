# Bounding the public `SessionState.Subagents` list (agentwatch-901)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-901. Grounded in the tree at `1555151`.

**Recommendation up front: close agentwatch-901 without changing behaviour.** The list does
grow without bound for the life of a session, at about 391 bytes of JSON per subagent. At the
subagent counts seen on this machine that is tens of KB per delta and well under a millisecond
of CPU. Every bounding scheme is a public behaviour change, and the obvious one (drop completed
subagents) interacts badly with the agentwatch-nvj completion sweep (§4). Revisit only if the
per-session distribution (open question 2) shows a long tail past N≈300.

## 0. Plan / ADR conflicts

Under the recommendation there are none: no code, plan, ADR or public-package edit. If Mark
picks B or C (§3), sign-off would require:

- **`session` is a public package, and `SessionState.Subagents` is a public field**
  (`session/state.go`, plan §4 struct at `agentwatch-plan.md:287`). B changes its semantics from
  cumulative to live-plus-recent; C adds fields. Both are public-API changes.
- **`docs/migration-from-agent-racer.md:29`** lists `Subagents` among the "unchanged fields
  (same name and semantics)". B contradicts that line and would need a note there.
- **Plan §4.5 / `source.SourceUpdate.Reset`** (`agentwatch-plan.md:188`, `source/source.go`
  `Reset`): "`Subagents` replaces the session's subagent list". B must keep this true, so a
  Reset-rebuilt list is also filtered (§3, B).
- **CLAUDE.md design rules** 1, 3 and 6 are unaffected: no new concepts, no private data, and
  state is still committed before delivery.

## 1. Current behaviour (code-grounded)

- **Growth.** `applyUpdate` (`monitor/monitor.go`) calls `mergeSubagents(state.Subagents,
  u.Subagents)` on every non-`Reset` update that carries subagents. `mergeSubagents` keeps
  entries absent from the update, appends new IDs and overlays non-zero fields on matches.
  Nothing removes an entry. A `Reset` update replaces the list wholesale
  (`mergeSubagents(nil, u.Subagents)`), and the source then re-emits every subagent it
  rebuilt.
- **Lifetime.** The list dies with the session: terminal sessions are deleted from the store
  after `completionRetention` (default 30 s, `monitor/options.go`; sweep in `PollOnce`). So the
  bound today is "subagents spawned by one session", and a long-lived or resumed Claude
  session is the only way to get a large N.
- **Fan-out.** `Subagents` is a field of `SessionState`, so every `EventDelta` (`monitor/events.go`)
  carries the session's whole subagent list in `Updates`, and `EventSnapshot` carries it for
  every session. There is no per-subagent delta. A delta is emitted only for a session with new
  data (`update.SessionID == ""` is skipped), and the default poll interval is 2 s
  (`monitor/options.go`).
- **Copies per delta.** `state.Clone()` in `PollOnce` copies the slice (`session/state.go`
  `Clone`). `Policy.Apply` clones again before redacting (`session/privacy.go`), and the wsapi
  broadcaster calls `applyPolicy` then `json.Marshal` once per event (`transport/wsapi/broadcaster.go`
  `HandleEvent`; `sendSnapshot` does the same per connecting client in `server.go`). So the
  work is one clone, one redacting clone and one marshal, each O(N). `mergeSubagents` also
  builds an O(N) index map on every update, and it runs under `m.mu`.
- **Consumers of the retained completed entries inside the monitor.** `appendCompletions`
  (`monitor.go`, agentwatch-nvj, `01580df`) treats an entry as a *new* completion when it is
  terminal in `next` but absent or non-terminal in `prev`. It therefore relies on completed
  entries staying in `existing.Subagents`.

## 2. Measurement

A throwaway test in package `monitor` (not committed, deleted) built a `SessionState` with N
completed subagents, each with a 17-char ID, a 28-char `toolu_` parent ID, slug, model,
timestamps and counts. It then timed 2,000 iterations of each step. WSL2, one run, no
isolation from other load, so read the times as order of magnitude.

| N subagents | `EventDelta` JSON bytes | `Clone` | `Policy.Apply` | `json.Marshal(Event)` | `mergeSubagents` (one touched entry) |
|---:|---:|---:|---:|---:|---:|
| 0 | 495 | 0 µs | 0.1 µs | 7 µs | 0 µs |
| 10 | 4,419 | 7 µs | 16 µs | 74 µs | 10 µs |
| 100 | 39,609 | 50 µs | 78 µs | 505 µs | 64 µs |
| 1000 | 391,509 | 748 µs | 677 µs | 5.0 ms | 918 µs |

Per-entry cost is 391 B of JSON (measured as `len(marshal(Subagents))/N`). Growth is exactly
linear and `Marshal` dominates at about 5 µs per entry.

**How big is N in practice?** `fsnotify-discovery.md` §2 counted 323 subagent transcripts
across 1,179 Claude sessions on this machine, so no one session exceeds N=323, which is about
126 KB per delta and about 1.6 ms (extrapolated from the table, not measured). The typical
session is far lower. I did not measure the per-session distribution (UNVERIFIED, open
question 2).

What this means at the default 2 s poll, for a session that is active and has new data on every
poll (the worst case): N=100 is about 20 KB/s per connected client, and N=323 about 63 KB/s.
That is fine on loopback. The CPU is paid once per event, not once per client, because the
broadcaster marshals once. N=1000 would cost 5 ms of marshal per event, and the sink is called synchronously from
`PollOnce` (`emitEvent`), so that time is spent on the poll goroutine. It is not reachable on
this machine.

Not measured: wsapi compression (no compression option appears in `transport/wsapi`, so I
assume the library default; UNVERIFIED), JSON consumers' parse cost, and the persisted
checkpoint size (`persistence.md` row C stores `SessionState`; at N=323 it is about 126 KB per
session, bounded by the same number).

## 3. Options

| | Scheme | Bounds | Public change | Verdict |
|---|---|---|---|---|
| **A** | Do nothing (recommended) | no | none | negligible at realistic N |
| B | Drop a completed subagent from `SessionState.Subagents` once it has been terminal for a retention period (mirror `completionRetention`) | live subagents plus recent completions | semantics of an existing public field | see §4: needs an ID tombstone |
| C | Cap the list at K newest, add summary fields (for example `SubagentsCompleted int`) | K | adds public fields, changes list semantics | works, but two changes in one |
| D | Send only changed subagents in `EventDelta`, full list only in snapshots | the wire only; state stays O(N) | protocol: `Updates` stops being full per-session state | rejected: breaks every consumer that replaces a session by its delta, and `ws-resume-filters.md` assumes full states |

**B in detail** (the only option I'd consider if the tail turns out long). Dropping must also
filter the `Reset` path, or each `Reset` re-inflates the list: apply the same age filter to
`mergeSubagents(nil, u.Subagents)`. Terminal age needs a timestamp: `SubagentState.CompletedAt`
already exists, so no new field is required.

## 4. Interactions

- **agentwatch-nvj (`appendCompletions`).** If B drops a terminal entry and the source later
  re-emits it as terminal (any `Reset`, or a non-reset poll that touches the finished file),
  the monitor sees "terminal now, absent before" and records a **new completion**. The slug
  sweep then re-terminates any active session with that slug in the same source, which is the
  bug nvj fixed. B must therefore keep a per-session set of completed subagent IDs (about 17 B
  each, against 391 B), or make `appendCompletions` ignore re-emissions whose `CompletedAt` is
  older than the last poll. That set is itself unbounded, though 23× smaller. C has the same
  problem for entries pushed past the cap.
- **agentwatch-mqj (`claude-subagent-cursor.md`).** That doc recommends closing and keeps
  completed entries in the Claude cursor. B is independent of it: the cursor still
  emits a touched completed entry, and the monitor then decides whether to show it. mqj §3's
  warning (never evict a completed entry from the *cursor*, or it resurrects as `waiting`)
  does not apply here, because the monitor-side drop is of an already-terminal entry whose
  source keeps its own state. mqj §5 logged this ticket; its bound-the-cursor analysis stays
  valid whichever way this one goes.
- **agentwatch-jjh** is refactoring `monitor/` now. B touches `mergeSubagents`, `applyUpdate` and
  `appendCompletions`, so it must be sliced after jjh merges. I did not edit any of them.
- **`persistence.md`.** Checkpointed state restores through the same merge path, so B's filter
  applies on restore too (same `CompletedAt` age rule), with no format change.
- **`needs-attention.md` / `process-liveness.md`** do not touch subagents beyond what
  `needs-attention.md:160` already says (parent-only), so there is no interaction.
- **Consumers (agent-racer).** I could not inspect agent-racer's use of `Subagents`
  (UNVERIFIED). The migration doc says the field is unchanged, so a consumer that renders
  subagent history, or counts completed subagents, would regress under B and C-without-counts.

## 5. Test plan (only if B is chosen)

Write each test first and watch it fail.

1. `mergeSubagents` keeps a terminal subagent younger than retention and drops one older, using
   the injected clock (`internal/clock`); a `Reset` update gets the same filter.
2. nvj regression: a dropped completed subagent that is re-emitted terminal does **not** add an
   `appendCompletions` entry, so a resumed session with the same slug stays active.
3. A completed subagent that reappears with new activity (not terminal) is re-added as live.
4. Size bound: after 1,000 spawn-and-complete cycles with retention elapsed,
   `len(Subagents)` is bounded by the live count and the ID set stays 17 B per entry.
5. Concurrency: `-race` test with concurrent `PollOnce`, `Snapshot` and `Get` while subagents
   churn, since the filter runs under `m.mu` and clones cross goroutines. The existing deadlock
   pattern for the monitor applies, and no new lock is introduced.

## 6. Slicing

Under A:
- (a) Close agentwatch-901, citing §2.
- (b) Optional, docs only: one sentence in `docs/migration-from-agent-racer.md` that
  `Subagents` is cumulative for the session's life.

Under B (after jjh merges):
- (a) Tests 1–3 plus age filter in `mergeSubagents`/`Reset` path.
- (b) Completed-ID set and the `appendCompletions` change, with tests 2 and 5.
- (c) Migration-doc and plan wording update.

## Open questions for Mark

1. Do you accept A (close as negligible at realistic N)?
2. Can you run this so the per-session distribution backs up §2? It is the same command as
   mqj's question 3 and the same answer serves both:
   `for d in ~/.claude/projects/*/*/subagents; do ls "$d"/agent-*.jsonl 2>/dev/null | wc -l; done | sort -n | tail -5`
3. If you want a bound anyway: B (age-based drop) or C (cap plus summary counts)? Either changes a
   public field, so what retention or cap, and do any known consumers display subagent history?
4. Is 5 ms of marshal at N=1000 on the poll goroutine acceptable, or should a per-event cost
   budget be written down somewhere (it is not today)?
