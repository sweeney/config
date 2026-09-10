# Config service — testing guide

This document covers the testing methodology, how to run each layer of the
test suite, and how the CI pipeline is wired up. It is the canonical
reference for anyone picking up work on this repo.

## Philosophy

**Test behaviour, not implementation.** Every test in this repo asserts
what the system does (HTTP status codes, response bodies, error sentinels,
side effects on state) rather than how it does it. No mocks of internal
functions; no assertions on which private methods were called.

**Mocks only at external boundaries.** The only things that are ever faked
are I/O boundaries that can't run in a unit test:

| Boundary | What we do |
|---|---|
| Database | `fakeRepo` — in-memory implementation of `domain.ConfigRepository` |
| R2 backup | `fakeBackup` — implements `domain.BackupService`, counts `TriggerAsync()` calls |
| Identity JWKS | `testIssuer` — in-process EC signer/verifier (handler tests only) |

The real SQLite store is exercised by the integration layer (layer 3) and
again end-to-end by `scripts/e2e.sh`. There are no `testify/mock` mocks —
everything is a hand-written fake with real behaviour.

**Red/green discipline.** Write the failing test first, then make it pass.
Do not write a test for code that already exists without first confirming
the test fails for the right reason.

**Race detector always on.** All unit/handler tests run with `-race`. The
CI pipeline enforces this (`go test -race -count=1`). Never disable the
race detector.

## Test layers

### 1. Service tests (`internal/service/`)

Pure business logic — no HTTP, no database, no file I/O.

The `fakeConfigRepo` is an in-memory map with mutex. It implements
`domain.ConfigRepository` exactly. The `fakeBackup` records how many times
`TriggerAsync()` was called so tests can assert "a backup was triggered"
or "a no-op PUT did not trigger a backup."

```bash
go test ./internal/service/...
```

Key things tested:

- Role enforcement: admin-only for create/delete/updateACL, role-gated
  reads/writes for get/put
- Name validation (regex `^[a-z0-9_-]{1,64}$`)
- Document validation: must be a JSON object, not too large, not too deep
- The `read_role >= write_role` ACL invariant (write implies read)
- No-op PUT detection (byte equality after compaction)
- `ErrConfigNamespaceNotFound` vs `ErrConfigForbidden` distinction: a
  namespace the caller can't read must return not-found to avoid leaking
  existence
- The role lattice (`public` < `user` < `admin`), enumerated exhaustively
  over every (namespace read role, caller role) pair rather than
  spot-checked — it is the authorization primitive everything else rests on
- The publish confirmation: required on the transition into `public`, not
  when already public and never when revoking; bound to the namespace name;
  not usable as an existence oracle

### 2. Handler tests (`internal/handler/`)

HTTP layer — tests the router, middleware, and response shapes. Uses
`httptest.Server` (real TCP, real `net/http`).

The `testIssuer` is a small in-process JWT signer/verifier that implements
`commonauth.TokenParser`. It lives in `router_test.go` and is not exported.
Its purpose is to let handler tests mint tokens with specific roles without
depending on identity's internal packages.

```bash
go test ./internal/handler/...
```

Key things tested:

- `401` on missing/malformed `Authorization` header
- `403` on service tokens (config v1 accepts user tokens only)
- Role enforcement at the HTTP boundary (mirroring service tests end-to-end)
- `404` vs `403` for user-invisible namespaces
- `X-Read-Role` / `X-Write-Role` response headers on GET and PATCH
- ACL headers absent on 404 (no existence oracle via headers)
- SPA bundle mounted/unmounted based on env config
- OpenAPI spec endpoints unauthenticated
- Error envelope shape — no SQL or stack traces in `message`
- The anonymous read path: `200` on a public namespace with no
  `Authorization` header, `404` on a private one, and those two responses
  being byte-identical to a `404` for a namespace that does not exist
- A present-but-invalid token still `401` where anonymous would have
  succeeded — a broken token is never silently downgraded
- An anonymous public read succeeding while the verifier reports
  `ErrKeysUnavailable`, and a presented token still getting `503`
- `Cache-Control` following the read role, `Vary: Authorization` on both
- That no route other than the single-namespace GET became anonymous
- `Access-Control-Allow-Origin: *` on a public namespace and not on a
  private one, and that the handler appends to `Vary` rather than replacing
  the value the CORS middleware set upstream
- The audit endpoint being admin-only even when the namespace is public,
  refusing service tokens, and returning an empty array rather than
  not-found for a namespace that never existed or has been deleted

### 3. Integration tests (`db/`, `internal/store/`)

The persistence layer against a real SQLite database. Each test opens a
fresh database with `db.Open` in a `t.TempDir()`, so there is no shared
state between tests and nothing to clean up.

Two files:

| File | What it covers |
|---|---|
| `db/db_test.go` | `db.Open` — file creation, migrations (including idempotency on reopen), WAL mode, file permissions, foreign keys |
| `internal/store/config_store_integration_test.go` | `ConfigStore` as a real implementation of `domain.ConfigRepository` — create/get/list/update/delete, ACL reads and writes, duplicate-name conflict, the role `CHECK` constraint rejecting invalid values, and the `config_audit` trail including its rollback with a failed mutation |

This is where migration correctness is verified directly: the tests assert
the `config_namespaces` table exists after `db.Open`, and that opening an
already-migrated database again is a no-op.

It is also the only layer where two guarantees can be tested honestly:

- **The schema rebuild** in `db/schema.go` — that an old-schema database is
  widened in place, that every column of every pre-existing row survives,
  that the index is recreated, and that a second `Open` does not rebuild.
