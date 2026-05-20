# Config service

Stores structured configuration as named JSON documents with per-namespace
role ACLs. Validates JWTs against the identity service's JWKS endpoint —
so you authenticate exactly the same way you do with identity.

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

**Service tokens (client credentials) are rejected.** v1 accepts user tokens
only. The `requireUserToken` middleware returns `403` if the token's `typ`
header is `at+jwt` (the OAuth 2.0 service token type).

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

All API endpoints are under `/api/v1/`. The only unauth endpoint is `/healthz`.

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

{"temperature":"home/sensors/temp","humidity":"home/sensors/humidity"}
```

Parse the body directly. The two response headers tell you the ACL roles for
this namespace without needing a second request:

| Header | Value | Meaning |
|---|---|---|
| `X-Read-Role` | `admin` or `user` | Role required to read |
| `X-Write-Role` | `admin` or `user` | Role required to write |

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
all namespaces; users see only those with `read_role=user`.

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
| `user` | `user` | ✓ anyone can read and write |
| `user` | `admin` | ✓ anyone can read; only admins write |
| `admin` | `admin` | ✓ admins only |
| `admin` | `user` | ✗ writers can't read — rejected |

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

---

## Error reference

All errors use the same envelope:

```json
{"error":"snake_case_code","message":"Human readable string"}
```

| HTTP | `error` | Cause |
|---|---|---|
| 400 | `invalid_name` | Namespace name doesn't match `^[a-z0-9_-]{1,64}$` |
| 400 | `invalid_role` | Role must be `admin` or `user`; or ACL constraint violated |
| 400 | `invalid_document` | Body isn't a JSON object, or nesting > 64 levels |
| 400 | `invalid_request` | Malformed JSON body |
| 401 | `unauthorized` | Missing `Authorization` header |
| 401 | `unauthorized` | Token expired, signature invalid, or wrong issuer |
| 403 | `account_disabled` | Token valid but the identity account is disabled |
| 403 | `forbidden` | Service token presented (user token required) |
| 403 | `forbidden` | Caller can read the namespace but their role doesn't satisfy `write_role` |
| 403 | `forbidden` | Operation requires `admin` (create/delete/patch-acl) |
| 404 | `not_found` | Namespace missing, or caller lacks `read_role` |
| 409 | `conflict` | `POST /namespaces` with a name that already exists |
| 413 | `document_too_large` | Stored document would exceed 64 KB |
| 413 | `request_too_large` | Request body exceeds 128 KB |
| 500 | `internal_error` | Server fault — retry with backoff, then file an issue |

---

## Role model and ACL matrix

Each namespace carries a `read_role` and a `write_role`, each `admin` or `user`.
Callers carry a role claim in their JWT.

**Role resolution:**

| Caller role | Satisfies `admin` requirement | Satisfies `user` requirement |
|---|---|---|
| `admin` | ✓ | ✓ |
| `user` | ✗ | ✓ |

**Per-operation requirements:**

| Operation | Required role |
|---|---|
| `GET /api/v1/config` | Any valid user token |
| `GET /api/v1/config/{ns}` | Satisfies namespace `read_role` |
| `PUT /api/v1/config/{ns}` | Satisfies namespace `write_role` |
| `DELETE /api/v1/config/{ns}` | `admin` (regardless of namespace ACL) |
| `POST /api/v1/config/namespaces` | `admin` |
| `PATCH /api/v1/config/namespaces/{ns}` | `admin` |

**No existence leak:** any operation by a caller who fails the `read_role`
check returns **404**, never 403. Callers who satisfy `read_role` but fail
`write_role` receive **403** (they already know the namespace exists from the
GET).

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
on document responses. Cache on the client side with your own TTL. A reasonable
default for most config is 1–5 minutes; shorter for anything the service needs
to react to quickly.

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
with `read_role` access can read everything in the document.

**Rate limiting.** The API allows 5 requests/second per IP (burst 20). Service
startups that need config often boot in parallel; this budget is intentionally
higher than identity's 30 req/min to accommodate boot bursts.

**CORS.** `PUT`, `PATCH`, `DELETE`, `POST` and `GET` against `/api/v1/*` paths
include CORS headers when the request `Origin` is in the allowed list. The SPA
admin UI (`/`, `/static/*`) has a separate, permissive CSP.

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
