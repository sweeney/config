# Config service — administrator guide

Operational notes for running the config service: namespace design,
backups, restore, role changes, public namespaces, identity-coupling
concerns. For the integration surface, see
[`README.md`](../README.md).

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

Creating a namespace straight at `read_role: public` additionally
requires a `confirm_public` field — see [Publishing a
namespace](#publishing-a-namespace).

## Choosing roles

There are three roles, ranked weakest to strongest: `public` (0),
`user` (1), `admin` (2). Every access decision is a rank comparison, and
two rules cover all of it:

- A caller may read a namespace when its own rank is at least the
  namespace's `read_role` rank.
- A namespace's `read_role` may be no stronger than its `write_role`.

`public` is a valid `read_role` only — never a `write_role`. See
[Public namespaces](#public-namespaces) below for what that buys and
what it costs.

Defaults that age well:

- **Admin-only for everything operational.** MQTT broker passwords (even
  if you shouldn't store those here), device serial numbers, network
  topology. `read_role: admin`, `write_role: admin`.
- **User-readable for discovery data.** MQTT topic names, friendly names,
  location coords. `read_role: user`, `write_role: admin`.
- **User-writable only when genuinely user data.** Per-user UI prefs,
  dashboard layouts. `read_role: user`, `write_role: user`.
- **Public only when the data is genuinely public.** Anything a client
  legitimately needs before it has a token. `read_role: public`,
  `write_role: admin`. Deliberate step, not a default — read the whole
  of [Public namespaces](#public-namespaces) first.

Users cannot delete namespaces or change ACLs regardless of `write_role`
— both operations are admin-only. Anonymous callers can only read, and
only namespaces at `read_role: public`.

### Changing roles later

Use `PATCH /api/v1/config/namespaces/{ns}` with the new `read_role` and
`write_role`. The document is untouched. Note that tightening
`read_role` from `user` to `admin` will immediately start returning 404
to in-flight user requests — there is no grace period.

PATCH rewrites **both** roles on every call: it is a replacement, not a
partial update, so whatever you send is the new ACL in full. Two
consequences worth holding on to — moving a namespace *into*
`read_role: public` needs an explicit confirmation, and moving it back
*out* is not immediate. Both are covered under
[Public namespaces](#public-namespaces).

```bash
curl -s -X PATCH \
  https://config.example.com/api/v1/config/namespaces/mqtt_topics \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}'
```

## Public namespaces

A namespace at `read_role: public` is readable **with no token at all**:
`GET /api/v1/config/{ns}` with no `Authorization` header returns the
document. It is the only anonymous path into the API.

`public` is a read role only. Nothing is ever anonymously writable — the
`write_role` CHECK does not accept `public`, and there is no request
shape that reaches a write without a verified token. A role the service
does not recognise is unranked and satisfies nothing, so a malformed
`role` claim in a token fails closed rather than falling through to
public.

Three things anonymous callers deliberately do **not** get:

- **Discovery.** `GET /api/v1/config` is unchanged: it still requires a
  token, still answers `401` without one, and never lists public
  namespaces to an anonymous caller. Knowing the name is the price of
  reading it anonymously; nothing advertises the names.
- **A distinguishable miss.** An anonymous `GET` of a private namespace
  and an anonymous `GET` of a namespace that does not exist return
  byte-identical `404`s. The public read path cannot be turned into an
  enumeration oracle.
- **The write role.** An authenticated read carries both `X-Read-Role`
  and `X-Write-Role`; an anonymous read carries `X-Read-Role` only.
  That a plain `user` token suffices to write a given namespace is a
  nudge toward where a stolen token would be worth pointing, and nobody
  without a token can act on it. The read role is kept because the read
  having succeeded already implies it.

A token that is *present* but invalid or expired still gets `401`. It is
never quietly downgraded to an anonymous read: a client holding a stale
token needs to be told to refresh it, not silently handed whichever
subset of config happens to be public.

### Publishing a namespace

Setting `read_role: public` — on `POST /api/v1/config/namespaces` or on
`PATCH /api/v1/config/namespaces/{ns}` — requires a `confirm_public`
field in the body whose value is exactly the namespace name. Without it:

```http
HTTP/1.1 400 Bad Request
{"error":"confirm_required","message":"making a namespace public requires confirm_public to equal the namespace name"}
```

Three reasons the guard exists:

- **Publishing is the one ACL change that cannot be undone.** Every
  other role change takes effect and that is the end of it. Revoking
  `public` stops future reads but cannot unfetch what has already been
  served.
- **PATCH rewrites both roles on every call.** Without the guard, a
  stale `public` sitting in a dropdown — or in a script's saved
  payload — would publish a namespace as a side effect of an edit that
  was about something else entirely.
- **Binding it to the name** stops a body being replayed against a
  different namespace. A payload that publishes `tariffs` is inert
  against `mqtt_topics`.

Publishing `tariffs`, which currently has `read_role: user`:

```bash
curl -s -X PATCH \
  https://config.example.com/api/v1/config/namespaces/tariffs \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "read_role":      "public",
    "write_role":     "admin",
    "confirm_public": "tariffs"
  }'
```

Confirmation guards the *transition* into public, not the state of
being public. A namespace that is already public does not need
`confirm_public` on a later PATCH that leaves `read_role` alone, and
revoking never needs it.

### Revoking

Revoke by PATCHing `read_role` back to `user` or `admin`.

Mind the read-no-stronger-than-write invariant on the way down. A
namespace at `read_role: public, write_role: user` can move to
`read_role: user` freely, but going straight to `read_role: admin`
must raise `write_role` to `admin` in the same PATCH — otherwise the
combination asks for readers stronger than writers and is rejected:

```bash
# rejected — 400 invalid_role: read=admin is stronger than write=user
-d '{"read_role":"admin","write_role":"user"}'

# accepted
-d '{"read_role":"admin","write_role":"admin"}'
```

It is an easy one to trip over, because the namespace was perfectly
valid a moment earlier and the field you were thinking about is the one
you got right.

### Revoke latency is the cache TTL, not the PATCH

`Cache-Control: public, max-age=60` is served to an **anonymous read of
a public namespace**, and to nothing else. An authenticated read of that
same namespace gets `private, no-store`, like every other authenticated
read. All of them carry `Vary: Authorization`, because the same URL
answers differently with and without a token and no cache may serve one
response to the other.

Note that the caching decision is keyed on the *caller*, not just the
namespace. `Vary` alone would keep a token-bearing copy correct, but it
would also store one copy per distinct token, and it would mark a
response served to an identified principal as shared-cacheable. Keying
on the caller also confines the window below to the anonymous copies,
which are the only ones a revoke cannot reach anyway.

The shared-cache header is most of the point of publishing: Cloudflare's
edge, and every browser behind it, can serve the document without
touching the service at all. It is also the thing to understand before
you publish anything.

**Flipping `read_role` back does not purge any of those caches.** The
PATCH stops the service from serving the document anonymously, and it
does that immediately, but edge and browser copies keep being handed out
until they expire. The practical revoke latency is the **60-second cache
TTL**, not the PATCH. Plan revocations around a minute, not an instant,
and treat anything you could not tolerate being readable for one more
minute as something you should not have published.

A manual Cloudflare cache purge for the URL is the only way to shorten
that window at the edge — nothing in the config service does it for you.
Browser caches you cannot purge at all.

The `404` is cached deliberately too — as `private, no-store`. Those
headers are set *before* the namespace lookup, so a miss carries them as
well as a hit. A `404` is heuristically cacheable under RFC 9111 §4.2.2,
and it is the only answer an anonymous caller gets for a private
namespace; without the headers a shared cache could key that anonymous
`404` without regard to `Authorization` and replay it to a user who can
actually read the namespace. Production sits behind Cloudflare, so this
was not a hypothetical.

### Public reads carry wildcard CORS headers

A `GET` of a `read_role: public` namespace also answers with
`Access-Control-Allow-Origin: *`. Nothing else does: private namespaces
keep the allow-list configured in `CORS_ORIGINS`, and so does every
write path.

The reasoning is the same as the shared-cache header above. A published
document is meant to be fetchable from anywhere, including a browser on
an origin this service has never heard of. Withholding the header
protects nothing, because a server-to-server caller ignores CORS
entirely — it only breaks the browser half of the audience, which is
the half least able to work around it. So the wildcard is scoped
exactly to what an admin deliberately published, and to nothing else.

Two headers travel with that wildcard and are easy to miss.

`Access-Control-Expose-Headers: X-Read-Role, X-Write-Role` goes out
alongside it. The CORS middleware only sends that header to allow-listed
origins, so without it the role headers on the response were present on
the wire but unreadable from JavaScript — for exactly the audience the
wildcard exists to serve.

And the **preflight** is answered for any origin, but only on the
single-namespace path (`/api/v1/config/{ns}` — exactly one path segment
after `/api/v1/config/`). A non-allow-listed `OPTIONS` there gets
`Access-Control-Allow-Origin: *`, `Allow-Methods: GET, OPTIONS`,
`Allow-Headers: Content-Type`, the expose-headers above, and
`Max-Age: 86400`. Previously every `OPTIONS` under `/api/` was answered
by the security-headers middleware, which emits CORS headers only for
allow-listed origins — so the wildcard held for *simple* GETs and
nothing else, and a client that set `Content-Type` on the GET, or sent
any custom header, preflighted and was refused.

Answering it permissively is safe because a preflight says only which
method and headers may be *attempted*. The GET still enforces the ACL,
and a namespace the caller may not read still answers `404`. Note what
is **not** on that list: `Authorization`. The wildcard exists for
anonymous reads; a cross-origin request carrying a token still goes
through `CORS_ORIGINS`. Every other route keeps allow-list behaviour
unchanged.

The practical consequence is worth stating plainly: a public namespace
can be fetched straight from front-end JavaScript on any site in the
world, with no token and without anyone adding that site to
`CORS_ORIGINS`. That is the intended behaviour — and it is one more
reason to read [What to publish](#what-to-publish) before you publish
anything, because "readable by anyone who knows the name" now includes
"readable by a script on a page you have never seen".

### Public reads survive an identity outage

An anonymous read verifies no token, so it never touches JWKS. While
identity is unreachable and every authenticated request is answering
`503` (see [Identity outages answer 503, not
401](#identity-outages-answer-503-not-401)), a tokenless `GET` of a
public namespace still returns `200`.

That makes public namespaces the one read path that does not depend on
identity being up, which is worth knowing when deciding where a client's
bootstrap config should live. The catch is that the client has to *not*
send the header: a request carrying a token during an outage is a
request the service cannot verify, and it gets the `503` like everyone
else. A client that wants the fallback has to retry without the
`Authorization` header, deliberately.

### What to publish

Publish only what you would be content to see indexed. Anonymous means
anonymous: no account behind the request, nothing but the per-IP rate
limit in front of it, and no record of who read it. Reads are not
audited for anyone, authenticated or not — the audit trail below
records ACL changes, not access.

Reasonable:

- Data a client legitimately needs before it can hold a token — service
  endpoints, feature flags that gate a login screen, supported regions.
- Reference data that is public anyway — tariff rates, opening hours,
  a list of names you would not mind a stranger reading.

Never:

- Anything you would not paste into a public web page. Documents are
  not supposed to hold secrets in the first place, but `public` removes
  the last accidental protection that policy had.
- Anything naming internal hosts, device IDs, MQTT topics or network
  topology. Collectively that is an inventory of the house.
- Per-user data of any kind. A namespace is a single shared document;
  there is no per-caller view of it, public or otherwise.

## Audit trail

Namespace lifecycle changes are recorded in a `config_audit` table: one
row per `create`, `acl_change` and `delete`, carrying the namespace, the
action, the old and new read/write roles, the actor's subject, and a
timestamp. Old roles are empty on a create; new roles are empty on a
delete. The table lives in the same SQLite file as the namespaces, so it
rides along in every R2 backup.

Three deliberate limits, worth knowing before you rely on it:

- **Document writes are not audited, and document bodies are never
  stored.** Every write already ships the whole database to R2, so
  auditing 64KB bodies would inflate both the database and every backup
  without bound. The table answers *who changed the rules, and when* —
  not *what was in it*.
- **The audit row is written in the same transaction as the mutation.**
  A mutation that fails leaves no audit row behind, and a row that is
  present always describes a change that really happened. A trail with
  gaps is worse than no trail, because it gets believed.
- **There is no foreign key to `config_namespaces`.** `PRAGMA
  foreign_keys` is ON, so a foreign key here would either cascade the
  rows away when a namespace is deleted — destroying the record of the
  deletion, which is the single event most worth keeping — or block the
  delete outright. The trail has to outlive the thing it describes.

### Reading it over the API

`GET /api/v1/config/namespaces/{ns}/audit` returns one namespace's
history as a JSON array, oldest first:

```bash
curl -s https://config.example.com/api/v1/config/namespaces/tariffs/audit \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

```json
[
  {
    "action":         "create",
    "new_read_role":  "user",
    "new_write_role": "admin",
    "actor":          "usr_01H8ZQK3M7",
    "at":             "2026-09-01T09:14:02.113Z"
  },
  {
    "action":         "acl_change",
    "old_read_role":  "user",
    "old_write_role": "admin",
    "new_read_role":  "public",
    "new_write_role": "admin",
    "actor":          "usr_01H8ZQK3M7",
    "at":             "2026-09-04T11:02:47.906Z"
  }
]
```

The old roles are absent on a `create` and the new roles on a `delete`,
which is the same thing the empty columns say below — absent rather than
empty, because there was no previous ACL and no resulting one. (The JSON
keys are `old_read_role` / `new_read_role`; the columns they come from
are `old_read` / `new_read`.)

**Admin-only, including when the namespace is public.** Publishing a
document does not publish its history. A document and its history are
different things, and every operation the trail records — create, ACL
change, delete — is admin-only already, so anything weaker here would
leak more through the history than through the resource itself. A `user`
token gets `403`, a service token gets `403` (service tokens are pinned
to the `user` role), and no token at all gets `401` — even on a
namespace anyone in the world can read anonymously.

The response carries `Cache-Control: private, no-store` and no wildcard
origin, unlike the public document reads it may be describing.

**An unknown or deleted namespace returns `200 []`, not `404`.** This is
not the no-existence-leak rule bending: the caller is an admin, who can
list every namespace anyway, so there is nothing here to leak. It is the
trail outliving the thing it describes, the same reason there is no
foreign key. *What happened to the namespace that is no longer here* is
exactly what this endpoint is for, and a `404` would withhold the answer
for precisely the namespaces where it matters most.

The admin SPA shows the same history in its namespace view, so the
routine "who published this, and when?" question needs neither a curl
nor a shell on the host.

### Direct access with `sqlite3`

The table is still queryable on the box, and that is the route to reach
for when you are already there, when the service is down, or when the
question spans namespaces — the API answers for one namespace at a time.

```bash
sudo -u config sqlite3 -header -column /var/lib/config/config.db \
  "SELECT at, action, old_read, new_read, actor
     FROM config_audit
    WHERE namespace = 'tariffs'
    ORDER BY at;"
```

```
at                          action      old_read  new_read  actor
--------------------------  ----------  --------  --------  ---------
2026-09-01T09:14:02.113Z    create                user      usr_01H8…
2026-09-04T11:02:47.906Z    acl_change  user      public    usr_01H8…
```

The cross-namespace question — when did *anything* become public, and
who did it — has no API equivalent for that reason:

```bash
sudo -u config sqlite3 -header -column /var/lib/config/config.db \
  "SELECT namespace, at, actor
     FROM config_audit
    WHERE action = 'acl_change'
      AND new_read = 'public'
      AND old_read <> 'public'
    ORDER BY at DESC;"
```

That is history, not current state: a namespace listed there may have
been revoked since. For what is readable anonymously *right now*, ask
the namespace table instead:

```bash
sudo -u config sqlite3 -header -column /var/lib/config/config.db \
  "SELECT name, write_role, updated_at FROM config_namespaces
    WHERE read_role = 'public';"
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

Failed backups log to stdout with an `audit:` line and do **not**
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
- Tokenless reads of `read_role: public` namespaces — there is no token
  to verify, so they keep answering `200` throughout an outage. See
  [Public reads survive an identity
  outage](#public-reads-survive-an-identity-outage).

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
