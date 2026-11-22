# Running token totals and cost estimates (agentwatch-xsn)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-xsn (also settles the contract question in agentwatch-ugx). Grounded in
the tree at `4fc47df`.

**Recommendation up front:**

- Add cumulative per-session usage as a **delta field**, the same way `MessageCountDelta`
  works. The monitor sums it and a `Reset` replaces it. The existing Reset, l72 resume and
  fyv forget paths then give correct totals with no new monitor logic.
- Keep `ContextTokens` and `OutputTokens` as **last-turn gauges** and fix the docs to say
  so. That closes ugx without a semantic break.
- **Core ships no price table and no cost field.** Consumers compute cost from `Usage` plus
  `Model`. An example shows how.
- Claude needs **dedupe by `message.id`**. Without it, totals are measured at about 2.4×
  the true output count (§1.3).

## 0. Plan / ADR / rule conflicts (read first)

Signing off means the first implementation commit makes these edits. Code comes after.

1. **Plan §4.4 (`SessionState`)** gains `Usage session.TokenUsage` and
   `SubagentState.Usage`. §4.4 lists `BurnRatePerMinute` as "removed from public state",
   and §13 Q5 says burn-rate metrics are "app-side until proven otherwise". Totals are not
   a rate. They are the input a consumer needs to derive one, and only the source can
   produce them correctly, because only it sees dedupe and re-reads. Proposed §4.4 note:
   "Cumulative token usage is core; rates, budgets and cost are derived by consumers."
   There is precedent: `CompactionCount` was in the same "not core" list and has since
   shipped (`session/state.go`).
2. **Plan §4.1 / `SourceUpdate`** gains `UsageDelta session.TokenUsage`, which `Reset`
   covers. The `Reset` doc comment (`source/source.go`) currently says "the three count
   deltas". That wording becomes "the count and usage deltas".
3. **`docs/source-implementation.md` "Delta vs absolute fields"** says token counts are
   "absolute — pass the current total". That is wrong for `OutputTokens` today (ugx). It
   gets rewritten as described in §2.1.
4. **ADR 0003 (opaque cursor).** No change to the rule. Claude, Codex, OpenCode and
   Antigravity cursors grow fields (§3.3). Per `persistence.md` §0.3(b), an old-format
   cursor must give a `Reset`, never deltas counted against a missing base.
5. **CLAUDE.md design rule 1 / plan §1** (no game/UI concepts; UI-agnostic). A shipped
   price table or a budget/alert concept would be a product concern in core. This proposal
   ships neither (§4).
6. **Plan §5 rule 2 / `session.PrivacyFilter`.** `Usage` carries no model name, so
   `RedactModel` needs no change. A per-model breakdown, if ever added (open question 4),
   would have to collapse under `RedactModel`.
7. **Public package list:** unchanged. `TokenUsage` lives in `session`. No new package.

## 1. What exists today

### 1.1 Core fields

`session.SessionState` (`session/state.go`) has `ContextTokens`, `OutputTokens`,
`TokenEstimated`, `MaxContextTokens`, `ContextUtilization` and `Model`. `SubagentState` has
`Model`, `ContextTokens` and `OutputTokens`. `source.SourceUpdate` (`source/source.go`)
carries the same fields plus `HasTokens`, which fqq added.

How `monitor.applyUpdate` (`monitor/monitor.go`) applies them:

- **Tokens are assigned, never summed.** The three fields are assigned when
  `HasTokens || ContextTokens != 0 || OutputTokens != 0 || TokenEstimated`. Otherwise the
  previous values are kept. This is fqq's unset-vs-zero fix.
- `MaxContextTokens` applies only when it is > 0, and `Model` only when it is non-empty.
- **`Reset` does not touch tokens.** It replaces only the three count deltas and
  `Subagents`. The token gauges need nothing more, because the reset update's own gauge
  values are assigned.
- Subagent tokens merge through `overlaySubagent`, where a non-zero value overwrites.
- No source sets `TokenEstimated` to true. `rg 'TokenEstimated\s*[:=]\s*true'` finds no
  hits outside tests.

### 1.2 Per source

