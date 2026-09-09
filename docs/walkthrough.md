# Config service walkthrough

Copy-paste tour through every endpoint, with real output captured from a
dev server. Matches `scripts/e2e.sh`; if the script drifts from
this doc, one of them is wrong.

Prerequisites: identity running on `:8181`, config-server running
standalone on `:8282` (`./bin/config-server`), and the admin password
from identity's first run. All examples assume:

```bash
ID=http://localhost:8181
CFG=http://localhost:8282
ADMIN_TOK=$(curl -s -X POST $ID/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"adminpassword1"}' \
  | jq -r .access_token)
```

---

## 1. Health probe (unauth)

```bash
curl -s $CFG/healthz
```

```json
{"jwks":{"Fetches":0,"FetchErrors":0,"KidMisses":0,"Rotations":0,"StaleServed":0,"KeyCount":0,"FetchedAt":"0001-01-01T00:00:00Z","LastFetchError":""},"status":"ok","version":"dev"}
```

The `jwks` block reports the JWKS verifier's cache/fetch counters (it appears
only when the configured verifier exposes them). It's zeroed until the first
token is verified — the JWKS is fetched lazily — so a zero `FetchedAt` and
empty key set are normal right after startup.

## 2. Create an admin-only namespace

```bash
curl -s -X POST $CFG/api/v1/config/namespaces \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":       "houses",
    "read_role":  "admin",
    "write_role": "admin",
    "document":   {"main":"Rivendell","guest":"Hobbiton"}
  }'
```

`HTTP 201`

```json
{"name":"houses","read_role":"admin","write_role":"admin"}
```

## 3. Create a user-readable namespace

```bash
curl -s -X POST $CFG/api/v1/config/namespaces \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":       "mqtt_topics",
    "read_role":  "user",
    "write_role": "admin",
    "document":   {"temperature":"home/sensors/temp","humidity":"home/sensors/humidity"}
  }'
```

`HTTP 201`

```json
{"name":"mqtt_topics","read_role":"user","write_role":"admin"}
```

## 4. List visible namespaces

```bash
curl -s $CFG/api/v1/config -H "Authorization: Bearer $ADMIN_TOK"
```

`HTTP 200`

```json
[
  {"name":"houses","read_role":"admin","write_role":"admin",
   "updated_at":"2026-04-24T17:06:34.818Z","created_at":"2026-04-24T17:06:34.818Z"},
  {"name":"mqtt_topics","read_role":"user","write_role":"admin",
   "updated_at":"2026-04-24T17:06:34.828Z","created_at":"2026-04-24T17:06:34.828Z"}
]
```

A non-admin token sees only `mqtt_topics` (and an empty list if no
user-readable namespaces exist).

## 5. Fetch a document

```bash
curl -s $CFG/api/v1/config/houses -H "Authorization: Bearer $ADMIN_TOK"
```

`HTTP 200`

```json
{"main":"Rivendell","guest":"Hobbiton"}
```

The response body **is** the stored document — no envelope, no metadata.

## 6. Replace the document

```bash
curl -s -X PUT $CFG/api/v1/config/houses \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"main":"Rivendell","guest":"Hobbiton","holiday":"Lothlorien"}'
```

`HTTP 200`

```json
{"changed":true,"name":"houses"}
```

## 7. No-op PUT (same body)

```bash
curl -s -X PUT $CFG/api/v1/config/houses \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"main":"Rivendell","guest":"Hobbiton","holiday":"Lothlorien"}'
```

`HTTP 200`

```json
{"changed":false,"name":"houses"}
```

`changed:false` means the server detected byte-identical content after
JSON compaction, skipped the write, and did not trigger a backup.
Scripts can idempotently re-apply configuration without cost.

## 8. Update ACL

```bash
curl -s -X PATCH $CFG/api/v1/config/namespaces/mqtt_topics \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}'
```

`HTTP 200`

```json
{"name":"mqtt_topics","read_role":"admin","write_role":"admin"}
```

## 9. Delete

```bash
curl -s -X DELETE $CFG/api/v1/config/houses \
  -H "Authorization: Bearer $ADMIN_TOK" -o /dev/null -w '%{http_code}\n'
```

```
204
```

Subsequent `GET /api/v1/config/houses` returns 404.

## 10. Publish a namespace

Create `tariffs` user-readable first, then publish it.

```bash
curl -s -X POST $CFG/api/v1/config/namespaces \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":       "tariffs",
    "read_role":  "user",
    "write_role": "admin",
    "document":   {"unit":0.24,"standing":0.51}
  }'
```

`HTTP 201`

```json
{"name":"tariffs","read_role":"user","write_role":"admin"}
```

Moving `read_role` to `public` without the confirmation:

