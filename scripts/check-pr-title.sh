#!/bin/sh
set -eu

title="${1:-}"
allowed='(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)'
scope='(\([a-z0-9][a-z0-9._/-]*\))?'

if ! printf '%s\n' "$title" | grep -E -q "^${allowed}${scope}!?: [a-z0-9].+[^.]$"; then
  printf '%s\n' \
    'pull request title must use Conventional Commits:' \
    '  <type>[optional scope][!]: <lower-case description without a period>' \
    '  example: feat(scheduler): prioritize critical builder jobs' >&2
  exit 2
fi

if [ "${#title}" -gt 100 ]; then
  printf 'pull request title must not exceed 100 characters (%s)\n' "${#title}" >&2
  exit 2
fi

printf 'valid Conventional Commit pull request title: %s\n' "$title"
