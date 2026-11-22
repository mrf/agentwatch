# wsapi resume (`?since=`) and per-source / per-session filters

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-4vz. Grounded in the tree at `80d3cef`. Prerequisite agentwatch-nj6
(snapshot/seq ordering) is merged; this doc builds on its contract.

## 0. Plan / ADR conflicts (read first)

| Where | Says today | This proposal | Edit sign-off requires |
|---|---|---|---|
| `agentwatch-plan.md` §9 WebSocket, "Snapshot/delta contract" | "There is no resume: an evicted or disconnected client reconnects for a fresh snapshot." | Resume from a bounded in-memory buffer; snapshot stays the fallback | Rewrite that sentence; add `WithResumeBuffer` to the §9 API list |
| ADR 0004, delivery contract 1 | "Consumers may use gaps to detect dropped events." | A filtered stream has gaps by design | Amend: gaps are only meaningful on an unfiltered stream. wsapi never drops silently anyway (§4) |
| ADR 0004, Consequences | "wsapi wraps `monitor.Event` directly … the wire format is the monitor format" | The snapshot frame gains one transport field, `epoch` (§2.2) | Amend 0004, or add ADR 0007 "WebSocket resume and filters" (recommended: 0007, 0004 gets a pointer) |
| `transport/wsapi/server.go` `Server` doc comment | "There is no resume point" | Superseded | Code comment, rewritten in the implementing slice |
| Plan §1 non-goals | "Replay file format", "Database persistence", "Multi-tenant isolation" | No conflict: the buffer is in memory, not a file, and not persisted. Filters are a bandwidth convenience, **not** authorization (§5) | None; §5 states the boundary |
| CLAUDE.md design rules / public packages | — | No new package or exported type. Only new `Option`s (rule 7). 400 bodies are fixed strings (rule 10) | None |

## 1. What happens today (verified on `80d3cef`)

- **Seq assignment.** `Monitor.seq` is an `atomic.Uint64` (`monitor/monitor.go` `Monitor`). It
  is incremented only at emit time inside `PollOnce`: lifecycle events first
  (`emitLifecycleEvents`), then one delta, then health events. No seq is consumed without a
  sink call. Seq starts at 0 in every process, so it is **not** unique across restarts.
- **Delivery order.** `PollOnce` is serialized by `pollSem`, so `HandleEvent` calls from the
  monitor arrive one at a time and in seq order. Tests and custom sinks can call
  `Server.HandleEvent` directly with any seq (`TestServer_SlowClientEviction` does), so the
  transport cannot *assume* contiguity. It has to check.
- **Snapshot cut.** `ServeHTTP` registers the conn (`broadcaster.addConn`), then
  `sendSnapshot` calls `Monitor.SnapshotWithSeq` and `conn.prime`. Events that arrive between
  registration and prime are held in `conn.held` (bounded by `cap(send)`) and queued behind the
  snapshot. Clients drop events with `Seq <= snapshot.Seq`, because a held event may already
  be reflected in the snapshot.
- **Fan-out.** `broadcaster.HandleEvent` applies the privacy policy, marshals **once**, copies
  the client list under `RLock`, then calls `trySend` outside the lock. If the buffer is full,
  the client is evicted (`removeConn`). There is no silent drop: a connection either receives
  every event after its cut, or it is closed.
- **Client→server.** `readPump` reads and discards. No client messages exist.
- **Snapshot contents.** The snapshot carries only `Sessions`. Health is not in it: a fresh
  client learns health from `GET /healthz` or from the next health *change*. This matters for
  §2.4.
- **Existing consumer.** agent-racer pins `agentwatch v0.1.3` and imports
  `monitor`/`session`/`source`/`sources/*`. It does **not** import `transport/wsapi`. It runs
  its own WS server and `payload`-envelope protocol. Its frontend already has a
  `lastSeq` / gap → `requestResync()` loop (`frontend/src/websocket.js:54-62`). That is the
  client pattern this proposal targets. Changing wsapi breaks nothing in racer today.

