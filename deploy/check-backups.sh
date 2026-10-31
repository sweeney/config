#!/usr/bin/env bash
#
# Confirm that config's R2 backups are still landing where they should, since
# config moved to sweeney/common/backup (config#6, deployed 2026-10-28 13:04 UTC).
#
# READ-ONLY. It lists R2 and reads the journal; it never restores, uploads,
# deletes, or edits anything, on the host or in R2.
#
# From the dev machine (the usual way):
#
#   ./deploy/check-backups.sh sweeney@<host>
#   ./deploy/check-backups.sh --dry-run sweeney@<host>   # print, run nothing
#   ./deploy/check-backups.sh --since 2026-10-29 sweeney@<host>
#
# That copies this script and lib.sh into a private mktemp -d directory on the
# host, runs it there with sudo (you are asked for your password: the env file
# is root 0600), and removes the directory.
#
# On the host directly, with lib.sh beside it:
#
#   sudo ./check-backups.sh --on-host [--since 2026-10-28T13:04:25Z]
#
# Checks, for backups at or after --since (a date, or an RFC 3339 UTC time):
#   1. --list-backups succeeds against R2
#   2. at least one backup exists under {IDENTITY_ENV}/backups/config/
#   3. no config backup was filed anywhere else (e.g. under identity/)
#   4. every UTC day after --since, up to today, has one: config runs a daily
#      backup at 00:00 UTC whether or not anything was written
#   5. the journal shows no backup_failure since --since
#
# Exit: 0 all checks passed, 1 a check failed, 2 a precondition failed.
#
# Paths can be overridden for testing:
#   CONFIG_ENV_FILE  (default /etc/config/config.env)
#   CONFIG_BIN       (default /opt/config/bin/config-server)
#   CONFIG_UNIT      (default config)
#   TODAY            (default: today, UTC, as YYYY-MM-DD)
#   SKIP_ROOT_CHECK  (set to 1 to bypass the root requirement — testing only)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=deploy/lib.sh
. "$HERE/lib.sh"

CONFIG_ENV_FILE="${CONFIG_ENV_FILE:-/etc/config/config.env}"
CONFIG_BIN="${CONFIG_BIN:-/opt/config/bin/config-server}"
CONFIG_UNIT="${CONFIG_UNIT:-config}"
SINCE="2026-10-28T13:04:25Z"   # the deploy of config#6
ON_HOST=0
DRY_RUN=0
REMOTE=""

PASS=0; FAIL=0

ok()    { printf "  [ ok ] %s\n" "$*"; PASS=$((PASS+1)); }
bad()   { printf "  [FAIL] %s\n" "$*"; FAIL=$((FAIL+1)); }
info()  { printf "         %s\n" "$*"; }
head_() { printf "\n== %s ==\n" "$*"; }

usage() { usage_from_header "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --since)   SINCE="${2:?--since needs a value}"; shift 2 ;;
    --on-host) ON_HOST=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage ;;
    -*)        printf "unknown option: %s\n\n" "$1" >&2; usage ;;
    *)         [ -z "$REMOTE" ] || die "only one host, got $REMOTE and $1"; REMOTE="$1"; shift ;;
  esac
done

# A bare date means the start of that UTC day.
case "$SINCE" in
  [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]) SINCE="${SINCE}T00:00:00Z" ;;
  [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z) ;;
  *) die "--since must be YYYY-MM-DD or YYYY-MM-DDTHH:MM:SSZ, got $SINCE" ;;
esac

# ── From the dev machine: copy this script over and run it there ──────────────
if [ "$ON_HOST" -eq 0 ]; then
  [ -n "$REMOTE" ] || usage
  printf "Read-only check of config's R2 backups on %s (since %s).\n" "$REMOTE" "$SINCE"
  printf "It copies this script into a private temporary directory on the host,\n"
  printf "runs it with sudo, then removes the directory.\n\n"
  if [ "$DRY_RUN" -eq 1 ]; then
    print_run_on_host "$REMOTE" "$0" --on-host --since "$SINCE"
    exit 0
  fi
  rc=0
  run_on_host "$REMOTE" "$0" --on-host --since "$SINCE" || rc=$?
  exit "$rc"
fi

# ── On the host ───────────────────────────────────────────────────────────────
[ "$DRY_RUN" -eq 0 ] || die "--dry-run applies to the dev-machine form; on the host the check is already read-only"
require_root "the env file is mode 600"
for c in python3 journalctl; do
  command -v "$c" >/dev/null 2>&1 || die "required command not found: $c"
