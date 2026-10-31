#!/usr/bin/env bash
#
# Tests for deploy/restore-backup.sh, against stub config-server, systemctl,
# setpriv, curl, ssh and scp on PATH, and real sqlite3 databases in a fixture
# directory, so no host, R2 or service is needed.
#
# Usage: deploy/restore-backup_test.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/restore-backup.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
command -v sqlite3 >/dev/null || { echo "sqlite3 is needed to run these tests" >&2; exit 2; }

mkdir -p "$WORK/bin"
# config-server: --list-backups prints $LISTING; --restore-backup KEY writes to
# $DB_PATH what RESTORE_MODE says (good, corrupt, notable, fail).
cat >"$WORK/bin/config-server" <<'EOF'
#!/usr/bin/env bash
echo "config-server $*" >>"$CALLS"
case "$1" in
  --list-backups) cat "$LISTING" ;;
  --restore-backup)
    case "${RESTORE_MODE:-good}" in
      good)    cp "$FIXTURES/good.db" "$DB_PATH" ;;
      corrupt) printf 'not a database at all, just bytes\n%.0s' {1..300} >"$DB_PATH" ;;
      notable) cp "$FIXTURES/notable.db" "$DB_PATH" ;;
      fail)    echo "download: get object: NoSuchKey" >&2; exit 1 ;;
    esac ;;
esac
EOF
cat >"$WORK/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "systemctl $*" >>"$CALLS"
EOF
# setpriv: record the user, then run the command as ourselves.
cat >"$WORK/bin/setpriv" <<'EOF'
#!/usr/bin/env bash
while [ "$1" != "--" ]; do shift; done; shift
echo "setpriv $1" >>"$CALLS"
exec "$@"
EOF
# curl /healthz: HEALTH=ok always healthy; old-only healthy only while the live
# database is the previous one (it has a namespace called "live"); never.
cat >"$WORK/bin/curl" <<'EOF'
#!/usr/bin/env bash
case "${HEALTH:-ok}" in
  ok) ;;
  old-only) [ "$(sqlite3 "$LIVE_DB" "SELECT name FROM config_namespaces" 2>/dev/null)" = live ] || exit 22 ;;
  never) exit 22 ;;
esac
echo '{"status":"ok","version":"06ccbb7"}'
EOF
cat >"$WORK/bin/sleep" <<'EOF'
#!/usr/bin/env bash
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
sqlite3 "$FIX/notable.db" 'CREATE TABLE other (x);'

ENV="$WORK/config.env"
printf 'IDENTITY_ENV=production\nR2_SECRET_ACCESS_KEY=s3cr3t-do-not-print\n' >"$ENV"

key() { printf 'production/backups/config/%s/config-%sT00:00:00Z.sqlite3' "${1//-//}" "$1"; }
printf '%s\n' \
  'Listing config backups for environment: production' '' \
  "1     2026-10-30 00:00:00       40KB  $(key 2026-10-30)" \
  "2     2026-10-31 00:00:00       40KB  $(key 2026-10-31)" \
  "3     2026-10-31 00:00:00       40KB  production/backups/identity/2026/11/01/identity-2026-11-01T00:00:00Z.sqlite3" \
  '' '3 backup(s) found.' '' \
  'To restore (stop the service first): /opt/config/bin/config-server --restore-backup <key>' \
  >"$WORK/listing"

# reset puts a live database (one namespace, "live") and a stale -wal in place.
reset() {
  rm -rf "$WORK/data"; mkdir -p "$WORK/data"
  sqlite3 "$WORK/data/config.db" "$schema INSERT INTO config_namespaces VALUES ('live');"
  echo stale-wal >"$WORK/data/config.db-wal"
  : >"$WORK/calls"
}
live() { sqlite3 "$WORK/data/config.db" 'SELECT group_concat(name) FROM config_namespaces' 2>&1; }

pass=0 fail=0
OUT=""
# run WANT_RC NAME [script args...] — runs the script; sets OUT; fails on rc.
run() {
  local want_rc=$1 name=$2 rc; shift 2
  CURRENT=$name
  OUT=$(PATH="$WORK/bin:$PATH" CALLS="$WORK/calls" LISTING="$WORK/listing" FIXTURES="$FIX" \
        LIVE_DB="$WORK/data/config.db" CONFIG_BIN="$WORK/bin/config-server" \
        CONFIG_ENV_FILE="${ENV_FILE:-$ENV}" CONFIG_WORKDIR="$WORK/data" CONFIG_USER=config \
        HEALTH_TIMEOUT=2 SKIP_ROOT_CHECK=1 "$SCRIPT" "$@" 2>&1 <<<"${ANSWER:-}") && rc=0 || rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    bad "exit $rc, want $want_rc"; return 1
  fi
  if grep -qF 's3cr3t-do-not-print' <<<"$OUT"; then bad "printed a credential from the env file"; return 1; fi
}
bad() { echo "FAIL  $CURRENT: $*"; printf '%s\n' "$OUT" | sed 's/^/      /'; fail=$((fail+1)); }
# check DESCRIPTION COMMAND... — one assertion about the last run.
check() {
  local what=$1; shift
  if "$@"; then return 0; fi
  bad "$what"; return 1
}
done_() { echo "ok    $CURRENT"; pass=$((pass+1)); }
has()      { grep -qF -- "$1" <<<"$OUT"; }
called()   { grep -qF -- "$1" "$WORK/calls"; }
notcalled() { ! called "$1"; }
safety_dir() { find "$WORK/data" -maxdepth 1 -name "pre-restore-*" -type d | head -1; }

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
  && check "stopped then started" grep -qx "systemctl stop config" "$WORK/calls" \
  && check "reports health" has "config is healthy" \
  && done_

