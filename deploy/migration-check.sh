#!/usr/bin/env bash
#
# Pre- and post-deploy checks for the public-read-role schema migration.
#
# Run this ON the config host (garibaldi), as root:
#
#   sudo ./migration-check.sh preflight    # before deploying
#   sudo ./migration-check.sh verify       # after deploying
#   sudo ./migration-check.sh backups      # list R2 backups
#   sudo ./migration-check.sh restart-check  # restart, confirm no second rebuild
#
# Every check is READ-ONLY except restart-check, which asks first. The
# database is opened with sqlite3 -readonly so this script cannot alter it
# even if a query were wrong.
#
# Paths can be overridden for testing:
#   CONFIG_ENV_FILE  (default /etc/config/config.env)
#   CONFIG_DB        (default /var/lib/config/config.db)
#   CONFIG_BIN       (default /opt/config/bin/config-server)
#   CONFIG_PORT      (default 8282)
#   CONFIG_UNIT      (default config)
#   STATE_FILE       (default /var/tmp/config-migration-preflight.state)
#   SKIP_ROOT_CHECK  (set to 1 to bypass the root requirement — testing only)

set -euo pipefail

CONFIG_ENV_FILE="${CONFIG_ENV_FILE:-/etc/config/config.env}"
CONFIG_DB="${CONFIG_DB:-/var/lib/config/config.db}"
CONFIG_BIN="${CONFIG_BIN:-/opt/config/bin/config-server}"
CONFIG_PORT="${CONFIG_PORT:-8282}"
CONFIG_UNIT="${CONFIG_UNIT:-config}"
STATE_FILE="${STATE_FILE:-/var/tmp/config-migration-preflight.state}"

# The schema step count this script expects afterwards. Bump it alongside
# currentSchemaVersion in db/schema.go when a step is added.
EXPECTED_SCHEMA_VERSION="${EXPECTED_SCHEMA_VERSION:-2}"

PASS=0; WARN=0; FAIL=0

trap 'rc=$?; if [ "$rc" -ne 0 ] && [ "${CLEAN_EXIT:-0}" -ne 1 ]; then
  printf "\n  !! script aborted unexpectedly (exit %s)\n" "$rc" >&2
  printf "     Nothing was modified; re-run once the cause is understood.\n" >&2
fi' EXIT

ok()   { printf "  [ ok ] %s\n" "$*"; PASS=$((PASS+1)); }
warn() { printf "  [warn] %s\n" "$*"; WARN=$((WARN+1)); }
bad()  { printf "  [FAIL] %s\n" "$*"; FAIL=$((FAIL+1)); }
info() { printf "         %s\n" "$*"; }
head_() { printf "\n== %s ==\n" "$*"; }

die() { printf "\nERROR: %s\n" "$*" >&2; CLEAN_EXIT=1; exit 2; }

require_root() {
  [ "${SKIP_ROOT_CHECK:-0}" = "1" ] && return 0
  [ "$(id -u)" -eq 0 ] || die "must run as root (use sudo) — the env file is mode 600 and the journal needs privileges"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# load_env sources the service's environment file. It is never echoed: it
# holds R2 credentials.
load_env() {
  [ -f "$CONFIG_ENV_FILE" ] || die "env file not found: $CONFIG_ENV_FILE"
  [ -r "$CONFIG_ENV_FILE" ] || die "env file not readable: $CONFIG_ENV_FILE (need root)"
  set -a
  # shellcheck disable=SC1090
  . "$CONFIG_ENV_FILE"
  set +a
}

# SQLite cannot open a WAL-mode database read-only unless it can use the
# -shm file, so -readonly fails outright (SQLITE_CANTOPEN, exit 14) on a
# cleanly-closed database. Detect what works once, prefer read-only, and fall
# back to a normal open. Either way this script only ever issues SELECT and
# read-only PRAGMA statements — nothing here can modify the database.
SQ_MODE=""

detect_sq_mode() {
  if [ ! -f "$CONFIG_DB" ]; then SQ_MODE="none"; return; fi
  if sqlite3 -readonly -batch "$CONFIG_DB" "SELECT 1;" >/dev/null 2>&1; then
    SQ_MODE="ro"
  elif sqlite3 -batch "$CONFIG_DB" "SELECT 1;" >/dev/null 2>&1; then
    SQ_MODE="rw"
  else
    SQ_MODE="none"
  fi
}

# sq runs one read query. It never returns a non-zero status: callers treat an
# empty result as "unknown" rather than having set -e abort the run.
#
# Detects the open mode on first use, so every subcommand works — not only
# the ones that happen to call check_basics first. Getting this wrong made a
# comparison of two empty strings look like a passing check.
sq() {
  [ -n "$SQ_MODE" ] || detect_sq_mode
  case "$SQ_MODE" in
    ro) sqlite3 -readonly -noheader -batch "$CONFIG_DB" "$1" 2>/dev/null || true ;;
    rw) sqlite3 -noheader -batch "$CONFIG_DB" "$1" 2>/dev/null || true ;;
    *)  printf '' ;;
  esac
}

