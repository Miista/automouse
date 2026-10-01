# AutoMouse

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
  account exists. Sessions are signed, httpOnly cookies, backed by a
  server-side session store that's persisted to disk (see below) so a
  container restart doesn't sign you out.
- **CSRF defense-in-depth**: state-changing POSTs reject mismatched Origin
  headers.
- **The dashboard port is published by default**, relying on the built-in
  admin login for protection instead of network isolation — this is
  intentional so the UI is reachable without extra reverse-proxy setup.
  Front it with Caddy/Authelia instead if you prefer, or remove the
  `ports:` block for Docker-network-only access.
- **The MAM session cookie never has to touch disk**: set it via
  `AUTOMOUSE_SETTING_MAM_ID` and it's read straight from the environment.
  If you enter it through the UI instead, it's persisted to `data/config.json`
  with `0600` permissions and is never echoed back in full over the API
  (masked to the last 4 characters).
- **Redaction by construction, not regex**: the cookie value is wrapped in a
  `Secret` type whose `String()`/`MarshalJSON()` always return
  `"[redacted]"`, so it cannot accidentally end up in a log line or JSON
  response.
- **Every purchase is verified against MAM's own reported balance**, not our
  own bookkeeping — a purchase that returns an HTTP error, a logical
  `{"success":false}` rejection, or an unconfirmed balance check is never
  silently recorded as a successful spend.

## Env-var settings override

Any setting can be pinned via an environment variable, following the same
convention as the [Shelfarr](https://github.com/Miista/shelfarr) project:

```
AUTOMOUSE_SETTING_<KEY>=<value>
```

For example:

```
AUTOMOUSE_SETTING_POINTS_BUFFER=5000
AUTOMOUSE_SETTING_MAM_ID=your_mam_id_cookie_value
```

When a setting is env-managed, the UI renders it as a locked, read-only
field with a lock icon and a "Managed by `AUTOMOUSE_SETTING_X`" caption —
it cannot be changed from the dashboard, and the API rejects attempts to
override it via `/api/settings`.

Overridable keys: `buy_vip`, `upload_strategy`, `points_buffer`,
`max_upload_gb_per_run`, `next_run_delay_minutes`, `mam_id`.

`upload_strategy` is one of `none`, `upload_only`, `wedge_only`, `alternate`
— see the Settings UI for what each does. This single field replaced what
used to be three independent, combinable flags; they were mutually
exclusive in practice but not enforced as such, which was a source of
ambiguity.

## Scheduler

The scheduler runs in a background goroutine, checking every 5 seconds
whether a scheduled run is due. Deactivating (the dashboard button, or
`POST /api/pause`) is a real state transition — no new run fires while
inactive. The dashboard makes active/inactive state obvious via a status
badge, addressing the original tool's confusing "how do I stop it?" UX.

VIP renewal always runs first, every run, independent of the purchase
strategy chosen for upload credit/wedges.

## Running

```sh
make build
make docker
docker compose up -d
```

Or without Docker:

```sh
go build -o automouse .
AUTOMOUSE_DATA_DIR=./data ./automouse
```

On first launch, open the dashboard and create an admin account before
configuring MAM settings.

## Other environment variables

- `AUTOMOUSE_DATA_DIR` — where `config.json` is persisted (default `/app/data`).
- `AUTOMOUSE_AUTH_DISABLED=true` — disables the admin login entirely. Only
  use this if you're binding strictly to loopback or otherwise fronting the
  app with your own auth (e.g. Authelia) and don't want a second login.
- `LOG_LEVEL` — zerolog level (`debug`, `info`, `warn`, `error`; default `info`).
