# Transport auth: shared Authenticator + shipped bearer tokens

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-los. Grounded in the tree at `ed91c50`.

## 0. Plan conflict (read first)

This proposal contradicts the plan as written:

- `agentwatch-plan.md` §1 lists "Built-in auth providers" as a v1 non-goal.
- §9 HTTP says "No auth provider built in"; §9 lists no `httpapi.WithAuthenticator`.
- §2 / CLAUDE.md "Package Visibility" do not list a shared auth package.
- §13 Q7 leaves the Authenticator shape (`(Identity, bool)`) open.

Per CLAUDE.md ("Violating them requires updating the plan first"), signing off on this
doc means the first implementation commit edits plan §1, §2, §5, §9, §13 Q7 and the
CLAUDE.md public-package list. If Mark wants to keep "no built-in providers", §3 drops
out and only §2 + §4 remain.

## 1. Problem and threat model

Today:

- `wsapi` has `WithAuthenticator(Authenticator)` with `Authenticate(*http.Request) (Identity, bool)`
  (`transport/wsapi/options.go:21`), checked after Host, rate limit and Origin
  (`transport/wsapi/server.go:100`). No implementation ships; only a test fake exists
  (`transport/wsapi/auth_test.go:15`).
- `httpapi` has only Host validation (`transport/httpapi/handler.go:75`). Anyone who can
  reach the listener reads every session's state.
- `internal/hostcheck` closes DNS rebinding: a rebound page arrives with a foreign
  `Host` and gets 403.

What Host/Origin checks do **not** cover:

| Scenario | Today | Covered by |
|---|---|---|
| Browser page on evil.example, DNS rebinding | 403 (hostcheck) | done |
| Cross-site page opening `ws://127.0.0.1` | 403 (origin) | done |
| Other local user on a shared box curls `127.0.0.1:8080` | **200** | auth (opt-in) |
| LAN host curls `<lan-ip>:8080` with a forged `Host: localhost` | **200** | remote guard (§4) + auth |
| LAN sniffing of a token over plain `ws://`/`http://` | n/a | TLS, consumer-owned |

Hostcheck defends *browsers*. A non-browser LAN client sets any `Host` it likes, so a
consumer who binds `0.0.0.0` is exposed even with no `WithAllowedHosts` entries. That
case is what the default in §4 closes.

