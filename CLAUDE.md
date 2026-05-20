# Config Service

A standalone key-value configuration store for self-hosted apps. Namespaces
hold JSON objects. Access is gated by role (`admin` / `user`) inherited from
identity-issued JWT tokens.

## What it does

Stores and serves JSON configuration namespaces over HTTP. Auth is handled
entirely by identity — config validates Bearer tokens by fetching identity's
JWKS, has no login flow of its own, and issues no tokens.

## Module and dependency

```
module github.com/sweeney/config
require github.com/sweeney/identity/common v0.1.0
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
See `docs/deployment.md` for the current production state and migration path —
the production server is currently running a bootstrap layout that differs
from what `deploy.sh` expects.

## CLI commands

```
./config-server --list-backups           # List R2 backups
./config-server --restore-backup [key]  # Restore from R2 backup
```

## Testing

```bash
go test -race -count=1 ./...   # unit + handler tests
./scripts/e2e.sh               # e2e (requires live identity + config)
```

See `docs/testing.md` for the full testing guide including philosophy, test
layers, and how to add new tests.

## Key implementation files

| Path | What it is |
|---|---|
| `internal/handler/router.go` | HTTP router — all endpoints, SPA mount, auth middleware |
| `internal/service/config_service.go` | Business logic — CRUD, role enforcement, ACL invariants |
| `internal/store/config_store.go` | SQLite store implementing `domain.ConfigRepository` |
| `internal/domain/config.go` | Types, error sentinels, `BackupService` interface |
| `internal/auth/middleware.go` | `RequireAuth` middleware (thin wrapper over `common/auth`) |
| `internal/config/config.go` | Env var loading (`ConfigSvcConfig`) |
| `db/db.go` | Opens SQLite with migrations via `common/db` |
| `db/migrations/001_init.sql` | Schema: `config_namespaces` table |
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
