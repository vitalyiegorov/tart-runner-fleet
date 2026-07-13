#!/bin/sh
set -eu

if [ "$#" -lt 1 ]; then
  printf 'usage: run-tool.sh <tool> [arguments...]\n' >&2
  exit 2
fi

tool="$1"
shift
root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
case "$tool" in
  actionlint|cyclonedx-gomod|deadcode|dupl|govulncheck) ;;
  *) printf 'unknown repository tool: %s\n' "$tool" >&2; exit 2 ;;
esac

executable="$(cd "$root/tools" && go tool -n "$tool")"
cd "$root"
exec "$executable" "$@"
