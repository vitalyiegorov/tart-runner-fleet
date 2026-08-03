# ADR 0033: A runner is bound to the job GitHub gave it

## Status

Accepted. Closes the churn [ADR 0028](0028-a-repeated-decision-is-a-new-attempt.md)
named in its own "Not addressed here" and [issue #123](https://github.com/vitalyiegorov/tart-runner-fleet/issues/123).
Generalizes the crossed-assignment rule of
[ADR 0016](0016-authoritative-runner-assignment.md) and reuses the release
semantics [ADR 0026](0026-queued-demand-expires-on-proven-absence.md) established
for demand that is queued but not being served.

## Context

The fleet spawns a VM *for a demand*. Every later question about that VM is
answered from that binding: is a job active, is it safe to deregister, may this
demand be planned again. GitHub, meanwhile, decides what a registered runner
actually executes, and it does not consult the fleet's reason for creating it.

Those two answers came apart on host `vitalii-mac-mini` on 2026-08-02.
`trf-xl-25a374b60f46dafe` was spawned at 09:29:32Z for
`rnw-community/rnw-community/30740997047/1/8670852748984054370` — the job
"Maestro (expo)". The broker handed its runner the sibling from the same workflow
run instead:

```
runner_demands
  runner_request_id 4961232247973546403  "Maestro (bare)"  JobStarted
      runner_name = trf-xl-25a374b60f46dafe          <-- genuinely busy
  runner_request_id 8670852748984054370  "Maestro (expo)"  JobAvailable
      (the demand the instance is bound to, still queued)
```

Both jobs were dispatched 41 ms apart. ADR 0016's alignment could not repair it:
that rule *swaps* two bindings, and a swap needs a counterpart. Here only one
request had been acquired by a VM, so `alignRunnerDemand` found no source
instance and answered `ErrUncertain` — for that event and for every redelivery of
it, forever.

Three consequences followed, all of them structural rather than unlucky.

**The instance never learned what it was doing.** With the `JobStarted` never
projected, the row stayed `Assigned`. `JobInactive`, derived from the *bound*
demand, reported an idle runner; `RunnerBusy`, derived from the *runner name*,
reported a live job. The two predicates disagreed by construction.

**A recovery drain fired every deadline and aborted every time.** The assignment
deadline elapsed at 09:44:48Z and planned a stalled-assignment drain. Its
execution-time re-check reads the bound demand, which showed nothing started, so
the executor proceeded — and GitHub refused to deregister a runner executing a
job. The runner-keyed re-check confirmed the refusal and the drain aborted
(`draining -> running` at 09:44:49Z). `abort` restores `Running`, `RunningSince`
is the row's `updated_at`, so the idle-runner deadline restarted and the
lingering-runner drain repeated the whole cycle at 09:59:51Z. Each turn cost a
durable transition, a GitHub round trip, and a deregistration attempt against a
runner doing real work.

**A queued job could never be served.** "Maestro (expo)" stayed `JobAvailable`,
so it was genuinely queued; but its own instance still incarnated it, so
`app.plannableDemands` correctly refused to spawn a second VM for it, and that
instance was busy with the sibling. The `xl` queue reported one job aged 1h28m
and rising, the SLO monitor reported `queue_incident,queue_slo_breached` for a
fleet that was behaving exactly as designed, and the runner stayed resident —
capping the `maestro` profile at 1 of 2 and blocking iOS pull requests for the
rest of the day.

## Decision

**The runner's binding follows GitHub.** When a scale-set event names a runner
(`RunnerName`) on a request that runner's instance is not bound to, and no other
instance incarnates that request, the instance is REBOUND: its durable
scheduling metadata takes the demand key of the job GitHub dispatched, and the
demand it was spawned for returns to the queue as an ordinary queued demand.

There is exactly one source of truth for what a runner is executing, and it is
GitHub. `alignRunnerDemand` now expresses both repairs of one rule: swap when a
counterpart exists (ADR 0016's crossed assignment), rebind when it does not.
`JobInactive` and `RunnerBusy` then read one fact again, so a busy runner is
never a lingering-runner candidate and no drain is planned for it at all. The
busy-drain invariant is unchanged; what changes is that it no longer has to be
upheld by a late abort.

**Ownership stays immutable.** Only `scheduling_metadata` is rewritten, under
the same compare-and-set on (state, version, prior metadata) that the swap uses.
The VM's name, its `ownership.resource_id`, and its spawn identity still name the
demand it was born for. That is provenance, and it is never rewritten.

**A rebound incarnation is a spent incarnation.** Because ownership still names
the released demand, the next spawn for that demand re-derived a byte-identical
content-addressed instance name and wedged on the `instances` primary key — the
deterministic simulation found this within one sweep of the rebind landing.
`SpawnGeneration` therefore counts a rebound row exactly as it counts a terminal
one: both are incarnations that can never execute the demand again, and both are
precisely the rows on which `domain.Instance.IncarnatesDemand` stops holding it.
Identity and admission now key on the same fact, so they cannot disagree. A live
row that still holds its demand, or declares no binding at all, is still not
counted, so a genuine double spawn still hard-fails.

**The released demand keeps its queue time.** This is the contested half of the
decision, and it is decided against resetting.

The inflated age of the incident — ten hours by the end of the day — was a
symptom of the demand never being *served*, not of the age being *preserved*.
The rebind cures it at the source: the demand re-enters the queue in the same
tick and is admitted as soon as capacity allows. The deterministic simulation
measures exactly this. Across 150 seeds, 119 stranded a demand before the change
and none does after, and the pinned incident asserts that the released job starts
within one boot of the capacity it was waiting for actually coming free.

Resetting would be wrong on four independent grounds:

- **It is not the fleet's fact.** `queue_time` is GitHub's statement about when
  a job entered the queue, and the job has genuinely been waiting since then;
  nobody has run it. Rewriting it would be the controller inventing an
  observation, and the queue SLO — which measures user-visible CI latency — would
  under-report a real breach.
- **It would not survive.** `first_queue_time` is preserved by the durable
  upsert and any redelivered `JobAvailable` carries GitHub's own `queue_time`,
  so a reset would be silently undone by the next message. A lie the system
  corrects back into the truth is worse than telling none.
- **ADR 0026 already decided the same question.** A demand revived after a wrong
  expiry returns "unchanged, with its original queue age ... Nothing about the
  demand's identity, age, or ordering is lost". A rebind release is the same
  class of event: the job never ran, and nothing about it should be forgotten.
- **It would invert ADR 0004's starvation guard.** Aging is the absolute
  scheduling priority precisely so that no job can be passed over indefinitely.
  A demand whose runner is repeatedly given a sibling would be pushed to the back
  of the global FIFO on every rebind — a livelock in which the unluckiest job
  never runs. Resetting the clock to hide an unbounded wait would convert a
  visible, fixable defect into an invisible, permanent one.

## Consequences

The churn is gone: the fleet plans no recovery drain for a runner that is
working, so there is nothing to abort, no repeated durable transition, and no
deregistration attempt against live work. The released demand is served in the
next admission pass rather than after the fleet is restarted. The queue SLO
reports the truth again, and capacity is not pinned behind a job the fleet is
not executing.

Rebinding may move an instance to another repository, and must: scheduling
metadata is only valid when the instance's repository and its demand's agree,
and repository caps must charge the work actually being done. That is safe for
control routing — keyed on (repo, profile) in `lifecycle.SourceKey` — because the
dispatched row arrived through this instance's own scale set, and every
repository whose demand a scale set delivers is one the fleet already spawns
instances into directly. A rebind can only move an instance to a repository it
could have been born in. The crossed assignment has swapped repositories on the
same reasoning since ADR 0016.

Four guards keep the rule fail-closed, and each leaves the previous
`ErrUncertain` answer — and therefore at-least-once redelivery — in place rather
than guessing:

- the instance must be `registering`, `online_idle`, `assigned`, or `running`,
  so it has a live GitHub runner and no cleanup in flight;
- the dispatched row must be able to name a demand key at all;
- the released demand must not already be `JobStarted` or `JobCompleted`, which
  would mean this runner really did serve it;
- the compare-and-set must win, so a row that moved is retried, never overwritten.

The deterministic simulation gained two properties and two physics edges it was
missing, and the edges matter beyond this decision. It now models GitHub's
refusal to deregister a runner that is executing a job — the edge that makes a
recovery drain a loop rather than a kill — and job durations that outlive a
recovery deadline, without which the entire abort class of ADR 0028 was
unreachable. Property (h) fails when a queued demand is held by an instance
executing something else; property (i) fails when one instance has more than one
recovery drain aborted. Two existing oracles were sharpened at the same time and
ADR 0031 records why: conservation does not charge a REBOUND instance to a
repository cap, because a cap bounds admission rather than GitHub's dispatch; and
bounded starvation counts pass-overs per cause rather than per demand, which is
finding 6 of issue #130 and was reported here as an unsignatured failure naming a
mechanism that had acted once.

Across 150 seeds of 200 ticks, run three times per arm, 119 seeds stranded a
demand before the change and none does after; the seeds that finish with no
finding at all go from 26 to 136. Every count reproduced identically in three
independent processes.

## Not addressed here

The scheduling findings ADR 0031 still documents are untouched; their corpus
frequencies move only because a fleet that no longer strands demand runs on and
reaches states a stranded one never got to. Issue #130's cross-process determinism work is also untouched,
though every measurement in this change reproduced identically across three
independent processes.

Nothing here changes what a drain is *allowed* to do. The executor still
re-verifies before every destructive step and still aborts on contrary evidence.
A wrong rebind is bounded to scheduling: it never deregisters a runner, never
marks a job complete, and is corrected by the next authoritative event.
