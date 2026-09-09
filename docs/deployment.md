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
- **Running on every open is what makes `--restore-backup` safe.**
  A restore drops an older SQLite file from R2 into `DB_PATH`. Without
  the rebuild happening at startup, restoring a pre-migration backup
  would silently revert the schema and every public namespace would
  begin failing at the CHECK. Because `Open` re-checks, restores
  self-heal on the next start.
- **Rolling back to the previous binary is safe, and fails closed.**
  The old code ignores the widened CHECK — a constraint that permits
  more than it used to breaks nothing — and its role check does not
  recognise `public` at all. An unranked role satisfies nothing, so a
  published namespace becomes admin-only under the old binary rather
  than staying anonymously readable. Rolling forward again restores it.

The `config_audit` table (`db/migrations/002_config_audit.sql`) is an
ordinary `CREATE TABLE IF NOT EXISTS` and needs no special handling; it
appears on the first start after the deploy.

Take the usual precaution of confirming a recent backup exists before
the first deploy that performs the rebuild:

```bash
./bin/config-server --list-backups | head
```

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
