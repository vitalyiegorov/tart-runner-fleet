#!/bin/sh
set -eu

# render-systemd.sh is the Linux twin of launchd/render-launchd.sh: it renders
# a node's `systemd --user` units from the release being installed, so no
# operator ever hand-edits a unit and no generation runs a file that came from
# somewhere other than its own release directory.
#
# It renders all three services of the node at once — the controller for the
# selected mode, the five-minute updater with its timer, and the updater
# handoff — because the two updater units carry release-specific paths and would
# otherwise have to be written by hand at cutover.

usage() {
  printf 'usage: render-systemd.sh MODE RELEASE_DIR STATE_DIR OUTPUT_DIR [CANARY_SCOPE CANARY_PROFILE]\n' >&2
  exit 2
}

if [ "$#" -lt 4 ]; then
  usage
fi

mode=$1
release_dir=$2
state_dir=$3
output_dir=$4
shift 4

case "$mode" in
  observe)
    unit_name=tart-runner-fleet.service
    socket_name=fleetd.sock
    [ "$#" -eq 0 ] || usage
    ;;
  shadow)
    unit_name=tart-runner-fleet-shadow.service
    socket_name=fleet-shadow.sock
    [ "$#" -eq 0 ] || usage
    ;;
  authority)
    unit_name=tart-runner-fleet-authority.service
    socket_name=fleetd.sock
    [ "$#" -eq 0 ] || usage
    ;;
  canary)
    unit_name=tart-runner-fleet-canary.service
    socket_name=fleet-canary.sock
    [ "$#" -eq 2 ] || usage
    canary_scope=$1
    canary_profile=$2
    ;;
  *) usage ;;
esac

# A rendered unit is executed by the user's service manager with no shell in
# between, but systemd expands its own `%` specifiers and `$` variables inside
# ExecStart, and splits unquoted words. Every argument in the templates is
# quoted, so refusing these characters is what makes that quoting unescapable.
validate_path() {
  value=$1
  case "$value" in
    /*) ;;
    *) printf 'systemd paths must be absolute\n' >&2; exit 2 ;;
  esac
  case "$value" in
    *'"'*|*'\'*|*'$'*|*'%'*|*';'*|*'
'*) printf 'systemd paths contain an unsupported unit-file metacharacter\n' >&2; exit 2 ;;
  esac
}

validate_selector() {
  value=$1
  if ! printf '%s\n' "$value" | LC_ALL=C grep -Eq '^[A-Za-z0-9._-]+$'; then
    printf 'canary selectors must use only letters, digits, dot, underscore, and hyphen\n' >&2
    exit 2
  fi
}

validate_path "$release_dir"
validate_path "$state_dir"
validate_path "$output_dir"
if [ "$mode" = canary ]; then
  validate_selector "$canary_scope"
  validate_selector "$canary_profile"
else
  canary_scope=''
  canary_profile=''
fi

# The immutable root is the grandparent of the release directory, which is the
# layout every install creates: $ROOT/releases/$VERSION. Deriving it keeps the
# argument list the same as the launchd renderer's; asserting the shape keeps a
# typo from silently pointing the updater at the wrong tree.
releases_dir=$(dirname -- "$release_dir")
if [ "$(basename -- "$releases_dir")" != releases ]; then
  printf 'release directory must live in <root>/releases/<version>: %s\n' "$release_dir" >&2
  exit 2
fi
root_dir=$(dirname -- "$releases_dir")
validate_path "$root_dir"

units_dir=${FLEET_UNITS_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user}
validate_path "$units_dir"
repository=${FLEET_RELEASE_REPOSITORY:-vitalyiegorov/tart-runner-fleet}
if ! printf '%s\n' "$repository" | LC_ALL=C grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$'; then
  printf 'release repository must be owner/name\n' >&2
  exit 2
fi
endpoint="unix://$state_dir/$socket_name"
interval=5m0s

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ ! -d "$output_dir" ]; then
  printf 'systemd output directory does not exist: %s\n' "$output_dir" >&2
  exit 2
fi

render() {
  template="$script_dir/$1"
  output="$output_dir/$2"
  if [ ! -f "$template" ]; then
    printf 'systemd template is missing: %s\n' "$template" >&2
    exit 2
  fi
  temporary=$(mktemp "$output.tmp.XXXXXX")
  # shellcheck disable=SC2064 -- expand the path now, while it is still in scope.
  trap "rm -f '$temporary'" EXIT HUP INT TERM
  LC_ALL=C awk \
    -v release_dir="$release_dir" \
    -v state_dir="$state_dir" \
    -v root_dir="$root_dir" \
    -v units_dir="$units_dir" \
    -v repository="$repository" \
    -v endpoint="$endpoint" \
    -v mode="$mode" \
    -v interval="$interval" \
    -v canary_scope="$canary_scope" \
    -v canary_profile="$canary_profile" '
    {
      gsub(/__RELEASE_DIR__/, release_dir)
      gsub(/__STATE_DIR__/, state_dir)
      gsub(/__ROOT__/, root_dir)
      gsub(/__UNITS_DIR__/, units_dir)
      gsub(/__REPOSITORY__/, repository)
      gsub(/__ENDPOINT__/, endpoint)
      gsub(/__MODE__/, mode)
      gsub(/__INTERVAL__/, interval)
      gsub(/__CANARY_SCOPE__/, canary_scope)
      gsub(/__CANARY_PROFILE__/, canary_profile)
      print
    }
  ' "$template" > "$temporary"
  if LC_ALL=C grep -q '__' "$temporary"; then
    printf 'rendered unit retains a template placeholder: %s\n' "$output" >&2
    exit 1
  fi
  if ! LC_ALL=C grep -q '^\[Unit\]$' "$temporary"; then
    printf 'rendered unit is not a systemd unit file: %s\n' "$output" >&2
    exit 1
  fi
  chmod 0600 "$temporary"
  mv -f "$temporary" "$output"
  trap - EXIT HUP INT TERM
}

render "$unit_name" "$unit_name"
render tart-runner-fleet-updater.service tart-runner-fleet-updater.service
render tart-runner-fleet-updater.timer tart-runner-fleet-updater.timer
render tart-runner-fleet-updater-handoff.service tart-runner-fleet-updater-handoff.service
