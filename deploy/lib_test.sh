#!/usr/bin/env bash
#
# Tests for deploy/lib.sh.
#
# Usage: deploy/lib_test.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

pass=0 fail=0
ok() { echo "ok    $1"; pass=$((pass + 1)); }
no() { echo "FAIL  $1"; shift; printf '      %s\n' "$@"; fail=$((fail + 1)); }
# eq NAME GOT WANT
eq() { if [ "$2" = "$3" ]; then ok "$1"; else no "$1" "got:  $2" "want: $3"; fi; }

# env_of FILE VAR prints VAR after load_env_file FILE, in a fresh shell, or
# "rc=N: output" if loading failed.
env_of() {
  local out rc=0
  out=$(bash -c '. "$1"; load_env_file "$2"; printf "%s" "${!3-<unset>}"' _ "$HERE/lib.sh" "$1" "$2" 2>&1) || rc=$?
  if [ "$rc" -eq 0 ]; then printf '%s' "$out"; else printf 'rc=%d: %s' "$rc" "$out"; fi
}

# ── load_env_file ─────────────────────────────────────────────────────────────
marker="$WORK/ran"
cat >"$WORK/env" <<EOF
# a comment
; another comment

IDENTITY_ENV=production
CORS_ORIGINS=https://a.example https://b.example
R2_SECRET=ab\$cd\`touch $marker\`ef
QUOTED="two words"
MIDWORD=abc"def"
SINGLE='\$HOME stays'
  SPACED  =  padded value
EMPTY=
EOF
eq "a plain value"                               "$(env_of "$WORK/env" IDENTITY_ENV)" "production"
eq "an unquoted space is part of the value, not a command" "$(env_of "$WORK/env" CORS_ORIGINS)" "https://a.example https://b.example"
eq "\$ and backticks stay literal"                "$(env_of "$WORK/env" R2_SECRET)" "ab\$cd\`touch $marker\`ef"
if [ -e "$marker" ]; then no "a backtick in a value is never run" "$marker exists"; else ok "a backtick in a value is never run"; fi
eq "double quotes are unwrapped"                 "$(env_of "$WORK/env" QUOTED)" "two words"
eq "quotes inside an unquoted word are kept, as systemd does" "$(env_of "$WORK/env" MIDWORD)" 'abc"def"'
# shellcheck disable=SC2016 # the literal $HOME is the point
eq "single quotes are unwrapped, nothing expanded" "$(env_of "$WORK/env" SINGLE)" '$HOME stays'
eq "whitespace around key and value is trimmed" "$(env_of "$WORK/env" SPACED)" "padded value"
eq "an empty value is set, and empty"            "$(env_of "$WORK/env" EMPTY)" ""

printf 'GOOD=1\nnot a pair\n' >"$WORK/bad1"
got=$(env_of "$WORK/bad1" GOOD)
case "$got" in rc=2*"line 2: not KEY=VALUE"*) ok "a line without = is refused, naming the line" ;; *) no "a line without = is refused, naming the line" "$got" ;; esac

printf 'BAD-NAME=s3cr3t\n' >"$WORK/bad2"
got=$(env_of "$WORK/bad2" X)
case "$got" in *s3cr3t*) no "an error never prints the value" "$got" ;; rc=2*"not a variable name"*) ok "a bad name is refused without printing the value" ;; *) no "a bad name is refused" "$got" ;; esac

printf 'LONG=first \\\n' >"$WORK/bad3"
got=$(env_of "$WORK/bad3" LONG)
case "$got" in rc=2*"continuation"*) ok "a continuation line is refused, not misread" ;; *) no "a continuation line is refused" "$got" ;; esac

# Where systemd's parsing differs from a plain reading (each case checked
# against systemd 257), the line is refused rather than misread.
refused() { # refused NAME LINE
  printf '%s\n' "$2" >"$WORK/refuse"
  local got
  got=$(env_of "$WORK/refuse" X)
  case "$got" in
    *s3cr3t*) no "$1" "printed the value: $got" ;;
    rc=2*"line 1: "*) ok "$1" ;;
    *) no "$1" "got: $got" ;;
  esac
}
# shellcheck disable=SC2016 # literal $, backticks and backslashes are the point
{
  refused 'refuses \$ in double quotes (systemd unescapes it)'        'X="s3cr3t\$y"'
  refused 'refuses \` in double quotes (systemd unescapes it)'        'X="s3cr3t\`y"'
  refused 'refuses \" and \\ in double quotes'                        'X="s3cr3t \"hi\" \\"'
  refused 'refuses a backslash in an unquoted value (systemd drops it)' 'X=s3cr3t\b'
  refused 'refuses adjacent double-quoted pieces (systemd joins them)' 'X="s3cr3t" "cd"'
  refused 'refuses adjacent single-quoted pieces'                     "X='s3cr3t' 'cd'"
  refused 'refuses a quote that is opened and not closed'             'X="s3cr3t'
}

printf 'NO_NEWLINE=last' >"$WORK/nonl"
eq "a last line without a newline is read" "$(env_of "$WORK/nonl" NO_NEWLINE)" "last"

# ── listing_env ───────────────────────────────────────────────────────────────
# shellcheck source=deploy/lib.sh
. "$HERE/lib.sh"
eq "listing_env reads the header"        "$(listing_env $'Listing config backups for environment: development\n\n#  Date')" "development"
eq "listing_env is empty with no header" "$(listing_env 'No backups found.')" ""

# ── usage_from_header ─────────────────────────────────────────────────────────
printf '#!/usr/bin/env bash\n#\n# Usage: thing\n#   more\n\nset -euo pipefail\n# not header\n' >"$WORK/script"
got=$( (usage_from_header "$WORK/script") 2>&1) || true
eq "usage stops at the end of the header" "$got" $'\nUsage: thing\n  more'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
