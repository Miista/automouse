# AutoMouse

A Go rewrite of [MAM-Spender-Web-Edition](https://github.com/Plungis/MAM-Spender-Web-Edition):
automates spending MyAnonamouse bonus points (VIP renewal, upload credit,
freeleech wedges) on a schedule, from a small self-hosted web dashboard.

The rewrite exists to close security gaps in the original Python tool, which
had no authentication on its control API, stored the session cookie in
plaintext with regex-based log redaction, and shipped a `docker-compose.yml`
that published the port with no auth at all.

## Security model

- **First-launch admin setup**, as in Sonarr/Radarr: nothing is reachable
  until you create an admin account. Password stored as a bcrypt hash.
- **Session auth on every route**, read-only state included. Signed, httpOnly
  cookies backed by a server-side store persisted to disk, so a container
  restart doesn't sign you out.
- **CSRF defense-in-depth**: state-changing POSTs reject mismatched `Origin`.
- **The MAM cookie never has to touch disk** — set `AUTOMOUSE_SETTING_MAM_ID`
  and it's read straight from the environment. Entered through the UI it
  persists to `data/config.json` at `0600`, and the API only ever returns it
  masked to the last 4 characters.
- **Redaction by construction, not regex**: the cookie is a `Secret` whose
  `String()`/`MarshalJSON()` always return `"[redacted]"`, so it cannot reach
  a log line or JSON response by accident.
- **Purchases are verified against MAM's own reported balance**, never local
  bookkeeping. An HTTP error, a `{"success":false}` rejection, or an
  unconfirmed balance check is never recorded as a successful spend.

The dashboard port is published by default, relying on the admin login
rather than network isolation, so the UI works without a reverse proxy. Front
it with Caddy/Authelia if you prefer, or drop the `ports:` block for
Docker-network-only access.

## Running

```sh
make docker          # builds the binary, then the image
docker compose up -d
```

On first launch, open the dashboard and create an admin account before
configuring MAM settings.

## Configuration

Any setting can be pinned via an environment variable, following the same
convention as [Shelfarr](https://github.com/Miista/shelfarr):

```
AUTOMOUSE_SETTING_POINTS_BUFFER=5000
AUTOMOUSE_SETTING_MAM_ID=your_mam_id_cookie_value
```

Overridable keys: `buy_vip`, `upload_strategy`, `points_buffer`,
`max_upload_gb_per_run`, `next_run_delay_minutes`, `mam_id`. An env-managed
setting renders as a locked field in the UI and is rejected if written via
`/api/settings`.

`upload_strategy` is one of `none`, `upload_only`, `wedge_only`, `alternate`.

Other variables:

- `AUTOMOUSE_DATA_DIR` — where `config.json` is persisted (default `/app/data`).
- `AUTOMOUSE_AUTH_DISABLED=true` — disables the admin login entirely. Only for
  loopback-bound deployments, or when you're fronting the app with your own
  auth and don't want a second login.
- `LOG_LEVEL` — zerolog level (`debug`, `info`, `warn`, `error`; default `info`).

## Scheduler

A background goroutine checks every 5 seconds whether a run is due.
Deactivating (dashboard button, or `POST /api/pause`) is a real state
transition — no new run fires while inactive, and the dashboard shows
active/inactive as a status badge.

VIP renewal always runs first, every run, independent of the strategy chosen
for upload credit and wedges.
