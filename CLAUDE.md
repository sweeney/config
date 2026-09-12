# Config Service

A standalone key-value configuration store for self-hosted apps. Namespaces
hold JSON objects. Access is gated by an ordered role lattice — `public` <
`user` < `admin` — where `user` and `admin` are inherited from
identity-issued JWT tokens. `public` is a read role only: a namespace with
`read_role: public` is readable with no token at all, but nothing is ever
anonymously writable, and `GET /api/v1/config` (list) always requires a
token. Publishing a namespace requires echoing its name back in
`confirm_public`; see **Public namespaces** in `docs/admin.md`. Namespace
creates, ACL changes, document writes and deletes are recorded in a
`config_audit` table, readable by admins at
`GET /api/v1/config/namespaces/{ns}/audit`. It records that a document
changed and who changed it, never the document itself.

## What it does

Stores and serves JSON configuration namespaces over HTTP. Auth is handled
entirely by identity — config validates Bearer tokens by fetching identity's
JWKS, has no login flow of its own, and issues no tokens.

Because of that, an identity outage means config cannot verify *anyone*. It
answers those requests `503` with `Retry-After` (`commonauth.ErrKeysUnavailable`
→ `writeTokenError` in `internal/auth/middleware.go`), never `401`: a `401`
tells every client to sign out at the moment they cannot sign back in. Bad and
expired tokens still get `401`. `/healthz` deliberately stays green throughout —
see **Identity coupling** in `docs/admin.md`.

## Module and dependency

```
module github.com/sweeney/config
require github.com/sweeney/identity/common v0.5.0
```

The `common/` sub-module (at `github.com/sweeney/identity/common`) provides:
shared DB helpers, JWKS token parsing, R2 backup, rate limiting, and the
OpenAPI YAML→JSON converter. See **Updating common/** below.

## Running locally

```bash
# Build
go build -o bin/config-server ./cmd/server/

# Run (requires a running identity at port 8181)
DB_PATH=/tmp/config.db PORT=8282 IDENTITY_ENV=development \
  RATE_LIMIT_DISABLED=1 IDENTITY_ISSUER_URL=http://localhost:8181 \
  IDENTITY_ISSUER=http://localhost:8181 ./bin/config-server
```

Optional env vars: `PORT` (default 8282), `DB_PATH` (default `config.db`),
`IDENTITY_ENV` (`development`|`production`), `IDENTITY_ISSUER_URL`,
`IDENTITY_ISSUER`, `CORS_ORIGINS`, `TRUST_PROXY` (`cloudflare`),
`RATE_LIMIT_DISABLED` (`1` for dev/test), `OAUTH_CLIENT_ID` +
`IDENTITY_PUBLIC_URL` (enable the admin SPA), `R2_*` for backups.

## Deploying

```bash
./deploy/deploy.sh sweeney@192.168.1.200
```

**Prerequisite:** `install.sh` must have been run on the target host first.
Production (garibaldi) is on the target layout that `deploy.sh` expects.
See `docs/deployment.md` for the layout and routine operations.

## CLI commands

```
./config-server --list-backups           # List R2 backups
./config-server --restore-backup [key]  # Restore from R2 backup
```

## Testing

```bash
go test -race -count=1 ./...                  # unit + handler tests
go test -race -count=1 -tags=integration ./...  # + real SQLite (store, migrations)
./scripts/e2e.sh                              # e2e (requires live identity + config)
```

The integration layer is build-tagged, so a plain `go test ./...` silently
skips it. CI runs both.

See `docs/testing.md` for the full testing guide including philosophy, test
layers, and how to add new tests.

## Key implementation files

| Path | What it is |
|---|---|
| `internal/handler/router.go` | HTTP router — all endpoints, SPA mount, auth middleware |
| `internal/service/config_service.go` | Business logic — CRUD, role enforcement, ACL invariants |
| `internal/store/config_store.go` | SQLite store implementing `domain.ConfigRepository` |
| `internal/domain/config.go` | Types, role lattice, error sentinels, `BackupService` interface |
| `internal/auth/middleware.go` | `RequireAuth` / `OptionalAuth` middleware (thin wrapper over `common/auth`) |
| `internal/config/config.go` | Env var loading (`ConfigSvcConfig`) |
| `db/db.go` | Opens SQLite with migrations via `common/db` |
| `db/schema.go` | Versioned schema steps run from `db.Open`, tracked in `PRAGMA user_version` — the ledger `common/db` lacks. Currently two table rebuilds SQLite cannot express as `ALTER` |
| `db/migrations/001_init.sql` | Schema: `config_namespaces` table |
| `db/migrations/002_config_audit.sql` | Schema: `config_audit` table (create / acl_change / document_write / delete) |
| `db/migrations/003_audit_actor_username.sql` | Adds `config_audit.actor_username` |
| `db/migrations/004_namespace_updated_by_username.sql` | Adds `config_namespaces.updated_by_username` |
| `internal/testutil/audit.go` | Shared in-memory audit recorder for the service and handler fakes |
| `spec/openapi.yaml` | OpenAPI 3.0 spec (served at `/openapi.json` and `/openapi.yaml`) |
| `ui/embed.go` + `ui/static/` | Embedded admin SPA assets |
| `cmd/server/server.go` | Entry point — env loading, wiring, HTTP server |
| `cmd/server/backups.go` | `--list-backups` and `--restore-backup` CLI |
| `deploy/` | systemd unit, env template, install/deploy scripts |
| `scripts/e2e.sh` | End-to-end test suite (requires live identity + config) |
| `docs/testing.md` | Testing philosophy and layer guide |
| `docs/deployment.md` | Deployment: current prod state, migration, ongoing deploy |
| `docs/admin.md` | Operational guide: namespaces, ACLs, backups, SPA |
| `docs/walkthrough.md` | Executable walkthrough of every endpoint |

## Updating common/

`common/` is a Go sub-module inside the identity repo at
`github.com/sweeney/identity/common`. To consume a new version:

1. Make and commit changes in `identity/common/` in the identity repo.
2. Tag the new version from the identity repo root:
   ```bash
   git tag common/v0.1.1
   git push origin common/v0.1.1
   ```
3. In this repo, update the requirement and tidy:
   ```bash
   go get github.com/sweeney/identity/common@v0.1.1
   go mod tidy
   ```
4. Run the test suite to confirm nothing broke.

The config repo **must not** import any package from
`github.com/sweeney/identity/internal/` — those are internal to identity.
Shared code belongs in `common/`.