Measured with a throwaway test (not committed), `json.Marshal` of representative events:

| Event | Bytes |
|---|---|
| delta, 1 fully populated session | 697 |
| lifecycle `updated` | 249 |
| health | 210 |
| snapshot, 20 sessions | 12,343 |

## 2. Resume

### 2.1 Client protocol

```
GET /ws?since=<seq>&epoch=<epoch>
```

- `since` is the last seq the client applied. `epoch` is the value from its last snapshot.
  Both must be present, or neither. One without the other, or a non-decimal `since`, gets
  **400 `invalid resume parameters`** before the upgrade.
- **Resume accepted:** the server sends no snapshot. It sends every buffered event with
  `Seq > since`, in order, then live events. The client keeps applying events with
  `Seq > lastSeq`, which is exactly today's rule.
- **Resume not possible:** the epoch differs, `since` is older than the buffer floor,
  `since` is newer than the head, or the buffer is disabled. The server behaves exactly as
  today: the snapshot comes first, then live events. No error is sent. A snapshot always
  replaces client state, so the fallback is self-describing.
- No "resume accepted" marker is needed. The snapshot is queued before the write pump starts
  (`sendSnapshot` before `go writePump`), so if one is coming, it is the first frame. If
  `since == head`, the client receives nothing until the next live event, and its state is
  already current.

### 2.2 Epoch

Seq resets on restart. Without an epoch, a client that resumes with `since=500` against a
restarted process that has reached seq 600 would be replayed 501–600 from a **different
monitor instance** and corrupt its state silently.

- `NewServer` generates the epoch: 16 bytes from `crypto/rand`, hex. It is per `Server`,
  because the buffer is per `Server`. Two `Server`s on one monitor (for example, two policies)
  share seq but have separate epochs and buffers. That is correct: each buffer holds its own
  post-policy bytes.
- Wire: only the snapshot frame carries it, via a wsapi-local wrapper
  `struct{ monitor.Event; Epoch string `json:"epoch,omitempty"` }`. Deltas, lifecycle and
  health frames are unchanged. Old clients ignore the unknown field.
- Alternative: the monitor owns the epoch and puts it on every `Event`. That would let a
  future replay-recorder sink use it too, but it changes the public envelope for a transport
  concern. Not recommended for v1 (Q2).

### 2.3 Buffer

- An unexported ring inside wsapi holds `{seq, data []byte, meta}` for each event. `data` is
  the bytes `HandleEvent` already marshals after applying the policy. Replay therefore costs
  no re-marshal, and it can never leak more than live delivery does. `meta` is the post-policy
  session IDs and sources, for filters (§3).
- **Bounds:** both a count cap and a byte cap. The oldest entries are evicted first.
  Proposed default: **1024 events / 1 MiB**. With the sizes above, a 1 MiB cap holds
  roughly 1,500 single-session deltas, so the count cap binds first in steady state. The byte
  cap only matters for bursts, such as a first poll that discovers 200 sessions in one delta.
  - Time horizon depends on event rate. A poll in which k sessions changed emits k lifecycle
    events and 1 delta. With 3 busy sessions at the default 2 s poll that is ≈2 events/s,
    so 1024 events cover ≈8.5 minutes. fsnotify-driven polling (`fsnotify-discovery.md`)
    would raise burst rates and shorten that window. This is why the cap is in events, not
    in time.
- **Floor and contiguity:** resume is valid iff `floor <= since <= head`, where
  `floor = oldest.seq - 1`. If an append arrives with `seq != head+1` (a hole, a reset, or
  a direct `HandleEvent` from a test), the ring keeps only the new entry and
  `floor = seq - 1`. A replay therefore never crosses a gap. The `sinkRelay` in
  `examples/httpserver` drops events until `set` is called, so its ring simply starts late.
- `WithResumeBuffer(events int, bytes int)`. `0` disables resume: every connect gets a
  snapshot, as today.

### 2.4 Exact cut (no duplicates, no gaps)

Today's snapshot cut tolerates overlap and relies on the client to dedupe. Replay can be
exact, so it should be:

