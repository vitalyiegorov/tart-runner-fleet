# ADR 0074: Drain guards re-verify the runner, not the spawning demand

## Status

Accepted. Amends ADR 0033 (the execution-time re-check of the stalled-assignment
and lingering-runner drains).

## Context

Issue #398, Mac Studio, 2026-10-21. A re-run of pony's `validate (scene)` booted
two `macos-4x7` VMs bound to the same demand. GitHub brokered the job to
`trf-macos-4x7-bdc7861cda0de002`. The other VM, `trf-macos-4x7-67fea656017b5f67`,
never got a job. At 19:37 the scheduler planned a stalled-assignment recovery
for it ("assignment never started"). Nothing happened after that, and nothing
was logged. At 21:18 the idle VM still held 4 CPU / 7 GiB, and it blocked the
node's update drain. Only the three-hour occupancy budget (ADR 0036) would have
released it.

When a recovery drain acts, it re-checks the premise it was planned on. The
stalled-assignment arm asked `JobStarted` and the lingering-runner arm asked
`JobActive`. Both read the one demand the VM was **spawned for**. The demand row
said the job had started, but it had started on the sibling, so the idle loser's
drain aborted back to Running. Every later plan would have aborted the same way.

The demand key is wrong in both directions, because GitHub brokers jobs
independently of the demand the fleet spawned for (ADR 0033):

- **Idle runner, busy demand.** Two instances are bound to one demand and the
  job runs on the sibling. The guard sees "started" and never reclaims the idle
  duplicate. This is issue #398.
- **Busy runner, idle demand.** GitHub hands this runner a job from a different
  demand while the spawning demand still reads assigned. The guard passes and
  the drain tries to deregister a runner that is executing a job. Only GitHub's
  `runner_busy` refusal stopped it. This was the first half of the 2026-08-02
  loop. ADR 0033 made it rare by rebinding the instance, but the guard itself
  never checked for it.

`RunnerBusy` already gives the right answer. It looks for any durable demand in
the instance's own scale-set binding with status `JobStarted` and this
instance's runner name. The event drain has re-checked with it since the
2026-07-25 incident, and its doc comment already called the demand key "the
wrong key once brokering is decoupled from spawning".

None of these aborts were logged either. `reportRetractedPremise` reports only
the stopped recovery, and only from the durable trace that phase leaves on the
row. Issue #246 made every recovery **plan** visible. A withdrawn plan stayed
invisible.

## Decision

1. **One guard for every runner-idle premise.** The stalled-assignment,
   lingering-runner and event drains all check `RunnerBusy` immediately before
   deregistering, through the same `verifyRunnerIdle`:
   - a job executing on this runner aborts the drain to Running;
   - an unreadable answer fails the guard (`drain_guard`) and retries, and
     neither deregisters nor aborts;
   - no job on this runner lets the drain proceed. GitHub re-queues any job that
     was assigned but had not started.

   `DrainControl.JobStarted` and `DrainControl.JobActive` are removed, and so is
   `ControlRouter.JobStarted`, which nothing else used.

   One narrow exception reads the spawning demand, and it can only make the
   guard more cautious. `RunnerBusy` matches on the runner name, and
   `DemandEvent.Valid` accepts a `JobStarted` without one. A start GitHub did
   not attribute is invisible to `RunnerBusy`, but nothing proves it is not on
   this runner, and the old demand-keyed guard would have aborted on it. So,
   after `verifyRunnerIdle` passes, the stalled-assignment and lingering-runner
   guards ask `DrainControl.StartUnattributed`. If the instance's **own**
   spawning demand is exactly `JobStarted` with an empty runner name, or cannot
   be read, the guard fails (`drain_guard`) and retries; it neither deregisters
   nor aborts. The exception is deliberately limited:
   - it does not apply to a completed demand, which has no live work;
   - it does not apply to a start named for a sibling, which is the #398
     duplicate and is still reclaimed;
   - it does not apply to an unnamed start of some **other** demand, because
     that would block every reclaim in the scale set indefinitely.

2. **Planning is unchanged.** The inventory still derives the lingering-runner
   candidate from `ControlRouter.JobActive`, which reads the spawning demand. A
   plan is never destructive. The guard is the gate, and it now asks the
   question that matters. In the normal case, one instance bound to its own
   demand with GitHub's runner name recorded on that demand, the two keys agree
   and behaviour is identical.

3. **Every abort is logged.** `DrainExecutor.OnAbort` receives a `DrainAbort`
   once the rollback to Running is durable. It carries the instance row, the
   phase name, and a closed-vocabulary `AbortEvidence`: `vm_running`,
   `runner_registered`, `runner_busy`, `guest_answered`, or
   `deregister_refused_runner_busy`. The daemon logs it as
   `instance drain aborted by fresh evidence` with instance, profile,
   repository, phase, evidence, run ID and job ID. The line is never rate
   limited, for the same reason as `instance recovery drain planned`. It carries
   no payload, JIT configuration or token.

   Amended by [ADR 0087](0087-an-aborted-drain-ends-its-operation-in-the-rollback.md)
   (issue #447): the rollback and the end of the drain operation are one
   transaction. The aborted drain is `completed` with
   `last_error=aborted:<evidence>` and no effect. A later event drain of the
   same instance gets its own identity.

## Consequences

- An idle duplicate whose demand ran on its sibling is reclaimed at its own
  assignment or idle deadline, not at the occupancy budget.
- A runner executing a job brokered from another demand is stopped by the
  fleet's own guard before any deregistration call. GitHub's refusal is no
  longer the first line of defence for these phases.
- A stalled assignment whose spawning demand already **completed** on a sibling
  now proceeds. Before, it aborted, because `JobStarted` counted completion as
  "started".
- The simulator's executor mirror (`drainAbortsNow`) asks the runner-keyed
  question for these phases, as production does. Every existing trace still
  holds every property.
- `fleet.v1`, the store schema and the configuration are unchanged.
  Observe/shadow authority limits are unchanged.

## Open risk

Runner-keyed evidence depends on GitHub recording the runner's name on the
`JobStarted` it sends. Decision 1 closes this gap for the instance's own
spawning demand (CodeRabbit finding on PR #399). One case stays open: an
unnamed start of a **different** demand, brokered to this runner. It would read
as idle, and the drain would reach the deregistration call. GitHub refuses to
remove a busy runner, and the busy-refusal abort is the backstop. Without
corroborating busy evidence, the drain keeps retrying the deregister stage
under ADR 0007's cleanup retries and never ends the job. The event drain already
carries the same exposure. GitHub's protocol names the runner on every start we
have observed.

The simulator cannot yet create two instances bound to one demand. The fleet's
own planner never does that, and the mechanism behind the 2026-10-21 duplicate
is still unknown. The replay is therefore a lifecycle-package test that drives
the real `ControlRouter` over a durable-demand table.