reset
run 0 "--latest picks the newest config backup, not another service's" --on-host --yes --latest \
  && check "restored the 31st" called "--restore-backup $(key 2026-10-31)" \
  && done_

reset
ANSWER=yes run 0 "asks first, and 'yes' restores" --on-host "$(key 2026-10-30)" \
  && check "prompted" has "Type 'yes' to restore" \
  && check "restored" [ "$(live)" = "a,b,c" ] \
  && done_

reset
ANSWER=no run 1 "anything but 'yes' aborts with nothing changed" --on-host "$(key 2026-10-30)" \
  && check "says so" has "Aborted; nothing changed." \
  && check "live database untouched" [ "$(live)" = live ] \
  && check "service not stopped" notcalled "systemctl stop" \
  && check "staging removed" [ ! -e "$WORK/data/restore-staging.sqlite3" ] \
  && done_

# ── Nothing changes when the backup is bad ────────────────────────────────────
for mode in corrupt notable fail; do
  reset
  RESTORE_MODE=$mode run 1 "a $mode download changes nothing" --on-host --yes "$(key 2026-10-30)" \
    && check "says nothing changed" has "nothing changed" \
    && check "live database untouched" [ "$(live)" = live ] \
    && check "stale -wal untouched" [ -e "$WORK/data/config.db-wal" ] \
    && check "service not stopped" notcalled "systemctl stop" \
    && check "staging removed" [ ! -e "$WORK/data/restore-staging.sqlite3" ] \
    && done_
done

# ── Rollback ──────────────────────────────────────────────────────────────────
reset
HEALTH=old-only run 1 "unhealthy on the restored database: puts the previous one back" --on-host --yes "$(key 2026-10-30)" \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "restored copy kept aside" [ -f "$(safety_dir)/restored-unhealthy/config.db" ] \
  && check "says it rolled back and is healthy" has "healthy again on the previous database" \
  && done_

reset
HEALTH=never run 1 "still unhealthy after the rollback says so" --on-host --yes "$(key 2026-10-30)" \
  && check "previous database live again" [ "$(live)" = live ] \
  && check "says it is still unhealthy" has "still not healthy on the previous database" \
  && done_

# ── Dry run and preconditions: nothing changes ────────────────────────────────
reset
run 0 "--dry-run checks the key and changes nothing" --on-host --dry-run "$(key 2026-10-30)" \
  && check "prints the plan" has "Dry run: nothing changed" \
  && check "no download" notcalled "--restore-backup" \
  && check "no systemctl" notcalled "systemctl" \
  && check "live database untouched" [ "$(live)" = live ] \
  && done_

reset
run 2 "a key not in the listing is refused" --on-host --yes "$(key 2026-10-29)" \
  && check "says why" has "is not one of the config backups" && check "no systemctl" notcalled systemctl && done_
run 2 "another service's key is refused" --on-host --yes production/backups/identity/2026/11/01/identity-2026-11-01T00:00:00Z.sqlite3 \
  && check "says why" has "is not one of the config backups" && done_
run 2 "a key and --latest together are refused" --on-host --latest "$(key 2026-10-30)" \
  && check "says why" has "not both" && done_
run 2 "no key at all is refused" --on-host \
  && check "says why" has "give a KEY or --latest" && done_
ENV_FILE=/nonexistent run 2 "a missing env file is a precondition failure" --on-host --latest \
  && check "says why" has "env file not found" && done_

# ── From the dev machine ──────────────────────────────────────────────────────
reset
run 0 "the remote form copies itself into a mktemp -d directory and runs with sudo" --dry-run --latest sweeney@example.invalid \
  && check "mktemp -d on the host" called "mktemp -d /tmp/restore-backup.XXXXXXXX" \
  && check "copied in" called "scp -q $SCRIPT sweeney@example.invalid:/tmp/restore-backup.TEST/restore-backup.sh" \
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