- `broadcaster.HandleEvent` takes `b.mu` (write lock, not `RLock`) for the ring append plus the
  client-list copy. `trySend` and eviction stay outside the lock, as today.
- A resuming registration takes the same lock for `addConn` plus copying the ring slice
  `(since, head]`. That copy is a slice of `[]byte` references, not data.
- Each event therefore lands on exactly one side of the cut. If it was appended before
  registration, it is in the replay copy and not delivered live. If it was appended after,
  it is not in the copy and is delivered live, held until prime.
- `conn.prime` gains a replay form: the write pump writes the replay slice first, then drains
  `send`. Replay is **not** pushed through `send`, so a replay longer than `sendBuffer` does
  not fail `prime`.
- A resumed client receives the health events it missed. A snapshot-fallback client still
  learns current health only as today (§1). That gap is pre-existing (Q9).

## 3. Filters

```
GET /ws?source=claude&source=codex&session=<id>[&since=…&epoch=…]
```

- Fixed per connection. To change filters, reconnect. There are no client→server subscribe
  messages, so `readPump` stays drain-only (Q4).
- Semantics: **union**. An event concerns a session if the session's source is in `source`
  **or** its ID is in `session` (Q5). Session matching is exact ID only. Subagents are not
  included through `ParentID` (Q6).
- IDs and sources are matched **post-policy**, against the identifiers the client actually
  sees. With a `Pseudonymizer`, clients filter by pseudonym. Two policy combinations are
  refused with **400 `invalid filter`**:
  - `RedactSessionID` without a Pseudonymizer, plus `session=`. Every ID is blank, so nothing
    can match.
  - `RedactSource` plus `source=`. The filter would be an oracle for the redacted field.
- Limits: at most 64 values per key, each at most 256 bytes. Over the limit → 400. Error
  bodies are fixed strings and never echo input.

| Event | Filtered client receives |
|---|---|
| snapshot | `Sessions` reduced to matches (may be empty; still sent, with `epoch`) |
| delta | `Updates` and `Removed` reduced to matches. The frame is skipped if both end up empty |
| lifecycle | sent iff `Lifecycle.SessionID` or `Lifecycle.Source` matches |
| health | sent iff there is no `source` filter, or `Health.Source` is in it. Health carries no session data |

- **Removals under a `source` filter.** `Event.Removed` is `[]string` with no source. The
  broadcaster keeps `srcOf map[postPolicyID]source`, filled from every
  `Updates`/`Sessions`/`Lifecycle` it sees and cleared on `Removed`. It is bounded by the
  store size. An unknown ID in `Removed` is **delivered**: a spurious removal is a no-op for
  the client, while a dropped one leaves a stale row forever. The alternative is adding the
  source to `Removed` in the monitor envelope (ADR 0004 change, not recommended).
- **Encoding cost.** Unfiltered clients and all-or-nothing matches (lifecycle, health, a delta
  whose updates all match) share the single marshaled `[]byte`. Only a partially matching
  delta or snapshot is re-marshaled for that client. On replay that path decodes the ring's
  immutable bytes, so the ring never retains `Event` values whose slices another sink might
  share.
- **Seq under filters.** Frames keep their monitor seq, so gaps are normal and gap detection
  is invalid (ADR 0004 amendment). On one WS connection that loses nothing: §1 shows wsapi
  evicts and never drops. agent-racer-style gap → resync must be disabled when filtering.
- **Filters + resume.** Replay applies the connection's current filter. If a client resumes
  with a *different* filter, its state would be wrong: newly matching sessions would be
  missing until their next update, and dropped ones would never be removed. The client must
  not send `since` after changing filters. The server cannot detect this case (Q4). A quiet
  filtered subscription's `since` lags the head and more often falls back to a snapshot. That
  snapshot is filtered, so it is small exactly when the lag is large.

## 4. Slow clients and rate limits (existing guarantees preserved)