| Source | Raw data | What it reports as `ContextTokens` / `OutputTokens` | Cumulative data available? |
|---|---|---|---|
| claude (`sources/claude/parser.go`, assistant case) | per-message `usage{input,cache_read,cache_creation,output}` | `input+cache_read+cache_creation` / `output` of the **last** usage record in the batch | no. Has to be summed, with dedupe (§1.3) |
| claude subagents (`parseSubagentAssistantMessage`, `subagents.go`) | same, per subagent file | same, last record | no. Summed into `subagentEntry` in the cursor |
| codex (`sources/codex/source.go` `parseTokenCountInfo`) | `token_count.info.{last_token_usage,total_token_usage}` | `last.input` / `last.output`. The code deliberately ignores `total_token_usage` | **yes**: `total_token_usage{input,cached_input,cache_write_input,output,reasoning_output,total}`. Measured monotone (§1.3) |
| pi (`sources/pi/parser.go` `parseMessage`) | per-message `usage{input,output,cacheRead,cacheWrite,totalTokens,cost{…},reasoning}` | `input+cacheRead+cacheWrite` / `output`, last message | no. Also see the bug in §1.3 |
| antigravity (`sources/antigravity/parser.go`) | protobuf usage on the **latest** planner step only | `in+cached` / `out` of the latest step | no. Would need a scan of every planner step (§3.3) |
| opencode (`sources/opencode/parser.go` `queryParseData`) | `session.tokens_input`, `session.tokens_output` columns | the column values | probably cumulative, since they are session-row columns. **UNVERIFIED**: no local OpenCode DB to measure |
| cursor, mock | none | none | none |

Model sourcing: claude reads `message.model` per record and skips `<synthetic>`. Codex
reads `session_meta.model`, the `turn_context` model and the `token_count` model
(`parseModel`). Pi reads `message.model` plus `model_change.modelId`. Antigravity reads the
executor metadata blob. OpenCode reads the `session.model` JSON (`parseModelJSON`). In
every source the last value wins, and `applyUpdate` keeps the previous model when an update
has none.

### 1.3 Measurements

These come from throwaway Python scripts over Mark's local data. Nothing was committed,
and only aggregate counts were read.

- **Claude duplicates usage per content block.** Over the 200 most recent transcripts
  (main-thread files only): 29,178 assistant records with `cwd` and usage, but only 14,934
  distinct `message.id`s. All 14,244 repeats carried **identical** usage, 0 differed, and
  every repeat was contiguous with its first record. Naive summing gives output 21,544,758
  against 8,817,414 deduped (**2.44×**), and input 1.91×. So totals must dedupe on
  `message.id`. Because repeats are contiguous, storing the last seen ID in the cursor is
  enough to dedupe across a batch boundary. (Not measured: whether a poll actually lands
  between two records of one message. It is plausible, because Claude Code appends block
  by block.)
- The same data shows that `MessageCount` counts records, not messages (filed as a
  finding, not in scope here).
- **Codex:** 18,751 `token_count` events across 50 rollouts. All of them had both `last`
  and `total`. `total_tokens` never decreased. 32 events repeated an identical
  `last_token_usage`, so summing `last` would overcount slightly. Use differences of
  `total`. `cached_input_tokens ≤ input_tokens` held in every event, which is consistent
  with `input` *including* cached, but that is not proof (**UNVERIFIED**). Not checked:
  whether `total` is unchanged on the repeated events.
- **Pi:** in 3,527 real usage objects, `cost` is always an **object**
  `{input,output,cacheRead,cacheWrite,total}`, but `tokenUsage.Cost` is declared `float64`.
  A throwaway Go test (deleted afterwards) fed `parseMessage` a real-shaped assistant
  message. The result was `messageDelta=0 hasTokens=false model=""`: the whole message is
  dropped. This is a live bug, filed as a finding. The totals slice depends on it being
  fixed.
- **Model mixing:** 0 of the 200 Claude main-thread files used more than one model.
  Mixing happens across subagents, and each subagent carries its own `Model`. This is why
  §2 adds no per-model map.

## 2. Contract

