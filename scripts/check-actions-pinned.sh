#!/bin/sh
set -eu

uses_lines="$(grep -R -n -E '^[[:space:]]*(-[[:space:]]+)?uses:' .github/workflows .github/actions 2>/dev/null || true)"
violations="$(printf '%s\n' "$uses_lines" | grep -E -v '@[0-9a-f]{40}([[:space:]]*(#.*)?)?$' || true)"

if [ -n "$violations" ]; then
  printf 'GitHub Actions must use immutable 40-character commit SHAs:\n%s\n' "$violations" >&2
  exit 1
fi

printf 'all GitHub Actions are pinned to immutable commit SHAs\n'
