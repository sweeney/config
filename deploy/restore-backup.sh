#!/usr/bin/env bash
#
# Restore config's SQLite database from an R2 backup, safely.
#
# From the dev machine (the usual way):
#
#   ./deploy/restore-backup.sh --dry-run --latest sweeney@192.168.1.200   # plan only
#   ./deploy/restore-backup.sh --latest sweeney@192.168.1.200
#   ./deploy/restore-backup.sh sweeney@192.168.1.200 \
#       production/backups/config/2026/10/31/config-2026-10-31T00:00:00Z.sqlite3
#
# That copies this script into a private mktemp -d directory on the host, runs
# it there with sudo (you are asked for your password: the env file is root
# 0600), and removes the directory.
#
# On the host directly:
#
#   sudo ./restore-backup.sh --on-host [--dry-run] [--yes] (--latest | KEY)
#
# What it does, in this order, so the live database is only touched once the
# replacement is known to be good:
#   1. checks the key is in this environment's listing (--latest: the newest)
#   2. downloads it to a staging file beside the database, as the service user,
#      and checks it with PRAGMA integrity_check and a read of config_namespaces
#   3. asks for confirmation (--yes skips it)
#   4. stops the service, moves the current database and its -wal/-shm into a
#      pre-restore-<time>/ directory beside it, and moves the staging file in
#   5. starts the service and waits for /healthz; if it is not healthy, puts the
#      previous database back, starts the service again, and exits 1
#
# The previous database is kept in pre-restore-<time>/; remove it once you are
# happy. Re-running is safe: each run stages afresh and keeps its own copy.
#
# --dry-run does steps 1 and the checks before it, prints the plan, and changes
# nothing.
#
# Exit: 0 restored, 1 aborted or failed (the service is left on the previous
# database), 2 a precondition failed (nothing changed).
#
# Paths can be overridden for testing:
#   CONFIG_ENV_FILE  (default /etc/config/config.env)
#   CONFIG_BIN       (default /opt/config/bin/config-server)
#   CONFIG_UNIT      (default config)
#   CONFIG_USER      (default config)
#   CONFIG_WORKDIR   (default /var/lib/config: the unit's WorkingDirectory)
#   HEALTH_TIMEOUT   (default 30 seconds)
#   SKIP_ROOT_CHECK  (set to 1 to bypass the root requirement — testing only)

set -euo pipefail

CONFIG_ENV_FILE="${CONFIG_ENV_FILE:-/etc/config/config.env}"
CONFIG_BIN="${CONFIG_BIN:-/opt/config/bin/config-server}"
CONFIG_UNIT="${CONFIG_UNIT:-config}"
CONFIG_USER="${CONFIG_USER:-config}"
CONFIG_WORKDIR="${CONFIG_WORKDIR:-/var/lib/config}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-30}"
ON_HOST=0
DRY_RUN=0
YES=0
LATEST=0
REMOTE=""
KEY=""

say() { printf "%s\n" "$*"; }
die() { printf "\nERROR: %s\n" "$*" >&2; exit 2; }
fail() { printf "\nFAILED: %s\n" "$*" >&2; exit 1; }

usage() {
  sed -n '2,50p' "$0" | sed 's/^#//; s/^ //'
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --on-host) ON_HOST=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --yes)     YES=1; shift ;;
    --latest)  LATEST=1; shift ;;
    -h|--help) usage ;;
    -*)        printf "unknown option: %s\n\n" "$1" >&2; usage ;;
    *)
      # From the dev machine the first word is the host; on the host, the key.
      if [ "$ON_HOST" -eq 0 ] && [ -z "$REMOTE" ]; then REMOTE="$1"
      elif [ -z "$KEY" ]; then KEY="$1"
      else die "unexpected argument: $1"
      fi
      shift ;;
  esac
done

if [ "$LATEST" -eq 1 ] && [ -n "$KEY" ]; then die "give a KEY or --latest, not both"; fi
if [ "$LATEST" -eq 0 ] && [ -z "$KEY" ]; then
  printf "missing the backup to restore: give a KEY or --latest\n\n" >&2; usage
fi

