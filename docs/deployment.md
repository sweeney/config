# Config service — deployment guide

## Current production state (garibaldi)

The service runs on garibaldi (192.168.1.200) under the target layout:

| | |
|---|---|
| Binary | `/opt/config/bin/config-server` (symlink to versioned binary) |
| System user | `config` |
| Working directory | `/var/lib/config` |
| DB path | `/var/lib/config/config.db` |
| Env file | `/etc/config/config.env` |

Routine operations:

```bash
sudo systemctl restart config
sudo systemctl status config
sudo journalctl -u config -n 50 --no-pager
```

## Routine deploys

```bash
./deploy/deploy.sh sweeney@garibaldi
```

This builds `linux/amd64`, uploads to `/opt/config/bin/`, symlinks, restarts
`config.service`, and verifies the version at `https://config.swee.net/healthz`.
Keeps the last 3 versioned binaries; older ones are pruned automatically.

## Schema change: the widened `read_role` CHECK

The `public` read role needed the `read_role` CHECK on
`config_namespaces` widened from `('admin', 'user')` to
`('admin', 'user', 'public')`. SQLite cannot `ALTER` a CHECK
constraint, so that is a full table rebuild: create the new shape
alongside, copy every column across, drop the old table, rename the new
one into place, recreate the index that went down with it. All of it in
one transaction.

There is nothing to run by hand. Deploying the new binary and
restarting is the whole procedure. What to know about it:

- **It lives in Go, not in `db/migrations/`.** It runs from `db.Open`,
  in `db/schema.go`. The migration runner in `common/db` keeps no
  ledger — it `Exec`s every `.sql` file on every startup and leans on
  `CREATE TABLE IF NOT EXISTS` for idempotence — so a rebuild written
  as SQL there would re-run on every boot.
- **It runs on every open and is idempotent.** Before rebuilding
  anything it probes the database: insert a row with
  `read_role='public'` inside a transaction that always rolls back. If
  the insert is accepted the schema is already wide enough and the
  rebuild is skipped. The probe asks the database about the semantics
  rather than pattern-matching the stored DDL text, so whitespace or
  quoting differences cannot fool it. It is also a no-op when the table
  does not exist yet — the `.sql` migrations still own creating it.
- **Once settled, the answer is recorded in `PRAGMA user_version`** and
  the probe is not repeated. That turns later boots into a genuine
  no-op rather than an inference re-derived from scratch every time —
  and it matters more than it looks: without it, a probe that failed
  for some unrelated reason would send a perfectly healthy database
  into a destructive rebuild, repeatedly, writing a fresh full-size
  snapshot each boot.
- **A brand-new database never takes the rebuild path at all.**
  `db/migrations/001_init.sql` now creates `config_namespaces` already
  accepting `read_role='public'`, so a fresh install is born at the
  right shape. Widening that statement is safe with the ledger-less
  runner precisely because it is `CREATE TABLE IF NOT EXISTS`: a
  database that already has the table does not re-run it, and so still
  reaches the rebuild.
- **Running on every open is what makes `--restore-backup` safe.**
  A restore drops an older SQLite file from R2 into `DB_PATH`. Without
  the rebuild happening at startup, restoring a pre-migration backup
  would silently revert the schema and every public namespace would
  begin failing at the CHECK. Because `Open` re-checks, restores
  self-heal on the next start.
- **It takes its own backup first, and refuses to proceed without
  one.** Before touching anything, it writes a consistent copy of the
  database beside it as
  `<DB_PATH>.pre-public-rebuild-<UTC timestamp>`, using SQLite's
  `VACUUM INTO` rather than a file copy — the database runs in WAL
  mode, where committed transactions may still be sitting in the `-wal`
  file, so copying the main file alone can silently lose them. If the
  snapshot cannot be written the service fails to start and the
  database is left untouched: un-migrated is recoverable, migrated with
  no way back is not. The timestamp means a second attempt can never
  overwrite the first snapshot. An empty table is not snapshotted —
  there is nothing to lose — and a fresh install leaves no such file
  anyway, having never rebuilt.
- **It counts the rows it copied before dropping anything.** The copy
  and the verification happen before the old table is dropped, so a
  mismatch rolls the whole thing back with the original intact.

The `config_audit` table (`db/migrations/002_config_audit.sql`) is an
ordinary `CREATE TABLE IF NOT EXISTS` and needs no special handling; it
appears on the first start after the deploy.

### What it looks like in the journal

`deploy.sh` tails `journalctl -u config` after restarting, which is
where you confirm what happened.

**Upgrading an existing host** — this is what garibaldi shows. First
start after the deploy:

```
config db: schema rebuild required for 14 row(s); snapshot written to
  /var/lib/config/config.db.pre-public-rebuild-20260910T110956Z
config db: schema rebuild complete — 14 row(s) migrated in 754µs
```

**Bringing up a new host** — no rebuild at all, just the skip line, on
the first start:

```
config db: schema check — read_role already permits 'public', no rebuild needed
```

`001_init.sql` creates `config_namespaces` already accepting
`read_role='public'`, so the probe finds nothing to do and there is no
snapshot file, no migration to wait on, and no rebuild lines. If you
are watching a new host's first boot for the rebuild output above, you
will not see it, and that is correct — its absence is the point.

**Upgrading a host already on the public read role** — the schema steps
are numbered, and a host that has step 1 but not step 2 runs only the
second:

