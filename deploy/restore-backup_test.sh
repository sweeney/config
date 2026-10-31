#!/usr/bin/env bash
#
# Tests for deploy/restore-backup.sh, against stub config-server, systemctl,
# setpriv, curl, mktemp, ssh and scp on PATH, and real sqlite3 databases in a
# fixture directory, so no host, R2 or service is needed.
#
# Usage: deploy/restore-backup_test.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/restore-backup.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
command -v sqlite3 >/dev/null || { echo "sqlite3 is needed to run these tests" >&2; exit 2; }
REAL_MKTEMP=$(command -v mktemp)
REAL_SLEEP=$(command -v sleep)
REAL_MV=$(command -v mv)

mkdir -p "$WORK/bin"
# config-server: --list-backups prints $LISTING; --restore-backup KEY writes to
# $DB_PATH what RESTORE_MODE says (good, empty, corrupt, notable, fail), and
# records the R2 secret it was given, to check the env file arrived intact.
cat >"$WORK/bin/config-server" <<'EOF'
#!/usr/bin/env bash
echo "config-server $*" >>"$CALLS"
case "$1" in
  --list-backups) cat "$LISTING" ;;
  --restore-backup)
    printf '%s' "${R2_SECRET_ACCESS_KEY-}" >"$SEEN_SECRET"
    case "${RESTORE_MODE:-good}" in
      good)    cp "$FIXTURES/good.db" "$DB_PATH" ;;
      empty)   cp "$FIXTURES/empty.db" "$DB_PATH" ;;
      corrupt) printf 'not a database at all, just bytes\n%.0s' {1..300} >"$DB_PATH" ;;
      notable) cp "$FIXTURES/notable.db" "$DB_PATH" ;;
      fail)    echo "download: get object: NoSuchKey" >&2; exit 1 ;;
    esac ;;
esac
EOF
# systemctl: records each call, with the live database's namespaces for start;
# SYSTEMCTL_FAIL=stop|start|start-once makes those fail.
cat >"$WORK/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  show) echo "${UNIT_WORKDIR-}"; exit 0 ;;
  start) echo "systemctl start $2 live=$(sqlite3 -readonly "$LIVE_DB" 'SELECT group_concat(name) FROM config_namespaces' 2>/dev/null)" >>"$CALLS" ;;
  *) echo "systemctl $*" >>"$CALLS" ;;
esac
case "${SYSTEMCTL_FAIL:-}:$1" in
  stop:stop|start:start) exit 1 ;;
  start-once:start)
    if [ ! -e "$WORK_DIR/started-once" ]; then : >"$WORK_DIR/started-once"; exit 1; fi ;;
esac
EOF
# setpriv: record the user, then run the command as ourselves.
cat >"$WORK/bin/setpriv" <<'EOF'
#!/usr/bin/env bash
while [ "$1" != "--" ]; do shift; done; shift
echo "setpriv $1" >>"$CALLS"
exec "$@"
EOF
# curl /healthz, by HEALTH: ok always; old-only only while the live database is
# the previous one (its one namespace is "live"); gate as old-only, or once
# $WORK/gate exists; never. Each call touches $WORK/curl-called.
cat >"$WORK/bin/curl" <<'EOF'
#!/usr/bin/env bash
: >"$WORK_DIR/curl-called"
prev() { [ "$(sqlite3 -readonly "$LIVE_DB" "SELECT name FROM config_namespaces" 2>/dev/null)" = live ]; }
case "${HEALTH:-ok}" in
  ok) ;;
  old-only) prev || exit 22 ;;
  gate) prev || [ -e "$WORK_DIR/gate" ] || exit 22 ;;
  never) exit 22 ;;
esac
echo '{"status":"ok","version":"06ccbb7"}'
EOF
# sleep: short, so a health wait loops many times within HEALTH_TIMEOUT.
cat >"$WORK/bin/sleep" <<EOF
#!/usr/bin/env bash
exec $REAL_SLEEP 0.05
EOF
# mv fails the first move of the live database's -wal when MV_FAIL=wal, as a
# one-off error would; later moves of it succeed.
cat >"$WORK/bin/mv" <<EOF
#!/usr/bin/env bash
if [ "\${MV_FAIL:-}" = wal ] && [ ! -e "$WORK/mv-failed" ]; then
  for a; do
    if [ "\$a" = "$WORK/data/config.db-wal" ]; then
      : >"$WORK/mv-failed"; echo "mv: cannot move \$a" >&2; exit 1
    fi
  done
