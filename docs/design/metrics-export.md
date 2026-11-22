# Metrics export adapter (Prometheus first)

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-u0q. Grounded in the tree at `ac29852`.

**Recommendation up front:** ship a Prometheus exporter as a **separate Go module**,
`github.com/mrf/agentwatch/metrics/promexport`. It is one type that is both a
`prometheus.Collector` and a `monitor.EventSink`:

- **Gauges are pulled at scrape time** from `Monitor.Snapshot()` and `Monitor.Health()`.
- **Counters are pushed** by a sink that only does O(1) increments.

Core takes no new dependency. Don't build OpenTelemetry yet: an OTel Collector can scrape
the Prometheus endpoint. Don't build a neutral metrics layer in core either, because it
would be a speculative public API (plan §12 rule 12).

Two findings change what "token counters" can mean (§2.3). First, there is no cumulative
token count in the public API today. Second, `OutputTokens` does not match its documented
contract.

## 0. Plan / ADR / CLAUDE.md conflicts (read first)

Signing off means the first implementation commit makes these edits, then code follows.

| Where | Says today | This proposal | Edit sign-off requires |
|---|---|---|---|
| plan §2 / CLAUDE.md "Package Visibility" | Public list has no metrics package | Adds `metrics/promexport` as a public package in its own module | Add it to both lists, and note "separate module" |
| plan §3 Module Layout | One `go.mod` at the root | Second `go.mod` at `metrics/promexport/` (plus one for its example, §7) | Add the nested module to the layout tree |
| CLAUDE.md "Build Gate" | `go vet ./...`, `go test ./...` at the root | Root `./...` skips nested modules, so the gate must also run in `metrics/promexport` | Add a second gate block (or a loop over modules) |
| `.github/workflows/ci.yml`, `.github/dependabot.yml` | Root module only | CI job and Dependabot `gomod` entry for the nested directory | Mechanical, in slice 1 |
| plan §9 HTTP | Routes: sessions, sessions/{id}, healthz, sources | Optional `GET /metrics` through `httpapi.WithMetricsHandler(http.Handler)` (§5). Only if Q4 = yes | Add the option and the route to §9 |
| plan §13 Q5 | Burn-rate/churn metrics: "app-side until proven otherwise" | Consistent. No burn-rate, churn or derived token-rate metrics | none |
| `cmd-binary.md` (proposal) | Binary lives in the root module | A `-metrics` flag would pull `client_golang` into the root `go.mod` | Q6 |

No ADR is contradicted. ADR 0004's delivery contract (events delivered after commit and
outside locks) is exactly what the sink half relies on.

Drift found while grounding this doc (filed in `.orchestrator-findings.jsonl`, not fixed
here): plan §4.6 says sink errors are "surfaced through monitor health". In fact
`monitor.go:(*Monitor).emitEvent` only logs them, and `Health` has no sink field. So a
`sink_errors_total` metric cannot be built from the public API (§2.4).

## 1. Problem

The monitor already has a pull API and a push API (plan §1). Users who run Prometheus and
Grafana have no metrics endpoint, so they would need a custom sink and their own metric
naming. The ticket asks for a standard exporter that keeps core dependency-free and honors
the sink rule: `EventSink` "must return quickly" (`monitor/events.go:EventSink` doc,
plan §4.6).

## 2. Signals that exist today

Every metric must come from the public API: `Monitor.Snapshot`, `Monitor.Health`,
`Monitor.Sources`, and the `monitor.Event` stream.

### 2.1 Pull: computed at scrape time

| Metric | Type | Labels | Derived from |
|---|---|---|---|
| `agentwatch_sessions` | gauge | `source`, `lifecycle`, `activity` | Count of `Snapshot()` entries. This includes terminal sessions still inside the retention window (`monitor.go` retention sweep) |
| `agentwatch_context_utilization_max` | gauge | `source` | Max `ContextUtilization` over active sessions with `MaxContextTokens > 0` (`session.ComputeContextUtilization` returns 0 otherwise). This is the alertable "a session is near its context limit" signal |
| `agentwatch_context_tokens` | gauge | `source` | Sum of `ContextTokens` over active sessions. Includes `TokenEstimated` sessions |
| `agentwatch_source_status` | gauge (state set, 0/1) | `source`, `status` | `Health()[src].Status`, one of the three `HealthStatus` values |
| `agentwatch_source_discover_failures` | gauge | `source` | `Health.DiscoverFailures`, the current run of consecutive failures (`health.go:Health` doc) |
| `agentwatch_source_parse_failures` | gauge | `source` | `Health.ParseFailures`, the number of sessions not yet recovered |
| `agentwatch_source_last_error` | gauge (state set, 0/1) | `source`, `category` | `Health.LastError`. Already a fixed five-value category (`LastError*` consts), so it is safe as a label (design rule 10) |
| `agentwatch_source_skipped_lines_total` | counter | `source` | `Health.SkippedLines`, a running total since monitor start (`health.go`). Its documented caveat ("may count the same line again") goes in the HELP text |

