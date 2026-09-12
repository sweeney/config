#!/usr/bin/env bash
#
# End-to-end test suite for the config service.
#
# Runs against live identity + config servers. Start them first:
#
#   # identity (port 8181)
#   ADMIN_USERNAME=admin ADMIN_PASSWORD=adminpassword1 \
#     DB_PATH=/tmp/e2e-identity.db PORT=8181 \
#     IDENTITY_ENV=development RATE_LIMIT_DISABLED=1 \
#     ./bin/identity-server identity &
#
#   # config (port 8282)
#   DB_PATH=/tmp/e2e-config.db PORT=8282 IDENTITY_ENV=development \
#     RATE_LIMIT_DISABLED=1 IDENTITY_ISSUER_URL=http://localhost:8181 \
#     IDENTITY_ISSUER=http://localhost:8181 ./bin/config-server &
#
# Then:
#   ./scripts/e2e.sh [identity_url] [config_url]
#
# Defaults: identity http://localhost:8181, config http://localhost:8282

ID_BASE="${1:-http://localhost:8181}"
CFG_BASE="${2:-http://localhost:8282}"
ADMIN_USER="${ADMIN_USERNAME:-admin}"
ADMIN_PASS="${ADMIN_PASSWORD:-adminpassword1}"
PASS=0
FAIL=0

check() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "  ✓ $desc"
    PASS=$((PASS+1))
  else
    echo "  ✗ $desc (expected: $expected, got: $actual)"
    FAIL=$((FAIL+1))
  fi
}

check_contains() {
  local desc="$1" needle="$2" haystack="$3"
  if echo "$haystack" | grep -q "$needle"; then
    echo "  ✓ $desc"
    PASS=$((PASS+1))
  else
    echo "  ✗ $desc (expected to contain: $needle)"
    FAIL=$((FAIL+1))
  fi
}

json_field() {
  # $1=json, $2=key — requires python3
  python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('$2',''))" "$1"
}

# ── 1. Service health ─────────────────────────────────────────────────
echo "=== 1. Service health ==="
ID_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$ID_BASE/health")
CFG_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$CFG_BASE/healthz")
check "identity /health returns 200" "200" "$ID_STATUS"
check "config /healthz returns 200" "200" "$CFG_STATUS"

# ── 2. Obtain admin and user tokens from identity ─────────────────────
echo
echo "=== 2. Obtain tokens ==="
ADMIN_LOGIN=$(curl -s -X POST "$ID_BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}")
ADMIN_TOK=$(json_field "$ADMIN_LOGIN" access_token)
if [ -n "$ADMIN_TOK" ]; then
  check "admin login returned access_token" "yes" "yes"
else
  check "admin login returned access_token" "yes" "no"
  echo "admin login response: $ADMIN_LOGIN"
  exit 1
fi

# Create a non-admin user, login as them.
USER_PASS="userpassword1"
USER_NAME="e2e-config-user-$$"
CREATE=$(curl -s -X POST "$ID_BASE/api/v1/users" \
  -H "Authorization: Bearer $ADMIN_TOK" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER_NAME\",\"password\":\"$USER_PASS\",\"display_name\":\"$USER_NAME\",\"role\":\"user\"}")
USER_ID=$(json_field "$CREATE" id)
check_contains "created non-admin user" "$USER_NAME" "$CREATE"

USER_LOGIN=$(curl -s -X POST "$ID_BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER_NAME\",\"password\":\"$USER_PASS\"}")
USER_TOK=$(json_field "$USER_LOGIN" access_token)
if [ -n "$USER_TOK" ]; then
  check "non-admin login returned access_token" "yes" "yes"
else
  check "non-admin login returned access_token" "yes" "no"
  exit 1
fi

# ── 3. Auth boundary ──────────────────────────────────────────────────
echo
echo "=== 3. Auth boundary ==="
check "list without auth = 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' $CFG_BASE/api/v1/config)"
check "list with bad bearer = 401" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer not-a-jwt' $CFG_BASE/api/v1/config)"

# ── 4. Admin creates namespaces (admin-only and user-readable) ────────
echo
echo "=== 4. Admin creates namespaces ==="
R=$(curl -s -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"houses","read_role":"admin","write_role":"admin","document":{"main":"Rivendell","guest":"Hobbiton"}}' \
  -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
