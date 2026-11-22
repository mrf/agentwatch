# cmd/agentwatch: a runnable binary with per-OS agent auto-detection

Status: **proposal — awaiting Mark's review. No implementation until signed off.**
Issue: agentwatch-kmu. Grounded in the tree at `68b0f4b`.

## 0. Plan conflicts (read first)

The plan describes a library with two examples. A shipped binary extends it in these places:

| Where | Says today | This proposal | Edit needed on sign-off |
|---|---|---|---|
| plan §1 Purpose | "a Go library" | adds one end-user binary | one sentence: the module also ships `cmd/agentwatch`, a thin consumer |
| plan §3 Module layout | no `cmd/` | `cmd/agentwatch/` + `cmd/agentwatch/internal/detect/` | add both to the tree |
| plan §7 rewrite table, §12 rule 3, CLAUDE.md rule 2 | "Examples and consumers choose paths"; no path defaults in `monitor` | path defaults live in `cmd/agentwatch/internal/detect` only | none for rule 2 (complies); §7 row reads "examples, `cmd/`, and consumers choose paths" |
| plan §5 rule 4, CLAUDE.md Code Style | "HTTP examples bind to `127.0.0.1`" | binary binds `127.0.0.1` too, and refuses non-loopback (§6) | say "examples and `cmd/`" |
| plan §8 Codex | env defaults are source-package behaviour, if any | `CODEX_HOME` etc. are read by the binary; sources stay env-free | none; the codex/pi godoc already tells consumers to do exactly this |
| plan §8 Gemini, §3 `sources/gemini` | listed | no gemini source exists; binary covers what exists | none here (pre-existing drift) |
| plan §1 non-goal "Built-in auth providers" | non-goal | binary gains `-token-file` **only if** `transport-auth.md` is signed off | none from this doc; it inherits that decision |
| plan §1 non-goal "Frontend embedding" | non-goal | binary serves JSON + WS only; no embedded UI | none |
| plan §10 Phase 7 | no binary release item | depends on open question 6 | add a release item if a release pipeline is chosen |
| CLAUDE.md Package Visibility | lists public/internal packages | `cmd/agentwatch` is a `main` package, not importable; its detector is under `cmd/.../internal` | add one line: "`cmd/` — binaries, never imported" |

Nothing here touches `monitor`, `source`, `session`, `sources/*` or the transports. If any slice
below needs a library change, it gets its own ticket.

## 1. Problem

To watch agents today a user must `go run ./examples/httpserver -dir ~/.claude/projects`. That
example watches **Claude only**, needs the path typed by hand, and is documented as an example,
not a tool. The six real sources (`claude`, `codex`, `cursor`, `opencode`, `pi`, `antigravity`)
each take an explicit root and read no environment (by design — plan §8, CLAUDE.md rule 2), so
every consumer re-derives the same per-OS paths. agent-racer already did this in its own
`backend/cmd/server/main.go:48-76`; nothing in agentwatch does.

## 2. Goals / non-goals

Goals:
- `agentwatch` with no flags finds every installed agent it has a source for and serves state on
  `127.0.0.1:8080` over the existing `httpapi` + `wsapi` transports.
- Every auto-detected path is overridable by flag; detection is printable (`agentwatch detect`).
- Path policy lives entirely in `cmd/`; the library gains no defaults.
- Cross-compiles with `CGO_ENABLED=0` (opencode uses `modernc.org/sqlite`, pure Go).

Non-goals (v1 of the binary):
- A UI, TUI, or embedded frontend.
- Daemonisation, service files, auto-start, or self-update.
- Persistence, replay, or a config-file format (open question 3).
- New sources (gemini) or source fixes (see §4 antigravity).
- Re-probing for agents installed after startup (restart picks them up; open question 5).

## 3. Relationship to `examples/httpserver`

Options: **replace** (delete the example), **wrap** (binary imports example code — impossible,
both are `main`), **coexist**.

