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

- Startup (config boots standalone; tokens just all fail until identity
  is reachable again)
- Database (fully separate SQLite file)
- Backups (separate R2 prefix)

If identity is down, config continues to serve cached JWKS but new
tokens will eventually fail to verify when the cache expires
(`JWKS_CACHE_TTL`, default 5 minutes). For a self-hosted deployment where
both services run on the same host behind the same tunnel, this is
rarely a concern in practice.

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

Identity currently issues user tokens with no `aud` claim. Any such
token is therefore valid on every service that trusts identity's JWKS —
config, plus any future sibling. If that matters for your deployment:

1. Set `REQUIRED_AUDIENCE=config` in config's env file. The verifier
   will then reject any token whose `aud` claim does not include
   `config`.
2. Have identity stamp `aud: "config"` (or a space-delimited list
   including it) on tokens intended for config use.

Both sides must be in lockstep: leaving config flagged on while
identity doesn't emit `aud` will reject every request. The feature is
off by default to keep the v1 JWT shape compatible.

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
| Audience | leave blank |

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
| `REQUIRED_AUDIENCE` | (unset) | Asserts incoming JWTs carry a matching `aud`. Mitigation against cross-service token replay; off until identity stamps `aud` on issuance. |
