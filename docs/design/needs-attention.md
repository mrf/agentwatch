# Needs-attention activity state (agentwatch-5c9)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-5c9. Grounded in the tree at `56fcefc`.

## 0. Plan conflicts (read first)

Signing off means the first implementation commit edits the plan, then code follows.

**Public enum change (`session.Activity`, plan §4.4 / `session/state.go`).** Add
`ActivityNeedsAttention Activity = "needs_attention"`. Today's `ActivityWaiting` means
"turn over, the human owes the next prompt"; the new value means "mid-turn, the agent is
blocked until the human decides something". Collapsing them would hide the only case
consumers want to interrupt someone for.

Compatibility:
- **Wire:** additive. `Activity.UnmarshalJSON` already accepts unknown strings, so an old
  Go client decodes `"needs_attention"` without error. JS/other clients see a new string.
- **Consumers switching on Activity** (agent-racer is the known one) hit their default
  branch. That is a behaviour change, not a break, but the release notes must say it and
  the version bump is minor-with-note (pre-1.0, so no semver obligation).
- No existing value changes meaning, except that sessions which used to show `working`
  while blocked on a question now show `needs_attention`.

**New public field (plan §4.4, §4.1).** A reason may not be free text (design rule 3:
no prompt text, assistant text or tool output; the question text of `AskUserQuestion` is
exactly that). Proposed fixed category enum:

```go
type AttentionReason string

const (
    AttentionPermission   AttentionReason = "permission"    // approve/deny a tool call
    AttentionQuestion     AttentionReason = "question"      // structured question to the user
    AttentionPlanApproval AttentionReason = "plan_approval" // plan presented, awaiting go-ahead
)
```

- `State.AttentionReason AttentionReason \`json:"attentionReason,omitempty"\``
  and the same field on `source.Update`.
- Unknown values round-trip as-is (same `UnmarshalJSON` pattern as `Activity`).
- Invariant, enforced by the monitor: `AttentionReason != ""` iff
  `Activity == needs_attention && Lifecycle == active`.
- `CurrentTool` already exposes the pending tool's *name* (rule 3 allows names, not
  content); no new field carries the tool's input.
- Privacy: add `AttentionReason` to `TestPrivacyFilter_FieldCoverage` as not sensitive.
  No new `Policy` flag.

**§4.5 transition table.** Needs-attention is an Activity change inside `active`, so the
existing "Active | parse returns new data | active | updated" row covers entry and exit.
Two edits are still needed because the table plus sweeps make promises about it:

| Current condition | Trigger | State after | Event |
|---|---|---|---|
| Active | no data past stale threshold **for its activity** | terminal | stale |
| Active, Activity=needs_attention | source update has `Terminal=true`, or stale sweep fires | terminal, **Activity cleared to idle, AttentionReason cleared** | terminal / stale |

The first row is the same clarification liveness-heartbeat.md §6 Q4 asks for; this doc
depends on it (see §3). The second is a normalisation, not a new transition: a dead
session must never keep advertising "needs you".

**Design rule 1.** Desktop/webhook notification is a consumer concern. Core exposes state
only; an `examples/notify` program may do edge detection and print/notify.

## 1. Per-source detection evidence

Corpus: last 400 top-level `~/.claude/projects/*/*.jsonl`, last 300 Codex rollouts, 95
pi sessions (scripts in the session scratchpad; no prompt text read into this doc).

### claude — PARTIAL (questions yes, permission prompts no)

**Claude Code writes nothing when a permission prompt is shown.** The last record is the
assistant record with `stop_reason:"tool_use"`; the next is the user `tool_result` after
the decision. A pending Bash call awaiting approval is byte-identical to a pending Bash
call running for ten minutes (liveness-heartbeat.md §1: 411/487 long tool calls have no
record at all in between).

The decision is recorded only after the fact, on the user `tool_result` record:

```json
{"type":"user", "permissionDecision":{"decision":"accept","source":"user_temporary"}, ...}
```

Values seen (45,846 records): `source` ∈ `config` (nearly all), `user_temporary` (24),
`user_permanent` (1), `hook` (6); `decision` ∈ `accept`/`reject`; `reasonType` ∈
`rule|classifier|mode|subcommandResults|safetyCheck|hook|other`. Of the 23 user-decided
prompts matched back to their `tool_use`, 15 were `AskUserQuestion`, 7 `Bash`, 1
`Write`; median wait 128s. Permission
mode in this corpus is `acceptEdits`/`auto` (`permission-mode` records), so prompts are
rare here but would be common under `default`.