Recommendation: **coexist, example unchanged.** The example is the ~170-line "how to wire the
library" reference the README points at; the binary is the batteries-included tool. They will
share ~40 lines (the `sinkRelay` that breaks the monitor↔wsapi cycle, `examples/httpserver/main.go:53-74`,
and the shutdown sequence). Duplicating that is cheaper than a new public helper, which plan
§12 rule 12 forbids speculatively. If a third copy appears, propose a `wsapi`-side fix then.

## 4. Per-OS auto-detection

Status key: **VERIFIED** = path exists on the authoring box (WSL2 Linux, 2026-11-22) and matches
the source's expected layout; **GODOC** = stated in the source package's godoc or tests, not
observed here; **UNVERIFIED** = no evidence in this tree or on this box — treat as a guess to be
confirmed before it ships.

| Source (option) | Linux | macOS | Windows | Env overrides | Evidence |
|---|---|---|---|---|---|
| `claude` (`WithRoot`) | `~/.claude/projects` **VERIFIED** | `~/.claude/projects` UNVERIFIED | `%USERPROFILE%\.claude\projects` UNVERIFIED | `CLAUDE_CONFIG_DIR` UNVERIFIED (not referenced anywhere in tree) | `sources/claude/source.go:4`; agent-racer `cmd/server/main.go:54` uses `filepath.Join(home, ".claude", "projects")` on every OS |
| `claude` (`WithSessionEndDir`) | no default | no default | no default | — | Written by a user-installed hook, not by Claude Code. agent-racer defaulted to `$XDG_STATE_HOME/agent-racer/session-end` (`internal/config/config.go:242`). Binary: off unless `-claude-session-end-dir` is given |
| `codex` (`WithRoot`) | `$CODEX_HOME` else `~/.codex` **VERIFIED** (`~/.codex/sessions` present) | same, UNVERIFIED | same under `%USERPROFILE%`, UNVERIFIED | `CODEX_HOME` GODOC | `sources/codex/source.go:3-15`; agent-racer `main.go:60-63` |
| `cursor` (`WithRoot`) | `~/.cursor` GODOC (absent on this box) | `~/.cursor` GODOC | `%USERPROFILE%\.cursor` UNVERIFIED | none | `sources/cursor/source.go:10-11`. `decodeProjectName` assumes `/`-separated paths (`source.go:192-198`), so Windows `WorkingDir` is likely wrong — UNVERIFIED |
| `opencode` (`WithDBPath`) | `$XDG_DATA_HOME/opencode/opencode.db` else `~/.local/share/opencode/opencode.db` GODOC (absent on this box) | UNVERIFIED (XDG-style vs `~/Library/Application Support` unknown) | UNVERIFIED | `XDG_DATA_HOME` GODOC (named as the thing the source does *not* read) | `sources/opencode/source.go:3-9` |
| `pi` (`WithRoot`) | `$PI_CODING_AGENT_SESSION_DIR`, else `${PI_CODING_AGENT_DIR:-~/.pi/agent}/sessions` **VERIFIED** | same, UNVERIFIED | same under `%USERPROFILE%`, UNVERIFIED | both GODOC | `sources/pi/source.go:8-21` |
| `antigravity` (`WithRoot`) | `~/.antigravitycli` GODOC — **contradicted on this box** | UNVERIFIED | UNVERIFIED | none | `sources/antigravity/source.go:4`, `parser_test.go:429`. Installed `agy` here writes `~/.gemini/antigravity-cli/conversations/<uuid>.db` (SQLite), not `<uuid>.json`; `~/.antigravitycli` is absent. Working-dir mapping scans `/proc` (`procmap.go:19-24`), so it is Linux-only |
| `mock` | never auto-detected | | | | test-only |

Consequences:
- **Antigravity is excluded from auto-detection** until the layout mismatch is resolved
  (filed as a finding, not fixed here). `-antigravity-root` still enables it explicitly.
- macOS and Windows columns need one confirmation pass on a real machine before the detection
  slice merges (open question 2). Until then the binary can ship Linux-verified and mark the
  others "best effort" in `detect` output.
- CI already runs `ubuntu`/`macos`/`windows` (`.github/workflows/ci.yml:17`), but CI runners
  have no agents installed, so CI proves the resolver's logic, not the paths.

### Detection semantics

