#!/usr/bin/env bash
#
# Tests for deploy/check-backups.sh, against a stub config-server and stub
# journalctl on PATH, so no host, R2 or journal is needed.
#
# Usage: deploy/check-backups_test.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/check-backups.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/bin"
# config-server --list-backups prints $LISTING, or fails when LIST_FAILS=1.
cat >"$WORK/bin/config-server" <<'EOF'
#!/usr/bin/env bash
[ "${LIST_FAILS:-0}" = 1 ] && { echo "list backups: operation error S3: ListObjectsV2: 403"; exit 1; }
cat "$LISTING"
EOF
# journalctl prints $JOURNAL.
cat >"$WORK/bin/journalctl" <<'EOF'
#!/usr/bin/env bash
cat "$JOURNAL"
EOF
# ssh and scp must never run in a dry run.
for c in ssh scp; do
  printf '#!/usr/bin/env bash\necho "%s was called" >&2; exit 99\n' "$c" >"$WORK/bin/$c"
done
chmod +x "$WORK/bin/"*

# As a systemd EnvironmentFile allows: an unquoted space, and a backtick that
# must never run. Sourced as bash, the first would run a command.
# shellcheck disable=SC2016 # the backtick is literal on purpose
printf 'IDENTITY_ENV=production\nCORS_ORIGINS=https://a.example https://b.example\nR2_SECRET_ACCESS_KEY=s3cr3t-do-not-print`touch %s/ran`\n' "$WORK" >"$WORK/config.env"

# listing writes a --list-backups output in common/backup v0.1.1's format, one
# row per key given, ending with its restore hint.
listing() {
  {
    printf 'Listing config backups for environment: production\n\n'
    printf '%-4s  %-20s  %8s  %s\n' "#" "Date" "Size" "Key"
    printf '%s\n' "────────────────────────────────────────"
    local i=1 k
    for k in "$@"; do
      printf '%-4d  %-20s  %6dKB  %s\n' "$i" "2026-10-30 00:00:00" 40 "$k"
      i=$((i+1))
    done
    printf '\n%d backup(s) found.\n' "$#"
    printf '\nTo restore (stop the service first): /opt/config/bin/config-server --restore-backup <key>\n'
  } >"$WORK/listing"
}
key() { printf 'production/backups/%s/%s/config-%sT00:00:00Z.sqlite3' "$1" "${2//-//}" "$2"; }

healthy_journal() {
  printf '%s\n' \
    "2026/10/29 00:00:00 audit: backup_success user=system detail=$(key config 2026-10-29)" \
    "2026/10/30 00:00:00 audit: backup_success user=system detail=$(key config 2026-10-30)" \
    >"$WORK/journal"
}

pass=0 fail=0
# expect NAME WANT_RC WANT_TEXT [script args...]
expect() {
  local name=$1 want_rc=$2 want_text=$3; shift 3
  local out rc
  out=$(PATH="$WORK/bin:$PATH" LISTING="$WORK/listing" JOURNAL="$WORK/journal" \
        CONFIG_BIN="$WORK/bin/config-server" CONFIG_ENV_FILE="${ENV_FILE:-$WORK/config.env}" \
        SKIP_ROOT_CHECK=1 TODAY=2026-10-30 "$SCRIPT" "$@" 2>&1) && rc=0 || rc=$?
  if [ "$rc" -ne "$want_rc" ]; then
    echo "FAIL  $name: exit $rc, want $want_rc"; printf '%s\n' "$out" | sed 's/^/      /'; fail=$((fail+1)); return
  fi
  if ! grep -qF -- "$want_text" <<<"$out"; then
    echo "FAIL  $name: output lacks: $want_text"; printf '%s\n' "$out" | sed 's/^/      /'; fail=$((fail+1)); return
  fi
  if grep -qF 's3cr3t-do-not-print' <<<"$out"; then
    echo "FAIL  $name: printed a credential from the env file"; fail=$((fail+1)); return
  fi
  echo "ok    $name"; pass=$((pass+1))
}

# A backup on 28 Oct at 00:00 predates the deploy and must not count.
listing "$(key config 2026-10-28)" "$(key config 2026-10-29)" "$(key config 2026-10-30)"
healthy_journal
expect "healthy: daily backups under config/" 0 "5 ok, 0 failure(s)" --on-host
expect "healthy: counts only those after the deploy" 0 "2 backup(s) under production/backups/config/" --on-host
expect "the restore hint is not read as a backup" 0 "environment production: 3 backup(s) listed" --on-host

listing "$(key config 2026-10-29)"
expect "a missing day fails" 1 "no backup on 2026-10-30" --on-host

listing "$(key config 2026-10-29)" "$(key config 2026-10-30)" "$(key identity 2026-10-30)"
expect "a backup filed under identity/ fails" 1 "filed outside production/backups/config/: production/backups/identity/" --on-host

listing "$(key config 2026-10-27)" "$(key config 2026-10-28)"
expect "nothing since the deploy fails" 1 "no backup under production/backups/config/ since" --on-host

listing "$(key config 2026-10-30)"
expect "--since narrows the window" 0 "5 ok, 0 failure(s)" --on-host --since 2026-10-29T23:00:00Z
expect "a bare --since date is the start of that day" 0 "since 2026-10-30T00:00:00Z" --on-host --since 2026-10-30

LIST_FAILS=1 expect "a failed R2 listing fails" 1 "--list-backups failed" --on-host

listing "$(key config 2026-10-29)" "$(key config 2026-10-30)"
printf '%s\n' "2026/10/30 00:00:01 audit: backup_failure user=system detail=upload timeout" >"$WORK/journal"
expect "a backup_failure in the journal fails" 1 "1 backup_failure line(s)" --on-host
healthy_journal

printf 'No backups found.\n' >"$WORK/listing"
expect "a listing with no header fails" 1 "could not read the environment" --on-host

expect "a malformed --since is refused" 2 "--since must be" --on-host --since yesterday
ENV_FILE=/nonexistent expect "a missing env file is a precondition failure" 2 "env file not found" --on-host
expect "the remote form needs a host" 2 "From the dev machine"
expect "--dry-run prints and runs nothing" 0 "would run:" --dry-run sweeney@example.invalid
expect "the remote copy goes in a private mktemp -d directory" 0 "mktemp -d" --dry-run sweeney@example.invalid
expect "the remote copy takes lib.sh with it" 0 "$HERE/lib.sh" --dry-run sweeney@example.invalid
if [ -e "$WORK/ran" ]; then echo "FAIL  nothing in the env file is run"; fail=$((fail+1)); else echo "ok    nothing in the env file is run"; pass=$((pass+1)); fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