What *is* detectable from the transcript alone:

- **Question:** last assistant record has `stop_reason:"tool_use"` with a `tool_use` block
  `name:"AskUserQuestion"` and no matching `tool_result` yet. 17 calls in corpus, 15 with
  a result; median wait
  48s, **max 1,095s** — today that session goes `stale` at 5m while the user is still
  answering.
- **Plan approval:** same shape with `name:"ExitPlanMode"`. **Zero occurrences in the
  corpus** (one `plan_mode_exit` attachment, unrelated shape). Shape assumed by analogy,
  UNVERIFIED — see open question 3.

**Permission prompts need an out-of-band signal.** Claude Code's hook system (already used
for `WithSessionEndDir` markers, `sources/claude/source.go`) has a `Notification` event
that, per Claude Code's hook docs, fires when a permission prompt is shown and carries a
notification type distinguishing permission prompts from idle prompts. **UNVERIFIED in
this session**: no local hook config uses it, and the docs domain is outside the
allowlist. The exact payload (`notification_type` values, presence of `session_id`,
whether a newer `PermissionRequest` event exists) must be confirmed before slice 5.

Known gap even with a hook: there is no "permission granted" signal. After the user
approves a long Bash call, nothing is written until it finishes, so the session would show
`needs_attention` for the tool's whole runtime. Mitigation candidates in open question 4.

### codex — NOT DETECTABLE from this corpus