fi
exec $REAL_MV "\$@"
EOF
# mktemp fails when MKTEMP_FAIL=1.
cat >"$WORK/bin/mktemp" <<EOF
#!/usr/bin/env bash
[ "\${MKTEMP_FAIL:-0}" = 1 ] && { echo "mktemp: failed" >&2; exit 1; }
exec $REAL_MKTEMP "\$@"
EOF
# ssh: answer mktemp with a directory and record everything else; scp records.
cat >"$WORK/bin/ssh" <<'EOF'
#!/usr/bin/env bash
echo "ssh $*" >>"$CALLS"
case "$*" in *mktemp*) echo /tmp/restore-backup.TEST ;; esac
EOF
cat >"$WORK/bin/scp" <<'EOF'
#!/usr/bin/env bash
echo "scp $*" >>"$CALLS"
EOF
chmod +x "$WORK/bin/"*

FIX="$WORK/fixtures"; mkdir -p "$FIX"
schema='CREATE TABLE config_namespaces (name TEXT PRIMARY KEY);'
sqlite3 "$FIX/good.db" "$schema INSERT INTO config_namespaces VALUES ('a'),('b'),('c');"
sqlite3 "$FIX/empty.db" "$schema"
sqlite3 "$FIX/notable.db" 'CREATE TABLE other (x);'

# The secret has a $ and backticks, and CORS_ORIGINS an unquoted space, as a
# systemd EnvironmentFile allows: sourced as bash, both would misbehave.
# shellcheck disable=SC2016 # literal $ and backticks are the point
SECRET='s3cr3t$HOME`touch '"$WORK"'/ran`-do-not-print'
ENV="$WORK/config.env"
printf 'IDENTITY_ENV=production\nCORS_ORIGINS=https://a.example https://b.example\nR2_SECRET_ACCESS_KEY=%s\n' "$SECRET" >"$ENV"

key() { printf '%s/backups/config/%s/config-%sT00:00:00Z.sqlite3' "${2:-production}" "${1//-//}" "$1"; }
listing() { # listing ENV KEY...
  local env=$1; shift
  {
    printf 'Listing config backups for environment: %s\n\n' "$env"
    local i=1 k
    for k in "$@"; do printf '%-4d  2026-10-31 00:00:00       40KB  %s\n' "$i" "$k"; i=$((i + 1)); done
    printf '\n%d backup(s) found.\n\n' "$#"
    printf 'To restore (stop the service first): /opt/config/bin/config-server --restore-backup <key>\n'
  }
}
listing production "$(key 2026-10-30)" "$(key 2026-10-31)" \
  "production/backups/identity/2026/11/01/identity-2026-11-01T00:00:00Z.sqlite3" >"$WORK/listing"

# reset puts a live database (one namespace, "live") and a stale -wal in place.
# Everything here reads the live database -readonly, as the script does: a
# read-write open would delete the stale -wal on close (Debian's sqlite3 does).
reset() {
  rm -rf "$WORK/data" "$WORK/started-once" "$WORK/curl-called" "$WORK/gate" "$WORK/ran" "$WORK/mv-failed"
  mkdir -p "$WORK/data"
  sqlite3 "$WORK/data/config.db" "$schema INSERT INTO config_namespaces VALUES ('live');"
  echo stale-wal >"$WORK/data/config.db-wal"
  : >"$WORK/calls"
}
live() { sqlite3 -readonly "$WORK/data/config.db" 'SELECT group_concat(name) FROM config_namespaces' 2>&1; }

# invoke ARGS... runs the script with the stubs and fixtures. It execs, so
# when run in the background $! is the script itself, for the signal tests.
invoke() {
  exec env PATH="$WORK/bin:$PATH" CALLS="$WORK/calls" LISTING="${LISTING_FILE:-$WORK/listing}" FIXTURES="$FIX" \
    WORK_DIR="$WORK" SEEN_SECRET="$WORK/seen-secret" LIVE_DB="$WORK/data/config.db" \
    CONFIG_BIN="$WORK/bin/config-server" CONFIG_ENV_FILE="${ENV_FILE:-$ENV}" \
    CONFIG_WORKDIR="${WORKDIR_OVERRIDE-$WORK/data}" CONFIG_USER=config \
    HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-1}" SKIP_ROOT_CHECK=1 "$SCRIPT" "$@"
}

