# ADR 0036: An instance may not hold its vector forever

## Status

Accepted. Answers the duration-aware policy
[ADR 0005](0005-bounded-drain-backfill.md) deferred for want of "an explicit,
enforceable job-runtime contract", and closes
[issue #223](https://github.com/vitalyiegorov/tart-runner-fleet/issues/223).

It adds a fifth recovery cause to the four
[ADR 0028](0028-a-repeated-decision-is-a-new-attempt.md) scopes by attempt, and
it is the first one whose premise a busy runner does not disprove, so it states
an explicit exception to the abort rule of
[ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md) and to the
retry-through-refusal rule of [ADR 0020](0020-diagnosable-drain-failures.md).
It does not change the busy-drain invariant of
[ADR 0016](0016-authoritative-runner-assignment.md) for any other path: no
planner may still drain a busy runner, and this one may do so only through a
ceiling an operator configured.

## Context

On 2026-08-09 the instance `trf-xl-05bbe1c83f21fcd6` was created at 18:07:28Z
with a 6 vCPU / 12288 MiB vector — sixty percent of the production Mac mini's
entire ten-core envelope — for job 93275690093 of
`rnw-community/rnw-community` run 31325708527, "Maestro (bare)".

The job **failed** at 18:09:12Z, a hundred seconds in, at step 8, "Set up
Android SDK". Steps 9 through 11 were skipped. Step 12, "Capture final device
state" — an `if: always()` cleanup step — then ran from 18:09:12Z to 19:22:50Z:
**73 minutes 38 seconds**, presumably waiting on `adb` for an emulator that
never booted, until GitHub cancelled it.

For those 73 minutes the fleet held 6 vCPU and 12 GiB for a job doing no useful
work, while on the same host:

- `rnw-repo/maestro` had 3 jobs queued, the oldest 85 minutes old;
- `sudoku-repo/builder` had 2 jobs queued — the owner's App Store and Play Store
  release — the oldest 80 minutes old;
- four cores sat idle the entire time.

Roughly 75 minutes at sixty percent of the host, held by a dead job, with the
owner's store release behind it. That is a larger utilization leak than any
scheduler defect found so far, and no scheduler improvement can address it,
because no admission policy can reclaim a vector the lifecycle refuses to
release.

### Why nothing saw it

Every gate behaved correctly. GitHub reported the job `in_progress` the whole
time, and it *was* in progress — executing a hung cleanup step. So:

- `stalledAssignment` did not apply: the instance was `Running`, not `Assigned`.
- `lingeringRunner` did not apply: the bound demand carried an active job, so
  `JobInactive` was false, which is exactly the guard that stops the fleet
  killing live work.
- The stopped-power and confirmed-inactive gates did not apply: the VM was on
  and the runner was busy.
- `DrainExecutor` would have aborted any drain that reached it, on fresh
  runner-scoped busy evidence, which is ADR 0033 working as designed.

Every one of those is a judgement about **whether work is happening**. The fleet
had no judgement about **how long one instance may hold the host**, and no
measurement to base one on. `domain.Instance` carried `AssignedSince` and
`RunningSince`, both derived from the durable row's `updated_at`, and both reset
by the state change that made the instance busy. Nothing carried the instant the
instance began occupying its vector, so the question could not even be asked.
There was no metric for instance occupancy age, no warning, and no cap. The leak
was found because the repository owner asked why a release was slow.

### Why ADR 0005 deferred this

ADR 0005 refused to describe its backfill as duration-bounded, and its reasoning
still holds exactly: "The controller does not have a trustworthy remaining-runtime
estimate for a GitHub Actions job and does not terminate a legitimate busy runner
merely to meet an internal scheduling deadline." It named what would be required
to revisit that — "an explicit, enforceable job-runtime contract and separate
replay evidence" — and this record supplies both.

The distinction that makes it safe is that the contract is not an estimate. The
fleet still has no idea how long a job will take. It has an operator's statement
of how long a profile's work *may* take, which is a different kind of claim: a
declared ceiling, not a prediction, and one whose violation is a fact rather
than an inference.

## Decision

### A profile states a ceiling; an instance carries a clock

`domain.Profile` gains `OccupancyBudget`, a wall-clock ceiling on how long ONE
instance of that profile may hold its resource vector. It belongs to the profile
because it is a statement about the work that profile runs: a macOS builder
legitimately spends forty-plus minutes on an App Store archive, and a one-core
Linux job that has been running an hour is not doing the work it was spawned
for.

`domain.Instance` gains `OccupiedSince`, populated from the durable row's
`created_at`. That choice is the whole measurement. `AssignedSince` and
`RunningSince` read `updated_at` because each bounds a dwell time in one state;
occupancy is not a dwell time. The host has been short those cores since the row
was created — a `Planned` instance already charges the envelope, see
`ConsumesHostResources` — and it does not get any of them back when a runner is
promoted from `Assigned` to `Running`. Reading `updated_at` here is precisely why
the incident was unmeasurable: an instance that had held six cores for
seventy-five minutes looked one state change old.

`Instance.Occupancy(now)` answers with a `(duration, measured)` pair rather than
a duration, so "not measurable" can never be read as "just started".

### Honest defaults, three configured states

Configuration states `occupancyBudgetSeconds` per profile, with three
distinguishable values:

| value | meaning |
| --- | --- |
| absent | unstated; the platform default applies |
| `0` | stated as unbounded; this profile is never reaped for age |
| *n* | this many seconds |

The platform defaults are **two hours on macOS** and **one hour on Linux**. They
are sized to the work each platform does on this fleet rather than to a round
number: macOS runs the builders, whose longest healthy run measured here is
forty-plus minutes, so the ceiling is roughly three times it; Linux runs the
fleet's own CI and short jobs, and an hour is already far outside anything that
has finished normally there. Both sit well inside GitHub's six-hour job maximum,
so the fleet reclaims the vector before the platform gives up on the job anyway.

A stated ceiling must be between five minutes and six hours. The floor stops a
typo turning the fleet into a job killer; the ceiling is GitHub's own maximum,
past which a budget can never fire and is therefore a mistake rather than a
policy.

Zero means no ceiling, and that is the fail-open default in code on purpose: a
destructive bound is never inferred from a configuration that does not state
one. The defaults are applied where configuration is translated into scheduler
policy, not where it is decoded, so a configuration file that does not mention
the field is still written back byte-for-byte identically and stays decodable by
an older strict release.

### The reap is the fifth recovery cause, and it is evaluated last

`occupancyExceeded` joins `confirmedInactive`, `stalledAssignment`, and
`lingeringRunner` in `assignmentRecoveries`. It requires a configured ceiling, a
powered-on VM, and a measurable occupancy, and fails closed on each.

It is deliberately evaluated **last**. Every other cause is evidence that no
work is happening, and each re-verifies that evidence at execution time and
aborts when it turns out to be wrong. A budget breach claims only that the hold
is too long. So whenever a safer cause also applies to the same instance, the
safer cause acts and the budget stands down.

The cause is part of the content-addressed operation identity, exactly as the
other three are, so a budget reap and a lingering-runner reap of one instance
are distinct attempts under ADR 0028's generation rule.

The drain operation carries the demand key, which no other drain does. Every
other drain cause implies there is no job left to name; this one ends a job
GitHub still believes is running, so the durable payload names the repository,
run, attempt, and job that were cut.

### The guest is stopped before the runner is deregistered

`DrainPhaseOccupancyBudget` is the one phase a busy runner does not disprove.
Two exceptions follow, and both are consequences of that single fact rather than
independent decisions.

First, the phase does not consult the busy guard, and a `runner_busy` refusal
does not abort it. Aborting returns the instance to `Running` with its vector
still held, and the next tick re-derives the same reap, so an abort here is an
infinite loop that reclaims nothing.

Second, the ephemeral guest is powered off **before** deregistration rather than
after it. GitHub refuses to remove a runner it considers to be executing a job,
and it would go on refusing for as long as the hung job ran, so deregistering
first can only retry until the operation dead-letters — with the vector still
held. That is the incident, not a fix for it.

Reclaiming a vector from a job that is still running requires ending the job,
and this is how the fleet ends it: by asking the ephemeral guest to power down.
It is the same graceful `Stop` every drain already performs, only ordered first.
No signal is sent to any process, nothing is killed, and the executor port's
stop is idempotent, so the ordinary stop later in the teardown chain is
unaffected. GitHub then reports the job as a lost-communication failure, and the
runner it would not remove becomes removable.

Deletion confirmation joins the phases that derive job inactivity from runner
absence, for the mirror-image reason the stalled and lingering phases do: there
will never be a completion event for a job that was cut, so waiting for one
would wait forever.

### Visibility before the cap, and truth in the reaping

`scheduler.Occupancies` is one pure projection of the same inputs the planner
uses. The metric, the warning, the `fleet doctor` finding, and the reap all read
it, so they cannot disagree about a hold. It reports, per instance, the age, the
ceiling, whether the hold has passed three quarters of the ceiling, whether it
has passed the ceiling, and — the part that matters most — whether **queued
demand would fit the vector being held**.

That last field is the difference between a slow job and a fleet incident.
An over-budget hold with nothing waiting is a long job, which is allowed. An
over-budget hold with work that fits behind it is 2026-08-09. `fleet doctor`
fails only on the conjunction, and `fleet status` renders it as `STARVING` in a
per-instance occupancy table.

Telemetry publishes `fleet_instance_occupancy_seconds`,
`fleet_instance_occupancy_budget_seconds`, and
`fleet_instance_occupancy_starving`, all labelled by profile and instance. They
are per-instance rather than per-profile because the fault is one instance
holding one vector too long, which an aggregate averages away; the published set
is bounded exactly as the dead-letter set is. The budget travels beside the age
because the age alone is unreadable — forty minutes is healthy on a builder and
a leak on a small Linux profile.

Two warnings are logged. One when a hold crosses three quarters of its ceiling
and again when it passes it, rate limited per instance and per state so an
escalation is never suppressed by the warning that preceded it. One when the
reap is planned, never rate limited, naming the job, the vector, how long it was
held, the ceiling, and that the job will end as a lost-communication failure on
GitHub. A reaped job and a flake look identical from GitHub's side; that line and
the operation payload are what tell them apart.

### The bound is a simulated property

The DST harness (ADR 0031) gains a generator event for a job that outlives its
expectation, and property (k): **no instance occupies its vector beyond its
profile budget**, allowing a bounded grace for the reclaim itself to complete.
The harness previously modelled only jobs that finish, and `long_job` models a
legitimately long suite; the new event models work that has stopped making
progress and will not end on its own. Its oracle reads the harness's own virtual
ground truth rather than the scheduler's arithmetic, so it cannot inherit the
defect it exists to catch.

## Consequences

The fleet can now state and enforce a bound it previously could not express, and
an operator can see a hold while it is happening instead of reconstructing it
afterwards. A vector is reclaimed within the ceiling plus one reclaim, rather
than whenever GitHub loses patience.

The cost is real and worth naming: **this is the first mechanism in the fleet
that can end a running CI job.** Four things bound it. It fires only on a
ceiling an operator configured, only on a profile, only after a measured
wall-clock duration, and only after every safer reclaim cause has declined. Its
defaults are generous by construction — three times the longest healthy macOS
run measured here — so a job it cuts has been doing nothing useful for longer
than any job on this fleet has ever legitimately taken. A guard test pins the
other side at budget-minus-one-second, and a healthy simulated job can never
reach any simulated profile's ceiling.

A profile whose real work grows past its ceiling will start losing jobs, and
that surfaces as a reap warning and a lost-communication failure rather than as
a slow queue. The remedy is to raise the ceiling, which is why it is
per-profile, operator-stated, and printed beside the age in every surface that
reports one.

Two things are deliberately not addressed. The budget does not distinguish a job
that is hung from one that is merely long — it cannot, and pretending otherwise
would be the untrustworthy remaining-runtime estimate ADR 0005 rejected. And it
does not bound an instance that never reaches `Assigned` or `Running`; the boot
timeout and the assignment deadline already own that ground.
