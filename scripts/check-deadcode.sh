#!/bin/sh
set -eu

output="$(./scripts/run-tool.sh deadcode -test ./...)"
if [ -n "$output" ]; then
  printf 'dead code detected:\n%s\n' "$output" >&2
  exit 1
fi

printf 'dead-code analysis clean\n'
