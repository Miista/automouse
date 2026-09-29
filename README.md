# mam-spender

A Go rewrite of [MAM-Spender-Web-Edition](https://github.com/Plungis/MAM-Spender-Web-Edition):
automates spending MyAnonamouse bonus points (VIP renewal, upload credit,
freeleech wedges) on a schedule, from a small self-hosted web dashboard.

This rewrite exists to fix security gaps in the original Python tool:

- The original had **no authentication** on its control API — anyone who
  could reach the port could read/replace your MAM session cookie and
  trigger spends.
- The session cookie was stored in plaintext with only regex-based log
  redaction.
- No CSRF protection.
- Its default `docker-compose.yml` published the port with no auth at all.

## Security model

- **First-launch admin setup**: on first run, the UI forces you to create an
  admin username/password before anything else is reachable (same pattern
  as Sonarr/Radarr). Password is stored as a bcrypt hash.
- **Session auth on every route**, including read-only state, once an admin
  account exists. Sessions are signed, httpOnly cookies backed by an
  in-memory server-side session store (restarting the process invalidates
  sessions — acceptable for this tool's scale).
- **CSRF defense-in-depth**: state-changing POSTs reject mismatched Origin
  headers.
- **The dashboard port is published by default**, relying on the built-in
  admin login for protection instead of network isolation — this is
  intentional so the UI is reachable without extra reverse-proxy setup.
  Front it with Caddy/Authelia instead if you prefer, or remove the
  `ports:` block for Docker-network-only access.
  (An earlier version of this tool also had an IP allowlist setting, ported
  from the original Python edition. It was removed: it checks the raw TCP
  peer address, which is meaningless — or actively misleading — behind a
  reverse proxy, since every request then appears to come from the proxy's
  IP rather than the real client. Session auth is the actual gate; use your
  reverse proxy's own access controls if you want IP-based restriction.)
- **The MAM session cookie never has to touch disk**: set it via
  `MAMSPENDER_SETTING_MAM_ID` and it's read straight from the environment.
  If you enter it through the UI instead, it's persisted to `data/config.json`
  with `0600` permissions and is never echoed back in full over the API
  (masked to the last 4 characters).
- **Redaction by construction, not regex**: the cookie value is wrapped in a
  `Secret` type whose `String()`/`MarshalJSON()` always return
  `"[redacted]"`, so it cannot accidentally end up in a log line or JSON
  response.

## Env-var settings override

Any setting can be pinned via an environment variable, following the same
convention as the [Shelfarr](https://github.com/Miista/shelfarr) project:

```
MAMSPENDER_SETTING_<KEY>=<value>
```

For example:

```
MAMSPENDER_SETTING_POINTS_BUFFER=5000
MAMSPENDER_SETTING_MAM_ID=your_mam_id_cookie_value
```

When a setting is env-managed, the UI renders it as a locked, read-only
field with a lock icon and a "Managed by `MAMSPENDER_SETTING_X`" caption —
it cannot be changed from the dashboard, and the API rejects attempts to
override it via `/api/settings`.

Overridable keys: `buy_vip`, `buy_upload_credit`, `alternate_fl_upload`,
`alternate_next_purchase`, `fl_only`, `points_buffer`,
`next_run_delay_minutes`, `mam_id`, `server_host`, `server_port`.

## Scheduler

The scheduler runs in a background goroutine, checking every 5 seconds
whether a scheduled run is due. Pausing (`Pause` button in the UI, or
`POST /api/pause`) is a real state transition — no new run fires while
paused. The dashboard makes running/paused state obvious via a status badge,
addressing the original tool's confusing "how do I stop it?" UX.

## Running

```sh
make build
make docker
docker compose up -d
```

Or without Docker:

```sh
go build -o mam-spender .
MAMSPENDER_ADDR=127.0.0.1:8765 MAMSPENDER_DATA_DIR=./data ./mam-spender
```

On first launch, open the dashboard and create an admin account before
configuring MAM settings.

## Other environment variables

- `MAMSPENDER_ADDR` — listen address (default `127.0.0.1:8765`).
- `MAMSPENDER_DATA_DIR` — where `config.json` is persisted (default `/app/data`).
- `MAMSPENDER_AUTH_DISABLED=true` — disables the admin login entirely. Only
  use this if you're binding strictly to loopback or otherwise fronting the
  app with your own auth (e.g. Authelia) and don't want a second login.
- `LOG_LEVEL` — zerolog level (`debug`, `info`, `warn`, `error`; default `info`).