- **What the rebuild does when it goes wrong**, which is the part worth
  having: that the pre-rebuild snapshot is a restorable database carrying
  the *old* schema and every row; that a snapshot which cannot be written
  aborts the migration and leaves the database untouched, rather than
  migrating with no way back; and that a fault injected mid-rebuild rolls
  back to a wholly un-migrated database with no rows lost. The clock used to
  name snapshots is pinned through `db/export_test.go` so the failure case
  can occupy the path in advance.
- **Audit atomicity** — that a mutation which fails takes its audit row with
  it. The role `CHECK` constraint is the failure injector, so the rollback
  is a real database rollback. A fake cannot assert this: both writes happen
  under one mutex there, so it would only be asserting its own construction.
  The fakes therefore model entry shape and presence only.

Both files are gated behind `//go:build integration`, so they are invisible
to a plain `go test ./...` (which reports `[no test files]` for both
packages). Run them with the build tag:

```bash
go test -race -count=1 -tags=integration ./...
```

### 4. E2e tests (`scripts/e2e.sh`)

Runs against live servers. Requires both identity and config running. Gets
admin and user tokens from identity, then exercises every config endpoint.

```bash
# Start identity (port 8181)
ADMIN_USERNAME=admin ADMIN_PASSWORD=adminpassword1 \
  DB_PATH=/tmp/e2e-identity.db PORT=8181 \
  IDENTITY_ENV=development RATE_LIMIT_DISABLED=1 \
  ./bin/identity-server identity &

# Start config (port 8282)
DB_PATH=/tmp/e2e-config.db PORT=8282 IDENTITY_ENV=development \
  RATE_LIMIT_DISABLED=1 IDENTITY_ISSUER_URL=http://localhost:8181 \
  IDENTITY_ISSUER=http://localhost:8181 ./bin/config-server &

./scripts/e2e.sh
```

The e2e suite covers the full integration path including real JWKS
verification — the config server fetches identity's public key and
validates every token cryptographically. It is the only layer that
exercises `internal/config/` (env loading) and the wiring in
`cmd/server/`; the store itself is covered more cheaply by layer 3.

## Running locally

```bash
# All unit + handler tests
go test ./...

# With race detector (same as CI)
go test -race -count=1 ./...

# Specific package
go test -v ./internal/handler/...

# Integration tests (real SQLite — build-tagged, not run by default)
go test -race -count=1 -tags=integration ./...

# E2e (requires running servers — see above)
./scripts/e2e.sh
```

## CI pipeline

The GitHub Actions workflow is at `.github/workflows/ci.yml`. It runs on
every push and PR.

### `test` job

1. `go mod verify` — reproducible build
2. `go vet` — static analysis
3. Unit + handler tests with `-race -count=1 -coverprofile`
4. Test result summary posted to the GitHub Actions step summary (pass/fail counts, coverage by package, slowest tests)
5. HTML coverage report uploaded as an artifact (retained 30 days)

### `integration` job

Runs the build-tagged integration layer as its own check:

```bash
go test -race -count=1 -tags=integration ./...
```

It repeats the `test` job's checkout / `setup-go` / `go mod verify` setup
but skips the coverage and step-summary machinery — it is a straight
pass/fail signal that the real SQLite store and migrations still work.

### `build` job

Builds `linux/amd64` and `linux/arm64` binaries in parallel. Reports
binary size. Uploads binaries as artifacts (retained 90 days).

The build matrix is the same shape as identity's CI:

```yaml
matrix:
  include:
    - goos: linux
      goarch: amd64
      suffix: linux-amd64
    - goos: linux
      goarch: arm64
      suffix: linux-arm64
```

There is no `go generate` step (config has no mocks). If you add mocks
later, add a `go generate ./... && git diff --exit-code` step before the
test run, following identity's CI pattern.

## Fake implementations

Both test files use hand-written fakes rather than generated mocks. The
pattern is:

```go
type fakeRepo struct {
    mu   sync.Mutex
    data map[string]*domain.ConfigNamespace
}
```

The fake is duplicated between `service/config_service_test.go` and
`handler/router_test.go` rather than shared via a `testutil` package.
This is intentional: each test file is self-contained, so it can be read
and understood without cross-referencing another file. Three lines of
duplication beats a premature abstraction.

## What is NOT tested here

| Thing | Where it is tested |
|---|---|
| JWT signature verification | `common/auth` (identity repo) |
| JWKS fetch / key rotation | `common/auth` (identity repo) |
| R2 backup upload | `common/backup` (identity repo) |
| Rate limiting | `common/ratelimit` (identity repo) |

These are tested in `github.com/sweeney/identity/common/`. The config repo
trusts those packages and does not re-test them.

## Adding new tests

For a new endpoint or behaviour:

1. Write the service-layer test first (`internal/service/config_service_test.go`)
2. Run it — confirm it fails for the right reason
3. Implement the behaviour
4. Add a handler-layer test (`internal/handler/router_test.go`)
5. Add integration coverage if the change touches `internal/store/` or
   `db/` — a new query, a column, or a migration belongs in
   `internal/store/config_store_integration_test.go` or `db/db_test.go`.
   Remember the `//go:build integration` tag and run
   `go test -race -count=1 -tags=integration ./...`
6. Add an e2e check to `scripts/e2e.sh` if it touches auth wiring or
   end-to-end data flow (not just for completeness — e2e is slow to run
   against a live stack)

Keep service tests focused on errors and invariants. Keep handler tests
focused on HTTP shapes (status codes, headers, body format). Avoid testing
the same business rule three times — once in the service layer is enough.
