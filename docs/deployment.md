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
| `REQUIRED_AUDIENCE` | (unset) | Asserts incoming JWTs carry a matching `aud`. Off until identity stamps `aud` on issuance. |