Why pull instead of rebuilding gauges from deltas: `Snapshot()` is the source of truth,
so a scrape can't drift from it. It also needs no per-session state in the exporter.
`Event.Removed` is bare IDs (`events.go:Event`), so an event-driven gauge would need its
own id→source map just to decrement counts. Scrape cost is one `Snapshot()` clone plus one
`Health()` copy. Both are O(sessions) and take `m.mu` briefly (`monitor.go:Snapshot`,
`Health`). The two calls take the lock separately, so session gauges and health gauges
can be one poll apart. That is acceptable for monitoring.

Skipped lines **must** be pulled. A health event is emitted only when the status changes
(`monitor.go` PollOnce: `if h, changed := m.updateHealth(...); changed`), so the event
stream never carries a `SkippedLines` increment on its own.

### 2.2 Push: maintained by the sink

| Metric | Type | Labels | Derived from |
|---|---|---|---|
| `agentwatch_events_total` | counter | `type` | Every `Event` by `Type`. In practice only `delta`, `lifecycle` and `health`: the monitor never emits `EventSnapshot`, because only wsapi builds one (`transport/wsapi/server.go`) |
| `agentwatch_lifecycle_events_total` | counter | `source`, `type` | `Event.Lifecycle.{Source,Type}`, one of the six `session.LifecycleEventType` values. `updated` fires once per poll for each session that produced data, so its rate is a write-activity rate |
| `agentwatch_health_changes_total` | counter | `source`, `status` | `EventHealth` (status changes only). This catches flaps that happen between scrapes, which the state-set gauge would miss |

`HandleEvent` touches only `CounterVec`s, which are goroutine-safe in `client_golang`. It
does no I/O, no locking of its own and no allocation that grows with sessions. It needs no
async queue (CLAUDE.md sink rule). It ignores `Updates`/`Sessions` payloads, so the cost
per event is O(1).

`LifecycleEvent.Reason` is **not** a label. Monitor-generated reasons are fixed strings,
but `EndReason` comes from the source (`monitor.go`, `lcEvent.Reason = update.EndReason`)
and has no bound.

### 2.3 Token counters: not exportable honestly today

The ticket lists "token counters". The public API has none:

- `ContextTokens` is a current-context gauge. Claude computes it as input + cache-read +
  cache-creation for the latest message (`sources/claude/parser.go`).
- `OutputTokens` is documented as an absolute running total. `docs/source-implementation.md`
  "Delta vs absolute fields" says "token counts ... are absolute — pass the current
  total". Claude, Codex, pi and antigravity overwrite it with the **latest message's**
  output instead. Measured with a throwaway (uncommitted) test: two assistant records with
  `output_tokens` 7 and 11 in one batch give `parseLines(...).outputTokens == 11`, not 18.
  Codex reads `last_token_usage.output_tokens` (`sources/codex/source.go`). OpenCode reads
  a session-level `tokens_output` column (`sources/opencode/parser.go:discoverSessions`),
  which is probably cumulative (UNVERIFIED). So the meaning differs by source.