```bash
curl -s -X PATCH $CFG/api/v1/config/namespaces/tariffs \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"public","write_role":"admin"}'
```

`HTTP 400`

```json
{"error":"confirm_required","message":"making a namespace public requires confirm_public to equal the namespace name"}
```

With `confirm_public` set to the namespace name:

```bash
curl -s -X PATCH $CFG/api/v1/config/namespaces/tariffs \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"public","write_role":"admin","confirm_public":"tariffs"}'
```

`HTTP 200`

```json
{"name":"tariffs","read_role":"public","write_role":"admin"}
```

The confirmation guards the *transition* into public. A later PATCH on
an already-public namespace does not need it, and revoking never does.
`public` is a read role only — `"write_role":"public"` is rejected.

## 11. Anonymous read

No `Authorization` header at all:

```bash
curl -si $CFG/api/v1/config/tariffs
```

`HTTP 200`

```http
Content-Type: application/json
X-Read-Role: public
X-Write-Role: admin
Cache-Control: public, max-age=60
Vary: Authorization

{"standing":0.51,"unit":0.24}
```

`Cache-Control: public, max-age=60` is what lets a CDN serve the
document without touching the service — and it is also the revoke
latency, see section 12. Everything non-public keeps
`private, no-store`. `Vary: Authorization` is on both, because this URL
answers differently with and without a token.

A private namespace, anonymously — `mqtt_topics` is `admin`/`admin`
after section 8:

```bash
curl -s $CFG/api/v1/config/mqtt_topics
```

`HTTP 404`

```json
{"error":"not_found","message":"namespace not found"}
```

A namespace that never existed, anonymously:

```bash
curl -s $CFG/api/v1/config/no_such_thing
```

`HTTP 404`

```json
{"error":"not_found","message":"namespace not found"}
```

Byte-identical, deliberately: the anonymous read path cannot be used to
discover which namespaces exist.

A token that is *present* but bad is not downgraded to an anonymous
read, even on a public namespace:

```bash
curl -s $CFG/api/v1/config/tariffs \
  -H 'Authorization: Bearer not-a-real-token'
```

`HTTP 401`

```json
{"error":"unauthorized","message":"invalid or expired token"}
```

Listing is unchanged — still token-only, and nothing advertises public
namespace names to an anonymous caller:

```bash
curl -s $CFG/api/v1/config
```

`HTTP 401`

```json
{"error":"unauthorized","message":"missing authorization header"}
```

## 12. Revoke

```bash
curl -s -X PATCH $CFG/api/v1/config/namespaces/tariffs \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"user","write_role":"admin"}'
```

`HTTP 200`

```json
{"name":"tariffs","read_role":"user","write_role":"admin"}
```

The service stops serving it anonymously the moment this returns —
against a local server, the next tokenless `GET` is already a 404.
Through a CDN it is not: nothing purges the edge or browser copies, so
in production the real revoke latency is the 60-second cache TTL. See
**Public namespaces** in `admin.md`.

Read may be no stronger than write, which bites on the way down. Had
`tariffs` been `write_role: user`, revoking straight to
`read_role: admin` would need `write_role` raised in the same call:

```bash
curl -s -X PATCH $CFG/api/v1/config/namespaces/tariffs \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"user"}'
```

`HTTP 400`

```json
{"error":"invalid_role","message":"read_role must be 'admin', 'user' or 'public'; write_role must be 'admin' or 'user'; and read_role may be no stronger than write_role"}
```

## 13. Error shapes

### Missing auth

```bash
curl -s $CFG/api/v1/config
```

`HTTP 401`

```json
{"error":"unauthorized","message":"missing authorization header"}
```

### Invalid namespace name

```bash
curl -s -X POST $CFG/api/v1/config/namespaces \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '{"name":"BAD NAME","read_role":"admin","write_role":"admin","document":{}}'
```

`HTTP 400`

```json
{"error":"invalid_name","message":"namespace name must match ^[a-z0-9_-]{1,64}$"}
```

### Invalid document (array instead of object)

```bash
curl -s -X PUT $CFG/api/v1/config/demo \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d '[1,2,3]'
```

`HTTP 400`

```json
{"error":"invalid_document","message":"document must be a JSON object"}
```

### Role denial: 404 for unreadable, 403 for readable-but-unwritable

Non-admin token on an admin-only namespace: `HTTP 404` (never 403 — no
existence leak).

Non-admin token on a `read_role=user, write_role=admin` namespace,
attempting to PUT: `HTTP 403`.

No token at all on any namespace that is not `read_role=public`:
`HTTP 404`, indistinguishable from a namespace that does not exist.
Every write path still demands a token — 401 without one, never an
anonymous write.

See `admin.md` for the full ACL matrix.