sq_file() {
  sqlite3 -readonly -noheader -batch "$1" "$2" 2>/dev/null \
    || sqlite3 -noheader -batch "$1" "$2" 2>/dev/null || true
}

# http_code prints curl's status, or 000 when the request could not be made.
# curl already emits 000 in that case, so an "|| echo" here would concatenate.
http_code() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null || true
}

check_basics() {
  head_ "Environment"
  need_cmd sqlite3; need_cmd curl; need_cmd awk; need_cmd df
  ok "required tools present (sqlite3, curl, awk, df)"

  if [ -f "$CONFIG_ENV_FILE" ]; then
    local mode; mode=$(stat -c '%a' "$CONFIG_ENV_FILE" 2>/dev/null || stat -f '%Lp' "$CONFIG_ENV_FILE" 2>/dev/null || echo "?")
    if [ "$mode" = "600" ]; then ok "env file $CONFIG_ENV_FILE present (mode $mode)"
    else warn "env file mode is $mode, expected 600 — it holds R2 credentials"; fi
  else
    bad "env file missing: $CONFIG_ENV_FILE"; return
  fi

  if [ -f "$CONFIG_DB" ]; then
    local owner size
    owner=$(stat -c '%U' "$CONFIG_DB" 2>/dev/null || stat -f '%Su' "$CONFIG_DB" 2>/dev/null || echo "?")
    size=$(du -h "$CONFIG_DB" 2>/dev/null | awk '{print $1}')
    ok "database $CONFIG_DB present ($size, owner $owner)"
    [ "$owner" = "config" ] || warn "database owner is '$owner', expected 'config' — the service may fail to write"
  else
    bad "database missing: $CONFIG_DB"
  fi

  detect_sq_mode
  case "$SQ_MODE" in
    ro) ok "database opens read-only" ;;
    rw) ok "database opens (read-write handle; this script issues only reads)"
        info "read-only open is unavailable for a WAL database with no -shm — expected" ;;
    *)  bad "cannot open the database at all — is it corrupt, or owned by another user?" ;;
  esac
}

check_service() {
  head_ "Service"
  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet "$CONFIG_UNIT" 2>/dev/null; then
    ok "$CONFIG_UNIT is active"
  else
    warn "$CONFIG_UNIT is not active (or systemctl unavailable) — health checks will be skipped"
    return
  fi
  local body
  if body=$(curl -sf --max-time 5 "http://localhost:${CONFIG_PORT}/healthz" 2>/dev/null); then
    local ver; ver=$(printf '%s' "$body" | grep -o '"version":"[^"]*"' | cut -d'"' -f4 || true)
    ok "/healthz responds (version ${ver:-unknown})"
  else
    bad "/healthz did not respond on localhost:${CONFIG_PORT}"
  fi
}

schema_permits_public() {
  sq "SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces';" | grep -q "'public'"
}

namespace_summary() {
  sq "SELECT read_role || '/' || write_role || '  ' || COUNT(*) FROM config_namespaces GROUP BY read_role, write_role ORDER BY 1;"
}

