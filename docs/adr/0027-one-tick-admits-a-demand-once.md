# ADR 0027: One tick admits a demand once

## Status

Accepted. Constrains the complementary admission passes introduced with
mixed-platform admission ([ADR 0012](0012-shared-cross-platform-capacity.md)) and
extended by [ADR 0024](0024-mixed-macos-profile-cohorts.md), and explains a
`plan_commit_failed` that [ADR 0025](0025-saturation-is-not-durable-failure.md)
classified but did not cause.

## Context

A tick may plan twice. The first pass admits the platform that owns the tick;
`fillLinuxRemainder` and `fillMacRemainder` then admit the other platform inside
whatever envelope the first pass left. `mixedRemainderInput` makes that safe for
capacity by modelling this tick's planned spawns as live occupancy, so the second
pass never over-commits the host.

It teaches the second pass what the first pass **cost**. It never tells it what
the first pass **claimed**, and both passes receive the same queue.

On 2026-08-02 on host `vitalii-mac-mini` those two facts met. A macOS builder job
was the aged priority head behind a single `xl` Linux runner that was executing a
workflow job, so it was never `OnlineIdle` and no handoff could drain it.
`planMacHandoff` could not spawn the head, so it admitted one aged smallest-tier
Linux job as its bounded drain backfill and latched `BackfillAdmitted`. `PlanTick`
then ran `fillLinuxRemainder` over the same Linux queue, which admitted the very
same demand a second time.

Both admissions are content-addressed from the same demand, so the plan carried
two operations with one identity and two instance intents with one instance name.
`reconcile.Controller.Commit` translated both and `ApplyPlan` refused the second
`INSERT` on the `instances` primary key. That error is neither `ErrConflict` nor
`ErrInvalid`, so it surfaced as `component=scheduler reason=plan_commit_failed`.

The plan is a pure function of its inputs. Nothing about the refused write
changed any input, so the next tick rebuilt the identical plan and was refused
identically. `scheduler_state.version` stayed pinned at 2421 from 09:59:51Z, no
row entered `plans`, `transition_history` recorded no mutation, and three
profiles queued past the SLO while the host sat at 87% CPU idle. The control
plane was wedged until an operator intervened — from a scheduling decision, not
from a store fault, reported under a token that sends the operator to the
database.

## Decision

**A complementary admission pass never sees a demand this plan already spawns.**
`fillLinuxRemainder` and `fillMacRemainder` filter their queue through
`demandsAwaitingAdmission` before planning, so a demand claimed by the first pass
cannot be claimed again by the second.

Filtering the input rather than deduplicating the finished plan is deliberate.
The second pass keeps its own budget honest: a demand the first pass already
claimed must not be counted a second time against slots, repository caps, or the
residual envelope. Deduplicating afterwards would hide a double count that had
already influenced what else the pass admitted.

The filter removes only what this plan claimed. Everything else the first pass
declined still reaches the residual envelope on the same tick, so no work-
conserving behaviour is lost.

## Consequences

The scheduler can no longer author a plan the durable layer must refuse. The
identity rules stay exactly as they were — spawn identity remains content-
addressed and idempotent within an attempt, and the durable layer still fails
closed on a repeated identity rather than silently collapsing two admissions.

`plan_commit_failed` becomes what ADR 0025 says it is: an unavailable or refusing
store. A recurring one is again a reason to look at the database rather than at
the planner.