# ── From the dev machine: copy this script over and run it there ──────────────
if [ "$ON_HOST" -eq 0 ]; then
  [ -n "$REMOTE" ] || usage
  for c in ssh scp; do
    command -v "$c" >/dev/null || die "required command not found: $c"
  done
  args=(--on-host)
  [ "$DRY_RUN" -eq 1 ] && args+=(--dry-run)
  [ "$YES" -eq 1 ] && args+=(--yes)
  if [ "$LATEST" -eq 1 ]; then args+=(--latest); else args+=("$KEY"); fi
  remote_args=$(printf ' %q' "${args[@]}")
  say "Restore of config's database on $REMOTE from R2 ($([ "$LATEST" -eq 1 ] && echo "the newest backup" || echo "$KEY"))."
  say "It copies this script into a private temporary directory on the host,"
  say "runs it with sudo, then removes the directory."
  [ "$DRY_RUN" -eq 0 ] || say "Dry run: it checks the backup and prints the plan, and changes nothing."
  say ""
  # mktemp -d on the host: a directory only this user can write, with an
  # unpredictable name, so nothing else on the host can swap the script that
  # is about to run as root.
  dir=$(ssh "$REMOTE" 'mktemp -d /tmp/restore-backup.XXXXXXXX') || die "could not create a temporary directory on $REMOTE"
  case "$dir" in
    /tmp/restore-backup.*) ;;
    *) die "unexpected temporary directory from $REMOTE: $dir" ;;
  esac
  scp -q "$0" "$REMOTE:$dir/restore-backup.sh" || die "could not copy the script to $REMOTE"
  # The directory is removed whatever the outcome, and the exit code kept.
  ssh -t "$REMOTE" "sudo bash $dir/restore-backup.sh$remote_args; rc=\$?; rm -rf $dir; exit \$rc"
  exit $?
fi

# ── On the host: preconditions (nothing changes until step 4) ─────────────────
if [ "${SKIP_ROOT_CHECK:-0}" != "1" ]; then
  [ "$(id -u)" -eq 0 ] || die "must run as root (use sudo): the env file is mode 600"
fi
for c in systemctl setpriv sqlite3 curl; do
  command -v "$c" >/dev/null 2>&1 || die "required command not found: $c"
done
[ -x "$CONFIG_BIN" ] || die "config binary not found or not executable: $CONFIG_BIN"
[ -f "$CONFIG_ENV_FILE" ] || die "env file not found: $CONFIG_ENV_FILE"
[ -r "$CONFIG_ENV_FILE" ] || die "env file not readable: $CONFIG_ENV_FILE (need root)"
# Sourced, never echoed: it holds the R2 credentials. Exported, as systemd's
# EnvironmentFile does, so the binary sees exactly what the service sees.
set -a
# shellcheck disable=SC1090
. "$CONFIG_ENV_FILE"
set +a
ENV_NAME="${IDENTITY_ENV:-development}"
PORT="${PORT:-8282}"