- 658/658 `turn_context` records have `approval_policy:"never"` (Mark's fleet config), so
  no approval prompt can occur and none is recorded. Whether rollouts persist approval
  request events under `on-request`/`untrusted` is **UNKNOWN**.
- `turn_context.approval_policy` *is* recorded, so the source can at least know a session
  cannot block on approval.
- `request_user_input_async` (4 calls, arg key `questions`) returns its
  `function_call_output` in 44–120 ms: it does **not** block the turn. Not a
  needs-attention signal.

### pi — NO PERMISSION CONCEPT FOUND

Record kinds: `message` (roles `user`/`assistant`/`toolResult`, stop reasons
`toolUse|stop|error|aborted|length`), `model_change`, `thinking_level_change`, `session`,
`compaction`. No record outside `message` mentions permission/approval/confirm. Treat as
never entering needs-attention. (`stopReason:"error"` — 131 — is a different "needs you"
category; open question 6.)

### cursor — UNKNOWN

No local corpus. Parser (`sources/cursor/parser.go`) sees only `role` + content blocks;
transcripts carry no stop marker. No evidence either way.

### opencode — UNKNOWN

No local DB. Tool parts carry a `state` field (`"completed"` in fixtures,
`sources/opencode/parser_test.go`); whether a permission-pending state is persisted is
unknown.

### antigravity — UNKNOWN

Parser reads `messages[]` with `functionCall`/`functionResponse` parts; nothing
permission-shaped in `sources/antigravity/testdata/*`. No evidence either way.

## 2. Entry and exit (claude, transcript path)

- **Enter** when a data-bearing batch's last activity-setting record is an enriched
  assistant record whose `tool_use` blocks include an attention tool. The update carries
  `Activity=needs_attention`, `AttentionReason`, `CurrentTool` as today.
- **Stay** across zero updates: the monitor keeps stored Activity when `Parse` returns no
  data, so the source needs no extra cursor state.
- **Exit** on the next data-bearing batch: the user `tool_result` sets `Working`
  (existing path), interrupts and new prompts likewise.
- Mixed message (attention tool plus other `tool_use` blocks): attention wins.
- Subagents: parent session only in v1. `SubagentState.Activity` never takes the new
  value (open question 5).
- Monitor `applyUpdate`: when `u.Activity != ""`, set `AttentionReason = u.AttentionReason`
  if Activity is needs_attention, else clear it. A reason without the Activity is ignored.

## 3. Interaction with stale/liveness and terminal states

- Today (`monitor/monitor.go` stale sweep) every active session goes stale at 5m of
  silence. A session blocked on a human is precisely the one that must not vanish.
- Proposal: needs_attention sessions use the *working* threshold from
  liveness-heartbeat.md option (e) (proposed 30m), not the 5m one. That design is not yet
  implemented (`rg WorkingStale` finds nothing), so either it lands first or this ticket
  adds the per-activity threshold itself (open question 2).
- Terminal update or stale sweep on a needs_attention session: monitor clears Activity to
  `idle` and AttentionReason before committing (§0 second row).
- Resume (`resumed` from terminal/removed): attention comes only from the resuming update.

## 4. Events and transports

- Entering or leaving needs-attention is an ordinary `updated` lifecycle event plus a
  `delta` carrying the session. No new `LifecycleEventType`: Activity is state, and adding
  an event type would need a §4.5 row for every activity edge.
- Consumers detect the edge by comparing the previous `Activity`. `examples/notify` shows
  the pattern (keep last Activity per ID, act on `→ needs_attention`).
- httpapi/wsapi: no code change; both serialize `State`. Add JSON-shape tests that
  pin `attentionReason` present-when-set and omitted otherwise.

## 5. Test plan (tests first, all under `-race`)

`session`: JSON round-trip of `needs_attention` and each reason; unknown reason
round-trips; `omitempty` when empty; `Clone` copies it; field-coverage sentinel updated.

`monitor` (mock source, fake clock):
1. Update with needs_attention + reason → stored, `updated` event, delta carries both.
2. Following update with `Working` → reason cleared.
3. Reason without needs_attention Activity → ignored.
4. needs_attention silent 10m → still active; silent past working threshold → `stale`,
   Activity idle, reason cleared.
5. `Terminal=true` update on a needs_attention session → terminal, reason cleared.
6. Zero updates leave needs_attention untouched.
7. Race: concurrent `PollOnce` + `Snapshot` with sessions flipping in and out.

`sources/claude` fixtures (new `testdata/ask_user_question.jsonl`, synthetic text):
pending `AskUserQuestion` → needs_attention/question; next batch with its `tool_result` →
Working; mixed tool_use message → attention; sidechain `AskUserQuestion` ignored; pending
`Bash` → Working (pins that permission is *not* guessed from silence).

Transports: one JSON-shape test each.

## 6. Implementation slicing

1. **plan**: edit §4.4 (field + enum), §4.5 (two rows), §4.1 (`Update` field).
   Doc-only.
2. **session**: `ActivityNeedsAttention`, `AttentionReason` type/consts/field, JSON,
   privacy sentinel. Blocked by 1.
3. **monitor**: applyUpdate invariant, terminal/stale normalisation, needs_attention in
   the long stale threshold (or after liveness-heartbeat lands). Blocked by 2.
4. **claude (transcript)**: AskUserQuestion (and ExitPlanMode per Q3) detection +
   fixtures. Blocked by 2.
5. **claude permission spike** (research, no code): confirm the Notification /
   PermissionRequest hook payload against Claude Code docs, prototype a marker, measure
   the post-approval gap. Produces a short design addendum; implementation is a later
   ticket gated on it.
6. **transports**: JSON-shape tests. Blocked by 2.
7. **examples/notify**: edge-detecting consumer printing to stdout. Blocked by 3.

## 7. Open questions for Mark

1. Name and shape: new Activity value plus `attentionReason` (this doc), or leave
   Activity alone and add only an `attention` object? The latter is wire-safer for
   agent-racer but keeps a blocked session reading `working`.
2. Stale policy: land liveness-heartbeat (e) first and reuse its working threshold, or
   give needs_attention its own threshold (or none — never stale while blocked)?
3. Include `plan_approval` / `ExitPlanMode` in v1 with no corpus evidence, or ship only
   `question` + `permission` and add it when a fixture exists?
4. Permission prompts via hook marker: worth the consumer setup cost (they must install a
   hook, like session-end today)? If yes, which exit rule for the post-approval gap —
   clear on a short timer after the marker, clear on any later hook event (PreToolUse /
   PostToolUse marker), or accept that long approved tools show needs_attention until done?
5. Subagents: parent-only in v1, or also mark `SubagentState.Activity`?
6. Should "stopped on error" (pi `stopReason:"error"`, Claude synthetic API-error turns)
   be an attention reason, or stay out of scope for this ticket?
7. Normalise to `idle` on terminal/stale (this doc), or keep Activity as-is and rely on
   consumers checking `Lifecycle`?