- Even a correct cumulative per-session total can't be summed into a Prometheus counter.
  Sessions leave `Snapshot()` at retention expiry, so the sum falls. A resumed-after-removal
  session is re-read from a zero cursor with `Reset` (`monitor.go` "re-read from a zero
  cursor"), so diffing per session would count it twice.

The same reasoning rules out `MessageCount`/`ToolCallCount` totals.

**Proposal:** v1 exports only the two context-token gauges in §2.1. A
`tokens_output_total` metric needs a monitor-level monotonic counter first. That is a core
change in plan §13 Q5's territory and is out of scope here (Q5). The `OutputTokens`
contract mismatch goes to findings as a separate bug.

### 2.4 Not observable through the public API

- Poll duration, poll count, per-source Discover/Parse latency. No event marks a poll, and
  a delta is emitted only when something changed.
- Sink errors. They are logged only (§0 drift).
- Events lost to a slow wsapi client. That is wsapi-internal, not monitor state.

Each of these would need a core hook, such as a `WithPollObserver` option. Not proposed
here; see Q5.

## 3. Package or module

| | In-module package | **Separate module (recommended)** |
|---|---|---|
| Core `go.mod` | Gains `client_golang` and its transitive requirements | Unchanged |
| Consumers who never import it | The requirements still join their module graph. MVS can raise shared versions (e.g. `golang.org/x/sys`, already an indirect dependency here). `govulncheck` and Dependabot noise apply to everyone | Unaffected |
| Versioning | Rides core tags | Own tags (`metrics/promexport/v0.1.0`). Requires a core version. Must be bumped when `Event`/`Health` change |
| Gate / CI | Free | Second gate block, CI job and Dependabot entry (§0) |
| `internal/` access | Yes | Go's internal rule is by import path, so it is *allowed* (gopls imports `golang.org/x/tools/internal/...` the same way). But it ties the module to unstable internals across versions. The design needs none (§5) |

The ticket's "core takes no metrics dependency" can only be met strictly by a separate
module. The cost is mechanical and paid once.

Name: `promexport` rather than `prometheus`, so consumers don't have to alias it against
`client_golang`'s `prometheus` package.

## 4. Prometheus, OTel, or neutral

1. **Prometheus via `client_golang` (recommended).** The pull model maps onto
   `Snapshot()`/`Health()` with no extra state. Consumers with an existing registry
   `MustRegister` it, which is the Go library idiom. OTel users scrape it with the OTel
   Collector's Prometheus receiver.
2. **OTel metric API.** Observable gauges via callbacks are a comparable fit, but the
   consumer must also wire an SDK and an exporter, and the API module tree is larger.
   Defer to a sibling module `metrics/otelexport` if someone asks. It would reimplement the
   §2 signal table and share no public code with promexport.
3. **Neutral adapter in core** (e.g. `monitor.MetricsSnapshot()`). Rejected. It is a new
   public API with no second consumer (plan §12 rule 12). The §2 table is cheap to compute
   from existing calls.
4. **Hand-rolled text exposition, zero dependencies, in-module.** Viable for gauges only:
   ~150 lines in httpapi, no sink, no new module. It loses registry integration and
   OpenMetrics negotiation, and we would own escaping correctness. This is the fallback if
   Mark prefers no second module (Q1).

## 5. API sketch (promexport)

```go
package promexport

func New(opts ...Option) (*Exporter, error)

// Bind attaches the monitor whose Snapshot/Health feed the gauges. Call once,
// before registering; a second call returns an error. Before Bind, Collect
// emits counters only.
func (e *Exporter) Bind(mon *monitor.Monitor) error

func (e *Exporter) HandleEvent(ctx context.Context, ev monitor.Event) error // monitor.EventSink
func (e *Exporter) Describe(ch chan<- *prometheus.Desc)                   // prometheus.Collector
func (e *Exporter) Collect(ch chan<- prometheus.Metric)

func WithPolicy(p session.Policy) Option   // RedactSource → source="redacted"; RedactModel suppresses the model label
func WithModelLabel(maxValues int) Option // opt-in `model` label on agentwatch_sessions only; overflow → "other"
func WithNamespace(ns string) Option      // default "agentwatch"
```

`Bind` exists because of the same construction cycle `examples/httpserver/main.go:sinkRelay`
works around. `monitor.New` needs the sink, and the gauges need the monitor. Q7 asks
whether to fix this once in core instead.

**HTTP exposure.** The exporter serves nothing itself. The consumer uses `promhttp`.
Mounting `promhttp.Handler()` next to httpapi on the same mux bypasses httpapi's Host check,
because that check lives in `(*handler).ServeHTTP` (`transport/httpapi/handler.go`) and
covers only httpapi's own routes. So a DNS-rebound page could read `/metrics`. Two options:

- **(a) recommended:** `httpapi.WithMetricsHandler(h http.Handler)` mounts `GET /metrics`
  inside httpapi. `/metrics` then gets the Host check, the remote guard and the optional
  `auth.Authenticator` from `transport-auth.md` §2/§4 for free. httpapi gains no
  dependency, because the option takes a plain `http.Handler`. Prometheus supports bearer
  tokens in scrape configs, so `auth.NewBearer` fits if that doc lands. A remote Prometheus
  then needs `WithAllowedHosts` and (under transport-auth §4) auth or
  `WithInsecureRemoteAccess`.
- **(b)** The example wraps `promhttp` with its own Host check. No core change, but every
  consumer has to copy it.

## 6. Cardinality and privacy

- **Never labels:** session ID (raw or pseudonymized: unbounded, and it churns), slug,
  `WorkingDir`, `Branch`, `CurrentTool`, `Reason`. Tool names include MCP
  `server__tool` strings that a consumer may consider sensitive. This is design rule 3 and
  plan §5 rule 2, applied to metrics output.
- **Bounded labels:** `source` (consumer-configured, `Sources()`), `lifecycle` (2),
  `activity` (4, or 5 if `needs-attention.md` lands), lifecycle `type` (6), `status` (3),
  `category` (5). Worst case for `agentwatch_sessions` with 10 sources is 10×2×5 = 100
  series.
- **Zero-fill:** emit every known lifecycle×activity combination for each name in
  `Sources()`, so a series doesn't go stale and break `== 0` alerts. The exporter lists the
  known values itself, because `session` exports no enumeration. A new core enum value
  needs an exporter bump, and a contract test (§8) catches it. Health series appear only
  after a source's first poll, because `Health()` has no entry before that.
- **Model:** off by default (`session.Policy.RedactModel` exists because model names "may
  reveal subscription tier"). When enabled, it is capped at N distinct values with
  overflow to `"other"`.

## 7. Example wiring

An example that imports promexport can't live in the root module's `examples/` without
adding the dependency to the root `go.mod`. Proposal: `examples/prometheus/` as its own
unpublished module, with `replace` directives to `../..` and `../../metrics/promexport`.
It builds a monitor with `NewMultiSink(wsSrv, exp)`, calls `exp.Bind(mon)`, registers on a
fresh `prometheus.NewRegistry()` (not the global default) and mounts it via (a) or (b). It
binds to `127.0.0.1` (CLAUDE.md). A checked-in `go.work` is an alternative (Q8). Any README
mention is shown to Mark as a diff first (readme-docs rule).

## 8. Test plan (write first, watch fail)

All in the nested module, using the `mock` source and polls driven by `PollOnce`.
`monitor.WithClock` takes an `internal/clock.Clock` (`monitor/options.go`). An
outside-module consumer can't implement it, because its `NewTicker` returns the internal
`clock.Ticker`. The nested module *can* import it by path, but only test files may do
so (§3 row "internal access").

1. **Golden scrape:** two sources, mixed lifecycle/activity. `prometheus/testutil`
   `CollectAndCompare` against expected text, including zero-filled series.
2. **Counters:** discover → update → terminal → removed → resumed. Assert
   `lifecycle_events_total` per type, and `events_total` against the count of events the
   sink saw.
3. **Health:** drive Discover failures past the threshold and back. Check the state-set
   gauge, `health_changes_total` = 2, and that `skipped_lines_total` is pulled even when no
   health event fired.
4. **Privacy:** a policy with `RedactSource`/`RedactModel`. A scrape-output scan asserts
   that no session ID, `WorkingDir`, `Branch` or tool name appears anywhere. This is a
   sentinel in the style of `TestPrivacyFilter_FieldCoverage`.
5. **Model cap:** N+3 distinct models give exactly N values plus `other`.
6. **Enum contract:** reflection or a constant list in the test fails when `session` gains
   an Activity/LifecycleEventType the exporter doesn't zero-fill.
7. **Race:** `PollOnce` loop plus concurrent `Collect` and `HandleEvent` under `-race`.
8. **Deadlock:** `Collect` called from inside `HandleEvent` (re-entrancy). Mirrors
   `monitor/race_test.go:TestRace_SinkDeliveryOutsideStoreLock`, with a timeout.
9. **Bind:** a second `Bind` errors. `Collect` before `Bind` emits counters only.
10. httpapi (if Q4 = yes): `/metrics` gets 403 on a foreign Host, 404 when the option is
    unset, and passes through the authenticator.

## 9. Implementation slicing

1. Module skeleton: `metrics/promexport/go.mod`, CI job, Dependabot entry, gate update in
   CLAUDE.md, plan/visibility edits from §0. Record `go mod graph | wc -l` for the new
   module in the commit message. The dependency footprint is UNVERIFIED until then.
2. Pull gauges plus health (tests 1, 3-pull, 6, 7, 9).
3. Sink counters (tests 2, 3-push, 8).
4. Privacy options and model cap (tests 4, 5).
5. `httpapi.WithMetricsHandler` (core, no dependency; test 10). Only if Q4 = yes.
6. `examples/prometheus` module.

Separate bug ticket, independent of the slices: reconcile `OutputTokens` with its
documented contract (§2.3).

## 10. Open questions for Mark

1. **Separate module** (recommended) or the zero-dependency hand-rolled exposition inside
   httpapi (§4 option 4, gauges only)?
2. **OTel:** defer (recommended, scrape via the Collector), or build `otelexport` alongside?
3. **Model label:** ship as opt-in with a cap, or leave it out of v1 entirely?
4. **`/metrics` via `httpapi.WithMetricsHandler`** (recommended, inherits Host and auth
   guards), or keep httpapi untouched and wrap in the example?
5. **Core observability hooks** (poll duration, sink errors, monotonic token/message
   counters): out of scope here. File as a follow-up design, or leave them app-side per
   §13 Q5?
6. **`cmd-binary.md` interplay:** if the binary should expose metrics, it needs its own
   module too, or the root takes `client_golang`. Which one?
7. **Construction cycle:** accept `Exporter.Bind`, or fix it once in core (e.g. a
   sink-factory option) so neither promexport nor wsapi consumers need a relay?
8. **Example module layout:** `replace` directives in `examples/prometheus/go.mod`
   (recommended), or a checked-in `go.work`?
