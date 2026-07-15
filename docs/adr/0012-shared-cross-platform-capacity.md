# ADR 0012: Work-conserving cross-platform capacity

## Status

Accepted.

## Context

The original host-mode invariant admitted either Linux or macOS instances but
never both. It simplified handoff reasoning, but a single 4-vCPU Maestro job
could strand half of the configured 8-vCPU/16-GiB envelope while several Linux
jobs waited. A same-platform backfill could fill a second Maestro slot only when
another Maestro job existed; it could not use the residual capacity for queued
Linux work.

The fleet's primary objective is bounded maximum utilization. Platform identity
is not itself a resource. CPU, memory, VM slots, per-repository concurrency,
profile concurrency, fresh host pressure, and lifecycle ownership are the
admission constraints.

Young jobs also need a generic throughput policy. Profile names are deployment
details and must not encode priority. Resource size is an available deterministic
proxy for completion cost; no trustworthy runtime estimate currently exists.

## Decision

Linux and macOS instances may coexist when their combined resource vectors fit
the single configured host envelope and the fresh host observation. The domain
reports this state as `mixed`. Normal macOS admission subtracts every consuming
Linux and macOS instance before spawning. Existing profile caps remain intact;
the configured 8-vCPU builder therefore remains exclusive because no residual
CPU can fit another VM.

When residual capacity cannot admit the selected platform, existing idle-drain,
durable handoff, and one-shot backfill rules remain in force. Destructive work
still requires fresh ownership and external-state confirmation.

For young work, ordering is:

1. control-plane lane before standard lane;
2. pull-request event precedence within a lane;
3. ascending dominant share of the configured CPU, memory, and slot envelope;
4. deterministic repository round-robin within an equal resource band.

The exact allocator still maximizes admitted job count. Once a demand crosses
the aging threshold it leaves this optimization and joins absolute global FIFO,
so large jobs cannot starve.

## Consequences

A running Maestro can share residual capacity with Linux small, medium, or large
jobs when the exact vector permits it, and Linux work can share residual capacity
with Maestro. Status and Prometheus telemetry expose `mixed` explicitly.

The algorithm does not claim that resource size perfectly predicts duration.
Historical duration prediction would require a separate bounded, durable model
with cold-start behavior and poisoning resistance. Until then, dominant resource
share is transparent, deterministic, configuration-independent, and protected
by aging.