cmd_preflight() {
  require_root; check_basics; check_service

  head_ "Current schema"
  local uv total
  uv=$(sq "PRAGMA user_version;" || echo "?")
  total=$(sq "SELECT COUNT(*) FROM config_namespaces;" || echo "?")

  if schema_permits_public; then
    warn "read_role already permits 'public' — this host looks migrated already"
    info "the deploy will then be a no-op for the schema, which is fine"
  else
    ok "read_role CHECK is the pre-migration shape (rebuild will run on first restart)"
  fi
  info "schema user_version = $uv   namespaces = $total"

  if sq "SELECT 1 FROM sqlite_master WHERE type='table' AND name='config_audit';" | grep -q 1; then
    info "config_audit already exists — expected on any host running a release since the audit trail landed"
  else
    info "config_audit does not exist yet — this host predates the audit trail"
  fi

  head_ "Namespaces"
  local summary_out; summary_out=$(namespace_summary)
  if [ -n "$summary_out" ]; then
    while IFS= read -r line; do info "$line"; done <<< "$summary_out"
  else
    info "(none)"
  fi
  local pub; pub=$(sq "SELECT COUNT(*) FROM config_namespaces WHERE read_role='public';" || echo 0)
  if [ "${pub:-0}" -gt 0 ]; then
    warn "$pub namespace(s) already marked public"
  else
    ok "no namespace is public (nothing becomes world-readable by deploying)"
  fi

  head_ "Disk headroom for the pre-rebuild snapshot"
  local dbdir dbbytes availbytes
  dbdir=$(dirname "$CONFIG_DB")
  dbbytes=$(du -k "$CONFIG_DB" 2>/dev/null | awk '{print $1}')
  availbytes=$(df -Pk "$dbdir" | awk 'NR==2{print $4}')
  info "database $(( dbbytes ))K, free on $dbdir $(( availbytes ))K"
  if [ "$availbytes" -gt $(( dbbytes * 3 )) ]; then
    ok "ample space for the snapshot (>3x database size)"
  elif [ "$availbytes" -gt $(( dbbytes * 2 )) ]; then
    warn "space is adequate but tight (<3x database size)"
  else
    bad "not enough free space for a full-size snapshot — the migration will refuse to proceed"
  fi

  head_ "R2 backups"
  load_env
  if [ -n "${R2_ACCOUNT_ID:-}" ] && [ -n "${R2_BUCKET_NAME:-}" ]; then
    ok "R2 configured (bucket ${R2_BUCKET_NAME})"
    if [ -x "$CONFIG_BIN" ]; then
      local out
      if out=$("$CONFIG_BIN" --list-backups 2>&1); then
        while IFS= read -r l; do info "$l"; done <<< "$(printf '%s' "$out" | head -5)"
        ok "--list-backups succeeded"
      else
        warn "--list-backups failed; the migration still takes its own local snapshot"
        while IFS= read -r l; do info "  $l"; done <<< "$(printf '%s' "$out" | head -3)"
      fi
    else
      warn "binary not executable at $CONFIG_BIN — skipping backup listing"
    fi
  else
    warn "R2 not configured in $CONFIG_ENV_FILE — no offsite backup before the migration"
  fi

  head_ "Recording state for comparison after the deploy"
  if { printf 'total=%s\nuser_version=%s\npublic=%s\ntaken=%s\n' \
         "$total" "$uv" "$pub" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
       sq "SELECT 'ns=' || name || ',' || read_role || ',' || write_role FROM config_namespaces ORDER BY name;"
     } > "$STATE_FILE" 2>/dev/null; then
    chmod 600 "$STATE_FILE" 2>/dev/null || true
    ok "state written to $STATE_FILE"
  else
    warn "could not write $STATE_FILE — 'verify' will skip the before/after comparison"
  fi

  summary "Preflight"
}

