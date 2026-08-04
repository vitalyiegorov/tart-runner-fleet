#!/bin/sh
set -eu

# podman-smoke.sh runs the fleet's Podman executor adapter against a real
# rootless podman on this machine.
#
# It is the acceptance evidence for issue #139 that no fake can produce: the
# argument vectors podman actually accepts, the JSON it actually prints, the
# container states it actually reports, `podman exec -i` carrying a JIT secret in
# on stdin, and `--device /dev/kvm` being a grant the runtime honours. Everything
# else about the adapter is covered by table-driven unit tests against a fake
# command runner, and by the executor-port conformance harness in
# tests/contract, both of which run on every machine.
#
# Two modes, and the difference is the whole point:
#
#   * best effort (default) -- a machine with no podman prints SKIP and exits 0.
#     CI runs it this way, because the fleet's own Linux runners are Tart guests
#     whose image does not currently ship a container runtime, and a gate that
#     fails for the absence of a thing the node under test does not need would
#     teach reviewers to ignore it.
#
#   * TRF_PODMAN_SMOKE=required -- a machine with no podman FAILS. This is how
#     the Geekom bring-up checklist runs it in docs/MULTI_NODE_PLAN.md, before
#     the node is promoted to authority. On node B, SKIP is not an answer.
#
# Environment:
#   TRF_PODMAN_SMOKE=required   fail instead of skipping when podman is unusable
#   TRF_PODMAN_IMAGE=<ref>      the image to test with (default: alpine)
#   GO=<binary>                 the Go toolchain to use (default: go)

image="${TRF_PODMAN_IMAGE:-docker.io/library/alpine:3.20}"
go_binary="${GO:-go}"
required=''
if [ "${TRF_PODMAN_SMOKE:-}" = required ]; then
  required=1
fi

unusable() {
  if [ -n "$required" ]; then
    printf 'podman smoke test FAILED: %s\n' "$1" >&2
    printf 'This node is configured to execute jobs in containers, so a container runtime is not optional.\n' >&2
    exit 1
  fi
  printf 'podman smoke test SKIPPED: %s\n' "$1"
  printf 'The adapter is still covered by its unit tests and by the executor-port conformance harness;\n'
  printf 'what is NOT covered here is the real command line. Run this on the container node with\n'
  printf 'TRF_PODMAN_SMOKE=required before promoting it to authority.\n'
  exit 0
}

if ! command -v podman >/dev/null 2>&1; then
  unusable 'podman is not installed on this machine'
fi

# The adapter refuses a root-ful runtime, so the smoke test must not pretend one
# is fine either: ADR 0034 puts approved third-party code on this node.
if ! podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null | grep -qx true; then
  unusable 'podman is installed but not rootless'
fi

printf 'podman: %s\n' "$(podman --version)"
printf 'image:  %s\n' "$image"

if ! podman image exists "$image" 2>/dev/null; then
  printf 'pulling %s\n' "$image"
  if ! podman pull --quiet "$image" >/dev/null; then
    unusable "cannot pull $image"
  fi
fi

# /dev/kvm is the Android profile's device grant (ADR 0034). It exists on the
# container node and does not exist in a nested CI guest, so the KVM assertions
# are enabled by its presence rather than assumed.
if [ -c /dev/kvm ] && podman run --rm --device /dev/kvm "$image" test -c /dev/kvm >/dev/null 2>&1; then
  printf 'kvm:    /dev/kvm is grantable\n'
  TRF_PODMAN_KVM=1
  export TRF_PODMAN_KVM
else
  printf 'kvm:    absent or not grantable; the device-grant assertions will skip\n'
fi

TRF_PODMAN_LIVE=1
TRF_PODMAN_IMAGE="$image"
export TRF_PODMAN_LIVE TRF_PODMAN_IMAGE

"$go_binary" test -count=1 -timeout 10m -v ./tests/integration -run 'TestPodman'

printf 'podman smoke test PASSED\n'
