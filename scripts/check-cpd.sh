#!/bin/sh
set -eu

threshold="${CPD_THRESHOLD:-100}"
case "$threshold" in
  ''|*[!0-9]*) printf 'CPD_THRESHOLD must be a positive integer\n' >&2; exit 2 ;;
esac
if [ "$threshold" -le 0 ]; then
  printf 'CPD_THRESHOLD must be a positive integer\n' >&2
  exit 2
fi

duplicates="$(./scripts/run-tool.sh dupl -plumbing -t "$threshold" cmd internal)"
if [ -n "$duplicates" ]; then
  printf 'copy/paste detection found Go clones (threshold %s tokens):\n%s\n' "$threshold" "$duplicates" >&2
  exit 1
fi

printf 'copy/paste detection passed (threshold %s tokens)\n' "$threshold"
