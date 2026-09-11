# Config service

Stores structured configuration as named JSON documents with per-namespace
role ACLs. Validates JWTs against the identity service's JWKS endpoint —
so you authenticate exactly the same way you do with identity. A namespace
can also be marked `read_role: public`, which makes it readable with no
token at all; nothing is ever anonymously writable.

- Default port: **8282**
- OpenAPI spec (live): `GET /openapi.json` or `GET /openapi.yaml`

---

## Table of contents

1. [Auth wiring — how tokens work](#auth-wiring)
2. [Getting a token](#getting-a-token)
3. [URL structure and response format](#url-structure-and-response-format)
4. [All endpoints](#all-endpoints)
5. [Error reference](#error-reference)
6. [Role model and ACL matrix](#role-model-and-acl-matrix)
7. [Complete worked examples](#complete-worked-examples)
8. [Client guidance](#client-guidance)
9. [Running locally](#running-locally)
10. [Deploy](#deploy)

---

## Auth wiring

**Short answer:** use the Bearer token from the identity service. No separate
login, no separate credentials.

```
Authorization: Bearer <access_token_from_identity>
```

When the config service receives a request it:

1. Extracts the `Authorization: Bearer …` header.
2. Fetches identity's public keys from
   `{IDENTITY_ISSUER_URL}/.well-known/jwks.json` (cached, refetched on new
   `kid`).
3. Verifies the JWT signature against those keys.
4. Checks `iss` matches `IDENTITY_ISSUER` and the token is not expired.
5. Reads the `role` claim (`admin` or `user`) to enforce per-namespace ACLs.

**The config service issues no tokens of its own.** Every Bearer token comes
from identity. Token expiry, refresh, and rotation are handled there.

**One endpoint is optionally authenticated.** `GET /api/v1/config/{ns}`
accepts a request with no `Authorization` header and treats it as the
synthetic `public` role, which satisfies namespaces at `read_role: public`
and nothing else. A header that *is* present must still verify: a bad or
expired token gets `401`, never a silent downgrade to an anonymous read.
Every other endpoint requires a token.

**Service tokens (client credentials) are accepted, as `user`.** A token
whose `typ` header is `at+jwt` (the OAuth 2.0 service token type) is verified
like any other and pinned to the `user` role, whatever the client is
otherwise entitled to. So a service token can read and write `user`
namespaces but can never create, delete, change an ACL, or read an audit
trail — those are admin-only, and no client-credentials grant reaches them.

### Token lifetime

Identity access tokens expire in 15 minutes. When the config service returns
`401 unauthorized` with `{"error":"unauthorized","message":"invalid or expired
token"}`, refresh via identity:

```bash
NEW_TOKEN=$(curl -s -X POST https://id.example.com/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH_TOKEN\"}" \
  | jq -r .access_token)
```

Persist the new refresh token immediately — the old one is invalidated.

### Key rotation

If identity rotates its JWT signing key (`--rotate-jwt-key`), the config
service refetches JWKS on the first request that carries the new `kid`. Expect
one extra round-trip; no restart needed.

---

## Getting a token

```bash
# Login with username + password
RESP=$(curl -s -X POST https://id.example.com/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"…"}')

ACCESS_TOKEN=$(echo "$RESP" | jq -r .access_token)
REFRESH_TOKEN=$(echo "$RESP" | jq -r .refresh_token)
```

The access token is a signed JWT. Pass it verbatim to the config service.

For automated services, register an OAuth client on identity and use the
Authorization Code + PKCE flow, or use a long-lived user account. The config
service does not support client credentials in v1.

---

## URL structure and response format

### Base URL

```
https://config.example.com
```

All API endpoints are under `/api/v1/`. `/healthz` needs no auth, and
`GET /api/v1/config/{ns}` serves `read_role: public` namespaces without one.
Everything else requires a Bearer token.

### Fetching a namespace document

```
GET /api/v1/config/{namespace}
Authorization: Bearer <token>
```

**The response body is the stored JSON object — no envelope, no wrapper.**

```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Read-Role: user
X-Write-Role: admin
Cache-Control: private, no-store
Vary: Authorization

{"temperature":"home/sensors/temp","humidity":"home/sensors/humidity"}
```

Parse the body directly. The two response headers tell you the ACL roles for
this namespace without needing a second request:

| Header | Value | Meaning |
|---|---|---|
| `X-Read-Role` | `admin`, `user` or `public` | Role required to read |
| `X-Write-Role` | `admin` or `user` | Role required to write. Sent only to a caller that presented a token |

`X-Write-Role` is withheld from anonymous callers: that a plain `user` token
suffices to write is a nudge toward where to point a stolen one, and nobody
without a token can act on it. `X-Read-Role` stays, because a successful read
already implies it.

`Cache-Control` follows the *caller*, not just the read role: `public,
max-age=60` only for an anonymous read of a `read_role: public` namespace, so
shared caches can serve it; `private, no-store` for everything else, an
authenticated read of that same public namespace included. `Vary:
Authorization` is set either way — the same URL answers differently with and
without a token, so a cache must never serve one response to the other. Both
headers are set before the namespace lookup, so a `404` carries them too.

A `read_role: public` namespace also answers with
`Access-Control-Allow-Origin: *` and `Access-Control-Expose-Headers:
X-Read-Role, X-Write-Role`, so it can be fetched — and its role headers read —
from browser JavaScript on any origin. Everything else keeps the
`CORS_ORIGINS` allow-list. See [Client guidance](#client-guidance).

### Namespace names

Names must match `^[a-z0-9_-]{1,64}$`: lowercase letters, digits, hyphens,
underscores, 1–64 characters. Examples: `mqtt_topics`, `house-config`,
`devices123`.

---

## All endpoints

### `GET /healthz` — health probe (no auth)

```bash
curl https://config.example.com/healthz
```

```json
{"status":"ok","version":"abc1234"}
```

The `version` field is the git commit short SHA baked in at build time.

---

### `GET /api/v1/config` — list visible namespaces

Returns summaries of every namespace the caller's role can read. Admins see
all namespaces; users see those with `read_role=user` and `read_role=public`.

**A token is always required here**, including for public namespaces: the
list endpoint answers `401` without one and never enumerates public
namespaces to an anonymous caller. Knowing a namespace's name is the price of
reading it anonymously; nothing advertises the names.

```bash
curl https://config.example.com/api/v1/config \
  -H "Authorization: Bearer $TOKEN"
```

```json
[
  {
    "name":       "mqtt_topics",
    "read_role":  "user",
    "write_role": "admin",
    "updated_at": "2026-05-01T10:00:00.000Z",
    "created_at": "2026-04-01T09:00:00.000Z"
  },
  {
    "name":       "houses",
    "read_role":  "admin",
    "write_role": "admin",
    "updated_at": "2026-04-15T14:22:00.000Z",
    "created_at": "2026-04-15T14:22:00.000Z"
  }
]
```

If the caller has the `user` role, `houses` would not appear in this list at
all (not even as a tombstone).

---

### `GET /api/v1/config/{ns}` — fetch a document

```bash
curl https://config.example.com/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN"
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Read-Role: user
X-Write-Role: admin

{"temperature":"home/sensors/temp","humidity":"home/sensors/humidity"}
```

The body is the verbatim stored document. There is no metadata wrapper. If the
namespace has `"document": {}` stored, you get `{}`.

**Returns 404 in two cases:**
- The namespace does not exist.
- The namespace exists but the caller's role does not satisfy `read_role`.

Both cases return the same response so callers cannot probe namespace existence
without read access.

#### Anonymous reads

Omit the header entirely and the request is treated as the `public` role:

```bash
curl -i https://config.example.com/api/v1/config/tariffs
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
X-Read-Role: public
Cache-Control: public, max-age=60
Access-Control-Allow-Origin: *
Access-Control-Expose-Headers: X-Read-Role, X-Write-Role
Vary: Authorization

{"standing":0.51,"unit":0.24}
```

Note what is missing: **no `X-Write-Role`**. An authenticated read of this same
namespace gets it; an anonymous one does not. And **`public, max-age=60` is the
anonymous answer only** — read the same namespace with a token and you get
`private, no-store`, like every other authenticated read. Caching is keyed on
the caller, not the namespace: `Vary` would keep a token-bearing copy correct,
but it would also store one copy per distinct token and mark a response served
to an identified principal as shared-cacheable.

The wildcard origin is part of publishing: a published document is meant to be
fetchable from anywhere, including a browser on an origin this service has
never heard of, and withholding the header would protect nothing because
server-to-server callers ignore CORS entirely. A public namespace is therefore
readable from front-end JavaScript on any site, with no token and no
`CORS_ORIGINS` entry. Private namespaces do not get it.
`Access-Control-Expose-Headers` rides along because the CORS middleware only
sends it to allow-listed origins — without it the role headers would be
unreadable from JS for exactly the audience the wildcard serves.

The **preflight** is answered for any origin too, but only on this path. See
[CORS](#client-guidance).

Anonymously, a private namespace and a namespace that does not exist return
byte-identical `404`s, so the anonymous path cannot be used to enumerate what
exists. Those `404`s carry `Cache-Control: private, no-store` and
`Vary: Authorization` as well — a `404` is heuristically cacheable, and it is
the only answer an anonymous caller gets for a private namespace, so without
them a shared cache could key it without regard to `Authorization` and replay
it to someone who *can* read that namespace. A header that is present but
carries a bad or expired token is `401` even on a public namespace — it is
never downgraded to an anonymous read.

Because an anonymous read verifies no token, it needs no JWKS and therefore
keeps working while identity is unreachable, when authenticated requests are
answering `503`. See **Identity coupling** in `docs/admin.md`.

---

### `PUT /api/v1/config/{ns}` — replace a document

Whole-document replacement. Requires the caller's role to satisfy `write_role`.

```bash
curl -X PUT https://config.example.com/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"temperature":"home/sensors/temp","humidity":"home/sensors/humidity","pressure":"home/sensors/pressure"}'
```

```json
{"name":"mqtt_topics","changed":true}
```

`changed: true` means the document was different and a write occurred.
`changed: false` means the submitted document was byte-identical (after JSON
compaction) to what was stored. No database write occurs, no backup is
triggered. Safe to call in an idempotent loop.

**Size limit:** 64 KB after JSON compaction. Returns `413` with
`"error":"document_too_large"` if exceeded. Request body cap is 128 KB.

**Only objects are accepted.** Arrays, strings, and other top-level JSON types
return `400` with `"error":"invalid_document"`.

---

### `DELETE /api/v1/config/{ns}` — delete a namespace (admin only)

```bash
curl -X DELETE https://config.example.com/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN"
```

```
HTTP/1.1 204 No Content
```

Empty body on success. Returns 404 if the namespace did not exist (or the
caller lacks read access — same as GET).

---

### `POST /api/v1/config/namespaces` — create a namespace (admin only)

```bash
curl -X POST https://config.example.com/api/v1/config/namespaces \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":       "mqtt_topics",
    "read_role":  "user",
    "write_role": "admin",
    "document":   {"temperature":"home/sensors/temp"}
  }'
```

```json
{"name":"mqtt_topics","read_role":"user","write_role":"admin"}
```

`document` is optional; omit it or pass `{}` for an empty document.

**ACL constraint:** `write_role` must be no less restrictive than `read_role`.
`write_role=user, read_role=admin` is rejected because it would create a
read-oracle: a writer who can't read the existing document can't safely perform
a read-modify-write. The valid combinations:

| `read_role` | `write_role` | Allowed? |
|---|---|---|
| `public` | `user` | ✓ anyone can read, tokenless; any user writes |
| `public` | `admin` | ✓ anyone can read, tokenless; only admins write |
| `user` | `user` | ✓ anyone can read and write |
| `user` | `admin` | ✓ anyone can read; only admins write |
| `admin` | `admin` | ✓ admins only |
| `admin` | `user` | ✗ writers can't read — rejected |
| any | `public` | ✗ `public` is a read role only — rejected |

**Publishing requires confirmation.** Setting `read_role: public` — here or on
PATCH — requires a `confirm_public` field whose value is exactly the namespace
name. Otherwise the call is rejected with `400 confirm_required`:

```bash
curl -X POST https://config.example.com/api/v1/config/namespaces \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "name":           "tariffs",
    "read_role":      "public",
    "write_role":     "admin",
    "document":       {"unit":0.24},
    "confirm_public": "tariffs"
  }'
```

Publishing is the one ACL change that cannot be undone — revoking stops future
reads but cannot unfetch what has already been served — and PATCH rewrites both
roles on every call, so without the guard a stale `public` in a saved payload
could publish a namespace as a side effect of an unrelated edit. Binding the
confirmation to the name also stops a body being replayed against a different
namespace.

---

### `PATCH /api/v1/config/namespaces/{ns}` — update ACL (admin only)

Changes `read_role` and `write_role`; the document is untouched.

```bash
curl -X PATCH https://config.example.com/api/v1/config/namespaces/mqtt_topics \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}'
```

```http
HTTP/1.1 200 OK
X-Read-Role: admin
X-Write-Role: admin
Content-Type: application/json

{"name":"mqtt_topics","read_role":"admin","write_role":"admin"}
```

Both roles are replaced on every call — this is a whole-ACL replacement, not a
partial update. Moving `read_role` to `public` requires `confirm_public` (see
above); a namespace that is already public does not need it again, and revoking
never does.

Watch the read ≤ write invariant when revoking. A namespace at
`read_role: public, write_role: user` can move to `read_role: user` freely, but
going straight to `read_role: admin` must raise `write_role` to `admin` in the
same PATCH, or the combination is rejected with `400 invalid_role`.

---

### `GET /api/v1/config/namespaces/{ns}/audit` — namespace history (admin only)

Returns the namespace's recorded lifecycle changes as a JSON array, oldest
first: one entry per `create`, `acl_change` and `delete`.

```bash
curl https://config.example.com/api/v1/config/namespaces/tariffs/audit \
  -H "Authorization: Bearer $TOKEN"
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

| Field | Notes |
|---|---|
| `action` | `create`, `acl_change` or `delete` |
| `old_read_role`, `old_write_role` | The ACL before the change; omitted on a `create` |
| `new_read_role`, `new_write_role` | The ACL after it; omitted on a `delete` |
| `actor` | Subject of the token that made the change |
| `at` | RFC 3339 UTC, millisecond precision |

**Admin-only, including when the namespace is `read_role: public`.** Publishing
a document does not publish its history: every operation the trail records is
admin-only already, so a weaker rule here would leak more through the history
than through the resource. A `user` token gets `403`, so does a service token
(pinned to the `user` role), and a request with no token gets `401` even on a
public namespace. The response is `Cache-Control: private, no-store` and never
carries a wildcard origin.

**Document writes are not audited, and document bodies are never stored.** The
trail answers *who changed the rules, and when* — never *what was in it*.

**An unknown or deleted namespace returns `200 []`, not `404`.** Entries
outlive the namespace they describe, and "what happened to the one that is no
longer here" is exactly what this answers. Nothing leaks by doing so — the
caller is already an admin, who can list every namespace anyway. A name that
does not match `^[a-z0-9_-]{1,64}$` is still `400 invalid_name`.

The admin SPA shows the same history in its namespace view. For direct
`sqlite3` access on the host, and for questions that span namespaces, see
**Audit trail** in `docs/admin.md`.

---

## Error reference

All errors use the same envelope:

```json
{"error":"snake_case_code","message":"Human readable string"}
```

| HTTP | `error` | Cause |
|---|---|---|
| 400 | `invalid_name` | Namespace name doesn't match `^[a-z0-9_-]{1,64}$` |
| 400 | `invalid_role` | `read_role` must be `admin`, `user` or `public`; `write_role` must be `admin` or `user`; or `read_role` is stronger than `write_role` |
| 400 | `confirm_required` | Setting `read_role: public` without `confirm_public` equal to the namespace name |
| 400 | `invalid_document` | Body isn't a JSON object, or nesting > 64 levels |
| 400 | `invalid_request` | Malformed JSON body |
| 401 | `unauthorized` | Missing `Authorization` header on an endpoint that requires one (everything except `GET /api/v1/config/{ns}`) |
| 401 | `unauthorized` | Token expired, signature invalid, or wrong issuer — including on a public namespace, where a bad token is never downgraded to an anonymous read |
| 403 | `account_disabled` | Token valid but the identity account is disabled |
| 403 | `forbidden` | Service token used for an admin-only operation — service tokens are pinned to the `user` role |
| 403 | `forbidden` | Token carries a role the service does not recognise |
| 403 | `forbidden` | Caller can read the namespace but their role doesn't satisfy `write_role` |
| 403 | `forbidden` | Operation requires `admin` (create/delete/patch-acl, and reading a namespace's audit trail — which stays admin-only even when the namespace is `read_role: public`) |
| 404 | `not_found` | Namespace missing, or caller lacks `read_role` — including an anonymous caller on any namespace that isn't `read_role: public` |
| 409 | `conflict` | `POST /namespaces` with a name that already exists |
| 413 | `document_too_large` | Stored document would exceed 64 KB |
| 413 | `request_too_large` | Request body exceeds 128 KB |
| 500 | `internal_error` | Server fault — retry with backoff, then file an issue |

---

## Role model and ACL matrix

Each namespace carries a `read_role` (`admin`, `user` or `public`) and a
`write_role` (`admin` or `user`). Callers carry a role claim in their JWT; a
request with no `Authorization` header is the synthetic `public` caller.

Roles are ranked `public` (0) < `user` (1) < `admin` (2), and two rules cover
every decision:

- A caller may read a namespace when its rank is at least the namespace's
  `read_role` rank.
- A namespace's `read_role` may be no stronger than its `write_role`.

A role the service does not recognise is unranked and satisfies nothing, so a
malformed `role` claim fails closed rather than falling through to `public`.

**Role resolution:**

| Caller role | Satisfies `admin` | Satisfies `user` | Satisfies `public` |
|---|---|---|---|
| `admin` | ✓ | ✓ | ✓ |
| `user` | ✗ | ✓ | ✓ |
| `public` (no token) | ✗ | ✗ | ✓ |

`public` is a valid `read_role` only, never a `write_role`. A namespace may be
readable without a token; nothing is ever anonymously writable.

**Per-operation requirements:**

| Operation | Required role |
|---|---|
| `GET /api/v1/config` | Any valid user token (never anonymous) |
| `GET /api/v1/config/{ns}` | Satisfies namespace `read_role`; anonymous when that is `public` |
| `PUT /api/v1/config/{ns}` | Satisfies namespace `write_role` |
| `DELETE /api/v1/config/{ns}` | `admin` (regardless of namespace ACL) |
| `POST /api/v1/config/namespaces` | `admin` |
| `PATCH /api/v1/config/namespaces/{ns}` | `admin` |
| `GET /api/v1/config/namespaces/{ns}/audit` | `admin`, even when the namespace is `read_role: public` |

**No existence leak:** any operation by a caller who fails the `read_role`
check returns **404**, never 403. Callers who satisfy `read_role` but fail
`write_role` receive **403** (they already know the namespace exists from the
GET).

The audit endpoint sits outside that rule in both directions: a non-admin gets
**403** rather than 404, and an admin asking about a namespace that does not
exist gets **`200 []`** rather than 404. Neither leaks anything, because only
admins reach it and admins can list every namespace anyway.

---

## Complete worked examples

### curl — full flow from login to read

```bash
ID=https://id.example.com
CFG=https://config.example.com

# 1. Get a token from identity
RESP=$(curl -s -X POST $ID/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"correct-horse-battery-staple"}')
TOKEN=$(echo "$RESP" | jq -r .access_token)
REFRESH=$(echo "$RESP" | jq -r .refresh_token)

# 2. Fetch a config namespace
curl -s $CFG/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN"
# → {"temperature":"home/sensors/temp","humidity":"home/sensors/humidity"}

# 3. Read-modify-write a single key
DOC=$(curl -s $CFG/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN")
UPDATED=$(echo "$DOC" | jq '.pressure = "home/sensors/pressure"')
curl -s -X PUT $CFG/api/v1/config/mqtt_topics \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$UPDATED"
# → {"name":"mqtt_topics","changed":true}

# 4. Refresh when the token nears expiry (15-min lifetime)
NEW=$(curl -s -X POST $ID/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}")
TOKEN=$(echo "$NEW" | jq -r .access_token)
REFRESH=$(echo "$NEW" | jq -r .refresh_token)   # always save the new one
```

---

### Go

```go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
)

func fetchConfig(baseURL, token, namespace string) (map[string]any, error) {
    req, err := http.NewRequest("GET",
        baseURL+"/api/v1/config/"+namespace, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Authorization", "Bearer "+token)

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        return nil, fmt.Errorf("namespace %q not found (or not readable with this token)", namespace)
    }
    if resp.StatusCode != http.StatusOK {
        var e struct{ Error, Message string }
        json.NewDecoder(resp.Body).Decode(&e)
        return nil, fmt.Errorf("config API %d: %s — %s", resp.StatusCode, e.Error, e.Message)
    }

    var doc map[string]any
    if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
        return nil, err
    }
    return doc, nil
}
```

---

### Python

```python
import requests

def fetch_config(base_url: str, token: str, namespace: str) -> dict:
    r = requests.get(
        f"{base_url}/api/v1/config/{namespace}",
        headers={"Authorization": f"Bearer {token}"},
        timeout=5,
    )
    if r.status_code == 404:
        raise KeyError(f"namespace {namespace!r} not found or not readable")
    r.raise_for_status()
    return r.json()   # the stored document, no envelope


def put_config(base_url: str, token: str, namespace: str, doc: dict) -> bool:
    r = requests.put(
        f"{base_url}/api/v1/config/{namespace}",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        json=doc,
        timeout=5,
    )
    r.raise_for_status()
    return r.json()["changed"]   # True if the server actually wrote
```

---

### Swift (URLSession)

```swift
func fetchConfig(baseURL: String, token: String, namespace: String) async throws -> [String: Any] {
    var request = URLRequest(url: URL(string: "\(baseURL)/api/v1/config/\(namespace)")!)
    request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")

    let (data, response) = try await URLSession.shared.data(for: request)
    let http = response as! HTTPURLResponse

    if http.statusCode == 404 {
        throw ConfigError.notFound(namespace)
    }
    guard http.statusCode == 200 else {
        throw ConfigError.apiError(http.statusCode)
    }

    return try JSONSerialization.jsonObject(with: data) as! [String: Any]
}
```

---

## Client guidance

**Token caching.** Access tokens are valid for 15 minutes. Cache the token in
memory and refresh it proactively (e.g. when < 60 s remain) rather than
waiting for a 401. Identity returns `expires_in` alongside the token if you
need to compute the deadline.

**Config caching.** The config service sets `Cache-Control: private, no-store`
on document responses, with one exception: an *anonymous* read of a
`read_role: public` namespace gets `public, max-age=60` so shared caches can
serve it. Send a token to that same namespace and you are back to
`private, no-store` — the decision follows the caller, not the namespace.
Cache on the client side with your own TTL. A reasonable default for most
config is 1–5 minutes; shorter for anything the service needs to react to
quickly. Note that the 60-second TTL on a public namespace is also its revoke
latency — un-publishing does not purge edge or browser caches. That window now
applies only to the anonymous copies, which are the only ones a revoke could
never have reached anyway.

**404 is authoritative.** If the namespace doesn't exist or your token can't
read it, you get 404. Don't retry 404s in a loop. Check your role and whether
the namespace was created.

**Concurrent writes are last-write-wins.** There is no ETag or `If-Match`
support in v1. If multiple writers need to coordinate, serialize at the
application layer.

**PUT is idempotent when content is unchanged.** The server compares the
submitted document (after JSON compaction) against the stored one. If they're
equal, `changed: false` is returned and no write or backup occurs. Safe to
re-apply configuration on every deploy.

**Don't store secrets here.** Config documents are intended for non-sensitive
structured data — MQTT topics, device names, feature flags, UI copy. Anyone
with `read_role` access can read everything in the document — and for a
`read_role: public` namespace, that is anyone at all. See **Public namespaces**
in `docs/admin.md` before publishing one.

**Rate limiting.** The API allows 5 requests/second per IP (burst 20). Service
startups that need config often boot in parallel; this budget is intentionally
higher than identity's 30 req/min to accommodate boot bursts.

**CORS.** `PUT`, `PATCH`, `DELETE`, `POST` and `GET` against `/api/v1/*` paths
include CORS headers when the request `Origin` is in the allowed list. The one
exception is a `GET` of a `read_role: public` namespace, which answers
`Access-Control-Allow-Origin: *` regardless of the origin — a published
document is meant to be fetchable from anywhere, and withholding the header
would only break browser callers, never server-to-server ones. It also carries
`Access-Control-Expose-Headers: X-Read-Role, X-Write-Role`, so those headers
are readable from JS on any origin rather than only allow-listed ones.

An `OPTIONS` **preflight** from a non-allow-listed origin is answered too, but
only for a single-namespace path (`/api/v1/config/{ns}` — exactly one segment
after `/api/v1/config/`):

```http
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, OPTIONS
Access-Control-Allow-Headers: Content-Type
Access-Control-Expose-Headers: X-Read-Role, X-Write-Role
Access-Control-Max-Age: 86400
```

Without it the wildcard covered only *simple* requests: set `Content-Type` on
the GET, or send any custom header, and the browser preflights and is refused.
Answering it is safe because a preflight says only which method and headers may
be *attempted* — the GET still enforces the ACL, and a namespace you cannot
read still answers `404`. `Authorization` is deliberately not on that list: the
wildcard is for anonymous reads, and a cross-origin request carrying a token
still needs a `CORS_ORIGINS` entry.

So you can fetch a public namespace from front-end JavaScript on any site with
no token and no `CORS_ORIGINS` entry; nothing else is reachable that way, and
every other route keeps allow-list behaviour unchanged. The SPA admin UI (`/`,
`/static/*`) has a separate, permissive CSP.

---

## Running locally

```bash
# Requires identity running on :8181 first
DB_PATH=config.db \
IDENTITY_ISSUER_URL=http://localhost:8181 \
  go run ./cmd/server/

# Or with rate limiting disabled for tests
RATE_LIMIT_DISABLED=1 \
DB_PATH=config.db \
IDENTITY_ISSUER_URL=http://localhost:8181 \
  go run ./cmd/server/
```

Environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8282` | Listen port |
| `DB_PATH` | `config.db` | SQLite file path |
| `IDENTITY_ENV` | `development` | `development` or `production` |
| `IDENTITY_ISSUER_URL` | `http://localhost:8181` (dev) | Base URL of identity (for JWKS) |
| `IDENTITY_ISSUER` | same as `IDENTITY_ISSUER_URL` | Expected `iss` claim in JWTs |
| `JWKS_CACHE_TTL` | (verifier default) | How long to cache JWKS, e.g. `5m` |
| `BACKUP_MIN_INTERVAL` | `30s` | Cooldown between per-write R2 backups |
| `TRUST_PROXY` | — | `cloudflare` to trust `CF-Connecting-IP` |
| `CORS_ORIGINS` | — | Comma-separated allowed origins |
| `RATE_LIMIT_DISABLED` | — | Set `1` to disable rate limiting (dev only) |
| `OAUTH_CLIENT_ID` | — | Enable admin SPA; OAuth client registered on identity |
| `IDENTITY_PUBLIC_URL` | same as `IDENTITY_ISSUER_URL` | Browser-facing identity URL for OAuth |
| `R2_ACCOUNT_ID` | — | Cloudflare R2 account ID |
| `R2_ACCESS_KEY_ID` | — | R2 access key |
| `R2_SECRET_ACCESS_KEY` | — | R2 secret key |
| `R2_BUCKET_NAME` | — | R2 bucket for backups |

### CLI commands

```bash
./bin/config-server                     # Start server (default)
./bin/config-server --list-backups      # List R2 backups
./bin/config-server --restore-backup [key]  # Restore from R2 backup
./bin/config-server --help
```

---

## Deploy

```bash
./deploy/deploy.sh sweeney@garibaldi
```

Builds a static linux/amd64 binary, uploads it to `/opt/config/bin/`, symlinks
it as the active version, and restarts the `config` systemd service. Keeps the
last 3 versioned copies.

First-time host setup: copy `deploy/install.sh`, `deploy/config.service`, and
`deploy/config-env.example` to the target and run `sudo bash install.sh`. See
`deploy/config-env.example` for all configurable options.

### Backups

When R2 credentials are set, every successful mutation asynchronously uploads
a copy of `config.db` to:

```
{IDENTITY_ENV}/backups/config/{YYYY/MM/DD}/config-{timestamp}.sqlite3
```

Rapid write bursts coalesce into one backup per `BACKUP_MIN_INTERVAL` (default
30 s). A `changed: false` PUT produces no backup. To list and restore:

```bash
./bin/config-server --list-backups
./bin/config-server --restore-backup 2026/05/01/config-20260501-120000.sqlite3
```
