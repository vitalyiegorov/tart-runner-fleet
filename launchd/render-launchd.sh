#!/bin/sh
set -eu

usage() {
  printf 'usage: render-launchd.sh MODE RELEASE_DIR STATE_DIR OUTPUT [CANARY_SCOPE CANARY_PROFILE]\n' >&2
  exit 2
}

if [ "$#" -lt 4 ]; then
  usage
fi

mode=$1
release_dir=$2
state_dir=$3
output=$4
shift 4

case "$mode" in
  observe)
    template_name=com.vitalyiegorov.tart-runner-fleet.plist
    [ "$#" -eq 0 ] || usage
    ;;
  shadow|authority)
    template_name="com.vitalyiegorov.tart-runner-fleet.$mode.plist"
    [ "$#" -eq 0 ] || usage
    ;;
  canary)
    template_name=com.vitalyiegorov.tart-runner-fleet.canary.plist
    [ "$#" -eq 2 ] || usage
    canary_scope=$1
    canary_profile=$2
    ;;
  *) usage ;;
esac

validate_path() {
  value=$1
  case "$value" in
    /*) ;;
    *) printf 'launchd paths must be absolute\n' >&2; exit 2 ;;
  esac
  case "$value" in
    *'<'*|*'>'*|*'&'*|*'\'*|*'
'*) printf 'launchd paths contain an unsupported XML metacharacter\n' >&2; exit 2 ;;
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
validate_path "$output"
if [ "$mode" = canary ]; then
  validate_selector "$canary_scope"
  validate_selector "$canary_profile"
else
  canary_scope=''
  canary_profile=''
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
template="$script_dir/$template_name"
if [ ! -f "$template" ]; then
  printf 'launchd template is missing: %s\n' "$template" >&2
  exit 2
fi
output_dir=$(dirname -- "$output")
if [ ! -d "$output_dir" ]; then
  printf 'launchd output directory does not exist: %s\n' "$output_dir" >&2
  exit 2
fi

temporary=$(mktemp "$output.tmp.XXXXXX")
cleanup() { rm -f "$temporary"; }
trap cleanup EXIT HUP INT TERM
LC_ALL=C awk \
  -v release_dir="$release_dir" \
  -v state_dir="$state_dir" \
  -v canary_scope="$canary_scope" \
  -v canary_profile="$canary_profile" '
  {
    gsub(/__RELEASE_DIR__/, release_dir)
    gsub(/__STATE_DIR__/, state_dir)
    gsub(/__CANARY_SCOPE__/, canary_scope)
    gsub(/__CANARY_PROFILE__/, canary_profile)
    print
  }
' "$template" > "$temporary"
chmod 0600 "$temporary"
if command -v plutil >/dev/null 2>&1; then
  plutil -lint "$temporary" >/dev/null
fi
mv -f "$temporary" "$output"
trap - EXIT HUP INT TERM