1. **Resolve** — a pure function builds one candidate per source from `(GOOS, home, getenv)`.
   It never touches disk, so all three OSes are table-tested on any runner.
2. **Probe** — `os.Stat` the candidate (the sessions directory or DB file, not just the parent).
   Present → enable. Absent → skip, logged at Info as `source=codex status=not-found`.
3. **Explicit beats detected** — a per-source flag enables that source with that path even if
   absent (the source then reports empty/unhealthy via `/healthz`). `-sources` restricts the set.
4. **Nothing enabled** → exit 3 with the probed list on stderr (`monitor.New` rejects zero sources,
   `monitor/options.go:109`, so this has to be caught before it).
5. **Closing** — the monitor never closes sources; the binary calls `Close` on any source that
   implements `io.Closer` (opencode) after `Run` returns.

Default discover window: 30m for every source (agent-racer used 30m claude / 10m codex;
one value is simpler and `-discover-window` overrides it).

## 5. CLI surface

Stdlib `flag` only — a CLI framework would land in `go.mod` for every library importer.

```
agentwatch [serve] [flags]   # default subcommand
agentwatch detect [flags]    # print the resolve/probe table and exit 0
agentwatch version           # module version from debug.ReadBuildInfo
```

`serve` flags:

| Flag | Default | Notes |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | see §6 |
| `-sources` | all detected | comma list; unknown name → exit 2 |
| `-claude-root`, `-codex-root`, `-cursor-root`, `-opencode-db`, `-pi-root`, `-antigravity-root` | detected | explicit enable |
| `-claude-session-end-dir` | unset | |
| `-poll` | `2s` | `monitor.WithPollInterval` |
| `-discover-window` | `30m` | per-source `WithDiscoverWindow` |
| `-stale` | monitor default | `monitor.WithStaleThreshold` |
| `-allowed-hosts`, `-allowed-origins` | none | passed to both transports / wsapi |
| `-insecure-remote` | false | §6 |
| `-log-level` | `info` | `debug|info|warn|error` |
| `-log-format` | `text` | `text|json` |

Env: the binary reads **only the agents' own variables** in §4. No `AGENTWATCH_*` mirror and no
config file in v1 (open question 3). `examples/stdout` reads `$AGENTWATCH_DIR`; that stays an
example-local convenience.

