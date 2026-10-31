# shellcheck shell=bash
#
# Shared helpers for the deploy/ scripts that run on the host with sudo
# (check-backups.sh, restore-backup.sh). Sourced, not run.
#
# The dev-machine form of each script copies itself and this file into a
# private directory on the host with run_on_host, so a script finds this file
# beside it in both places:
#
#   . "$(cd "$(dirname "$0")" && pwd)/lib.sh"

LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { printf "\nERROR: %s\n" "$*" >&2; exit 2; }

# usage_from_header prints a script's header comment, from line 2 to the
# first line that is not a comment, and exits 2.
usage_from_header() {
  sed -n '2,/^[^#]/{/^#/p;}' "$1" | sed 's/^#//; s/^ //'
  exit 2
}

# require_root refuses to go on unless running as root. SKIP_ROOT_CHECK=1
# bypasses it, for the tests only.
require_root() {
  if [ "${SKIP_ROOT_CHECK:-0}" != "1" ]; then
    [ "$(id -u)" -eq 0 ] || die "must run as root (use sudo): $1"
  fi
}

# load_env_file exports the KEY=VALUE lines of a systemd EnvironmentFile,
# without evaluating them, so each variable gets what the service gets.
# Sourcing the file as bash would not: an unquoted space makes the rest of
# the line a command, run as root, and $ or backticks in a value (an R2
# secret, say) would be expanded where systemd keeps them literally.
#
# Handled as systemd does: blank lines and lines starting with # or ; are
# skipped; whitespace around the key and value is trimmed; a value wrapped
# in one pair of double or single quotes is unwrapped. Where systemd does
# something a plain reading would not, the line is refused rather than
# misread (checked against systemd 257): any backslash (systemd unescapes
# \$, \`, \" and \\ in double quotes, drops a lone one unquoted, and joins
# lines ending in one), a quote inside a quoted value (systemd joins
# adjacent quoted pieces), and a quote that is opened and not closed.
# Errors name the line, never the value, which may be a secret.
load_env_file() {
  local file=$1 line key value n=0
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    line="${line#"${line%%[![:space:]]*}"}"
    case "$line" in '' | '#'* | ';'*) continue ;; esac
    case "$line" in *=*) ;; *) die "$file line $n: not KEY=VALUE" ;; esac
    case "$line" in *\\) die "$file line $n: continuation lines are not supported" ;; esac
    key="${line%%=*}"
    key="${key%"${key##*[![:space:]]}"}"
    [[ $key =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || die "$file line $n: not a variable name"
    value="${line#*=}"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    case "$value" in *\\*) die "$file line $n: backslashes are not supported (systemd would unescape them)" ;; esac
    local q="${value:0:1}"
    if [ "$q" = '"' ] || [ "$q" = "'" ]; then
      local inner="${value:1}"
      if [ "${#value}" -lt 2 ] || [ "${value: -1}" != "$q" ]; then
        die "$file line $n: a quote is opened and not closed"
      fi
      inner="${inner%"$q"}"
      case "$inner" in *"$q"*) die "$file line $n: quoted pieces are not supported (systemd would join them)" ;; esac
      value=$inner
    fi
    export "$key=$value"
  done <"$file"
}

# listing_env prints the environment a --list-backups listing is for, from
# its header. That is the environment the binary resolved, which is not
# always IDENTITY_ENV as written: the binary maps anything but "production"
# to "development".
listing_env() {
  sed -n 's/^Listing [^ ]* backups for environment: \([^ ]*\).*/\1/p' <<<"$1" | head -1
}

# run_on_host REMOTE SCRIPT ARGS... copies SCRIPT and this file into a
# private mktemp -d directory on REMOTE, runs SCRIPT there with sudo and
# ARGS, removes the directory whatever the outcome, and returns SCRIPT's exit
# code. mktemp -d gives a directory only this user can write, with an
# unpredictable name, so nothing else on the host can swap what is about to
# run as root.
run_on_host() {
  local remote=$1 script=$2 base dir args rc=0
  shift 2
  base=$(basename "$script")
  for c in ssh scp; do
    command -v "$c" >/dev/null || die "required command not found: $c"
  done
  # shellcheck disable=SC2029 # expanded here on purpose: the name is ours
  dir=$(ssh "$remote" "mktemp -d /tmp/${base%.sh}.XXXXXXXX") || die "could not create a temporary directory on $remote"
  case "$dir" in
    "/tmp/${base%.sh}."*) ;;
    *) die "unexpected temporary directory from $remote: $dir" ;;
  esac
  scp -q "$script" "$LIB_DIR/lib.sh" "$remote:$dir/" || die "could not copy the script to $remote"
  args=$(printf ' %q' "$@")
  ssh -t "$remote" "sudo bash $dir/$base$args; rc=\$?; rm -rf $dir; exit \$rc" || rc=$?
  return "$rc"
}

# print_run_on_host REMOTE SCRIPT ARGS... prints what run_on_host would do.
print_run_on_host() {
  local remote=$1 script=$2 base
  shift 2
  base=$(basename "$script")
  printf "would run:\n  ssh %q mktemp -d /tmp/%s.XXXXXXXX    # -> DIR\n" "$remote" "${base%.sh}"
  printf "  scp %q %q %q\n" "$script" "$LIB_DIR/lib.sh" "$remote:DIR/"
  printf "  ssh -t %q sudo bash DIR/%s%s; rm -rf DIR\n" "$remote" "$base" "$(printf ' %q' "$@")"
}