### 2.1 Gauges (ugx): keep last-turn semantics, fix the docs

`ContextTokens` means the prompt size of the latest turn, including cache, which is what
`ContextUtilization` divides. `OutputTokens` means the output of the latest turn. That is
what claude, codex, pi and antigravity already do. agent-racer consumes these unchanged
(`docs/migration-from-agent-racer.md`). Redefining `OutputTokens` as cumulative would
silently change a public field's meaning, and cumulative output now has its own home in
`Usage.Output`.

OpenCode is the outlier. If its columns are cumulative, its `ContextTokens` is not a
context size. The OpenCode slice should confirm this against a real DB. Then it should
either derive the gauges from the latest message row or report the columns only through
`UsageDelta` and leave the gauges unset (`HasTokens=false`). See open question 1.

### 2.2 New types

```go
// session
// TokenUsage is cumulative token usage. Input excludes cached tokens; a
// source that cannot split cache traffic reports everything as Input.
type TokenUsage struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cacheRead,omitempty"`
	CacheWrite int64 `json:"cacheWrite,omitempty"`
	Output     int64 `json:"output"`
}

SessionState.Usage  TokenUsage `json:"usage"`           // main thread only
SubagentState.Usage TokenUsage `json:"usage,omitzero"`

// source
SourceUpdate.UsageDelta session.TokenUsage `json:"usageDelta,omitzero"`
```

- **Why `int64`:** cache reads re-count the whole context on every turn, so a long session
  passes 2³¹ easily (100k context × 25k turns), and `int` is 32-bit on 32-bit targets. JSON
  numbers stay exact up to 2⁵³.
- **Why deltas, not absolutes plus a flag:** a zero delta means "nothing new", so the fqq
  unset-vs-zero problem cannot arise, and no `HasUsage` flag is needed. With `Reset`, the
  delta is the absolute total and replaces the old value. This is exactly
  `MessageCountDelta`, which is already proven correct under the following paths:
  - **Source Reset** (truncation, replacement, bad cursor, and the pvd/9ep fixes): the
    total is replaced.
  - **l72 resume of a removed session:** the monitor re-reads from a zero cursor onto a
    fresh base, so whole-history deltas sum to the whole-history total exactly once.
  - **fyv forget, then reappearance:** a zero cursor into an empty store, reported as
    Discovered, so totals are again whole-history exactly once.
  - **persistence.md checkpoint:** state and cursor are saved together, so the delta base
    is consistent with no extra work.
- **Normalization:** `Input` is always *uncached* input. Codex reports
  `input − cached_input`, which depends on the UNVERIFIED inclusion in §1.3. That must be
  confirmed by the slice's first test against a real rollout fixture.
- **Reasoning output:** Codex `reasoning_output_tokens` and Pi `reasoning` are omitted from
  v1 (open question 3).
- **Subagents:** `Usage` covers the main thread only. Each `SubagentState.Usage` is the
  subagent's own cumulative total, assigned absolutely by the source through the
  `subagentEntry` cursor state the source already keeps. A pure helper
  `func (s SessionState) TotalUsage() TokenUsage` sums the main thread and its subagents,
  so consumers do not each reinvent it. `overlaySubagent` overwrites `Usage` when the
  incoming value is non-zero, which is the same rule its other fields follow.
- `Clone` stays shallow-safe, because `TokenUsage` is a value type.

### 2.3 Aggregate totals

**None in core for v1.** Consumers sum `TotalUsage()` over `Snapshot()`. The known
limitation is that retention removes sessions, so the sum covers retained sessions only. A
monitor-level "since start" counter would have to survive removal *and* the l72 re-read.
It would need the session's last total remembered in `m.removed`, and adding a resumed
session's whole-history delta to it would double-count. That complexity buys a number most
consumers can keep themselves from `EventDelta`. See open question 2.

## 3. Source obligations

### 3.1 Contract text (for `docs/source-implementation.md`)

"`UsageDelta` is the usage first seen in this batch. Count each API response once: if the
log repeats a response, dedupe it. If the data only gives a running total, report the
difference from the total stored in your cursor. If that total went down, re-read and set
`Reset`. On `Reset`, `UsageDelta` is the whole-history total."

### 3.2 Shared test helper

`internal/testutil.CheckRewriteResets` (`internal/testutil/reset.go`) gains a usage
assertion. After a rewrite, the update must have `Reset` set, and its `UsageDelta` must
equal a from-zero parse of the new content. The file sources (claude, codex, pi, cursor)
already call this helper, so for them the no-double-count property is enforced in one
place. opencode and antigravity are SQLite sources that do not use it, so each needs its
own reset-usage test (§6).

### 3.3 Per source

- **claude:** sum deduped usage per `message.id` and skip `<synthetic>`. Add `lastMsgID`
  to `claudeCursor`, and to each `subagentEntry` for subagent files. An old cursor without
  the field decodes to `""`, which risks at most one double-counted message at the
  upgrade boundary. Alternatively, treat a missing field as an old format and `Reset`
  (open question 5).
- **codex:** the cursor changes from a bare `jsonl.Position` to `{pos, prevTotal}`. Delta
  is `lastTotalInBatch − prevTotal`, and a decrease means `Reset`. An old cursor is a
  format change, so it gives `Reset` (one full re-read per rollout on upgrade).
- **pi:** fix the `cost` type first (finding). Then sum per-message usage. Repeats are not
  measured (**UNVERIFIED**), so the slice must check for them.
- **opencode:** the cursor already stores `tokensIn,tokensOut` (`parser.go` cursor format).
  Delta is the column difference. A decrease means a re-read with `Reset`, which matches
  pvd. There is no cache split, so everything is `Input`, assuming cumulative semantics
  hold (open question 1).
- **antigravity:** it already re-reads the whole DB on each fingerprint change and diffs
  against cursor totals (`source.go` Parse). It would add a sum of usage over every
  planner step and two cursor fields. The cost is O(steps) protobuf decodes per change
  (**UNVERIFIED** magnitude, so benchmark in the slice). If that is too slow, it reports
  no usage, which is allowed.
- **cursor, mock:** no usage. Mock gains a scripted `UsageDelta` for monitor tests.

## 4. Cost

**Recommendation: core ships no price table, no pricing option and no cost field in v1.**

- **Prices go stale and differ by contract.** They also have tiers (long-context premiums,
  batch rates, cache-write TTL classes). A table in a library either ships wrong numbers
  or forces a release on every price change. Its staleness would sit behind a public field
  that looks authoritative.
- **Consumers already hold all the inputs:** `Model` and `Usage` per session and per
  subagent. Cost is a pure function of the two. Rounding is then the consumer's choice,
  done once on cumulative totals rather than accumulated per turn.
- **A `WithPricing(func(model string, u TokenUsage) (microUSD int64, ok bool))` option**
  was considered. It only moves the multiplication into the monitor, and it adds a public
  field whose meaning depends on consumer configuration. Rejected for v1. It could be added
  later without breaking anything.
- **Pi's self-reported `cost.total`** is a cost computed by the source with its own table.
  Surfacing it would make "cost" mean different things per source. Leave it out (open
  question 6).
- **Example:** `examples/` gains a small `costestimate` function with a few clearly
  labelled, dated illustrative prices and a "verify against the provider" comment. It uses
  integer micro-dollars and rounds half-even once, at display. It lives with the examples,
  not in a library package.

## 5. Wire format and compatibility

- **Additive.** `SessionState` JSON gains `"usage":{…}` (always present), and subagents
  gain `"usage"` when non-zero. httpapi and wsapi serialize `SessionState` as-is
  (`transport/wsapi/policy.go` `applyPolicyStates`), so nothing changes in the transports.
  Older Go and JS clients ignore the unknown key.
- **ws-resume-filters.md:** usage changes ride `EventDelta` like any field. A field filter,
  if that doc adds one, gets `usage` as a filterable key. No other interaction.
- **persistence.md:** the checkpoint `state` gains `usage` automatically. Checkpoint
  version 1 is not yet shipped, so no migration is needed. If it ships first, a restored
  state without `usage` decodes to zero and stays consistent with its old cursor, because
  an old-format cursor gives `Reset` and rebuilds the total.
- **claude-subagent-cursor.md:** its "slim completed entries" option B drops token fields
  from `subagentEntry`. Under this proposal `Usage` must **not** be dropped. A re-touched
  slim entry would emit zero usage, `overlaySubagent` would ignore it, and that is safe,
  but the source would lose the cumulative base and undercount from then on.

## 6. Test plan (strict TDD: each test is written first and seen to fail)

- `session`: `TotalUsage` sums the main thread plus subagents. JSON round-trip with `usage`
  present and absent. `omitzero` on the subagent field.
- `monitor/tokens_test.go`: deltas accumulate across batches. `Reset` replaces. A zero
  delta keeps the total. l72: remove, resume, and the total equals whole-history once.
  fyv: forget, reappear, and the total is whole-history once. `Snapshot` mutation does
  not leak (clone). Race: concurrent `PollOnce`/`Snapshot` under `-race` with usage-bearing
  mock updates, extending the `monitor/race_test.go` pattern.
- `testutil.CheckRewriteResets`: the usage assertion (§3.2), which covers the file
  sources' reset tests.
- claude: a fixture with N content-block records per message gives one count. A message
  split across two `Parse` calls gives one count (via the cursor). `<synthetic>` is
  excluded. Subagent usage is cumulative across batches. An old cursor still decodes.
- codex: total-diff across batches. The repeated identical event adds nothing (only if
  `total` is unchanged; establish this first). A decreasing total gives `Reset`. An old
  cursor gives `Reset`. `Input` excludes cached.
- pi: regression for the object-shaped `cost` (finding), then usage summing.
- opencode / antigravity: column or step diffs, and decrease gives `Reset`.

## 7. Implementation slices (each its own ticket, in order)

1. **Pi cost-shape fix** (the finding). It is independent and should land first.
2. **ugx docs:** rewrite the gauge semantics in `source-implementation.md` and the plan
   §4.4 note. Docs only.
3. **Core:** `session.TokenUsage`, `Usage` fields, `TotalUsage`, `SourceUpdate.UsageDelta`,
   `applyUpdate` and `overlaySubagent`, the monitor tests, the `CheckRewriteResets`
   extension, and the mock source. Sources report nothing yet, so all totals stay zero.
4. **claude** (dedupe plus cursor fields, main thread and subagents).
5. **codex** (cursor format change).
6. **pi**, after slice 1.
7. **opencode**, which first verifies column semantics against a real DB and resolves the
   §2.1 gauge outlier.
8. **antigravity**, with a benchmark. It may conclude "no usage".
9. **`examples/` cost estimate.**

Slices 4–8 are independent of each other once slice 3 lands. Per the worktree rules, none
of them should be spawned before slice 3 merges.

## 8. Open questions for Mark

1. **ugx gauge contract.** Recommended: keep `ContextTokens`/`OutputTokens` as last-turn
   gauges, document them, and put cumulative values in `Usage`. Alternative: make
   `OutputTokens` cumulative. That is a silent meaning change for agent-racer. For
   OpenCode, should the gauges be derived from message rows, or left unset with only
   `Usage` reported?
2. **Aggregate counter.** Recommended: none in core; consumers sum `Snapshot`. Do you want
   a monitor-level since-start total despite the retention and resume bookkeeping it needs?
3. **Reasoning tokens.** Recommended: omit from v1. Codex and Pi report them separately,
   but whether Codex's `output` already includes them is UNVERIFIED. Add a `Reasoning`
   field later, as an additive change.
4. **Per-model breakdown** (`UsageByModel`). Recommended: no. Main-thread model mixing
   measured 0/200, and subagents already carry their own `Model`. Revisit if mid-session
   `/model` switches matter for cost.
5. **Claude old-cursor upgrade.** Accept a possible one-message double count at the
   boundary, or force a one-time `Reset` re-read of every live transcript?
6. **Cost in core.** Confirm: no table, no `WithPricing`, no field, and an example only.
   Should Pi's self-reported cost ever be surfaced?
7. **Field name.** `Usage` / `UsageDelta`, or `Tokens` / `TokensDelta`?
