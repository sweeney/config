#!/usr/bin/env bash
#
# Restore config's SQLite database from an R2 backup, safely.
#
# From the dev machine (the usual way):
#
#   ./deploy/restore-backup.sh --dry-run --latest sweeney@<host>   # plan only
#   ./deploy/restore-backup.sh --latest sweeney@<host>
#   ./deploy/restore-backup.sh sweeney@<host> \
#       production/backups/config/2026/10/31/config-2026-10-31T00:00:00Z.sqlite3
#
# That copies this script and lib.sh into a private mktemp -d directory on the
# host, runs it there with sudo (you are asked for your password: the env file
# is root 0600), and removes the directory.
#
# On the host directly, with lib.sh beside it:
#
#   sudo ./restore-backup.sh --on-host [--dry-run] [--yes] (--latest | KEY)
#
# What it does, in this order, so the live database is only touched once the
# replacement is known to be good:
#   1. checks the key is one of this environment's config backups, as the
#      listing names it (--latest: the newest)
#   2. downloads it to a staging file beside the database, as the service user
#      and with the service's environment, while the service keeps running;
#      checks it with PRAGMA integrity_check and counts its namespaces. A bad
#      download, or one with no namespaces (unless --yes), changes nothing
#   3. shows the current and backup namespace counts and asks for
#      confirmation (--yes skips it)
#   4. creates pre-restore-<time>-<random>/ beside the database and checks it
#      is writable, then stops the service, moves the database and its
#      -wal/-shm into it, and moves the staging file into place
#   5. starts the service and waits up to HEALTH_TIMEOUT seconds for /healthz
#
# If anything goes wrong after the stop (the start fails, the service is not
# healthy, a step errors, or the script is interrupted), it puts the previous
# database back and starts the service again, then exits 1 saying where each
# file is. From the stop on, a dropped connection (SIGHUP) does not interrupt
# it, and everything it says is also written to pre-restore-.../restore.log.
#
# The previous database stays in pre-restore-.../; remove it once you are
# happy. Re-running is safe: each run stages afresh and keeps its own copy.
#
# --dry-run does step 1, prints the plan, and changes nothing.
#
# Exit: 0 restored, 1 aborted or failed (the service is left on the previous
# database), 2 a precondition failed (nothing changed).
#
# Paths can be overridden for testing:
#   CONFIG_ENV_FILE  (default /etc/config/config.env)
#   CONFIG_BIN       (default /opt/config/bin/config-server)
#   CONFIG_UNIT      (default config)
#   CONFIG_USER      (default config)
#   CONFIG_WORKDIR   (default: the unit's WorkingDirectory, else /var/lib/config)
#   HEALTH_TIMEOUT   (default 30 seconds)
#   SKIP_ROOT_CHECK  (set to 1 to bypass the root requirement — testing only)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=deploy/lib.sh
. "$HERE/lib.sh"

CONFIG_ENV_FILE="${CONFIG_ENV_FILE:-/etc/config/config.env}"
CONFIG_BIN="${CONFIG_BIN:-/opt/config/bin/config-server}"
CONFIG_UNIT="${CONFIG_UNIT:-config}"
CONFIG_USER="${CONFIG_USER:-config}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-30}"
ON_HOST=0
DRY_RUN=0
YES=0
LATEST=0
REMOTE=""
KEY=""

LOG=/dev/null
# say never fails: from the stop on, the terminal may be gone.
say() {
  printf '%s\n' "$*" 2>/dev/null || true
  printf '%s\n' "$*" >>"$LOG" 2>/dev/null || true
}
fail() { say ""; say "FAILED: $*"; exit 1; }

usage() { usage_from_header "$0"; }

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
  args=(--on-host)
  [ "$DRY_RUN" -eq 1 ] && args+=(--dry-run)
  [ "$YES" -eq 1 ] && args+=(--yes)
  if [ "$LATEST" -eq 1 ]; then args+=(--latest); else args+=("$KEY"); fi
  say "Restore of config's database on $REMOTE from R2 ($([ "$LATEST" -eq 1 ] && echo "the newest backup" || echo "$KEY"))."
  say "It copies this script into a private temporary directory on the host,"
  say "runs it with sudo, then removes the directory."
  [ "$DRY_RUN" -eq 0 ] || say "Dry run: it checks the backup and prints the plan, and changes nothing."
  say ""
  rc=0
  run_on_host "$REMOTE" "$0" "${args[@]}" || rc=$?
  exit "$rc"
fi

# ── On the host: preconditions (nothing changes until step 4) ─────────────────
require_root "the env file is mode 600"
for c in systemctl setpriv sqlite3 curl mktemp; do
  command -v "$c" >/dev/null 2>&1 || die "required command not found: $c"