- **Eviction unchanged.** A client more than `sendBuffer` frames behind on **live** traffic is
  evicted (`TestServer_SlowClientEviction`). Replay sits outside `send`, so a long replay
  doesn't count against the buffer. But while a slow client is still writing its replay, live
  frames accumulate in `send`, so the same eviction rule applies.
- **Reconnect loops.** An evicted client that resumes with the same `since` gets the same
  replay plus more. Each attempt goes through the rate limiter as today (Host → rate limit →
  Origin → auth, `server.go` `ServeHTTP`; default 12/min, burst 4), so a loop is throttled to
  429. Q8 asks whether to fall back to a snapshot when the replay would be larger than one.
- `WithMaxConnections` is unchanged. Parameter validation runs after auth and before
  `websocket.Accept`, so malformed requests never hold a connection slot. They do consume a
  rate token, which throttles parameter fuzzing.
- `conn.held` is unchanged and still bounded by `cap(send)`.

## 5. Host / Origin / auth (`transport-auth.md`)

- Check order is unchanged. The new parameter validation is one step, inserted after
  authentication and before the upgrade (between steps 6 and 7 of `transport-auth.md` §5). A rebound or
  cross-site page is rejected before its parameters are even parsed.
- **Filters are not authorization.** Any client can omit them and receive everything. This
  matches plan §1 "Multi-tenant isolation" and `transport-auth.md` §1 "every authenticated
  identity sees everything". A future identity-scoped view would be a server-forced filter
  derived from `Identity`. It is out of scope here, but this design leaves room for it: the
  filter is a per-conn value fixed at upgrade.
- **Query strings land in access logs.** `transport-auth.md` §3 rejects `?access_token=` for
  that reason. `since`/`epoch` are not sensitive. Session IDs are "potentially sensitive"
  (plan §5 item 2). Under a Pseudonymizer the URL holds pseudonyms. Under a pass-through
  policy it holds raw IDs, which the client already receives in frames, but a URL also reaches
  proxy logs. Q4 asks whether that is acceptable.
- `transport-auth.md` proposes always echoing subprotocol `agentwatch.v1`. Nothing here needs
  a new subprotocol (§6).

## 6. Versioning and compatibility

- **Existing clients** send no parameters and see byte-identical traffic, except for one new
  `epoch` field on the snapshot, which JSON clients ignore.
- **New client, old server:** the server ignores unknown query parameters, so the client gets
  a snapshot, which is correct by §2.1. The client must also filter locally, because an old
  server ignores `source=`/`session=`. A snapshot without `epoch` tells the client the server
  has no resume support, so it should not send `since` there.
- **No subprotocol bump.** The changes are additive, and every unsupported case degrades to
  today's behaviour. If filters ship in a later release than resume, `epoch` does not signal
  filter support. Clients filter defensively in any case (Q5 covers whether to echo the
  applied filter).
- **agent-racer:** no impact, because it does not use wsapi (§1). If it migrates, its
  reconnect becomes `?since=lastSeq&epoch=…`, and its gap → resync stays valid only while it
  stays unfiltered.
- **`cmd-binary.md`:** if resume defaults on, the binary needs no new flags.

## 7. Test plan (write first, watch fail; all under `-race`)

Ring (unit, unexported):
1. Count cap and byte cap evict oldest first. Floor advances. An entry larger than the byte
   cap alone leaves an empty ring with `floor = seq`.
2. A non-contiguous append resets the ring to that entry. `rangeAfter(since)` reports
   not-available for `since < floor` and `since > head`.

Resume (server, `httptest` + `dialWS` helpers):
3. Connect, read the snapshot and its epoch, receive seqs 1..5, disconnect. Emit 6..9,
   reconnect with `since=5`: the first frame is seq 6, then 7–9, then a live 10. No snapshot.
4. `since == head`: no frame until the next event. That event has seq head+1.
5. A stale `since` (evicted), a future `since`, a wrong epoch, or a disabled buffer → the
   first frame is a snapshot.
6. `since` without `epoch`, `epoch` without `since`, or a non-numeric `since` → 400. No
   upgrade, `ClientCount` unchanged.