done
[ -x "$CONFIG_BIN" ] || die "config binary not found or not executable: $CONFIG_BIN"
[ -f "$CONFIG_ENV_FILE" ] || die "env file not found: $CONFIG_ENV_FILE"
[ -r "$CONFIG_ENV_FILE" ] || die "env file not readable: $CONFIG_ENV_FILE (need root)"
# Parsed as systemd does, never sourced or echoed: it holds the R2 credentials.
load_env_file "$CONFIG_ENV_FILE"
TODAY="${TODAY:-$(date -u +%Y-%m-%d)}"

head_ "R2 listing"
if listing=$("$CONFIG_BIN" --list-backups 2>&1); then
  ok "--list-backups succeeded"
else
  bad "--list-backups failed:"
  printf '%s\n' "$listing" | head -5 | sed 's/^/         /'
  printf "\n%d ok, %d failure(s)\n" "$PASS" "$FAIL"
  exit 1
fi

head_ "Backups since $SINCE"
# One line per finding, "ok|bad|info<TAB>text", so the counting stays in bash.
findings=$(printf '%s\n' "$listing" | python3 -I -c '
import re, sys
from datetime import date, timedelta

since, today = sys.argv[1], date.fromisoformat(sys.argv[2])
env = None
keys = []
for line in sys.stdin:
    m = re.match(r"Listing \S+ backups for environment: (\S+)", line)
    if m:
        env = m.group(1)
    m = re.search(r"(\S+/backups/\S+/config-(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ)\.sqlite3)\s*$", line)
    if m:
        keys.append((m.group(1), m.group(2)))

def out(kind, text):
    print(kind + "\t" + text)

if env is None:
    out("bad", "could not read the environment from the listing header")
    sys.exit()
home = env + "/backups/config/"
# At or after: a bare --since date is 00:00, exactly when the daily run fires.
recent = [(k, t) for k, t in keys if t >= since]
good = sorted(t for k, t in recent if k.startswith(home))
stray = [k for k, t in recent if not k.startswith(home)]

out("info", "environment %s: %d backup(s) listed, %d at or after %s" % (env, len(keys), len(recent), since))
if good:
    out("ok", "%d backup(s) under %s since %s, newest %s" % (len(good), home, since, good[-1]))
else:
    out("bad", "no backup under %s since %s" % (home, since))
if stray:
    for k in stray:
        out("bad", "config backup filed outside %s: %s" % (home, k))
else:
    out("ok", "none filed outside %s (e.g. under identity/)" % home)

days = {t[:10] for t in good}
d = date.fromisoformat(since[:10]) + timedelta(days=1)
missing = []
while d <= today:
    if d.isoformat() not in days:
        missing.append(d.isoformat())
    d += timedelta(days=1)
if missing:
    out("bad", "no backup on %s; the daily 00:00 UTC run should have made one" % ", ".join(missing))
else:
    out("ok", "a backup on every UTC day after %s, through %s" % (since[:10], today.isoformat()))
' "$SINCE" "$TODAY")

while IFS=$'\t' read -r kind text; do
  case "$kind" in
    ok)   ok "$text" ;;
    bad)  bad "$text" ;;
    info) info "$text" ;;
  esac
done <<<"$findings"

head_ "Journal since $SINCE"
journal_since="${SINCE%Z}"; journal_since="${journal_since/T/ }"
if jl=$(journalctl -u "$CONFIG_UNIT" --since "$journal_since UTC" --no-pager -o cat 2>&1); then
  failures=$(printf '%s\n' "$jl" | grep -c 'backup_failure' || true)
  successes=$(printf '%s\n' "$jl" | grep -c 'backup_success' || true)
  if [ "$failures" -eq 0 ]; then
    ok "no backup_failure in the journal ($successes backup_success)"
  else
    bad "$failures backup_failure line(s) in the journal; the latest:"
    printf '%s\n' "$jl" | grep 'backup_failure' | tail -1 | sed 's/^/         /'
  fi
else
  bad "could not read the journal for $CONFIG_UNIT"
fi

printf "\n──────────────────────────────────────────\n"
printf "  %d ok, %d failure(s)\n" "$PASS" "$FAIL"
printf "──────────────────────────────────────────\n"
[ "$FAIL" -eq 0 ]
