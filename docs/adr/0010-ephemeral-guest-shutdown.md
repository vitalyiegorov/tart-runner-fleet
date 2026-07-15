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