Out of scope: TLS termination, cookies/sessions, OAuth/OIDC, per-identity authorization
(every authenticated identity sees everything — plan §1 non-goal "multi-tenant
isolation"), exposing `Identity` to handlers.

## 2. The shared Authenticator

### Where it lives

Both public transports must accept the same type, so it must be public. Options:

1. **New `transport/auth` package (recommended).** Holds `Identity`, `Authenticator`,
   `AuthenticatorFunc`, `ErrUnauthenticated`, and the bearer implementation. No
   dependency on `coder/websocket`, `monitor` or `session`.
2. `httpapi` imports `wsapi.Authenticator`: makes every HTTP-only consumer pull in
   `coder/websocket`, and makes the HTTP package depend on the WS one. Rejected.
3. A copy per package: `Identity` would be two distinct named types, so one
   implementation could not satisfy both interfaces. Rejected.

"Don't promote speculatively" (plan §2, rule 12) is about implementation details shared
between packages. This is a contract consumers implement — the category §2 says *is*
public — and two shipped packages need it today. Keep the surface minimal: no context
helpers, no token generator, no middleware export.

### Signature

```go
package auth

type Identity struct {
    ID string `json:"id"` // consumer-chosen label, never a secret
}

// ErrUnauthenticated: missing or invalid credentials. Transports answer 401.
var ErrUnauthenticated = errors.New("auth: unauthenticated")

type Authenticator interface {
    // Authenticate returns the caller's identity, an error wrapping
    // ErrUnauthenticated to deny, or any other error for an internal failure.
    Authenticate(ctx context.Context, r *http.Request) (Identity, error)
}

type AuthenticatorFunc func(ctx context.Context, r *http.Request) (Identity, error)
```

- Explicit `ctx` satisfies CLAUDE.md rule 4 (an authenticator may block on a remote
  check) and lets the transport bound it; transports pass `r.Context()`.
- `error` over `bool` resolves plan §13 Q7: it separates "denied" (401, routine, debug
  log) from "authenticator broken" (500, warn log). The bool shape cannot.

### Migrating wsapi

`wsapi.WithAuthenticator(wsapi.Authenticator)` takes the old `(Identity, bool)` shape, so
its parameter type cannot change without breaking implementers. Two paths:

- **A — non-breaking (what the ticket asked for).** `wsapi.Identity` becomes a type alias
  `= auth.Identity` (field-compatible). The old `wsapi.Authenticator` interface and
  `wsapi.WithAuthenticator` stay, marked `Deprecated:`, and are adapted internally
  (`false` → `ErrUnauthenticated`). Both packages gain `WithAuth(auth.Authenticator)`.
  The two wsapi options write one field; last wins. Cost: a permanent
  `WithAuth`/`WithAuthenticator` naming wart until v1 removes the old one.
- **B — breaking in v0.4.0.** `wsapi.Authenticator = auth.Authenticator` and
  `wsapi.Identity = auth.Identity` aliases; `WithAuthenticator(auth.Authenticator)` in
  both packages. Old implementers get a compile error (loud, not silent) and migrate in
  one line. Precedent: v0.3.0 already shipped `feat(wsapi)!` (m9j) and qw4 was `!`.

Recommendation: **B** — the module is v0, the break is compile-time, and the names stay
clean forever. A is fully workable if Mark wants zero API breaks (open question 1).

## 3. Shipped bearer-token authenticator

```go
type Token struct {
    ID     string // becomes Identity.ID; safe to log
    Secret string
}

func NewBearer(tokens []Token) (*Bearer, error)
func (b *Bearer) SetTokens(tokens []Token) error // atomic replace, for rotation
func (b *Bearer) Authenticate(ctx context.Context, r *http.Request) (Identity, error)
```

- **Token source:** constructor argument only. The library reads no env var, file or
  flag (CLAUDE.md rule 2 spirit). `examples/httpserver` gains a `-token-file` flag; the
  example, not the library, decides where tokens live.
- **Validation in `NewBearer`/`SetTokens`:** non-empty list; non-empty unique IDs;
  unique secrets; secret ≥ 32 chars from `[A-Za-z0-9._~+-]`. That charset is the
  intersection of RFC 6750 `token68` and RFC 7230 `tchar`, so one secret works in both
  `Authorization` and `Sec-WebSocket-Protocol`; base64url-unpadded output of 24+ random
  bytes fits. The error names the offending token's *ID*, never the secret.
- **Compare:** store `sha256(secret)`; hash the presented value, then
  `subtle.ConstantTimeCompare` against **every** stored hash with no early exit. Equal
  hash lengths mean neither length nor match position leaks through timing.
- **Rotation / multiple tokens:** the set is held in an `atomic.Pointer`; `SetTokens`
  swaps it. Overlap old and new tokens during a rollover, then drop the old one. An
  open WebSocket authenticated with a removed token **stays open** — auth runs only at
  upgrade (open question 5).

### Where the credential travels

| Carrier | HTTP | Browser WS | Leak risk | Proposal |
|---|---|---|---|---|
| `Authorization: Bearer <t>` | yes | no (JS can't set headers) | low; often redacted by proxies | **accept** |
| `Sec-WebSocket-Protocol: agentwatch.bearer.<t>` | n/a | yes | header logged by some proxies; not in URL | **accept** |
| `?access_token=<t>` | yes | yes | URL lands in access logs, proxy logs, history | **reject** |
| First-message auth after upgrade | n/a | yes | lowest | **reject** |
| Cookie | yes | yes | needs issuing flow + CSRF story | out of scope |

- **Subprotocol scheme:** a browser calls
  `new WebSocket(url, ["agentwatch.v1", "agentwatch.bearer." + token])`. Chrome fails the
  handshake if it offered protocols and none is echoed, so wsapi always sets
  `AcceptOptions.Subprotocols: []string{"agentwatch.v1"}`. coder/websocket echoes only
  protocols in the server list, so the bearer entry can never be reflected. Clients that
  offer no protocol are unaffected. (Kubernetes uses the same pattern for browser exec.)
- **First-message auth** is rejected: the upgrade happens unauthenticated, so anonymous
  clients hold connection slots under `WithMaxConnections`, the snapshot has to be
  deferred, and failures become close codes instead of a clean 401.
- **Query tokens** are rejected because the transports cannot control the consumer's
  access logging. A consumer who accepts that risk writes a 10-line `AuthenticatorFunc`.
- The bearer scheme name is matched case-insensitively (RFC 7235). `Authorization` wins
  if both carriers are present.

## 4. Defaults

- **Loopback, no authenticator:** unchanged; open, as today. Auth on loopback is opt-in.
  The multi-user-box risk is documented in the package doc, which points to
  `NewBearer` or a Unix socket with file permissions.
- **Remote peer, no authenticator: refuse (new default, behaviour-breaking).**
  `NewServer`/`NewHandler` cannot refuse a non-loopback *bind* because the library never
  sees the listener: `NewHandler` returns an `http.Handler`, and the consumer's
  `http.Server` owns `Addr`. So the guard runs per request: with no authenticator, a
  request whose peer is not loopback gets **403**. "Peer" is:
  - `httpapi`: `r.RemoteAddr`.
  - `wsapi`: the trusted-proxy-resolved client (`clientAddress`,
    `transport/wsapi/ratelimit.go:172`), so a configured proxy can't launder LAN clients.
  - Unix-socket connections (`http.LocalAddrContextKey` holds a `*net.UnixAddr`) count as
    local, since their `RemoteAddr` is not an IP and would otherwise be refused.
- **Opt-out:** `WithInsecureRemoteAccess()` on both packages, for consumers who
  firewall at another layer.
- **Known gap:** a same-host reverse proxy without `WithTrustedProxies` shows a loopback
  peer, so the guard passes. That is the operator's explicit exposure choice; it gets a
  package-doc note. httpapi has no trusted-proxy option (open question 4).

Compatibility: anyone serving to the LAN without auth starts getting 403 after upgrading.
It does not show up at compile time, so it ships as `feat(httpapi,wsapi)!` with a release
note naming both remedies (`WithAuth…` or `WithInsecureRemoteAccess`).

## 5. Error behaviour and check ordering

Order in both transports (wsapi keeps its existing order and inserts the guard):

1. stopped → 503 (wsapi only)
2. Host → 403 `host not allowed` (DNS rebinding first, so rebound loopback traffic
   cannot drain the loopback rate bucket — existing rationale at `server.go:77`)
3. rate limit → 429 (wsapi only today). Unauthenticated attempts consume tokens keyed
   by RemoteAddr per qw4, so token guessing is throttled
4. Origin → 403 (wsapi only), before auth so a cross-site page cannot probe whether a
   token is valid
5. remote guard (no authenticator configured) → 403 `forbidden`
6. authenticate:
   - `ErrUnauthenticated` → **401** `unauthorized` plus `WWW-Authenticate: Bearer realm="agentwatch"`.
     Missing and invalid credentials get identical responses.
   - other error → **500** `internal error`. `err.Error()` is logged at Warn and never
     written to the response (CLAUDE.md rule 10).
7. upgrade / route

Bodies: httpapi keeps its JSON `{"error": "..."}` envelope; wsapi keeps plain text.
401 = "credentials would help", 403 = "they won't, or the request is not allowed here".

Logging: failed auth is logged at Debug with `addr` only, so failed attempts can't flood
the log. The token, `Authorization` and `Sec-WebSocket-Protocol` are never logged.
Successful WS connects log `identity=<Token.ID>` next to `addr`.

httpapi has no rate limiter. With ≥32-char random tokens, online guessing is infeasible
anyway, so a limiter there is DoS hygiene, not auth strength, and is not in this scope
(open question 3).

## 6. Test plan (write first, watch fail)

`transport/auth`:
- `NewBearer`/`SetTokens` reject: empty list, empty/duplicate ID, duplicate secret,
  short secret, out-of-charset secret; the error text contains the ID, not the secret.
- Header: valid → `Identity{ID}`; `bearer` lower-case accepted; `Basic`, empty, wrong
  token, a valid token's prefix, and the token plus a suffix → `errors.Is(err, ErrUnauthenticated)`.
- Subprotocol: token found in a comma list and across repeated header lines; non-bearer
  entries ignored; header beats subprotocol when both are present.
- Multiple tokens each map to their own ID; `SetTokens` racing `Authenticate` under
  `-race`; a removed token fails after the swap.

`transport/httpapi`:
- With auth: all four routes 401 without creds (with `WWW-Authenticate`) and 200 with.
- Bad Host plus bad creds → 403 (Host before auth).
- Authenticator returning a non-sentinel error → 500; body lacks the error text.
- Remote guard: `RemoteAddr` `192.0.2.1:1` without auth → 403; `127.0.0.1` and `[::1]` → 200;
  Unix-socket local addr → 200; `WithInsecureRemoteAccess` → 200; with auth → 401 path.

`transport/wsapi`:
- The existing `auth_test.go` cases pass unchanged (path A) or after a one-line
  `AuthenticatorFunc` port (path B).
- Browser-style dial with `Subprotocols{"agentwatch.v1","agentwatch.bearer.<t>"}`
  connects, and `conn.Subprotocol() == "agentwatch.v1"` — never the bearer entry.
- Bad Origin plus bad creds → 403; repeated bad creds → 429 (rate limiting precedes auth).
- Remote guard via a trusted proxy: XFF names a LAN client, no auth → 403.
- A slog capture across failed and successful auth never contains the secret.

`examples/httpserver`: a token-file run serves with auth; no flag keeps today's behaviour.

## 7. Open questions for Mark

1. **Migration path:** A (non-breaking, `WithAuth` naming wart) or B (break in v0.4.0,
   clean `WithAuthenticator` everywhere)? Recommend B.
2. **Plan reversal:** OK to drop "built-in auth providers" from the §1 non-goals and ship
   `auth.Bearer`? If not, ship only the shared interface plus the remote guard.
3. **httpapi rate limiting:** add one (lifting wsapi's limiter to `internal/ratelimit`)
   in a follow-up, or leave HTTP unthrottled given high-entropy tokens?
4. **httpapi trusted proxies:** should httpapi gain `WithTrustedProxies` so the remote
   guard sees through a proxy, matching wsapi?
5. **Rotation and live sockets:** is "removed token keeps its open WS" acceptable, or
   should `SetTokens` signal wsapi to drop affected connections?
6. **`/healthz` exemption:** keep it behind auth (current proposal; health names sources)
   or offer an option to exempt it for probes?
7. **Remote guard strictness:** should a non-loopback peer also require TLS
   (`r.TLS != nil`) unless opted out, since a bearer token over plain LAN HTTP is
   sniffable?
8. **Query-token carrier:** confirm it is not shipped (consumers can write their own).
