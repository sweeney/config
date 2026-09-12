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
   "updated_at":"2026-04-24T17:06:34.818Z",
   "updated_by":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","updated_by_username":"sweeney",
   "created_at":"2026-04-24T17:06:34.818Z"},
  {"name":"mqtt_topics","read_role":"user","write_role":"admin",
   "updated_at":"2026-04-24T17:06:34.828Z",
   "updated_by":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","updated_by_username":"sweeney",
   "created_at":"2026-04-24T17:06:34.828Z"}
]
```

A non-admin token sees only `mqtt_topics` (and an empty list if no
user-readable namespaces exist).

`updated_by` is the subject of whoever last wrote the namespace — its
document or its ACL — and is always present; `updated_by_username` is the
name that subject went by at the time, recorded at write time rather than
looked up now, and omitted where none was recorded (a service token has no
user behind it). Both move when the document moves: after the PUT in section
6 they name whoever made it. That write also leaves a `document_write` row in
the audit trail of section 13 — these fields are the same answer without
reading the history.

They appear here and not on the single-namespace `GET` of section 5, which
can be answered anonymously for a public namespace (section 11). Publishing a
document does not publish who edits it; listing always needs a token.

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
JSON compaction, skipped the write, did not trigger a backup, and wrote
no audit row. Scripts can idempotently re-apply configuration without
cost, and every `document_write` in the trail is a real change.

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
Access-Control-Allow-Origin: *
Vary: Origin, Authorization

{"standing":0.51,"unit":0.24}
```

`Cache-Control: public, max-age=60` is what lets a CDN serve the
document without touching the service — and it is also the revoke
latency, see section 12. Everything non-public keeps
`private, no-store`. `Vary: Authorization` is on both, because this URL
answers differently with and without a token.

`Access-Control-Allow-Origin: *` appears only for a `read_role: public`
namespace; private ones keep whatever `CORS_ORIGINS` allows. A published
document is meant to be fetchable from anywhere, and withholding the
header would protect nothing — a server-to-server caller ignores CORS
entirely, so the only thing it stops is a browser. This document can
therefore be fetched from front-end JavaScript on any site, with no
token and no `CORS_ORIGINS` entry.

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

## 13. Audit trail

`tariffs` has been created (section 10), published (10) and revoked
(12), and its document has not been written since. The trail has all
three, oldest first:

```bash
curl -s $CFG/api/v1/config/namespaces/tariffs/audit \
  -H "Authorization: Bearer $ADMIN_TOK"
```

`HTTP 200`

```json
[
  {"action":"create","new_read_role":"user","new_write_role":"admin",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:35.101Z"},
  {"action":"acl_change","old_read_role":"user","old_write_role":"admin",
   "new_read_role":"public","new_write_role":"admin",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:35.402Z"},
  {"action":"acl_change","old_read_role":"public","old_write_role":"admin",
   "new_read_role":"user","new_write_role":"admin",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:35.688Z"}
]
```

`actor` is the identity subject and is always present; `actor_username` is
the name it went by at the time, recorded when the change was made rather
than looked up now — so the trail stays legible with identity unreachable,
and does not change meaning if someone is later renamed. It is omitted
where no username was recorded, and rows predating the column stay that way.

The old roles are absent on a `create`, the new roles on a `delete`, and
all four on a `document_write`, which moves no roles. The trail records
document writes as well as ACL changes — the PUT of section 6 is in the
`houses` history below — but never what was in the document: a
`document_write` says the contents changed, by whom and when, and
nothing more. The no-op PUT of section 7 left no row at all, having
changed nothing.

Admin-only, even though `tariffs` was public a moment ago: publishing a
document does not publish its history. To see that, you need a non-admin
token — create a `user` account and log in as it:

```bash
curl -s -X POST $ID/api/v1/users \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"username":"tourist","password":"touristpassword1","display_name":"tourist","role":"user"}'

USER_TOK=$(curl -s -X POST $ID/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"tourist","password":"touristpassword1"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["access_token"])')

curl -s $CFG/api/v1/config/namespaces/tariffs/audit \
  -H "Authorization: Bearer $USER_TOK"
```

`HTTP 403`

```json
{"error":"forbidden","message":"insufficient role for this operation"}
```

`403`, not the usual `404` — this endpoint has no existence to hide,
because only an admin can reach it and an admin can list everything
anyway. With no token at all it is `401`, public namespace or not.

`houses` was written in section 6 and deleted in section 9, and still
has a history:

```bash
curl -s $CFG/api/v1/config/namespaces/houses/audit \
  -H "Authorization: Bearer $ADMIN_TOK"
```

`HTTP 200`

```json
[
  {"action":"create","new_read_role":"admin","new_write_role":"admin",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:34.818Z"},
  {"action":"document_write",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:34.901Z"},
  {"action":"delete","old_read_role":"admin","old_write_role":"admin",
   "actor":"adcc1b9d-64f9-4a0f-b4e9-ab51a164b1c9","actor_username":"sweeney","at":"2026-04-24T17:06:35.002Z"}
]
```

A name that never existed is an empty array, not a 404:

```bash
curl -s $CFG/api/v1/config/namespaces/no_such_thing/audit \
  -H "Authorization: Bearer $ADMIN_TOK"
```

`HTTP 200`

```json
[]
```

Deliberate: entries outlive the namespace they describe, so *what
happened to the one that is no longer here* is the question this
endpoint exists to answer. A `404` for a deleted namespace would refuse
it in exactly the case that matters. (A name that is not a legal
namespace name is still `400 invalid_name`.)

## 14. Error shapes

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