check "create admin-only 'houses' = 201" "201" "$STATUS"

R=$(curl -s -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"mqtt","read_role":"user","write_role":"admin","document":{"base":"config"}}' \
  -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
check "create user-readable 'mqtt' = 201" "201" "$STATUS"

# Duplicate name.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"houses","read_role":"admin","write_role":"admin","document":{}}')
check "duplicate create returns 409" "409" "$STATUS"

# Invalid name.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"BAD NAME","read_role":"admin","write_role":"admin","document":{}}')
check "invalid name returns 400" "400" "$STATUS"

# ── 5. Non-admin cannot create ───────────────────────────────────────
echo
echo "=== 5. Non-admin cannot create ==="
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $USER_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"x","read_role":"user","write_role":"user","document":{}}')
check "user create = 403" "403" "$STATUS"

# ── 6. Role-gated reads ──────────────────────────────────────────────
echo
echo "=== 6. Role-gated reads ==="
# Admin reads both.
BODY=$(curl -s -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/houses")
check_contains "admin reads 'houses'" "Rivendell" "$BODY"

BODY=$(curl -s -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/mqtt")
check_contains "admin reads 'mqtt'" "config" "$BODY"

# ACL headers on admin GET.
HEADERS=$(curl -sI -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/houses")
check_contains "GET 'houses' includes X-Read-Role: admin"  "X-Read-Role: admin"  "$HEADERS"
check_contains "GET 'houses' includes X-Write-Role: admin" "X-Write-Role: admin" "$HEADERS"

HEADERS=$(curl -sI -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/mqtt")
check_contains "GET 'mqtt' includes X-Read-Role: user"   "X-Read-Role: user"  "$HEADERS"
check_contains "GET 'mqtt' includes X-Write-Role: admin" "X-Write-Role: admin" "$HEADERS"

# User reads mqtt (read_role=user) but is 404 on houses (read_role=admin).
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $USER_TOK" "$CFG_BASE/api/v1/config/mqtt")
check "user reads 'mqtt' = 200" "200" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $USER_TOK" "$CFG_BASE/api/v1/config/houses")
check "user on 'houses' = 404 (no 403 leak)" "404" "$STATUS"

# ── 7. Role-gated writes ─────────────────────────────────────────────
echo
echo "=== 7. Role-gated writes ==="
# User tries to write mqtt (write_role=admin) → 403 (they can read it).
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $USER_TOK" -H 'Content-Type: application/json' \
  -d '{"base":"hijack"}')
check "user PUT on readable+admin-write 'mqtt' = 403" "403" "$STATUS"

# User tries to write houses (not readable) → 404.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT "$CFG_BASE/api/v1/config/houses" \
  -H "Authorization: Bearer $USER_TOK" -H 'Content-Type: application/json' \
  -d '{"main":"hijack"}')
check "user PUT on hidden 'houses' = 404 (no existence leak)" "404" "$STATUS"

# Admin PUT succeeds.
R=$(curl -s -X PUT "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"base":"config","broker":"192.168.1.10"}' \
  -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
check "admin PUT on 'mqtt' = 200" "200" "$STATUS"
check_contains "admin PUT returned changed=true" '"changed":true' "$R"

# No-op PUT.
R=$(curl -s -X PUT "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"base":"config","broker":"192.168.1.10"}' \
  -w '\n%{http_code}')
check_contains "no-op PUT returned changed=false" '"changed":false' "$R"

# Malformed body.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PUT "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '[1,2,3]')
check "PUT non-object body = 400" "400" "$STATUS"

# ── 8. List visibility ───────────────────────────────────────────────
echo
echo "=== 8. List visibility ==="
ADMIN_LIST=$(curl -s -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config")
USER_LIST=$(curl -s -H "Authorization: Bearer $USER_TOK" "$CFG_BASE/api/v1/config")
# Document writes are not audited, so updated_by is the only record of who
# last changed a namespace's contents — and until now it was stored but never
# returned by any endpoint.
check_contains "list says who last wrote each namespace" '"updated_by"' "$ADMIN_LIST"
check_contains "and names them, not just their id" "\"updated_by_username\":\"$ADMIN_USER\"" "$ADMIN_LIST"
# Deliberately on the authenticated list and not on the namespace GET, which
# may be anonymous for a public namespace.
check "who wrote it is not disclosed anonymously" "401" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config")"

check_contains "admin sees 'houses'" "houses" "$ADMIN_LIST"
check_contains "admin sees 'mqtt'" "mqtt" "$ADMIN_LIST"
check_contains "user sees 'mqtt'" "mqtt" "$USER_LIST"
if echo "$USER_LIST" | grep -q "houses"; then
  echo "  ✗ user must NOT see 'houses' in list"
  FAIL=$((FAIL+1))
else
  echo "  ✓ user does not see 'houses' in list"
  PASS=$((PASS+1))
fi

# ── 9. Update ACL ────────────────────────────────────────────────────
echo
echo "=== 9. Update ACL ==="
# User cannot PATCH.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/mqtt" \
  -H "Authorization: Bearer $USER_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"user","write_role":"user"}')
check "user PATCH ACL = 403" "403" "$STATUS"

# Admin can PATCH.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/mqtt" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}')
check "admin PATCH ACL = 200" "200" "$STATUS"

# After the ACL change, user no longer sees mqtt.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $USER_TOK" "$CFG_BASE/api/v1/config/mqtt")
check "after ACL tighten, user GET 'mqtt' = 404" "404" "$STATUS"

# ── 10. Delete ───────────────────────────────────────────────────────
echo
echo "=== 10. Delete ==="
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $USER_TOK")
check "user DELETE = 403" "403" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/mqtt" \
  -H "Authorization: Bearer $ADMIN_TOK")
check "admin DELETE = 204" "204" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/mqtt")
check "GET after DELETE = 404" "404" "$STATUS"

# ── 11. Public namespaces (anonymous reads) ──────────────────────────
echo
echo "=== 11. Public namespaces ==="

# Publishing at create time requires confirm_public to echo the namespace name.
R=$(curl -s -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"tariffs","read_role":"public","write_role":"user","document":{"unit":0.24}}' \
  -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
BODY=$(echo "$R" | sed '$d')
check "create public without confirm_public = 400" "400" "$STATUS"
check_contains "rejection names confirm_required" "confirm_required" "$BODY"

# A confirmation for a different namespace must not work — that is the whole
# point of binding it to the name rather than using a boolean.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"tariffs","read_role":"public","write_role":"user","document":{},"confirm_public":"houses"}')
check "create public with mismatched confirm = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"tariffs","read_role":"public","write_role":"user","document":{"unit":0.24},"confirm_public":"tariffs"}')
check "create public with confirm_public = 201" "201" "$STATUS"

# public is never a valid write role, confirmation or not.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"nope","read_role":"public","write_role":"public","document":{},"confirm_public":"nope"}')
check "public write_role rejected = 400" "400" "$STATUS"

# The anonymous read — no Authorization header at all. This is the whole
# feature, and it is the one thing only a live stack can prove.
R=$(curl -s "$CFG_BASE/api/v1/config/tariffs" -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
BODY=$(echo "$R" | sed '$d')
check "anonymous GET of public namespace = 200" "200" "$STATUS"
check_contains "anonymous read returns the document" "0.24" "$BODY"

HDRS=$(curl -s -D - -o /dev/null "$CFG_BASE/api/v1/config/tariffs")
check_contains "public namespace is shared-cacheable" "max-age=60" "$HDRS"
# Asserted on the header's value, not on the literal line: the CORS
# middleware sets Vary: Origin before any handler runs, so the real header is
# "Origin, Authorization" and matching the whole line would miss it. Both
# values must survive — dropping Origin would let a shared cache serve a
# response to an origin it was not built for.
VARY=$(echo "$HDRS" | tr -d '\r' | awk -F': ' 'tolower($1)=="vary"{print $2}')
check_contains "Vary lists Authorization" "Authorization" "$VARY"
check_contains "Vary still lists Origin from the CORS middleware" "Origin" "$VARY"

# Matched exactly rather than with check_contains: "Origin: *" as a grep
# pattern means "Origin:" followed by any number of spaces, which would pass
# whether or not the asterisk were there.
acao() {
  curl -s -D - -o /dev/null "$@" | tr -d '\r' \
    | awk -F': ' 'tolower($1)=="access-control-allow-origin"{print $2}'
}
check "public read allows any origin" "*" "$(acao "$CFG_BASE/api/v1/config/tariffs")"

# A preflight must also succeed, or the wildcard only holds for simple GETs
# and any client sending Content-Type is refused before the read happens.
PF=$(curl -s -D - -o /dev/null -X OPTIONS "$CFG_BASE/api/v1/config/tariffs" \
  -H 'Origin: https://unknown.example' -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: content-type' | tr -d '\r')
check "preflight from an unknown origin allows any origin" "*" \
  "$(echo "$PF" | awk -F': ' 'tolower($1)=="access-control-allow-origin"{print $2}')"
check_contains "preflight allows Content-Type" "Content-Type" \
  "$(echo "$PF" | awk -F': ' 'tolower($1)=="access-control-allow-headers"{print $2}')"
check_contains "preflight exposes the role headers" "X-Read-Role" \
  "$(echo "$PF" | awk -F': ' 'tolower($1)=="access-control-expose-headers"{print $2}')"

# The wildcard is for anonymous reads; a token-bearing cross-origin request
# still goes through CORS_ORIGINS.
ACH=$(echo "$PF" | awk -F': ' 'tolower($1)=="access-control-allow-headers"{print $2}')
if echo "$ACH" | grep -qi "authorization"; then
  check "preflight does not advertise Authorization" "absent" "present"
else
  check "preflight does not advertise Authorization" "absent" "absent"
fi

# A 404 is cacheable by default and is the only answer an anonymous caller
# gets for a private namespace, so it must not be storable without regard to
# Authorization.
NF=$(curl -s -D - -o /dev/null "$CFG_BASE/api/v1/config/houses" | tr -d '\r')
check "anonymous 404 is not cacheable" "private, no-store" \
  "$(echo "$NF" | awk -F': ' 'tolower($1)=="cache-control"{print $2}')"
check_contains "anonymous 404 varies on Authorization" "Authorization" \
  "$(echo "$NF" | awk -F': ' 'tolower($1)=="vary"{print $2}')"

# Only the answer that actually was anonymous is shared-cacheable.
AUTHPUB=$(curl -s -D - -o /dev/null "$CFG_BASE/api/v1/config/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK" | tr -d '\r')
check "authenticated read of a public namespace is not shared-cacheable" "private, no-store" \
  "$(echo "$AUTHPUB" | awk -F': ' 'tolower($1)=="cache-control"{print $2}')"

# The write role is not disclosed to callers who cannot act on it.
check "anonymous read is not told the write role" "" \
  "$(curl -s -D - -o /dev/null "$CFG_BASE/api/v1/config/tariffs" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-write-role"{print $2}')"
check "authenticated read still gets the write role" "user" \
  "$(echo "$AUTHPUB" | awk -F': ' 'tolower($1)=="x-write-role"{print $2}')"
check "private read does not" "" \
  "$(acao -H "Authorization: Bearer $ADMIN_TOK" "$CFG_BASE/api/v1/config/houses")"

# A private namespace and a nonexistent one must be indistinguishable, or the
# 404 becomes an existence oracle for anonymous callers.
PRIV=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config/houses")
MISS=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config/nosuchnamespace")
check "anonymous GET of private namespace = 404" "404" "$PRIV"
check "anonymous GET of missing namespace = 404" "404" "$MISS"

# A token that was presented but does not verify is never downgraded to
# anonymous, even where anonymous would have succeeded.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer not-a-real-token" "$CFG_BASE/api/v1/config/tariffs")
check "broken token on a public namespace = 401" "401" "$STATUS"

# A public read role must not open any other route.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config")
check "anonymous list still = 401" "401" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$CFG_BASE/api/v1/config/tariffs" \
  -H 'Content-Type: application/json' -d '{"unit":9.99}')
check "anonymous PUT to a public namespace = 401" "401" "$STATUS"

# Publishing an existing namespace through PATCH. Uses its own namespace:
# 'mqtt' was deleted back in section 10, and reusing it made these checks
# pass or fail on a missing namespace rather than on the publish guard.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"feeds","read_role":"user","write_role":"admin","document":{"poll":30}}')
check "create private 'feeds' for the publish flow = 201" "201" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/feeds" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"public","write_role":"admin"}')
check "publish via PATCH without confirm = 400" "400" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config/feeds")
check "refused publish left it unreadable = 404" "404" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/feeds" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"public","write_role":"admin","confirm_public":"feeds"}')
check "publish via PATCH with confirm = 200" "200" "$STATUS"
R=$(curl -s "$CFG_BASE/api/v1/config/feeds" -w '\n%{http_code}')
check "anonymous read after publish = 200" "200" "$(echo "$R" | tail -n1)"
check_contains "published document is served anonymously" "30" "$(echo "$R" | sed '$d')"

# Revoking is an ordinary edit — no confirmation — and closes the anonymous
# read at the origin immediately. (Caches downstream keep serving until the
# max-age above expires; that is the real revoke latency.)
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/feeds" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"user","write_role":"admin"}')
check "revoke without confirm = 200" "200" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config/feeds")
check "anonymous read after revoke = 404" "404" "$STATUS"

# Locking a user-writable public namespace all the way down needs the write
# role raised in the same call, or it violates writers-are-readers.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"user"}')
check "public -> admin read with user write = 400" "400" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X PATCH "$CFG_BASE/api/v1/config/namespaces/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"read_role":"admin","write_role":"admin"}')
check "public -> admin read with admin write = 200" "200" "$STATUS"

# ── 12. Audit trail ──────────────────────────────────────────────────
echo
echo "=== 12. Audit trail ==="

# 'tariffs' was created public and then locked down to admin, so its history
# is exactly the transition an operator would come here looking for.
R=$(curl -s "$CFG_BASE/api/v1/config/namespaces/tariffs/audit" \
  -H "Authorization: Bearer $ADMIN_TOK" -w '\n%{http_code}')
STATUS=$(echo "$R" | tail -n1)
BODY=$(echo "$R" | sed '$d')
check "admin GET audit = 200" "200" "$STATUS"
check_contains "history records the create" '"action":"create"' "$BODY"
check_contains "history records the ACL change" '"action":"acl_change"' "$BODY"
check_contains "history records the role it moved away from" '"old_read_role":"public"' "$BODY"
check_contains "history records who did it" '"actor"' "$BODY"
# Content changes are recorded too, as events — never the body.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$CFG_BASE/api/v1/config/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"unit":0.99,"note":"e2e-secret-value"}')
check "document write accepted" "200" "$STATUS"
R2=$(curl -s "$CFG_BASE/api/v1/config/namespaces/tariffs/audit" -H "Authorization: Bearer $ADMIN_TOK")
check_contains "history records the document write" '"action":"document_write"' "$R2"
if echo "$R2" | grep -q "e2e-secret-value"; then
  check "document bodies are never stored in the trail" "absent" "PRESENT"
else
  check "document bodies are never stored in the trail" "absent" "absent"
fi
# A write that changes nothing is not an event.
BEFORE=$(echo "$R2" | grep -o '"action"' | wc -l | tr -d ' ')
curl -s -o /dev/null -X PUT "$CFG_BASE/api/v1/config/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"unit":0.99,"note":"e2e-secret-value"}'
AFTER=$(curl -s "$CFG_BASE/api/v1/config/namespaces/tariffs/audit" -H "Authorization: Bearer $ADMIN_TOK" | grep -o '"action"' | wc -l | tr -d ' ')
check "a no-op write adds no history entry" "$BEFORE" "$AFTER"
# The username is recorded at write time from the token, so the trail names
# who acted without a later lookup against identity — which would not work
# during an outage, nor after a rename.
check_contains "history names the actor, not just their id" "\"actor_username\":\"$ADMIN_USER\"" "$BODY"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  "$CFG_BASE/api/v1/config/namespaces/tariffs/audit" \
  -H "Authorization: Bearer $USER_TOK")
check "user GET audit = 403" "403" "$STATUS"

STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  "$CFG_BASE/api/v1/config/namespaces/tariffs/audit")
check "anonymous GET audit = 401" "401" "$STATUS"

# Publishing a document does not publish its history: a namespace anyone can
# read is still admin-only on the trail.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X POST "$CFG_BASE/api/v1/config/namespaces" \
  -H "Authorization: Bearer $ADMIN_TOK" -H 'Content-Type: application/json' \
  -d '{"name":"pubaudit","read_role":"public","write_role":"admin","document":{"a":1},"confirm_public":"pubaudit"}')
check "create public 'pubaudit' = 201" "201" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' "$CFG_BASE/api/v1/config/pubaudit")
check "its document reads anonymously = 200" "200" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  "$CFG_BASE/api/v1/config/namespaces/pubaudit/audit")
check "its history does not = 401" "401" "$STATUS"

# The trail outlives the namespace — that is the whole point of it having no
# foreign key.
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/pubaudit" -H "Authorization: Bearer $ADMIN_TOK")
check "deleted 'pubaudit' = 204" "204" "$STATUS"
R=$(curl -s "$CFG_BASE/api/v1/config/namespaces/pubaudit/audit" \
  -H "Authorization: Bearer $ADMIN_TOK" -w '\n%{http_code}')
check "history survives the delete = 200" "200" "$(echo "$R" | tail -n1)"
check_contains "and records the deletion" '"action":"delete"' "$(echo "$R" | sed '$d')"

# A namespace that never existed is an empty history, not a 404 — asking
# about one that is gone is the reason this endpoint exists.
R=$(curl -s "$CFG_BASE/api/v1/config/namespaces/neverexisted/audit" \
  -H "Authorization: Bearer $ADMIN_TOK" -w '\n%{http_code}')
check "unknown namespace audit = 200" "200" "$(echo "$R" | tail -n1)"
check "unknown namespace audit is an empty array" "[]" "$(echo "$R" | sed '$d')"

# ── 13. SPA logout revokes the refresh token ──────────────────────────
# Regression: the SPA used to call /api/v1/auth/logout without an
# Authorization header — silently 401'd. This test uses the same call
# shape the SPA now uses and asserts the refresh token is dead server-side.
echo
echo "=== 13. SPA logout revokes refresh token ==="
LOGOUT_LOGIN=$(curl -s -X POST "$ID_BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}")
LOGOUT_ACCESS=$(json_field "$LOGOUT_LOGIN" access_token)
LOGOUT_REFRESH=$(json_field "$LOGOUT_LOGIN" refresh_token)
if [ -n "$LOGOUT_ACCESS" ] && [ -n "$LOGOUT_REFRESH" ]; then
  check "fresh login returned access + refresh tokens" "yes" "yes"
else
  check "fresh login returned access + refresh tokens" "yes" "no"
  echo "login response: $LOGOUT_LOGIN"
  exit 1
fi

# Pre-check refresh works, capturing the rotated pair (identity rotates on every use).
ROTATED=$(curl -s -X POST "$ID_BASE/api/v1/auth/refresh" \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$LOGOUT_REFRESH\"}")
LOGOUT_ACCESS=$(json_field "$ROTATED" access_token)
LOGOUT_REFRESH=$(json_field "$ROTATED" refresh_token)
if [ -n "$LOGOUT_ACCESS" ] && [ -n "$LOGOUT_REFRESH" ]; then
  check "refresh works before logout" "yes" "yes"
else
  check "refresh works before logout" "yes" "no"
  echo "refresh response: $ROTATED"
  exit 1
fi

# Logout the SPA way — Authorization: Bearer + refresh_token in body.
LOGOUT_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$ID_BASE/api/v1/auth/logout" \
  -H "Authorization: Bearer $LOGOUT_ACCESS" \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$LOGOUT_REFRESH\"}")
check "SPA logout call accepted" "204" "$LOGOUT_STATUS"

POST_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$ID_BASE/api/v1/auth/refresh" \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$LOGOUT_REFRESH\"}")
check "refresh fails after logout (token revoked)" "401" "$POST_STATUS"

# ── 14. Cleanup ──────────────────────────────────────────────────────
echo
echo "=== 14. Cleanup ==="
if [ -n "$USER_ID" ]; then
  STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
    -X DELETE "$ID_BASE/api/v1/users/$USER_ID" \
    -H "Authorization: Bearer $ADMIN_TOK")
  check "deleted e2e user" "204" "$STATUS"
fi
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/houses" \
  -H "Authorization: Bearer $ADMIN_TOK")
check "deleted 'houses' namespace" "204" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/tariffs" \
  -H "Authorization: Bearer $ADMIN_TOK")
check "deleted 'tariffs' namespace" "204" "$STATUS"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' \
  -X DELETE "$CFG_BASE/api/v1/config/feeds" \
  -H "Authorization: Bearer $ADMIN_TOK")
check "deleted 'feeds' namespace" "204" "$STATUS"

# ── Summary ──────────────────────────────────────────────────────────
echo
echo "════════════════════════════"
echo "  PASS: $PASS   FAIL: $FAIL"
echo "════════════════════════════"
[ "$FAIL" -eq 0 ]