cmd_verify() {
  require_root; check_basics; check_service

  head_ "Schema after migration"
  local uv total
  uv=$(sq "PRAGMA user_version;" || echo "?")
  total=$(sq "SELECT COUNT(*) FROM config_namespaces;" || echo "?")

  if schema_permits_public; then ok "read_role CHECK now permits 'public'"
  else bad "read_role CHECK does NOT permit 'public' — the migration did not run"; fi

  if sq "SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces';" | grep -q "write_role IN ('admin', 'user')"; then
    ok "write_role CHECK unchanged (nothing is anonymously writable)"
  else
    warn "could not confirm the write_role CHECK is still narrow"
  fi

  # user_version is a step counter, not a flag: it rises as schema steps are
  # added. Assert it is at least the step this script knows about rather than
  # pinning an exact number, which would fail every future deploy.
  if [ -n "$uv" ] && [ "$uv" -ge "$EXPECTED_SCHEMA_VERSION" ] 2>/dev/null; then
    ok "schema version = $uv (steps recorded; later boots skip them outright)"
  else
    bad "schema version = ${uv:-unknown}, expected at least $EXPECTED_SCHEMA_VERSION — later boots will re-derive the answer"
  fi

  if sq "SELECT sql FROM sqlite_master WHERE type='table' AND name='config_audit';" | grep -q "document_write"; then
    ok "config_audit records document writes, not only ACL changes"
  else
    bad "config_audit.action does not admit 'document_write' — the trail will not record content changes"
  fi

  if sq "SELECT 1 FROM sqlite_master WHERE type='table' AND name='config_audit';" | grep -q 1; then
    ok "config_audit table present"
  else
    bad "config_audit table missing"
  fi

  if sq "SELECT 1 FROM sqlite_master WHERE type='index' AND name='idx_config_namespaces_read_role';" | grep -q 1; then
    ok "idx_config_namespaces_read_role recreated after the rebuild"
  else
    bad "idx_config_namespaces_read_role is missing — the rebuild drops and recreates it"
  fi

  head_ "Data"
  info "namespaces now = $total"
  if [ -r "$STATE_FILE" ]; then
    local before; before=$(awk -F= '/^total=/{print $2}' "$STATE_FILE")
    if [ "$before" = "$total" ]; then ok "namespace count unchanged since preflight ($before)"
    else bad "namespace count changed: $before before, $total now"; fi

    local drift=0
    while IFS= read -r line; do
      case "$line" in ns=*) ;; *) continue ;; esac
      local rec; rec=${line#ns=}
      local name; name=${rec%%,*}
      local nowrec; nowrec=$(sq "SELECT name || ',' || read_role || ',' || write_role FROM config_namespaces WHERE name='${name//\'/\'\'}';")
      if [ "$nowrec" != "$rec" ]; then
        bad "ACL changed for '$name': was '${rec}', now '${nowrec:-<missing>}'"
        drift=1
      fi
    done < "$STATE_FILE"
    [ "$drift" -eq 0 ] && ok "every namespace kept its exact read/write roles"
  else
    warn "no preflight state at $STATE_FILE — skipping before/after comparison"
  fi

  head_ "Pre-rebuild snapshot"
  local snap
  snap=$(ls -1t "${CONFIG_DB}".pre-public-rebuild-* 2>/dev/null | head -1 || true)
  if [ -n "$snap" ]; then
    ok "snapshot present: $(basename "$snap") ($(du -h "$snap" | awk '{print $1}'))"
    if sq_file "$snap" "SELECT 1;" >/dev/null 2>&1; then
      ok "snapshot opens as a valid SQLite database"
      local snaprows
      snaprows=$(sq_file "$snap" "SELECT COUNT(*) FROM config_namespaces;")
      if sq_file "$snap" "SELECT sql FROM sqlite_master WHERE type='table' AND name='config_namespaces';" | grep -q "'public'"; then
        warn "snapshot already permits 'public' — it may post-date the migration"
      else
        ok "snapshot carries the pre-migration schema (it is what you would restore)"
      fi
      info "snapshot holds $snaprows namespace(s)"
      [ "$snaprows" = "$total" ] || warn "snapshot row count ($snaprows) differs from current ($total) — expected if data changed after the deploy"
    else
      bad "snapshot exists but does not open as a database"
    fi
  else
    if [ "$total" = "0" ]; then
      ok "no snapshot, and none expected (the table was empty)"
    else
      warn "no snapshot file found — expected if this host was already migrated before today"
    fi
  fi

  head_ "Journal"
  if command -v journalctl >/dev/null 2>&1; then
    local j; j=$(journalctl -u "$CONFIG_UNIT" -n 200 --no-pager 2>/dev/null | grep "config db:" | tail -5 || true)
    if [ -n "$j" ]; then
      while IFS= read -r l; do info "$l"; done <<< "$j"
      ok "schema lines found in the journal"
    else
      warn "no 'config db:' lines in the last 200 journal entries"
    fi
  else
    warn "journalctl unavailable — skipping"
  fi

  head_ "Behaviour"
  local aprivate
  aprivate=$(sq "SELECT name FROM config_namespaces WHERE read_role <> 'public' LIMIT 1;" || true)
  if [ -n "$aprivate" ]; then
    local code
    code=$(http_code "http://localhost:${CONFIG_PORT}/api/v1/config/${aprivate}")
    if [ "$code" = "404" ]; then
      ok "anonymous read of a private namespace returns 404 (not 401, not 500)"
    elif [ "$code" = "000" ]; then
      warn "could not reach the service to probe '$aprivate' — is it running?"
    else
      bad "anonymous read of '$aprivate' returned $code, expected 404"
    fi
  else
    warn "no private namespace to probe"
  fi
  local code2
  code2=$(http_code "http://localhost:${CONFIG_PORT}/api/v1/config")
  if [ "$code2" = "401" ]; then ok "anonymous list still requires a token (401)"
  elif [ "$code2" = "000" ]; then warn "could not reach the service to probe the list endpoint"
  else bad "anonymous list returned $code2, expected 401"; fi

  summary "Verify"
  printf "\nNext: run '%s restart-check' to confirm a second restart does not rebuild again.\n" "$(basename "$0")"
}