# DB_PATH as the service resolves it: relative to its WorkingDirectory.
db="${DB_PATH:-config.db}"
case "$db" in /*) ;; *) db="$CONFIG_WORKDIR/$db" ;; esac
data_dir=$(dirname "$db")
[ -d "$data_dir" ] || die "database directory not found: $data_dir"
staging="$data_dir/restore-staging.sqlite3"

# as_service_user runs a command as the service user with this environment,
# so every file it writes is owned the way the service needs.
as_service_user() { setpriv --reuid="$CONFIG_USER" --regid="$CONFIG_USER" --init-groups -- "$@"; }

# ── 1. The key ────────────────────────────────────────────────────────────────
listing=$("$CONFIG_BIN" --list-backups 2>&1) || {
  printf '%s\n' "$listing" | head -5 >&2
  die "--list-backups failed, so the key cannot be checked"
}
home="$ENV_NAME/backups/config/"
keys=$(printf '%s\n' "$listing" | awk '{print $NF}' \
  | grep -E "^${ENV_NAME}/backups/config/[0-9]{4}/[0-9]{2}/[0-9]{2}/config-[0-9TZ:.-]+\.sqlite3$" \
  | sort || true)
[ -n "$keys" ] || die "no config backups under $home"
if [ "$LATEST" -eq 1 ]; then
  KEY=$(printf '%s\n' "$keys" | tail -1)
elif ! printf '%s\n' "$keys" | grep -Fxq -- "$KEY"; then
  die "$KEY is not one of the config backups under $home (list them with --list-backups)"
fi

say ""
say "Restoring config ($ENV_NAME) on $(hostname)"
say "  backup:   $KEY"
say "  database: $db"
if [ -e "$db" ]; then
  say "  current:  $(wc -c <"$db" | tr -d ' ') bytes, modified $(date -u -r "$db" '+%Y-%m-%d %H:%M:%S UTC' 2>/dev/null || echo '?')"
else
  say "  current:  (none)"
fi

if [ "$DRY_RUN" -eq 1 ]; then
  say ""
  say "Dry run: nothing changed. Without --dry-run this would:"
  say "  1. download the backup to $staging and check it"
  say "  2. ask for confirmation (unless --yes)"
  say "  3. stop $CONFIG_UNIT, move $db (and any -wal/-shm) into"
  say "     $data_dir/pre-restore-<time>/, and move the download into place"
  say "  4. start $CONFIG_UNIT and wait for http://127.0.0.1:$PORT/healthz,"
  say "     putting the previous database back if it is not healthy"
  exit 0
fi

# ── 2. Stage and check the download, with the service still running ──────────
say ""
say "Downloading to $staging ..."
rm -f -- "$staging" "$staging.download"
# A fresh path, so --restore-backup has nothing to overwrite and does not ask.
if ! out=$(DB_PATH="$staging" as_service_user "$CONFIG_BIN" --restore-backup "$KEY" </dev/null 2>&1); then
  printf '%s\n' "$out" | tail -5 >&2
  rm -f -- "$staging" "$staging.download"
  fail "download failed; nothing changed"
fi
[ -s "$staging" ] || fail "download produced no file at $staging; nothing changed"
check=$(as_service_user sqlite3 -readonly "$staging" 'PRAGMA integrity_check;' 2>&1 || true)
if [ "$check" != "ok" ]; then
  rm -f -- "$staging"
  fail "the backup failed PRAGMA integrity_check ($check); nothing changed"
fi
if ! count=$(as_service_user sqlite3 -readonly "$staging" 'SELECT count(*) FROM config_namespaces;' 2>&1); then
  rm -f -- "$staging"
  fail "the backup has no readable config_namespaces table ($count); nothing changed"
fi
# Checking it may have left a -shm beside it; nothing should follow it into place.
rm -f -- "$staging-wal" "$staging-shm"
say "  ok: integrity_check passed, $count namespace(s)"

# ── 3. Confirm ────────────────────────────────────────────────────────────────
if [ "$YES" -eq 0 ]; then
  say ""
  say "This stops $CONFIG_UNIT and replaces $db with that backup."
  printf "Type 'yes' to restore: "
  read -r answer || answer=""
  if [ "$answer" != "yes" ]; then
    rm -f -- "$staging"
    say "Aborted; nothing changed."
    exit 1
  fi
fi

# ── 4. Swap ───────────────────────────────────────────────────────────────────
safety="$data_dir/pre-restore-$(date -u +%Y%m%dT%H%M%SZ)"
say ""
say "Stopping $CONFIG_UNIT ..."
systemctl stop "$CONFIG_UNIT"
# From here an unexpected error must say where the previous database is.
trap 'printf "\nFAILED part-way. The previous database is in %s; %s is stopped.\n" "$safety" "$CONFIG_UNIT" >&2' ERR
as_service_user mkdir -m 700 "$safety"
for f in "$db" "$db-wal" "$db-shm"; do
  # A -wal left beside the restored file would be replayed into it.
  if [ -e "$f" ]; then mv -- "$f" "$safety/"; fi
done
mv -- "$staging" "$db"
say "  previous database moved to $safety/"

# ── 5. Start and check ────────────────────────────────────────────────────────
healthy() {
  local i body
  for ((i = 0; i < HEALTH_TIMEOUT; i++)); do
    if body=$(curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" 2>/dev/null) \
       && grep -q '"status":"ok"' <<<"$body"; then
      HEALTH_BODY=$body
      return 0
    fi
    sleep 1
  done
  return 1
}

say "Starting $CONFIG_UNIT ..."
systemctl start "$CONFIG_UNIT"
if healthy; then
  trap - ERR
  say ""
  say "Restored $db from $KEY; $CONFIG_UNIT is healthy."
  say "  $(grep -o '"version":"[^"]*"' <<<"$HEALTH_BODY" || true)"
  say "The previous database is in $safety/; remove it once you are happy."
  exit 0
fi

say ""
say "$CONFIG_UNIT is not healthy after ${HEALTH_TIMEOUT}s on the restored database; putting the previous one back."
systemctl stop "$CONFIG_UNIT"
as_service_user mkdir -p -m 700 "$safety/restored-unhealthy"
for f in "$db" "$db-wal" "$db-shm"; do
  if [ -e "$f" ]; then mv -- "$f" "$safety/restored-unhealthy/"; fi
done
for f in "$safety"/*; do
  if [ -f "$f" ]; then mv -- "$f" "$data_dir/"; fi
done
systemctl start "$CONFIG_UNIT"
trap - ERR
if healthy; then
  fail "restore rolled back: $CONFIG_UNIT is healthy again on the previous database. The restored copy is in $safety/restored-unhealthy/; see journalctl -u $CONFIG_UNIT"
fi
fail "restore rolled back, but $CONFIG_UNIT is still not healthy on the previous database either; see journalctl -u $CONFIG_UNIT"
