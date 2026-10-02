# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go rewrite of the Python [MAM-Spender-Web-Edition](https://github.com/Plungis/MAM-Spender-Web-Edition):
a self-hosted dashboard that automates spending MyAnonamouse bonus points
(VIP renewal, upload credit, freeleech wedges) on a schedule. The rewrite
exists primarily to close the original's security gaps — see README.md's
"Security model" section, which is the authoritative statement of intent.
Changes that weaken those properties (unauthenticated routes, plaintext or
loggable secrets, dropping CSRF checks) are regressions against the whole
point of the project.

## Commands

```sh
make build              # CGO_ENABLED=0 GOOS=linux build -> ./automouse
ARCH=amd64 make build   # cross-build for a different host arch
make docker             # make build, then docker build -t automouse:local .
go test ./... -run TestName   # single test
gofmt -l . && go vet ./...    # CI fails on either
```

CI (`.github/workflows/ci.yml`) runs gofmt check, `go vet`, `go build`, and
`go test ./... -shuffle=on -race -count=1 -cover`. Release is goreleaser,
publishing to `ghcr.io/miista/automouse`.

Only `internal/mamclient` has tests today (~78% coverage); every other
package is at 0%. The `-race` flag matters if you add more: `store` and
`scheduler` are both concurrently accessed.

`internal/mamclient/testdata/` holds **real responses captured from MAM's
live API**, with account-identifying values (uid, username, ratio, points,
byte counts, VIP expiry, timestamps) swapped for innocent stand-ins. Key
names, nesting, JSON types and every MAM-authored string are verbatim —
don't "tidy" them, and don't hand-write new fixtures: the whole point is
that they record what MAM actually returns. `Client.baseURL` is a field
rather than a constant solely so tests can point at an `httptest` server.

What the captures proved, and what the tests pin:
- `uid` comes back as a JSON **number**, so `UserID`'s `float64` fallback is
  the branch that runs in production — the string path never does.
- Purchase rejections are **HTTP 200** with `{"success":false}`. This is why
  `get()` checks the body, not just the status.
- Rejection text differs per spend type (`"Insufficient funds"` vs
  `"Not enough bonus, s1"`) — never pattern-match on it.
- An invalid cookie returns **403 with a plain-text body**, not JSON.
- `jsonLoad.php?uid=<id>` and bare `jsonLoad.php` return identical payloads.

Not covered, deliberately: successful purchase responses (capturing one
costs points) and three of the four layouts in `mamDateLayouts` (MAM only
emits `2006-01-02 15:04:05`). A test pins that live format so a change
surfaces as a failure rather than a silent 1970 epoch.

Deployment is Docker-only. The Dockerfile does **not** build Go — it copies a
host-prebuilt binary into a `scratch` image, which is why `make docker`
depends on `make build`. It sets `WORKDIR /` and copies the dashboard to
`/web/static`, so `main.go`'s relative `web/static` constant resolves; to run
the binary directly when debugging, do it from the repo root for the same
reason.

## Architecture

`main.go` wires four singletons and serves on `:8765`:
`store.Open(dataDir)` → `auth.NewManager(st)` → `scheduler.New(st, log)` →
`api.New(st, auth, sched, log)`, with `web/static` served at `/`.

- **`internal/store`** — the single source of truth. The entire application
  document (`State`: admin account, auth HMAC key + live sessions, settings,
  totals, history, scheduler on/paused, next run time) lives in one
  `config.json` under the data dir, written `0600` via write-tmp-then-rename.
  All access goes through `View(func(State))` / `Update(func(*State))`,
  which hold the mutex and persist on every update. History is capped at 300
  entries inside `Update`.
- **`internal/auth`** — first-launch admin setup (Sonarr/Radarr pattern),
  bcrypt password hash, opaque session IDs signed with an HMAC key. Both key
  and session set are persisted in `store.Auth` so restarts don't log the
  user out. `AUTOMOUSE_AUTH_DISABLED=true` bypasses enforcement entirely.
- **`internal/settings`** — the `AUTOMOUSE_SETTING_<KEY>` override layer.
  `Resolve(persisted)` returns effective settings plus a `Managed` map of
  key → env var; the API uses that both to render fields read-only and to
  *reject* writes to env-managed keys. Adding a new overridable setting means
  adding to `definitions` **and** adding an `apply(...)` line in `Resolve`.
- **`internal/scheduler`** — two independent background tickers. `loop()`
  polls every 5s and fires a run when due and `SchedulerOn && !Paused`;
  `pointsRefreshLoop()` refreshes the displayed balance on its own interval
  *regardless* of scheduler state, so a disabled scheduler doesn't show a
  stale balance. `runOnce` is where purchase ordering lives: VIP always runs
  first, every run, independent of `UploadStrategy`.
- **`internal/mamclient`** — thin HTTP client for myanonamouse.net. `Secret`
  (secret.go) wraps the session cookie: its `String()` and `MarshalJSON()`
  always return `"[redacted]"`, so redaction is structural rather than
  regex-based on log output. Keep the cookie inside `Secret` end-to-end.
- **`internal/api`** — `Routes` is the full surface. Only `/healthz`,
  `/api/setup` and `/api/login` are unauthenticated; everything else is
  wrapped in `guarded`, and state-changing POSTs additionally in `csrfGuard`
  (Origin-header check). Any new route should follow that same wrapping.
- **`web/static`** — no build step. Plain `index.html` / `style.css` /
  `app.js` with a vendored `petite-vue.iife.js`. Edit the files directly.

## Invariants worth preserving

- `upload_strategy` is a single enum (`none` | `upload_only` | `wedge_only` |
  `alternate`), deliberately replacing three combinable booleans that were
  mutually exclusive in practice but unenforced. Don't reintroduce flags.
- A purchase is only recorded as successful when MAM's own reported balance
  confirms it — not from local bookkeeping. HTTP errors, `{"success":false}`,
  and unconfirmed balance checks must all fail the run.
- `mam_id` is masked (last 4 chars) in every API response; never echo it back
  in full.
- `LOG_LEVEL` parsing in `main.go` deliberately checks for a non-empty value
  first: `zerolog.ParseLevel("")` returns `NoLevel, nil`, which silently
  suppresses all log output. Don't "simplify" that back.