cmd_backups() {
  require_root; need_cmd curl
  load_env
  [ -x "$CONFIG_BIN" ] || die "binary not executable: $CONFIG_BIN"
  if [ -z "${R2_ACCOUNT_ID:-}" ]; then
    die "R2 is not configured in $CONFIG_ENV_FILE"
  fi
  printf "Listing R2 backups (bucket %s)\n\n" "${R2_BUCKET_NAME:-unknown}"
  "$CONFIG_BIN" --list-backups
}

cmd_restart_check() {
  require_root
  command -v systemctl >/dev/null 2>&1 || die "systemctl not available"

  printf "This RESTARTS %s. Brief downtime; no data is modified.\n" "$CONFIG_UNIT"
  printf "It confirms the migration does not run a second time.\n\n"
  printf "Type 'yes' to continue: "
  local answer; read -r answer
  [ "$answer" = "yes" ] || die "aborted at your request — nothing was done"

  local before_uv; before_uv=$(sq "PRAGMA user_version;")
  [ -n "$before_uv" ] || before_uv="?"

  # Mark the journal position with an opaque cursor rather than a timestamp.
  # journalctl interprets --since in LOCAL time, so a UTC timestamp silently
  # reaches an hour into the past on a UTC+1 host and picks up the previous
  # boot's lines — which reads as "the migration ran again".
  local -a jargs=()
  local cur
  cur=$(journalctl -u "$CONFIG_UNIT" -n 0 --show-cursor --no-pager 2>/dev/null | sed -n 's/^-- cursor: //p' || true)
  if [ -n "$cur" ]; then
    jargs=(--after-cursor="$cur")
  else
    jargs=(--since="$(date +'%Y-%m-%d %H:%M:%S')")
  fi

  systemctl restart "$CONFIG_UNIT"
  sleep 3

  head_ "After restart"
  if systemctl is-active --quiet "$CONFIG_UNIT"; then ok "$CONFIG_UNIT restarted and is active"
  else bad "$CONFIG_UNIT is not active after restart"; journalctl -u "$CONFIG_UNIT" -n 20 --no-pager; summary "Restart check"; return; fi

  local lines
  lines=$(journalctl -u "$CONFIG_UNIT" "${jargs[@]}" --no-pager 2>/dev/null | grep "config db:" || true)
  while IFS= read -r l; do info "$l"; done <<< "${lines:-(none)}"

  local rebuilt=0 settled=0
  printf '%s' "$lines" | grep -qE "schema rebuild required|widening .*\.action" && rebuilt=1
  printf '%s' "$lines" | grep -qE "settled|schema brought to version" && settled=1

  if [ "$rebuilt" -eq 1 ]; then
    bad "the migration ran AGAIN on this restart — it should be a no-op"
  elif [ "$settled" -eq 1 ]; then
    ok "restart logged the settled line and did not rebuild"
  else
    warn "no schema line seen for this restart — check the journal manually"
  fi

  local after_uv; after_uv=$(sq "PRAGMA user_version;")
  [ -n "$after_uv" ] || after_uv="?"
  if [ "$after_uv" = "?" ] || [ "$before_uv" = "?" ]; then
    warn "could not read user_version before/after (got '$before_uv' -> '$after_uv')"
  elif [ "$before_uv" = "$after_uv" ]; then
    ok "user_version unchanged ($after_uv)"
  else
    warn "user_version changed: $before_uv -> $after_uv"
  fi

  summary "Restart check"
}

summary() {
  printf "\n──────────────────────────────────────────\n"
  printf "  %s: %d ok, %d warning(s), %d failure(s)\n" "$1" "$PASS" "$WARN" "$FAIL"
  printf "──────────────────────────────────────────\n"
  CLEAN_EXIT=1
  if [ "$FAIL" -gt 0 ]; then
    printf "\nSomething is wrong. Do not proceed until it is understood.\n"
    exit 1
  fi
  [ "$WARN" -gt 0 ] && printf "\nWarnings are not blockers, but read them before continuing.\n"
  exit 0
}

usage() {
  sed -n '2,26p' "$0" | sed 's/^#//; s/^ //'
  CLEAN_EXIT=1
  exit 2
}

case "${1:-}" in
  preflight)     cmd_preflight ;;
  verify)        cmd_verify ;;
  backups)       cmd_backups ;;
  restart-check) cmd_restart_check ;;
  *)             usage ;;
esac