pass=0 fail=0
OUT=""
# run WANT_RC NAME [script args...] — runs the script; sets OUT; fails on rc.
run() {
  local want_rc=$1 name=$2 rc; shift 2
  CURRENT=$name
  OUT=$(invoke "$@" 2>&1 <<<"${ANSWER:-}") && rc=0 || rc=$?
  if [ "$rc" -ne "$want_rc" ]; then bad "exit $rc, want $want_rc"; return 1; fi
  if grep -qF 's3cr3t' <<<"$OUT"; then bad "printed a credential from the env file"; return 1; fi
}
bad() { echo "FAIL  $CURRENT: $*"; printf '%s\n' "$OUT" | sed 's/^/      /'; fail=$((fail + 1)); }
# check DESCRIPTION COMMAND... — one assertion about the last run.
check() {
  local what=$1; shift
  if "$@"; then return 0; fi
  bad "$what"; return 1
}
done_() { echo "ok    $CURRENT"; pass=$((pass + 1)); }
has()       { grep -qF -- "$1" <<<"$OUT"; }
called()    { grep -qF -- "$1" "$WORK/calls"; }
notcalled() { ! called "$1"; }
not_in_out() { ! has "$1"; }
safety_dir() { find "$WORK/data" -maxdepth 1 -name "pre-restore-*" -type d | head -1; }
# in_order A B — call A was made, and before call B.
in_order() {
  local a b
  a=$(grep -nF -- "$1" "$WORK/calls" | head -1 | cut -d: -f1)
  b=$(grep -nF -- "$2" "$WORK/calls" | tail -1 | cut -d: -f1)
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
# untouched — the live database, its WAL and the service are as reset left them.
# The -wal is checked before live() opens the database, so no open can hide
# a missing one.
untouched() {
  [ -e "$WORK/data/config.db-wal" ] && [ "$(live)" = live ] \
    && notcalled "systemctl stop" && [ ! -e "$WORK/data/restore-staging.sqlite3" ]
}

# ── Restores ──────────────────────────────────────────────────────────────────
reset
run 0 "restores the given key, keeping the previous database" --on-host --yes "$(key 2026-10-30)" \
  && check "live database is the backup" [ "$(live)" = "a,b,c" ] \
  && check "previous database kept" [ -f "$(safety_dir)/config.db" ] \
  && check "stale -wal moved aside, not replayed" [ ! -e "$WORK/data/config.db-wal" ] \
  && check "stale -wal kept with the previous database" [ -f "$(safety_dir)/config.db-wal" ] \
  && check "no staging file left" [ ! -e "$WORK/data/restore-staging.sqlite3" ] \
  && check "downloaded as the service user" called "setpriv $WORK/bin/config-server" \
  && check "downloaded the key asked for" called "--restore-backup $(key 2026-10-30)" \
  && check "stopped, then started on the restored database" in_order "systemctl stop config" "systemctl start config live=a,b,c" \
  && check "started only once" [ "$(grep -c 'systemctl start' "$WORK/calls")" -eq 1 ] \
  && check "a log is kept with the previous database" grep -qF "Restored" "$(safety_dir)/restore.log" \
  && check "reports health" has "config is healthy" \
  && done_

reset
run 0 "the env file reaches the binary as systemd would pass it, and nothing in it runs" --on-host --yes "$(key 2026-10-30)" \
  && check "secret arrived byte for byte" [ "$(cat "$WORK/seen-secret")" = "$SECRET" ] \
  && check "the backtick in it never ran" [ ! -e "$WORK/ran" ] \
  && done_

reset
run 0 "--latest picks the newest config backup, not another service's" --on-host --yes --latest \
  && check "restored the 31st" called "--restore-backup $(key 2026-10-31)" \
  && done_

reset
ANSWER=yes run 0 "asks first, showing both namespace counts, and 'yes' restores" --on-host "$(key 2026-10-30)" \
  && check "prompted" has "Type 'yes' to restore" \
  && check "shows the current count" has "current: 1 namespace(s)" \
  && check "shows the backup's count" has "backup:  3 namespace(s)" \
  && check "restored" [ "$(live)" = "a,b,c" ] \
  && done_

reset
ANSWER=no run 1 "anything but 'yes' aborts with nothing changed" --on-host "$(key 2026-10-30)" \
  && check "says so" has "Aborted; nothing changed." \
  && check "nothing touched" untouched \
  && done_

reset
printf 'IDENTITY_ENV=Production\nR2_SECRET_ACCESS_KEY=x\n' >"$WORK/env-odd"
listing development "$(key 2026-10-30 development)" >"$WORK/listing-dev"
ENV_FILE="$WORK/env-odd" LISTING_FILE="$WORK/listing-dev" \
  run 0 "the environment is the one the listing names, not IDENTITY_ENV as written" --on-host --yes --latest \
  && check "restored the development key" called "--restore-backup $(key 2026-10-30 development)" \
  && done_

reset
WORKDIR_OVERRIDE="" UNIT_WORKDIR="$WORK/data" run 0 "the database directory comes from the unit's WorkingDirectory" --on-host --yes --latest \
  && check "restored there" [ "$(live)" = "a,b,c" ] \
  && done_

# ── Nothing changes when the backup is bad ────────────────────────────────────
for mode in corrupt notable fail; do
  reset
  RESTORE_MODE=$mode run 1 "a $mode download changes nothing" --on-host --yes "$(key 2026-10-30)" \
    && check "says nothing changed" has "nothing changed" \
    && check "nothing touched" untouched \
    && done_
done

reset
ANSWER=yes RESTORE_MODE=empty run 1 "a backup with no namespaces is refused without --yes" --on-host "$(key 2026-10-30)" \
  && check "says why" has "has no namespaces" \
  && check "nothing touched" untouched \
  && done_
reset
RESTORE_MODE=empty run 0 "--yes restores a backup with no namespaces" --on-host --yes "$(key 2026-10-30)" \
  && check "restored" [ "$(live)" = "" ] \
  && done_

reset
MKTEMP_FAIL=1 run 1 "no directory for the previous database: fails before the stop" --on-host --yes "$(key 2026-10-30)" \
  && check "says nothing changed" has "nothing changed" \
  && check "nothing touched" untouched \
  && done_

reset
SYSTEMCTL_FAIL=stop run 1 "a failed stop swaps nothing and starts the service again" --on-host --yes "$(key 2026-10-30)" \
  && check "says so" has "Nothing was swapped" \
  && check "live database untouched" [ "$(live)" = live ] \
  && check "started again" called "systemctl start config" \
  && done_

# ── Rollback ──────────────────────────────────────────────────────────────────
reset
HEALTH=old-only run 1 "unhealthy on the restored database: puts the previous one back" --on-host --yes "$(key 2026-10-30)" \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "its -wal back beside it" [ -e "$WORK/data/config.db-wal" ] \
  && check "restored copy kept aside" [ -f "$(safety_dir)/restored-unhealthy/config.db" ] \
  && check "says it rolled back and is healthy" has "healthy again on the previous database" \
  && done_

reset
MV_FAIL=wal run 1 "a swap that fails between the database and its -wal keeps that -wal with the previous database" --on-host --yes "$(key 2026-10-30)" \
  && check "the previous -wal is still beside config.db" grep -qx stale-wal "$WORK/data/config.db-wal" \
  && check "it was not filed as the restored copy's" [ ! -e "$(safety_dir)/restored-unhealthy/config.db-wal" ] \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "says it rolled back" has "healthy again on the previous database" \
  && done_

reset
SYSTEMCTL_FAIL=start-once run 1 "a failed start on the restored database rolls back" --on-host --yes "$(key 2026-10-30)" \
  && check "says the start failed" has "systemctl start config failed on the restored database" \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "started again on the previous database" called "systemctl start config live=live" \
  && check "says it is healthy again" has "healthy again on the previous database" \
  && done_

reset
SYSTEMCTL_FAIL=start run 1 "a start that fails during the rollback too still restores the files and says so" --on-host --yes "$(key 2026-10-30)" \
  && check "previous database in place" [ "$(live)" = live ] \
  && check "says it is still not healthy" has "still not healthy on the previous database" \
  && check "says where things are" has "Where things are:" \
  && done_

reset
HEALTH=never run 1 "still unhealthy after the rollback says so" --on-host --yes "$(key 2026-10-30)" \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "says it is still unhealthy" has "still not healthy on the previous database" \
  && done_

# signal SIGNAL THEN: start a restore that waits on health (HEALTH=gate), send
# SIGNAL once it is waiting, then run THEN (e.g. open the gate).
signal() {
  local sig=$1 then=$2 pid i rc
  invoke --on-host --yes "$(key 2026-10-30)" >"$WORK/out" 2>&1 </dev/null &
  pid=$!
  for ((i = 0; i < 100; i++)); do [ -e "$WORK/curl-called" ] && break; "$REAL_SLEEP" 0.05; done
  kill -s "$sig" "$pid" 2>/dev/null || true
  $then
  wait "$pid" && rc=0 || rc=$?
  OUT=$(cat "$WORK/out")
  return "$rc"
}
open_gate() { : >"$WORK/gate"; }
nothing() { :; }

reset
CURRENT="a TERM during the health wait rolls back"
if HEALTH=gate HEALTH_TIMEOUT=5 signal TERM nothing; then rc=0; else rc=$?; fi
check "exit 1 (got $rc)" [ "$rc" -eq 1 ] \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "says it rolled back" has "healthy again on the previous database" \
  && done_

reset
CURRENT="a HUP (a dropped ssh) after the stop does not interrupt the restore"
if HEALTH=gate HEALTH_TIMEOUT=5 signal HUP open_gate; then rc=0; else rc=$?; fi
check "exit 0 (got $rc)" [ "$rc" -eq 0 ] \
  && check "restored" [ "$(live)" = "a,b,c" ] \
  && done_

reset
start=$SECONDS
HEALTH=never HEALTH_TIMEOUT=2 run 1 "the health wait is bounded by HEALTH_TIMEOUT seconds" --on-host --yes "$(key 2026-10-30)" \
  && check "took $((SECONDS - start))s for two 2s waits" [ $((SECONDS - start)) -le 8 ] \
  && done_

# ── Dry run and preconditions: nothing changes ────────────────────────────────
reset
run 0 "--dry-run checks the key and changes nothing" --on-host --dry-run "$(key 2026-10-30)" \
  && check "prints the plan" has "Dry run: nothing changed" \
  && check "no download" notcalled "--restore-backup" \
  && check "no stop or start" notcalled "systemctl" \
  && check "live database untouched" [ "$(live)" = live ] \
  && done_

reset
run 2 "a key not in the listing is refused" --on-host --yes "$(key 2026-10-29)" \
  && check "says why" has "is not one of the config backups" && check "nothing touched" untouched && done_
run 2 "another service's key is refused" --on-host --yes production/backups/identity/2026/11/01/identity-2026-11-01T00:00:00Z.sqlite3 \
  && check "says why" has "is not one of the config backups" && done_
run 2 "a key and --latest together are refused" --on-host --latest "$(key 2026-10-30)" \
  && check "says why" has "not both" && done_
run 2 "no key at all is refused" --on-host \
  && check "says why" has "give a KEY or --latest" && done_
ENV_FILE=/nonexistent run 2 "a missing env file is a precondition failure" --on-host --latest \
  && check "says why" has "env file not found" && done_
printf 'No backups found.\n' >"$WORK/listing-none"
LISTING_FILE="$WORK/listing-none" run 2 "a listing with no header is refused" --on-host --latest \
  && check "says why" has "could not read the environment" && done_
run 2 "--help prints the header only" --help \
  && check "header shown" has "Restore config's SQLite database" \
  && check "stops at the header" not_in_out "set -euo pipefail" && done_

# ── From the dev machine ──────────────────────────────────────────────────────
reset
run 0 "the remote form copies itself and lib.sh into a mktemp -d directory and runs with sudo" --dry-run --latest sweeney@example.invalid \
  && check "mktemp -d on the host" called "mktemp -d /tmp/restore-backup.XXXXXXXX" \
  && check "script and lib copied in" called "scp -q $SCRIPT $HERE/lib.sh sweeney@example.invalid:/tmp/restore-backup.TEST/" \
  && check "runs with the flags passed through" called "sudo bash /tmp/restore-backup.TEST/restore-backup.sh --on-host --dry-run --latest" \
  && check "removes the directory" called "rm -rf /tmp/restore-backup.TEST" \
  && done_
reset
run 0 "the remote form passes a key and --yes through" --yes sweeney@example.invalid "$(key 2026-10-30)" \
  && check "key and --yes passed" called "--on-host --yes $(key 2026-10-30)" \
  && done_

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