```
config db: widening config_audit.action for 31 row(s); snapshot written to
  /var/lib/config/config.db.pre-audit-rebuild-20260912T193200Z
config db: config_audit widened, 31 row(s) carried across
config db: schema brought to version 2
```

A database old enough to need both steps runs both on one boot and
takes a snapshot for each, named for the step that took it —
`pre-public-rebuild-…` and `pre-audit-rebuild-…`. That is the path a
restored pre-migration backup takes.

**Every start after that, on any host**, logs the settled line:

```
config db: schema settled at version 2, nothing to do
```

`PRAGMA user_version` is a step counter, and it is the ledger
`common/db`'s migration runner does not have: steps at or below the
recorded number are skipped outright rather than each re-deciding
whether it has already run. The run that applied them recorded it, so
later boots short-circuit without probing the database at all. They still say so, deliberately: silence
would leave "checked and settled" and "a binary that never checked"
looking identical, and which of those you are looking at is the whole
question after a deploy.

One more line is worth recognising, because it appears at the moment
something is already confusing:

```
config db: WARNING schema is at version 3 but this binary only knows 2 —
  this database was written by a newer release.
```

That is a rollback: the database has had steps applied by a later
build. The service starts anyway — every step this binary knows about
is applied, and refusing to start would turn a rollback into an outage
— but it will not understand columns or constraints the newer build
added, so roll forward rather than leaving it there.

`--restore-backup` re-opens the question. An older SQLite file dropped
into `DB_PATH` carries its own `user_version`, so the next start asks
again from scratch — which is exactly what makes a restore of
a pre-migration backup self-healing.

If the rebuild fails, the service does not start: the error surfaces,
the transaction rolls back, and `deploy.sh`'s `/healthz` gate fails the
deploy. The database is left wholly un-migrated — not half-migrated —
and the snapshot is still on disk.

Confirming a recent R2 backup before the first deploy that performs the
rebuild is still worth doing, since the snapshot is only as good as the
disk it sits on:

```bash
./bin/config-server --list-backups | head
```

### Rolling back

Rolling back to the previous binary is safe and fails closed. This has
been verified, not assumed — the pre-feature binary was run against a
migrated, populated database:

- It **starts normally**. The old `.sql` migrations are
  `CREATE TABLE IF NOT EXISTS`, so re-running them does not revert the
  widened CHECK.
- A namespace with `read_role: public` becomes **admin-only**. The old
  role check does not recognise `public`, and an unrecognised role
  satisfies nothing, so a `user` token gets `404` and an anonymous
  request gets `401`. It closes the door rather than opening it.
- The old binary **refuses to write** `read_role: public` (400), so it
  cannot create rows its own reader would not understand.
- `config_audit` is left **untouched** — the old binary simply never
  looks at it, and its rows are still there afterwards.
- **Rolling forward again works**, including reading documents the old
  binary wrote while it was in place.

So the sequence is just: deploy the previous binary, restart, done.
There is no schema step to undo. Restoring the pre-rebuild snapshot is
**not** required and generally should not be done — it would discard
every write made since the migration.

One thing a rollback does cost, which is worth writing down at the
time: **the audit trail gains a silent gap.** The old binary does not
write `config_audit` rows, so any namespace created, ACL changed, or
namespace deleted while rolled back leaves no trace, and nothing marks
the discontinuity. Someone reading a namespace's history later sees an
unbroken sequence that quietly omits that window. If you roll back,
note the start and end times somewhere the next person will find them.

## First-time install on a new host

```bash
# Build the binary locally
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/config-server ./cmd/server/

# Copy install files to the target
scp deploy/install.sh deploy/config.service deploy/config-env.example \
    bin/config-server user@host:/tmp/

# Run on the target (requires full sudo)
ssh user@host "sudo bash /tmp/install.sh"

# Edit the env file
ssh user@host "sudo nano /etc/config/config.env"

# Start
ssh user@host "sudo systemctl start config"
```

## Environment variables

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8282` | Listen port |
| `DB_PATH` | `config.db` | SQLite file |
| `IDENTITY_ENV` | `development` | `production` requires HTTPS for identity |
| `IDENTITY_ISSUER_URL` | `http://localhost:8181` | Base URL for JWKS fetch |
| `IDENTITY_ISSUER` | `IDENTITY_ISSUER_URL` | Expected JWT `iss` claim |
| `JWKS_CACHE_TTL` | `5m` | Duration string |
| `BACKUP_MIN_INTERVAL` | `30s` | Duration; `0` disables throttling |
| `TRUST_PROXY` | (unset) | `cloudflare` honours `CF-Connecting-IP` |
| `CORS_ORIGINS` | (unset) | Comma-separated allowed origins |
| `RATE_LIMIT_DISABLED` | `0` | `1` disables rate limiting (dev/test only) |
| `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `R2_BUCKET_NAME` | (unset) | Required together for R2 backups |
| `OAUTH_CLIENT_ID` | (unset) | Mounts the admin SPA at `/` when set |
| `IDENTITY_PUBLIC_URL` | `IDENTITY_ISSUER_URL` | Browser-facing identity URL (when behind a proxy with a different hostname) |
| `REQUIRED_AUDIENCE` | (unset) | Asserts incoming JWTs carry a matching `aud`. **Set to `config` in production.** Identity stamps `aud` on issuance; see admin.md. |
