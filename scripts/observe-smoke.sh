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
#      mode with a ready controller.
#   2. The `scheduler` observation is FRESH, which it is only when the host and
#      instance observations feeding it were both fresh. On a Linux node that is
#      the /proc probe answering; anything else means the machine could not be
#      read, which is the failure this smoke test exists to catch.
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

binary="${1:-}"
work="$(mktemp -d)"
daemon_pid=''
cleanup() {
  if [ -n "$daemon_pid" ] && kill -0 "$daemon_pid" 2>/dev/null; then
    kill "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  if [ -s "$work/fleet.stderr.log" ]; then
    printf -- '--- daemon stderr ---\n' >&2
    cat "$work/fleet.stderr.log" >&2
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ -z "$binary" ]; then
  binary="$work/fleet"
  CGO_ENABLED=0 go build -o "$binary" ./cmd/fleet
fi

# The checked-in example is the repository's own observe-mode starting point, so
# this proves the shipped configuration boots rather than a bespoke one written
# to make the assertion pass.
cp config/fleet.example.json "$work/fleet.json"
"$binary" config validate "$work/fleet.json"

# Everything the daemon touches lives in one temporary directory, and the health
# listener takes an ephemeral port: a smoke run must never collide with the
# node's own live daemon, whose state and fixed port are exactly what it would
# otherwise take.
endpoint="unix://$work/fleetd.sock"
"$binary" run --mode=observe \
  --config "$work/fleet.json" \
  --database "$work/fleet.db" \
  --admin-socket "$work/fleetd.sock" \
  --health-address 127.0.0.1:0 \
  >"$work/fleet.stdout.log" 2>"$work/fleet.stderr.log" &
daemon_pid=$!

status=''
attempt=0
while [ "$attempt" -lt 60 ]; do
  if ! kill -0 "$daemon_pid" 2>/dev/null; then
    printf 'daemon exited before reaching the observe steady state\n' >&2
    exit 1
  fi
  if status="$("$binary" status --require-ready --output json --endpoint "$endpoint" 2>/dev/null)"; then
    break
  fi
  status=''
  attempt=$((attempt + 1))
  sleep 1
done
if [ -z "$status" ]; then
  printf 'daemon did not become ready within 60 seconds\n' >&2
  exit 1
fi

printf '%s\n' "$status" > "$work/status.json"
"$binary" doctor --output json --endpoint "$endpoint" > "$work/doctor.json"
exec python3 scripts/observe-smoke-assert.py "$work/status.json"