7. **Exact cut:** a goroutine calls `HandleEvent` with contiguous seqs at full speed while 16
   clients resume concurrently, each at a different `since`. Every client's sequence is
   strictly `since+1, since+2, …` with no duplicate and no gap. Add a deterministic variant
   using `SetAfterRegisterHook`, mirroring `TestServer_DeltaDuringConnectArrivesAfterSnapshot`.
8. A replay longer than `sendBuffer` (buffer 1, replay 50) is fully delivered to a reading
   client.
9. A resumed client that stops reading is evicted under live flood (same shape as
   `TestServer_SlowClientEviction`).
10. Resume reconnects consume rate-limit tokens: the burst+1-th attempt gets 429.
11. Replayed frames are post-policy. Reuse `TestPolicy_Pseudonymize_CorrelatesAcrossEvents`
    data.
12. Deadlock coverage, each bounded by a timeout: `Stop()` during a replay write; `removeConn`
    racing a resuming registration; client disconnect mid-replay, after which the handler
    returns promptly (like `linger_test.go`).

Filters:
13. `source=`: snapshot subset, delta subset, lifecycle and health by source.
14. `session=`: exact ID; a removal is delivered; a delta with no matching entries is not
    sent.
15. Union of `source` and `session`.
16. A removal under `source=` is resolved through `srcOf`; an unknown removed ID is delivered.
17. `RedactSessionID` + `session=` → 400. `RedactSource` + `source=` → 400. A Pseudonymizer
    plus `session=<pseudonym>` matches.
18. Filter + resume: the replay is filtered, and the resumed seqs are a filtered subsequence
    of the unfiltered run.
19. Limits: 65 values or a 257-byte value → 400, and the body is the fixed string.

Regression: the existing `server_test.go`, `snapshotseq_test.go`, `policy_test.go`,
`origin_test.go`, `host_test.go`, `auth_test.go`, `ratelimit*_test.go` and `linger_test.go`
pass unmodified.

## 8. Implementation slicing

1. **Docs:** plan §9 text and API list, ADR 0007 plus a pointer in ADR 0004, and the wsapi
   package doc. No code.
2. **Ring:** the unexported ring type with tests 1–2.
3. **Resume:** epoch on the snapshot, parameter parsing, exact-cut registration, the replay
   path in `prime`/`writePump`, and `WithResumeBuffer`. Tests 3–12.
4. **Live filters:** parsing, matching, subsetting, policy refusals, and `srcOf`. Tests 13–17
   and 19.
5. **Filters on replay:** tests 18 and a filtered variant of 7.

Slices 3 and 4 are independent once 2 lands. Slice 5 needs both. Serial spawning applies per
`worktree-workflow.md`.

## 9. Open questions for Mark

1. Ship resume at all, and is it **on by default** (1024 events / 1 MiB, inert for clients
   that send no `since`) or opt-in? Recommendation: on.
2. Should the epoch be owned by the wsapi `Server` (recommended) or by the monitor and carried
   on every `Event`?
3. ADR shape: a new ADR 0007 (recommended) or amend 0004 in place?
4. Filter carrier: query parameters fixed at upgrade (recommended), with raw session IDs in
   URLs under a pass-through policy, or a first-message subscribe protocol that would also
   allow changing filters mid-stream? Under query parameters, "don't resume after changing
   filters" is a client rule the server cannot enforce. Is that acceptable?
5. `source` + `session`: union (recommended) or intersection? Should the server echo the
   applied filter on the snapshot, so clients can detect an old server?
6. Should `session=<id>` also match subagents (`ParentID == id`)? Recommendation: no for v1.
7. Malformed resume parameters: 400 (recommended, strict) or silent snapshot fallback?
8. Fall back to a snapshot when the replay would be larger than the snapshot, to break
   slow-client reconnect loops sooner? Recommendation: defer until observed.
9. Pre-existing gap, out of scope: the snapshot carries no health, so a fresh or fallback client
   doesn't know current health until `/healthz` or the next change. Should that be a separate
   ticket?
