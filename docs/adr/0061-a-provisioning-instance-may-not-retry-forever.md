# ADR 0061: A provisioning instance may not retry forever

## Status

Accepted. Adds a seventh recovery cause beside the six
[ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md) and
[ADR 0040](0040-a-guest-that-stopped-answering-is-not-running.md) left scoped
to Assigned and Running. It does not change any existing cause, and it does not
change the provisioning retry policy.

## Context

On 2026-10-11 at 20:01Z the Mac Studio cloned `trf-macos-4x7-7afca955a01a9e1e`
for an iOS Maestro shard of `vitalyiegorov/suuudokuuu`. The guest booted, became
Reachable, and answered on the network: ping and port 22 both worked. `tart exec`
into it hung without ever returning. Bootstrap runs through `tart exec`, so every
bootstrap attempt failed.

The provisioning operation then retried bootstrap against the same VM every
thirty seconds. Provisioning retries have no attempt ceiling and no elapsed
bound (`lifecycleRetryMaxAttempts = 0`), and a failed attempt leaves the row in
the state it failed in. Seven hours and 395 attempts later the instance was
still Reachable, still holding 4 CPU and 7168 MiB.

Every gate that might have released it was out of scope by construction:

- All six recovery causes require Assigned or Running. A Reachable instance never
  registered a runner, so it is neither.
- The guest-liveness probe judges Running instances only, and a probe that hangs
  counts as a saturated guest rather than a dead one.
- The occupancy and progress checks reported the hold, and `fleet doctor` named
  it, but neither acts.

The Studio is the only node serving macOS, and the scheduler's macOS handoff had
reserved the next slot for a demand waiting on this vector. Three iOS shards
across two repositories waited between seven and nine and a half hours behind
one guest that could never succeed.

## Decision

### A provisioning step has a deadline

`domain.Instance` gains `ProvisioningSince`: the durable row's `updated_at`
while the instance is Cloning, Booting, or Reachable, and zero otherwise. A
failed provisioning attempt does not change the row, so this is how long
provisioning has been stuck at one step.

`provisionStalled` is the seventh cause. It fires when that dwell reaches the
assignment deadline (`assignedTimeoutSeconds`, 15 minutes by default). Reusing
that deadline rather than adding a key is deliberate. A runner that has not got
past one provisioning step in that long would already have missed its
assignment deadline, and a new key would be a second number to tune for the
same promise.

It is narrow and fail-closed:

- **Planned is excluded.** Its VM does not exist yet, and a planned instance may
  legitimately wait on a cross-platform drain it depends on.
- **Registering is excluded.** The instance FSM does not allow it to drain.
- An unknown entry time and an unconfigured deadline never fire.

It is the only cause a provisioning instance can have, so it is planned on its
own and carries the demand key, which the operator's log line names.

### The drain never aborts

`DrainPhaseProvisionStalled` (8) re-checks one thing before deregistering: that
no runner on this instance's name is executing a job. A stalled provisioning
could have started a runner on its last attempt just as the plan committed. If
that runner is busy, or busy evidence cannot be read, the guard fails and the
drain retries.

It never aborts to Running, which is what every other runner-premise phase does
on busy evidence. The instance never reached Running, and returning it there
would invent a busy runner where none ever registered. For the same reason a
`runner_busy` deregistration refusal does not abort it (`abortsOnBusyRefusal`).

Deletion is confirmed from runner absence, as for every phase whose job never
started (`derivesInactivityFromRunnerAbsence`). The drain then takes the
ordinary path: deregister (a no-op when no runner exists), stop, delete. The
still-claimed provisioning operation completes on its next attempt, because
ProvisionExecutor already completes without acting once its row is draining.
The demand then respawns as a fresh generation on a new clone.

### What it does not do

- It does not bound the provisioning retry policy. Retrying a transient
  failure on the same VM is still right, and an attempt ceiling alone would
  dead-letter the operation with the VM still held, which is the incident.
- It does not judge why the guest failed. A guest that fails bootstrap for a
  reason a fresh clone cannot fix, such as a missing image capability, is
  re-cloned once per deadline instead of held forever. That is bounded churn the
  operator can see in the recovery log, not a silent hold.

## Evidence

- `internal/scheduler/provision_stalled_test.go` covers the planning edge: each
  of the three states fires, and young, Planned, Registering, unknown-entry and
  unconfigured cases are left alone.
- `internal/lifecycle/executor_provision_stalled_test.go` covers the drain edge:
  the drain reaches Deleted, busy evidence is held at the guard, and a busy
  refusal never aborts.
- `tests/simulation/provision_stalled_test.go` replays the incident with a new
  trace-only fault, `wedged_bootstrap`, under which the next reachable guest
  never gets past bootstrap. The wedged guest is reclaimed, both jobs run, and
  the fleet settles. With the cause disabled, the simulator's existing liveness
  property fires on the same trace, with the instance still `reachable` and the
  queued demand aged 66 minutes.
- The fault is not added to the trace generator's random draw. Adding a kind
  reshuffles every generated trace and every regression pinned to a seed.

### The corpus moves, and not because of the new cause

The generated-trace corpus (`TestSimCorpusIsIdenticalAcrossRuns`, 64 seeds ×
200 ticks) changes digest in every arm, with zero findings before and after.
None of that change comes from the new cause. With the cause enabled and the
simulator change below reverted, the digest is byte-identical to `main`.

The change is a world-model correction this work needed. When a job completes
while its instance is still provisioning, the demand projection queues an event
drain that depends on the provisioning operation (`enqueueDemandDrain`), and
`ProvisionExecutor` then completes without acting, because the row is already
draining. The simulator did not model that. Its still-claimed provisioning
operation went on stepping the instance's lifecycle, and so performed the
drain's edges itself, as a second drain. It now completes the way production
does (`supersededProvisioning`), and leaves the drain to the drain operation.
The provisioning-stalled reclaim depends on exactly that hand-over.
