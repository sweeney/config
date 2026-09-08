# Config service — administrator guide

Operational notes for running the config service: namespace design,
backups, restore, role changes, identity-coupling concerns. For the
integration surface, see [`README.md`](../README.md).

## Creating namespaces

Only admin users can create namespaces. Each create call must supply
`name`, `read_role`, `write_role`, and an initial `document` (which may
be `{}`).

```bash
curl -s -X POST https://config.example.com/api/v1/config/namespaces \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":       "mqtt_topics",
    "read_role":  "user",
    "write_role": "admin",
    "document":   { "temperature": "home/sensors/temp" }
  }'
```

Names must match `^[a-z0-9_-]{1,64}$`. Keep them descriptive but short:
`mqtt_topics`, `house_names`, `sensor_locations`, `prefs`.

## Choosing roles

Defaults that age well:

- **Admin-only for everything operational.** MQTT broker passwords (even
  if you shouldn't store those here), device serial numbers, network
  topology. `read_role: admin`, `write_role: admin`.
- **User-readable for discovery data.** MQTT topic names, friendly names,
  location coords. `read_role: user`, `write_role: admin`.
- **User-writable only when genuinely user data.** Per-user UI prefs,
  dashboard layouts. `read_role: user`, `write_role: user`.

Users cannot delete namespaces or change ACLs regardless of `write_role`
— both operations are admin-only.

### Changing roles later

Use `PATCH /api/v1/config/namespaces/{ns}` with the new `read_role` and
`write_role`. The document is untouched. Note that tightening
`read_role` from `user` to `admin` will immediately start returning 404
to in-flight user requests — there is no grace period.

```bash
curl -s -X PATCH \
  https://config.example.com/api/v1/config/namespaces/mqtt_topics \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}'
```

## Document design patterns

### Flat keys for loose collections

```json
{
  "main": "Rivendell",
  "guest": "Hobbiton",
  "holiday": "Lothlorien"
}
```

Good for: friendly name lookups, topic-to-device mappings, mode flags.
Reads return the whole object; callers extract the one they need.

### Nested objects for structured records

```json
{
  "kitchen": {
    "topic": "home/kitchen/temp",
    "device_id": "esp32-kitchen",
    "calibration_offset": -0.4
  },
  "living_room": {
    "topic": "home/living_room/temp",
    "device_id": "esp32-living",
    "calibration_offset": 0.0
  }
}
```

Good for: when each entry has multiple attributes. Don't abuse this for
more than ~50 entries — consider splitting by category into multiple
namespaces (`sensors_indoor`, `sensors_outdoor`, …) if you cross that.

### Size hygiene

Documents are capped at **64KB** after JSON compaction. The cap exists
to protect the backup pipeline and to discourage using the config
service as a blob store. If you're bumping the cap, you probably want a
different data store (a tiny DB, a file, a proper CMS).

## Backups

### On-write triggers

Every successful `POST`/`PUT`/`PATCH`/`DELETE` triggers an async R2
upload. The backup manager coalesces triggers within a cooldown window
(`BACKUP_MIN_INTERVAL`, default **30s**): rapid bursts collapse into
one upload at the end of the window.

RPO implications:
- **Slow-trickle writes** (once-in-a-while admin edits) → each write
  has its own backup ≤ 30s after it returns.
- **Script storm** (100 writes in 10s) → one backup at T+30s, covering
  the whole burst. Data in between is not individually recoverable.

If your workload has critical write-every-N-seconds traffic, either set
`BACKUP_MIN_INTERVAL=0` (disable throttling; every write uploads) or
increase R2 pricing / capacity planning accordingly. For a the
default is right.

### Scheduled backups

A daily snapshot runs at **03:00 UTC** regardless of write activity.
This is independent of the per-write trigger and cannot be disabled
short of removing R2 credentials from the env.

### Failure handling

Failed backups log to stdout and increment the audit trail but do **not**
fail the user's request. Rationale: a successful PUT has already been
committed to SQLite; surfacing the backup error to the caller would
imply an all-or-nothing guarantee we don't provide. Monitor the logs
or audit stream for sustained `backup: upload failed` entries.

### R2 key layout

```
{env}/backups/config/{YYYY/MM/DD}/config-{RFC3339-timestamp}.sqlite3
```

Example:

```
production/backups/config/2026/04/24/config-2026-04-24T15:30:45Z.sqlite3
```

## Restoring from backup

```bash
# List available config backups
./bin/config-server --list-backups

# Restore the most recent (interactive picker)
./bin/config-server --restore-backup

# Or restore a specific key
./bin/config-server --restore-backup \
  production/backups/config/2026/04/24/config-2026-04-24T15:30:45Z.sqlite3
```

The restore writes to the path from `DB_PATH` (default `config.db`). Stop
the config service before restoring — the restore CLI overwrites the
file even if the service is running, which will corrupt the SQLite WAL.

```bash
sudo systemctl stop config
sudo -u config ./bin/config-server --restore-backup
sudo systemctl start config
```

## Identity coupling

The config service depends on identity at runtime for token verification
via JWKS. It does **not** depend on identity for:

- Startup (config boots standalone; tokens fail with `503` until identity
  is reachable again — see below)
- Database (fully separate SQLite file)
- Backups (separate R2 prefix)

If identity is down, config keeps serving cached JWKS, so most requests
carry on working. Once the cache goes stale beyond `MaxStaleAge`
(default 30 minutes), or a token arrives with a `kid` no cached key
covers, config can no longer check the token at all.

### Identity outages answer 503, not 401

When config cannot verify a token because identity is unreachable, it
answers **`503` with `Retry-After`**, not `401`:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 30
{"error":"keys_unavailable","message":"cannot verify tokens right now — ..."}
```

This matters because `401` has a specific meaning to clients: *your
credentials are bad, sign out and log in again*. If an identity outage
reported itself that way, every config user would be signed out at the
same moment — and could not sign back in, because logging in needs the
service that is down. A brief blip would become a mass re-authentication
event. `503` says *try again shortly*, which is both true and survivable.

Note that no `WWW-Authenticate` header is sent on the `503`: that header
invites re-authentication, which is the behaviour being avoided.

A token that is genuinely bad — malformed, bad signature, expired,
audience mismatch — still gets `401`. That really is the caller's problem.

This rests on `ErrKeysUnavailable`, added in `common` v0.4.0 and mapped in
`internal/auth/middleware.go` (`writeTokenError`).

### /healthz stays green during a JWKS outage

Deliberate, and worth knowing before you wire up alerting: `/healthz`
reports `200` even when every authenticated request is answering `503`.

Two reasons. `deploy/deploy.sh` gates deploys on `/healthz`, so an
identity blip would otherwise fail deploys that have nothing to do with
identity. More importantly, anything that restarts config on an unhealthy
signal would **discard the cached JWKS keys** — which are precisely what
lets config ride out a brief outage — turning a survivable blip into a
hard one.

Alert on the `jwks` counters in the `/healthz` body instead: `StaleServed`
climbing means config is running on cached keys it can no longer confirm, and
`LastFetchError` carries the most recent failure.

### Rotating identity's JWT key

When you run `./identity-server --rotate-jwt-key`, the config service
will see unknown `kid` in incoming tokens and refetch JWKS automatically.
No restart needed. Throttling prevents a bad-token storm from stampeding
identity; observed fetch rate is bounded by `RefetchMinInterval`
(default 10s).

### Revocation lag

Config validates tokens statelessly (signature + `exp` + `iss`) against
identity's public JWKS. That means **disabling a user on identity, or
logging them out of all devices, does not immediately revoke their
access on config**. An already-issued access token stays valid until
its `exp` — up to **15 minutes** (identity's default access-token TTL).

Implications:

- Firing an admin on identity means they retain config write access for
  up to 15 min.
- A session-compromise detection on identity
  (`token_family_compromised`) clears tokens on identity but not on
  config.
- There is no introspection / blacklist path exposed to config.

This is the fundamental tradeoff of JWKS-based auth (fast, stateless,
no coupling — at the cost of delayed revocation). Mitigations if you
need tighter revocation:

- **Shorten identity's access token TTL.** 5 min is reasonable for
  high-security deployments. Refresh rotation already runs on every use.
- **Route config mutations through an introspection round-trip.**
  Identity's `POST /oauth/introspect` can answer "is this token still
  live?" against its own token table. Adds a hop to every write but
  keeps reads fast via JWKS.
- **Rotate identity's JWT key immediately after disabling a principal.**
  Forces the verifier to refetch JWKS and invalidates *every* token
  signed by the old key — blunt but effective.

For a self-hosted setup, the default 15-min window is usually fine.

### Cross-service token replay

A token carrying no `aud` claim is valid on every service that trusts
identity's JWKS — config, plus any sibling. Config defends against this
with `REQUIRED_AUDIENCE`, which is **enabled in production**
(`REQUIRED_AUDIENCE=config` in `/etc/config/config.env`): the verifier
rejects any token whose `aud` does not include `config`.

Both sides must stay in lockstep. Config running with the flag set
while identity does not stamp `aud` on a given token rejects every
request carrying that token, so the two constraints are:

1. `REQUIRED_AUDIENCE=config` in config's env file.
2. Identity stamps `aud: "config"` (or a space-delimited list including
   it) on every token intended for config use — including tokens issued
   to the admin SPA's own OAuth client.

The flag remains unset by default so a fresh deployment keeps the v1
JWT shape. Turning it on is a restart, not a redeploy: edit the env
file and `sudo systemctl restart config`. Note that `/healthz` is
unauthenticated and stays green even if every authenticated request is
being rejected — verify with a real Bearer call against
`GET /api/v1/config`, and by loading the admin SPA.

## Admin UI (optional)

The config service can serve a small browser SPA at `/` for managing
namespaces (list, view, edit JSON document, change ACL, delete). The
SPA is **off by default** — set `OAUTH_CLIENT_ID` and
`IDENTITY_PUBLIC_URL` in the env file to enable it.

Architecture:

- The SPA is a single `index.html` + a few static `.js` / `.css` files,
  embedded into the binary at `ui/static/`.
- It authenticates against identity via OAuth Authorization Code with
  PKCE (the same flow `examples/spa-demo` uses). Tokens land in
  `localStorage`; the SPA calls `/api/v1/config/*` with a `Bearer`
  header.
- There is no server-side session, no cookie crypto, no CSRF
  middleware — all auth state is in the browser.
- A path-aware CSP keeps `/api/*` locked to `default-src 'none'`
  while permitting `script-src 'self'` + the identity origin in
  `connect-src`/`form-action` for the SPA.

### 1. Register the OAuth client on identity

In identity's admin UI (`https://id.example.com/admin/oauth`), click
**New OAuth client** and fill in:

| Field | Value |
|---|---|
| Client ID | `config-spa` (or any unique slug) |
| Name | `Config Admin SPA` |
| Redirect URIs | `https://config.example.com/` (one per line) |
| Grant types | `authorization_code`, `refresh_token` |
| Token endpoint auth method | `none` (public client; PKCE is the secret) |
| Scopes | leave blank |
| Audience | `config` — required, since production runs `REQUIRED_AUDIENCE=config`. A client registered with a blank audience yields tokens the config verifier rejects, so the SPA would 401 on every API call while other clients keep working. See [Cross-service token replay](#cross-service-token-replay). |

Save. Note the client ID — you'll feed it to config below.

PKCE is **mandatory** for `none` auth — identity's authorize endpoint
already enforces this.

### 2. Configure config service

Add to `/etc/config/config.env`:

```
OAUTH_CLIENT_ID=config-spa
IDENTITY_PUBLIC_URL=https://id.example.com
```

`IDENTITY_PUBLIC_URL` defaults to `IDENTITY_ISSUER_URL`, so this line
is optional when both are the same hostname.
Restart `config.service`. The startup log will print:

```
config: admin UI mounted at /; oauth client_id=config-spa, identity public url=https://id.example.com
```

### 3. Cloudflared route

If you tunnel both services from one Cloudflare Tunnel, your
`config.yml` looks like:

```yaml
tunnel: <tunnel-uuid>
credentials-file: /etc/cloudflared/<uuid>.json

ingress:
  - hostname: id.example.com
    service: http://localhost:8181
  - hostname: config.example.com
    service: http://localhost:8282
  - service: http_status:404
```

Then DNS-route `config.example.com` to the tunnel:

```
cloudflared tunnel route dns <tunnel-uuid> config.example.com
```

Visit `https://config.example.com/` in a browser. You'll be redirected
to identity for login (existing passkey/password works). After signing
in, identity redirects back with `?code=…`; the SPA exchanges it for
tokens and lands on the namespace list.

### 4. Threat-model notes

- **localStorage tokens are XSS-readable.** This is acceptable here
  because (a) the SPA loads no third-party scripts, (b) CSP forbids
  inline scripts and `eval`, and (c) the threat model is a
  single-admin deployment, not a SaaS with many tenants. If your
  deployment differs (third-party scripts, multiple users), consider
  a backend-for-frontend (BFF) pattern instead.
- **CSP is restrictive but pragmatic.** `script-src 'self'` only;
  `connect-src` includes identity for OAuth; `frame-ancestors 'none'`
  prevents click-jacking.
- **Tab lifetime.** Access tokens expire in 15 min; the SPA
  auto-refreshes via the refresh token. PKCE state lives in
  `sessionStorage` so a half-finished login doesn't leak across
  browser sessions.
- **No revocation push.** As with the API, disabling a user on
  identity does not immediately log them out of config — see
  "Revocation lag" above.

## Configuration reference

Environment variables for the config service (systemd env file typically
at `/etc/config/config.env`):

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8282` | Listen port |
| `DB_PATH` | `config.db` | SQLite file |
| `IDENTITY_ENV` | `development` | `production` requires HTTPS for identity |
| `IDENTITY_ISSUER_URL` | `http://localhost:8181` (dev) | Required in production |
| `IDENTITY_ISSUER` | `IDENTITY_ISSUER_URL` | Expected JWT `iss` claim |
| `JWKS_CACHE_TTL` | `5m` | Duration string |
| `BACKUP_MIN_INTERVAL` | `30s` | Duration; 0 disables throttling |
| `TRUST_PROXY` | (unset) | `cloudflare` honours `CF-Connecting-IP` |
| `CORS_ORIGINS` | (unset) | Comma-separated allowed origins |
| `RATE_LIMIT_DISABLED` | `0` | `1` disables rate limiting (dev/test only) |
| `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET_NAME` | (unset) | Required together for backups |
| `OAUTH_CLIENT_ID` | (unset) | Public OAuth client_id registered on identity. Mounts the admin SPA at `/` when set. |
| `IDENTITY_PUBLIC_URL` | `IDENTITY_ISSUER_URL` | URL the *browser* uses to reach identity (overrides the issuer URL when behind a reverse proxy with a different public hostname). |
| `REQUIRED_AUDIENCE` | (unset) | Asserts incoming JWTs carry a matching `aud`. Mitigation against cross-service token replay; **set to `config` in production**. |