`detect` prints source, resolved path, and found/not-found to local stdout. Paths are fine there
(local terminal, the user's own files); they never reach a transport (plan §5 rule 1).

## 6. Transports, bind, Host/Origin, auth

- Both transports mounted as in the example: `/ws` → `wsapi`, `/` → `httpapi`.
- Host validation and origin defaults are the transports' own; the binary only forwards
  `-allowed-hosts` / `-allowed-origins`.
- **Bind guard (binary-level):** unlike the library, the binary owns the listener. If `-addr`
  resolves to a non-loopback IP (or an empty host, i.e. all interfaces) and no auth is
  configured, exit 2 with "refusing non-loopback bind without auth; pass -insecure-remote".
  This is the binary-side twin of `transport-auth.md` §4's per-request guard and is useful
  whether or not that doc is accepted.
- **Auth:** none in the first slices. If `transport-auth.md` is signed off, a follow-up adds
  `-token-file` (one `id:secret` per line, file must be `0600` on Unix) feeding `auth.NewBearer`,
  and a configured token satisfies the bind guard. If it is rejected, the bind guard plus
  `-insecure-remote` is the whole story.

## 7. Logging, shutdown, exit codes

- `log/slog` to stderr; the same logger goes to `monitor.WithLogger` and `wsapi.WithLogger`.
  Startup logs each enabled source and the listen address (`listening addr=127.0.0.1:8080`).
- Signals: `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`. On Windows only Ctrl+C
  (`os.Interrupt`) is relied on; SIGTERM delivery there is UNVERIFIED.
- Shutdown order: `http.Server.Shutdown` (5s) → `wsSrv.Stop()` → cancel monitor ctx → wait for
  `Run` to return → close `io.Closer` sources. A second signal exits immediately with 1.
- Exit codes: `0` clean shutdown after a signal; `1` runtime failure (listen error, `Run` error,
  forced shutdown); `2` usage error (bad flag, unknown source, refused bind); `3` no sources
  enabled.

## 8. Release / distribution

Options: (a) `go install github.com/mrf/agentwatch/cmd/agentwatch@<tag>` only; (b) goreleaser
GitHub Releases for linux/darwin/windows × amd64/arm64 on tag push.

Recommendation: **(a) for the first release**, (b) once the macOS/Windows paths are confirmed.
(a) needs no new CI and no signing story; (b) adds a workflow (Dependabot's `github-actions`
entry covers it) and puts unsigned binaries in front of macOS Gatekeeper. Version comes from
`debug.ReadBuildInfo`, which `go install @tag` populates; (b) would add `-ldflags` instead.

## 9. Test plan (write first, watch fail)

`cmd/agentwatch/internal/detect`:
- Table tests per GOOS (`linux`, `darwin`, `windows`) × env (`CODEX_HOME` set/unset,
  `PI_CODING_AGENT_SESSION_DIR` vs `PI_CODING_AGENT_DIR` precedence, `XDG_DATA_HOME` set/unset)
  → expected candidate per source. Uses `filepath` semantics for the target OS via injected joins,
  so Windows cases pass on Linux.
- Antigravity is absent from the auto set; present when explicitly configured.
- Probe against a `t.TempDir()` tree: present/absent/file-where-dir-expected.

`cmd/agentwatch` (testable `run(ctx, args, stdout, stderr) int`):
- Exit codes: unknown flag → 2; unknown `-sources` name → 2; `-addr 0.0.0.0:0` without
  `-insecure-remote` → 2; `[::]:0`, `:0`, LAN IP → 2; `127.0.0.1:0`, `[::1]:0`, `localhost:0` → allowed;
  no sources found → 3 and stderr lists every probed path.
- Explicit root beats detection; `-sources` restricts.
- Integration: claude fixture tree under a temp HOME, `-addr 127.0.0.1:0`, parse the logged
  address, `GET /sessions` returns the fixture session, `GET /ws` gets a snapshot; cancel ctx →
  exit 0 within 6s, under `-race`.
- Shutdown closes an `io.Closer` source exactly once (fake source).
- `detect` output golden test (paths from the temp HOME only).

## 10. Implementation slicing

Each slice is one ticket, merged before the next is spawned (they are serial).

1. **kmu.1 docs** — the plan/CLAUDE.md edits from §0. No code.
2. **kmu.2 resolver** — `cmd/agentwatch/internal/detect`: resolve + probe + tests. No `main` yet.
3. **kmu.3 serve** — `cmd/agentwatch/main.go`: `run`, flags, transport wiring, bind guard,
   shutdown, exit codes, integration test.
4. **kmu.4 detect/version subcommands.**
5. **kmu.5 docs + release** — README section (diff shown to Mark first, per the README rule),
   install instructions for the chosen distribution option.
6. **Later, gated on `transport-auth.md`:** `-token-file`.

## 11. Open questions for Mark

1. **Coexist vs replace:** keep `examples/httpserver` unchanged alongside the binary (recommended),
   or delete it and point the README at `cmd/agentwatch`?
2. **Unverified paths:** can you confirm the macOS/Windows rows (§4) on a real machine, or should
   the binary ship Linux-verified and label the rest "best effort"?
3. **Config:** flags only in v1 (recommended), or also `AGENTWATCH_*` env mirrors / a config file?
4. **Bind guard:** OK for the binary to refuse non-loopback binds without auth (exit 2) even before
   `transport-auth.md` is decided?
5. **Late-installed agents:** startup-only detection (recommended), or re-probe periodically and
   add sources live (needs a monitor API that doesn't exist)?
6. **Distribution:** `go install` only for now (recommended), or goreleaser binaries from day one?
7. **Antigravity:** exclude from auto-detection until the source matches the current `agy`
   layout (recommended), or block this work on fixing that source first?
8. **Name:** binary called `agentwatch` (same as module; plan §13 Q1 still lists the name as a
   placeholder)?
