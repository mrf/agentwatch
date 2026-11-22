# Session-end detection via agent process liveness (agentwatch-6pz)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-6pz. Grounded in the tree at `de58e50` (includes agentwatch-q3v).

## 0. Plan / ADR conflicts (read first)

Signing off means the first implementation commit edits these, then code follows.

1. **ADR 0006 (Proposed) — integration pattern cannot feed lifecycle.** ADR 0006 puts
   process data in a public `integrations/process` package delivered through an
   `EnrichmentSink` decorator. A sink sees events *after* state commits (design rule 6) and
   cannot change monitor state, so it cannot end a session. This doc moves session→process
   mapping into the **sources** (each one knows its agent's on-disk process record) and
   shares OS code through an **internal** package. Proposed ADR edit: add a "Lifecycle use"
   section stating that liveness for lifecycle is source-internal, and that
   `integrations/process` (still unbuilt — no `integrations/` dir on main) remains the
   consumer-facing enrichment API if ever needed.
2. **ADR 0006 §2/§7 are stale.** It cites `sources/gemini/procmap.go` as precedent; that
   became `sources/antigravity/procmap.go` and agentwatch-q3v deleted it. **There is no
   `/proc` code on main today** (`rg -l proc --type go` hits only unrelated files). ADR
   0006 §7 ("Gemini procmap consolidation") should be marked obsolete.
3. **ADR 0006 §1 — public vs internal.** It makes the process package public. CLAUDE.md
   ("do not promote an internal package speculatively") and plan §12.12 argue the shared
   code starts as `internal/proclive`. No public surface is added by this doc.
4. **Plan §4.5 / ADR 0005 — no new transition, one clarification.** Process exit is
   reported as `Update{Terminal: true, EndReason: "process exited"}`, which is the
   existing row "Active | source update has `Terminal=true` | terminal | terminal". But the
   table is silent on a `Terminal=true` update for a session that is *already* terminal,
   and the code resumes it: in PollOnce's `switch` the
   `existing.Lifecycle == LifecycleTerminal` case comes before `case update.Terminal`, so
   the update is applied as a `resumed` and the session goes active again (code reading;
   not reproduced here — filed as a finding, since Claude's end markers can hit it too when
   a marker arrives after the stale sweep). Proposed row:

   | Current condition | Trigger | State after | Event |
   |---|---|---|---|
   | Terminal | source update has `Terminal=true` and no other data | terminal | none |

   Without it, any session that went stale before its process died would be resurrected
   by the exit report. (Alternative in open question 2.)
5. **Design rule 3 (no PID in public v1 state).** Kept: PIDs, process start times and
   namespace ids live only in unexported source fields (§5). Not in `SessionHandle`
   (public, JSON-tagged), `Update`, `State`, cursors, `EndReason` or health
   strings.
6. **Design rule 2 (no path defaults in monitor).** Kept: the monitor gains nothing.
   Each source gets an opt-in option whose path the consumer/example chooses.
7. **Design rule 6 (commit before delivery, outside locks).** Unaffected: the exit report
   arrives through `Parse` and takes the existing commit-then-deliver path. Probes run in
   the source, outside monitor locks.

## 1. Problem

A session ends today in one of two ways (`monitor/monitor.go`, PollOnce):

- the source returns `Terminal=true` — only Claude does, and only when the consumer
  installed the SessionEnd hook and passed `WithSessionEndDir` (`sources/claude/source.go`);
- the stale sweep fires after `staleThreshold` (default 5m, `monitor/options.go`) of no data.

So a crashed, `kill -9`'d or `tmux kill-window`'d agent keeps showing as active for 5m,
and for 30m if liveness-heartbeat (e) lands with its proposed working threshold. That doc
names the cost ("a crashed agent that was `Working` lingers up to 30m") and asks in §6 Q5
whether a process-alive probe should replace guessing from silence. This doc is that probe:
**when the OS says the agent process that owned a session is gone, end the session at the
next poll.**

## 2. Mapping a session to a live process

Liveness is tri-state and source-private: **Alive**, **Dead**, **Unknown**. Only Dead acts;
Unknown always falls back to today's stale sweep. Evidence below is from this machine
(Linux, WSL2) on 2026-11-22, collected with read-only scripts in the session scratchpad
that printed only flag names, field types and counts.

### claude — exact mapping via `~/.claude/sessions/<pid>.json` (Linux: VERIFIED)

Claude Code writes one JSON file per process, named by PID. Field types observed:
`pid` number, `sessionId` string, `cwd`, `startedAt` number, `procStart` string,
`pidDomain` string, `status` string, `updatedAt` number, plus `tmux`, `name`, `kind`,
`entrypoint`, `version`, and others.

- **810** directory entries, of which **540** `*.json`: **32** with a live PID, **508**
  with a dead one; at least one did not parse as JSON. Files outlive their process (whether clean exit removes them is UNVERIFIED), so file
  existence is not liveness.
- `procStart` equals field 22 of `/proc/<pid>/stat` (start time in clock ticks) for **8/8**
  live files checked — a PID-reuse guard.
- `pidDomain` looks like `linux:<machine-id>:pid:[<pid-namespace inode>]`; for **29/29**
  live processes its namespace inode equals `readlink /proc/<pid>/ns/pid`. Older files omit
  it. Dead files carry other namespace inodes (earlier WSL instances).
- Every live file's `sessionId` had a matching `projects/*/<sessionId>.jsonl` (8/8).
- `updatedAt` is not a heartbeat (an idle live file was 5,479s old).
- `status` ∈ `idle|busy|shell|waiting|null`. Semantics UNVERIFIED; `waiting` is a candidate
  permission-prompt signal for needs-attention slice 5 (not used here).

Rule: the session is **Alive** if any file claiming its `sessionId` has a `pidDomain` equal
to the monitor's own domain and `/proc/<pid>/stat` start time equal to `procStart`;
**Dead** if the source previously saw it Alive and no such claim remains (process gone,
start time differs, or the file was removed); **Unknown** otherwise (no claim, domain
missing or foreign, read error).

A single process can move to a new `sessionId` (`/clear`, resume). The old session then
loses its live claim while its process lives on. v1 treats only process death as Dead;
a claim that moves is Unknown (open question 5).

### antigravity — open file descriptor (Linux: OBSERVED, lifetime UNVERIFIED)

Current main (post-q3v): one SQLite `<uuid>.db` per conversation under the configured
root; `WorkingDir` comes from conversation metadata; no process inspection. A running
`agy` process held `conversations/<uuid>.db` (+ `-wal`, `-shm`) open. Mapping: a live `agy`
process (argv0 basename, as the removed procmap did) whose `/proc/<pid>/fd/*` resolves to
`h.Path`. Whether agy keeps the DB open while idle, and closes it when switching
conversations, is UNVERIFIED — spike before implementing.

### codex — open file descriptor (Linux: OBSERVED for the CLI only)

A `codex` CLI process held its `sessions/YYYY/MM/DD/rollout-*.jsonl` open (1 of 1 CLI
processes). The app-server process held no rollout (0 of 1). Whether the CLI holds the
rollout for the session's whole life, including idle, is UNVERIFIED; app-server-hosted
threads are Unknown. Same `h.Path` fd match as antigravity.

### pi, cursor, opencode — Unknown

No running process to observe. opencode stores all sessions in one DB, so an fd match
identifies a process, not a session — always Unknown. These sources change nothing.

### Other OSes

| | claude (pid file) | fd-based (codex, antigravity) |
|---|---|---|
| Linux | `/proc/<pid>/stat` field 22 + `/proc/self/ns/pid` — VERIFIED | `/proc/<pid>/fd` readlink — OBSERVED |
| macOS | pid file presumably written (UNVERIFIED); start time via `kern.proc.pid` sysctl (`x/sys/unix.SysctlKinfoProc`, already an indirect dep at v0.49.0) — UNVERIFIED; `procStart`/`pidDomain` format on macOS unknown | needs libproc `proc_pidinfo` (cgo or raw syscall) — out of scope, Unknown |
| Windows | `OpenProcess` + `GetProcessTimes` via `x/sys/windows` — UNVERIFIED; format unknown | out of scope, Unknown |
| WSL ↔ Windows | a WSL monitor cannot see Windows `claude.exe` PIDs and vice versa; `pidDomain` mismatch → Unknown | Unknown |
| Containers / other PID ns | `pidDomain` namespace mismatch → Unknown | Unknown (PIDs not visible) |

v1 ships Linux only, stubs elsewhere returning Unknown (ADR 0006 §5's stub strategy).
macOS gets its own ticket once someone can run a corpus check there.

## 3. Lifecycle effect

- **Dead → one exit report.** On the poll where a session moves Alive→Dead, `Parse`
  returns `Terminal=true`, `EndReason: "process exited"`, `EndedAt` = detection time,
  merged with any data read in the same call (final records). The source latches the
  session so it reports exit once; new data later (e.g. `claude --resume` in a new process)
  is an ordinary `resumed` and re-arms the latch when the new process is seen Alive.
- **Only observed transitions act.** A session discovered while already Dead (monitor
  started after the crash) is left to the stale sweep. That avoids needing a new
  "Untracked + Terminal" row: today a `Terminal=true` update on an untracked session takes
  the `!existed` case and becomes active/`discovered` (code reading) — open question 4.
- **Precedence with end markers.** A SessionEnd marker carries a real reason and usually
  lands first; the liveness latch is set by any terminal report, so no second report.
- **Discovery window.** `Parse` only runs for handles `Discover` returns. A silent session
  that ages out of `WithDiscoverWindow` gets no probe; the stale sweep still covers it.
- **With liveness-heartbeat (e).** Complementary: (e) lengthens tolerance for silent
  `Working` sessions; this shortens it back to one poll when the process is known dead. It
  removes (e)'s crash cost for claude (and codex/antigravity if spikes pass).
- **With needs-attention.** Same: a session blocked on a human stays active under the long
  threshold, and a crashed one ends at the next poll instead of advertising "needs you" for
  30m. needs-attention §0's normalisation (Activity → idle on terminal) applies unchanged.
- **Alive does not suppress stale** in v1. Using Alive to keep idle-but-open sessions active
  indefinitely would need a source→monitor heartbeat (liveness-heartbeat option (a)) and
  changes what `active` means for consumers — open question 3.

## 4. False positives and failure modes

| Risk | Mitigation |
|---|---|
| PID reuse | claude: `procStart` vs `/proc/<pid>/stat` field 22. fd sources: a reused PID would have to also hold the same file open — not a practical risk. |
| Multiple sessions per process | claude: one current `sessionId` per file; session switch is Unknown (above). opencode: always Unknown. agy/codex: fd per file, so per-session if the agent closes old files (UNVERIFIED). |
| Multiple processes per session | Alive if *any* claim is live. |
| Monitor in another namespace / container / WSL side | `pidDomain` mismatch or invisible PIDs → Unknown, never Dead. |
| Other-user processes, `hidepid` /proc | read errors → Unknown. |
| Partial pid-file write | JSON parse error → keep last verdict for that file this poll. |
| Agent closes its fd while idle (codex/agy) | would read as Dead → premature `terminal`, then `resumed` on next data. Gate: spikes in §7 must show the fd is held while idle, else that source stays Unknown. |
| Undocumented Claude Code file format | best-effort: unknown shape → Unknown; fixture-pinned parser; option is opt-in. |

## 5. Privacy and PIDs

- PIDs, `procStart`, `pidDomain`, fd targets live in an unexported per-source map keyed by
  session ID, pruned when a handle is no longer discovered.
- Never put in `SessionHandle`, `Update`, cursors, `EndReason` (fixed string
  `"process exited"`), or health errors (wrap with a category, no paths/PIDs — rule 10).
- **Never read argv beyond argv0.** Claude Code's argv carries the full prompt text
  (observed while surveying; this is what tripped the first survey script). The pid file's
  `cwd`, `name`, `tmux` fields are not read into state.
- Add a privacy test: an exit report's `State` JSON contains no PID digits from the
  fixture and no fixture path.

## 6. Cost

- claude: one `ReadDir` of the sessions dir per poll (810 entries here), re-parsing only
  files whose mtime changed, plus one `/proc/<pid>/stat` read per claimed, discovered
  session. Steady state ≈ 1 ReadDir + N small reads per 2s poll.
- fd sources: `ReadDir /proc`, argv0 read per process (~hundreds), `readlink` of fds of
  matching processes only. Cache the scan for one poll interval (antigravity's old
  procmap used a TTL cache; same pattern).
- Benchmark in slice 3 against a fake 1,000-entry procfs; budget < 5ms per poll.

## 7. Test plan (tests first, all under `-race`)

`internal/proclive` (fake procfs root via unexported option):
1. start time parsed from a `stat` line whose comm contains spaces and `)`.
2. own PID namespace read from `ns/pid` link; missing → error.
3. fd holders: process with matching argv0 holding the path → found; non-matching argv0
   holding it → not found; unreadable fd dir → skipped.
4. stub build: every call returns Unknown, no error.

`monitor` (mock source, fake clock):
5. Active + `Terminal=true` → `terminal` (existing pin, keep).
6. Stale-terminal + `Terminal=true`, no data → stays terminal, **no event** (new row).
7. Terminal + `Terminal=true` with data → `resumed` (pins what "no other data" excludes —
   or the chosen alternative, open question 2).

`sources/claude` (fixture sessions dir + fake procfs):
8. live claim → no Terminal; process removed next poll → one Terminal, reason "process
   exited"; following poll → no second report.
9. `procStart` mismatch (PID reused) → Dead.
10. foreign / missing `pidDomain` → Unknown, never Terminal.
11. session first seen with only dead claims → no Terminal (left to stale).
12. claim moves to another `sessionId` → old session Unknown.
13. end marker and process death in the same poll → one Terminal with the marker's reason.
14. truncated pid file → previous verdict kept.
15. privacy: no PID or path in the update or the resulting state JSON.
16. option unset → zero probe calls (fake counts reads).

codex / antigravity: same shape as 8–11 with fd fixtures, after their spikes.

## 8. Implementation slicing

1. **docs**: plan §4.5 row (§0.4), ADR 0006 amendment (§0.1–0.3). Doc-only.
2. **monitor**: Terminal-on-terminal-without-data stays terminal (tests 6–7). Independent
   of the rest; also fixes the end-marker-after-stale case.
3. **internal/proclive**: Linux + stub, fake-root tests, benchmark (tests 1–4).
4. **claude**: `WithLiveSessionsDir(path)` option + liveness tracker + exit report
   (tests 8–16). Blocked by 2, 3.
5. **codex spike** (no code): does the CLI hold the rollout open across idle turns and
   close it at exit? app-server behaviour? Short addendum to this doc.
6. **antigravity spike** (no code): same questions for `<uuid>.db`, plus conversation
   switching inside one process.
7. **codex / antigravity implementation**: only for sources whose spike passes. Blocked by
   3 and the spike.
8. **examples**: `examples/*` pass `~/.claude/sessions` explicitly (rule 2).

## 9. Open questions for Mark

1. Approach: source-internal liveness reported through `Terminal=true` (this doc), or a
   new optional `source.LivenessProber` interface the monitor calls in its sweep? The
   latter centralises policy but adds public API and a monitor-side transition.
2. §4.5: Terminal + `Terminal=true` (no data) → stay terminal, no event (this doc)? Or
   update `Reason`/`CompletedAt` silently, or emit a second `terminal` event?
3. Should Alive ever suppress the stale sweep (idle-but-open sessions stay `active` for
   hours)? This doc says not in v1.
4. Sessions already Dead at discovery: leave to stale (this doc), or add an
   "Untracked + `Terminal=true` → terminal" row so a restarted monitor ends them at once?
5. A live process switching `sessionId` (`/clear`, resume): end the old session
   immediately, or keep it Unknown (this doc)?
6. Reliance on Claude Code's undocumented `~/.claude/sessions/<pid>.json`: acceptable as an
   opt-in best-effort signal with Unknown fallback?
7. ADR 0006: amend (this doc) or supersede with a new ADR that drops the public
   `integrations/process` plan until a consumer asks for it?
8. Event shape: `terminal` with `Reason: "process exited"` (this doc, since the source
   reports it), or `stale` with a distinct reason so consumers keep treating it as
   "inferred, not declared"?