done
[ -x "$CONFIG_BIN" ] || die "config binary not found or not executable: $CONFIG_BIN"
[ -f "$CONFIG_ENV_FILE" ] || die "env file not found: $CONFIG_ENV_FILE"
[ -r "$CONFIG_ENV_FILE" ] || die "env file not readable: $CONFIG_ENV_FILE (need root)"
# Exported as systemd's EnvironmentFile does, so the binary sees what the
# service sees. Parsed, never sourced or echoed: it holds the R2 credentials.
load_env_file "$CONFIG_ENV_FILE"
PORT="${PORT:-8282}"

# DB_PATH as the service resolves it: relative to the unit's WorkingDirectory.
workdir="${CONFIG_WORKDIR:-$(systemctl show -p WorkingDirectory --value "$CONFIG_UNIT" 2>/dev/null || true)}"
workdir="${workdir:-/var/lib/config}"
db="${DB_PATH:-config.db}"
case "$db" in /*) ;; *) db="$workdir/$db" ;; esac
data_dir=$(dirname "$db")
[ -d "$data_dir" ] || die "database directory not found: $data_dir"
# The service user may not be able to enter the directory we were started in
# (the private mktemp -d one); work from the database's, as the service does.
cd "$data_dir"
staging="$data_dir/restore-staging.sqlite3"

# as_service_user runs a command as the service user, keeping this
# environment, so every file it writes is owned the way the service needs.
as_service_user() { setpriv --reuid="$CONFIG_USER" --regid="$CONFIG_USER" --init-groups -- "$@"; }

namespaces() { as_service_user sqlite3 -readonly "$1" 'SELECT count(*) FROM config_namespaces;' 2>/dev/null; }

# ── 1. The key ────────────────────────────────────────────────────────────────
listing=$("$CONFIG_BIN" --list-backups 2>&1) || {
  printf '%s\n' "$listing" | head -5 >&2
  die "--list-backups failed, so the key cannot be checked"
}
# The environment as the binary resolved it, not IDENTITY_ENV as written.
ENV_NAME=$(listing_env "$listing")
if [ -z "$ENV_NAME" ]; then
  printf '%s\n' "$listing" | head -3 >&2
  die "could not read the environment from the --list-backups header"
fi
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

prev_exists=0
current="(none)"
if [ -e "$db" ]; then
  prev_exists=1
  current="$(namespaces "$db" || echo '?') namespace(s), $(wc -c <"$db" | tr -d ' ') bytes, modified $(date -u -r "$db" '+%Y-%m-%d %H:%M:%S UTC' 2>/dev/null || echo '?')"
fi

say ""
say "Restoring config ($ENV_NAME) on $(hostname)"
say "  backup:   $KEY"
say "  database: $db"
say "  current:  $current"

if [ "$DRY_RUN" -eq 1 ]; then
  say ""
  say "Dry run: nothing changed. Without --dry-run this would:"
  say "  1. download the backup to $staging and check it"
  say "  2. ask for confirmation (unless --yes)"
  say "  3. stop $CONFIG_UNIT, move $db (and any -wal/-shm) into"
  say "     $data_dir/pre-restore-<time>-<random>/, and move the download into place"
  say "  4. start $CONFIG_UNIT and wait up to ${HEALTH_TIMEOUT}s for http://127.0.0.1:$PORT/healthz,"
  say "     putting the previous database back if it does not come up healthy"
  exit 0
fi

# ── Recovery: one path for every way out from here ────────────────────────────
# phase says how far the restore got, so recover knows what to undo:
#   staging       nothing live touched: remove the staging file
#   stopping      the stop was asked for: start the service again
#   swapping/swapped  files moved: roll back to the previous database
#   restored      restored and healthy
phase=staging
safety=""

healthy() {
  local end=$((SECONDS + HEALTH_TIMEOUT)) body
  while :; do
    if body=$(curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" 2>/dev/null) \
       && grep -q '"status":"ok"' <<<"$body"; then
      HEALTH_BODY=$body
      return 0
    fi
    [ "$SECONDS" -lt "$end" ] || return 1
    sleep 1
  done
}

rollback() {
  local f base unhealthy="$safety/restored-unhealthy"
  say ""
  say "Rolling back to the previous database."
  systemctl stop "$CONFIG_UNIT" >>"$LOG" 2>&1 || say "  warning: systemctl stop $CONFIG_UNIT failed"
  # If the previous database was moved aside (or there was none), whatever is
  # at the live path now is the restored copy: keep it for diagnosis.
  if [ -e "$safety/$(basename "$db")" ] || [ "$prev_exists" -eq 0 ]; then
    for f in "$db" "$db-wal" "$db-shm"; do
      if [ -e "$f" ]; then
        if ! { as_service_user mkdir -p "$unhealthy" && mv -- "$f" "$unhealthy/"; }; then
          say "  warning: could not move $f aside"
        fi
      fi
    done
  fi
  for f in "$safety"/*; do
    base=$(basename "$f")
    if [ ! -f "$f" ] || [ "$base" = restore.log ]; then continue; fi
    if [ -e "$data_dir/$base" ]; then
      say "  warning: $data_dir/$base exists, so $f was left where it is"
    else
      mv -- "$f" "$data_dir/" || say "  warning: could not move $f back"
    fi
  done
  rm -f -- "$staging" "$staging.download"
  if systemctl start "$CONFIG_UNIT" >>"$LOG" 2>&1 && healthy; then
    say "Rolled back: $CONFIG_UNIT is healthy again on the previous database."
  else
    say "$CONFIG_UNIT is still not healthy on the previous database either; see journalctl -u $CONFIG_UNIT"
  fi
  say ""
  say "Where things are:"
  for f in "$db" "$unhealthy/$(basename "$db")"; do
    if [ -e "$f" ]; then say "  $f"; fi
  done
  say "  log: $LOG"
}

recover() {
  local rc=$?
  trap - EXIT
  trap '' INT TERM HUP PIPE
  set +e
  case "$phase" in
    restored) exit 0 ;;
    staging)
      rm -f -- "$staging" "$staging.download" "$staging-wal" "$staging-shm"
      exit "$rc" ;;
    stopping)
      systemctl start "$CONFIG_UNIT" >>"$LOG" 2>&1 || say "  warning: systemctl start $CONFIG_UNIT failed"
      rm -f -- "$staging" "$staging.download"
      say "Nothing was swapped; $CONFIG_UNIT was started again on the previous database."
      exit 1 ;;
    *)
      phase=rolling_back
      rollback
      exit 1 ;;
  esac
}
trap recover EXIT
trap 'exit 1' INT TERM

# ── 2. Stage and check the download, with the service still running ──────────
say ""
say "Downloading to $staging ..."
rm -f -- "$staging" "$staging.download"
# A fresh path, so --restore-backup has nothing to overwrite and does not ask.
if ! out=$(DB_PATH="$staging" as_service_user "$CONFIG_BIN" --restore-backup "$KEY" </dev/null 2>&1); then
  printf '%s\n' "$out" | tail -5 >&2
  fail "download failed; nothing changed"
fi
[ -s "$staging" ] || fail "download produced no file at $staging; nothing changed"
check=$(as_service_user sqlite3 -readonly "$staging" 'PRAGMA integrity_check;' 2>&1 || true)
[ "$check" = "ok" ] || fail "the backup failed PRAGMA integrity_check ($check); nothing changed"
count=$(namespaces "$staging") || fail "the backup has no readable config_namespaces table; nothing changed"
# Checking it may have left a -shm beside it; nothing should follow it into place.
rm -f -- "$staging-wal" "$staging-shm"
say "  ok: integrity_check passed, $count namespace(s)"
if [ "$count" -eq 0 ] && [ "$YES" -eq 0 ]; then
  fail "the backup has no namespaces, which is not a database worth restoring; pass --yes to restore it anyway. Nothing changed"
fi

# ── 3. Confirm ────────────────────────────────────────────────────────────────
if [ "$YES" -eq 0 ]; then
  say ""
  say "This stops $CONFIG_UNIT and replaces the current database with the backup:"
  say "  current: $current"
  say "  backup:  $count namespace(s)"
  printf "Type 'yes' to restore: "
  read -r answer || answer=""
  if [ "$answer" != "yes" ]; then
    say "Aborted; nothing changed."
    exit 1
  fi
fi

# ── 4. Swap ───────────────────────────────────────────────────────────────────
# Created, and checked, before the stop: a failure here still changes nothing.
safety=$(as_service_user mktemp -d "$data_dir/pre-restore-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX") \
  || fail "could not create a directory for the previous database in $data_dir; nothing changed"
as_service_user test -w "$safety" || fail "$safety is not writable by $CONFIG_USER; nothing changed"
LOG="$safety/restore.log"
say "Log: $LOG"

# From the stop on, a dropped connection must not leave the job half done.
trap '' HUP PIPE
say ""
say "Stopping $CONFIG_UNIT ..."
phase=stopping
systemctl stop "$CONFIG_UNIT" >>"$LOG" 2>&1 || fail "systemctl stop $CONFIG_UNIT failed"

# The moves take milliseconds and must not be split by a Ctrl-C.
trap '' INT TERM
phase=swapping
for f in "$db" "$db-wal" "$db-shm"; do
  # A -wal left beside the restored file would be replayed into it.
  if [ -e "$f" ]; then mv -- "$f" "$safety/"; fi
done
mv -- "$staging" "$db"
phase=swapped
trap 'exit 1' INT TERM
say "  previous database moved to $safety/"

# ── 5. Start and check ────────────────────────────────────────────────────────
say "Starting $CONFIG_UNIT ..."
if ! systemctl start "$CONFIG_UNIT" >>"$LOG" 2>&1; then
  fail "systemctl start $CONFIG_UNIT failed on the restored database"
fi
if ! healthy; then
  fail "$CONFIG_UNIT did not come up healthy within ${HEALTH_TIMEOUT}s on the restored database"
fi
phase=restored
say ""
say "Restored $db from $KEY; $CONFIG_UNIT is healthy."
say "  $(grep -o '"version":"[^"]*"' <<<"$HEALTH_BODY" || true)"
say "The previous database is in $safety/; remove it once you are happy."
