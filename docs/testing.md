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

The real SQLite store is exercised only in the e2e suite. There are no
`testify/mock` mocks — everything is a hand-written fake with real
behaviour.

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

### 3. E2e tests (`scripts/e2e.sh`)

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
validates every token cryptographically. This is the only layer that
exercises `internal/store/` (real SQLite) and `internal/config/` (env
loading).

## Running locally

```bash
# All unit + handler tests
go test ./...

# With race detector (same as CI)
go test -race -count=1 ./...

# Specific package
go test -v ./internal/handler/...

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
| SQLite migration correctness | Implicitly by e2e (first-run migration) |
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
5. Add an e2e check to `scripts/e2e.sh` if it touches auth wiring or
   end-to-end data flow (not just for completeness — e2e is slow to run
   against a live stack)

Keep service tests focused on errors and invariants. Keep handler tests
focused on HTTP shapes (status codes, headers, body format). Avoid testing
the same business rule three times — once in the service layer is enough.
