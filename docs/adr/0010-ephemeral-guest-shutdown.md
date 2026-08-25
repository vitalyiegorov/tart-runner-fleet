# ADR 0010: Power off a guest when its ephemeral runner exits

## Status

Accepted

## Context

GitHub removes an ephemeral runner after one job. A delayed or missing broker
completion event can therefore leave a healthy Tart guest running after its
runner listener has exited. The runner no longer appears in GitHub, but the VM
continues to reserve CPU, memory, a profile slot, and host-mode exclusivity.
Controller-side ownership recovery cannot safely infer that a running guest is
idle and must not delete it merely to recover capacity.

## Decision

The secret-safe guest bootstrap helper starts `run.sh` beneath a fixed detached
supervisor. When the runner exits, the supervisor requests a non-interactive
guest shutdown with `/usr/bin/sudo -n /sbin/shutdown -h now`. Runner, shell,
sudo, and shutdown paths are validated executable paths; dynamic values remain
positional arguments, and the JIT configuration remains environment-only.

The VM power transition is the authoritative lifecycle signal. Existing fresh
Tart inventory, exact GitHub runner absence, durable drain, deregistration, and
owned deletion then converge through the controller's retry-safe cleanup state
machine. Active runners are never interrupted, and unknown or still-running
guests remain fail-closed.

Every immutable Linux and macOS base must install the bootstrap binary from the
same verified fleet release as the controller. A controller update alone does
not mutate immutable bases; base rollout is an explicit idle-fleet operation
with the previous base retained for rollback.

## Consequences

Lost completion messages no longer create invisible capacity leaks. Cleanup
remains ownership-safe and event-driven instead of relying on destructive host
scavenging or time-based guesses. Base-image release parity becomes a required
upgrade invariant and is observable during operational audits.

## Amendment 2026-08-25: a guest with no power switch is told so (issue #273)

The mechanism above — `systemd-run --scope`, then `sudo -n /sbin/shutdown -h
now` — describes a virtual machine, and until node B every guest was one. A
rootless Podman container is not: it has no init system to place the runner
under and no machine to power off. Both halves of the mechanism fail there, and
the first fails in the worst available way. `/usr/bin/systemd-run` and
`/sbin/shutdown` are present in the geekom runner image because `playwright
install-deps` pulls the systemd package, so the launcher's existence probes pass
and `systemd-run --scope` then exits with *"System has not been booted with
systemd as init system (PID 1)"*. The supervisor never writes its readiness
marker, and the operator reads a bootstrap-stage failure that names nothing.
Removing systemd from the image inverts the failure instead of repairing it: the
probes then fail and the launcher refuses to start at all.

**The guest is told what it is; it does not guess.** `internal/daemon` is the one
package that knows which backend it wired (ADR 0034), so it is the only place
that can answer, and it answers on the argument vector it already assembles:
`podman exec -i <name> /usr/local/libexec/tart-runner-fleet-bootstrap
--container`. A guest invoked without the flag is a virtual machine, and every
sentence of the record above applies to it unchanged — which is what keeps this
amendment a no-op for node A and for every image built before it. Inferring the
answer inside the guest, from PID 1 or from the binaries on disk, was rejected
for the reason the incident demonstrates: what a guest can observe about itself
is what its image happened to install, and that is not the same question.

**In a container, the runner's exit is the whole of the teardown.** The
supervisor becomes the runner rather than outliving it to press a switch that
does not exist, and no `sudo` or `shutdown` path is required, probed, or read.
What this record asks of a VM's power transition — that an exited ephemeral
listener stop holding capacity — the container path takes from the daemon: the
same durable drain, deregistration, and owned deletion stop and remove the
container. The invariant is unchanged; only the actor that carries it out is.
The one-shot rule is likewise untouched: a container is created and never
adopted (ADR 0034, amendment 2026-08-04), so no guest of either kind serves a
second job.

**Evidence.** `internal/guestbootstrap` proves both shapes on every commit: a
container guest starts with no wrapper and no poweroff tools present, and the
same launcher still refuses a VM guest that is missing either. The end of the
chain — the real helper starting a real runner inside a real rootless container
— is `TestPodmanBootstrapsARunnerInAContainerWithNoInitSystem` in
`tests/integration`, opt-in behind `scripts/podman-smoke.sh` because CI has no
container runtime. That test is the gap this incident exposed: the smoke suite
passed on the container node and the first real job still failed, because
nothing in it had ever launched a runner.
