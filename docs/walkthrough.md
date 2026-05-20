# Config service walkthrough

Copy-paste tour through every endpoint, with real output captured from a
dev server. Matches `scripts/e2e-config.sh`; if the script drifts from
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
{"status":"ok","version":"dev"}
```

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

## 10. Error shapes

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

See `admin.md` for the full ACL matrix.
