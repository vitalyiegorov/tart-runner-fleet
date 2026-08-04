#!/bin/sh
set -eu

# observe-smoke.sh boots the real daemon on this machine, in observe mode, from
# a minimal configuration, and proves it reaches the observe steady state.
#
# This is the acceptance evidence for issue #138: on a Linux host it is a node
# with no Tart, no scale sets and no executor measuring its own machine through
# /proc, which is exactly Phase 1 Part A of docs/MULTI_NODE_PLAN.md. It is not
# Linux-only, because the same assertions describe the steady state of a macOS
# node and an operator bringing up either should be able to run one command.
#
# Steady state is three claims, and each is observed rather than assumed:
#
#   1. `fleet status --require-ready` exits 0, reporting this build in observe
#      mode with a ready controller. Readiness needs a tick whose plan status is
#      `ready`, which needs all three observations fresh, so this one assertion
#      already carries most of the others.
#   2. The `scheduler` observation is FRESH. On a Linux node that is the /proc
#      probe answering; anything else means the machine could not be read, which
#      is the failure this smoke test exists to catch.
#   3. Every host measurement is plausible and no instance of any profile is
#      held. A node with no executor holds nothing, and it must say so as a
#      measurement rather than fail closed.
#
# Usage: observe-smoke.sh [FLEET_BINARY]. With no argument it builds the daemon
# from the working tree with `go build`.
#
# It expects a machine that is not already serving a fleet: on a node whose live
# daemon owns running guests, this daemon's own inventory correctly reports
# untracked instances and refuses to call the observation fresh.

# readyBudgetSeconds bounds how long the daemon has to reach the steady state.
# It is wall clock, not an attempt count: each poll costs a process start plus a
# socket round trip, so counting attempts silently shortened the budget on a
# slow machine, and a nested Linux guest opening SQLite for the first time is
# exactly such a machine. On timeout the script prints everything needed to tell
# a slow node from a broken one, so raising this number is never the first
# response to a failure.
readyBudgetSeconds=120

binary="${1:-}"
work="$(mktemp -d)"
daemon_pid=''
health_port=''
diagnose() {
  printf -- '--- fleet status (no --require-ready) ---\n' >&2
  "$binary" status --output json --endpoint "$endpoint" >&2 2>&1 || \
    printf 'status unavailable (exit %s)\n' "$?" >&2
  printf -- '--- fleet doctor ---\n' >&2
  "$binary" doctor --output json --endpoint "$endpoint" >&2 2>&1 || \
    printf 'doctor unavailable (exit %s)\n' "$?" >&2
  if [ -n "$health_port" ]; then
    for route in healthz readyz; do
      printf -- '--- GET /%s ---\n' "$route" >&2
      curl -sS --max-time 5 "http://127.0.0.1:$health_port/$route" >&2 2>&1 || \
        printf 'health endpoint unreachable\n' >&2
      printf '\n' >&2
    done
  fi
}
cleanup() {
  if [ -n "$daemon_pid" ] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  for log in fleet.stdout.log fleet.stderr.log; do
    if [ -s "$work/$log" ]; then
      printf -- '--- daemon %s ---\n' "$log" >&2
      cat "$work/$log" >&2
    fi
  done
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ -z "$binary" ]; then
  binary="$work/fleet"
  CGO_ENABLED=0 go build -o "$binary" ./cmd/fleet
fi

# The checked-in example is the repository's own observe-mode starting point, so
# the smoke test proves the shipped configuration boots rather than a bespoke one
# written to make the assertion pass.
#
# One key is removed, and only one: `hostBudget`. It is a declaration about ONE
# specific machine — the example states the live Mac mini's 10 cores and
# 23552 MiB — and `app.budgetExceedsHost` fails the host observation closed when
# the machine cannot honour it (issue #136). That is correct behaviour, and it
# is what a node claiming capacity it does not have deserves; it just means the
# example's budget is a false claim on every other machine, including a two-core
# CI guest. An unset budget imposes no bound and is a documented byte-for-byte
# no-op, so removing it leaves every other guard, profile, and target exactly as
# shipped.
python3 - "config/fleet.example.json" "$work/fleet.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    config = json.load(handle)
removed = config.pop("hostBudget", None)
with open(sys.argv[2], "w", encoding="utf-8") as handle:
    json.dump(config, handle, indent=2)
print(f"smoke configuration: {sys.argv[1]} without hostBudget={removed!r}")
PY
"$binary" config validate "$work/fleet.json"

# Everything the daemon touches lives in one temporary directory, and the health
# listener takes a port claimed a moment ago and released: a smoke run must never
# collide with the node's own live daemon, whose state and fixed port are exactly
# what it would otherwise take. The port is known rather than ephemeral so that a
# failing run can print the health endpoint's own answer.
health_port="$(python3 -c 'import socket
probe = socket.socket()
probe.bind(("127.0.0.1", 0))
print(probe.getsockname()[1])
probe.close()')"
endpoint="unix://$work/fleetd.sock"
"$binary" run --mode=observe \
  --config "$work/fleet.json" \
  --database "$work/fleet.db" \
  --admin-socket "$work/fleetd.sock" \
  --health-address "127.0.0.1:$health_port" \
  >"$work/fleet.stdout.log" 2>"$work/fleet.stderr.log" &
daemon_pid=$!

status=''
started="$(date +%s)"
while :; do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    printf 'daemon exited before reaching the observe steady state\n' >&2
    exit 1
  fi
  if status="$("$binary" status --require-ready --output json --endpoint "$endpoint" 2>/dev/null)"; then
    break
  fi
  status=''
  elapsed="$(( $(date +%s) - started ))"
  if [ "$elapsed" -ge "$readyBudgetSeconds" ]; then
    printf 'daemon did not reach the observe steady state within %ss (waited %ss)\n' \
      "$readyBudgetSeconds" "$elapsed" >&2
    diagnose
    exit 1
  fi
  sleep 1
done

printf '%s\n' "$status" > "$work/status.json"
"$binary" doctor --output json --endpoint "$endpoint" > "$work/doctor.json"
printf 'became ready after %ss\n' "$(( $(date +%s) - started ))"
exec python3 scripts/observe-smoke-assert.py "$work/status.json"
