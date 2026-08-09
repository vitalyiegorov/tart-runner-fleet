# ADR 0024: macOS profiles may coexist when their exact vectors fit

## Status

Accepted, default off. Completes for macOS profiles what
[ADR 0012](0012-shared-cross-platform-capacity.md) decided for platforms.
[ADR 0014](0014-opt-in-macos-exclusive-admission.md)'s exclusive policy is
unchanged and still available.

## Context

ADR 0012 relaxed platform exclusivity on the grounds that "platform identity is
not itself a resource": Linux and macOS instances coexist whenever their
combined vectors fit the envelope. Profile exclusivity *within* macOS remained.
`planMacOS` spawns a profile only after every foreign-profile macOS instance is
drained, a busy foreign instance blocks the spawn outright, and
`macProfileCanGrow` vetoes growth the moment any other profile is live. One
macOS profile cohort at a time, switched by drain-and-wait.

The invariant simplified handoff reasoning when macOS admission was young. It
is now the dominant throughput limiter on the production host. Measured on
2026-07-30: one 6-vCPU builder running, two 4-vCPU maestros queued, four of ten
physical cores and ten GiB of memory idle, and the queue SLO breached — builder
6 + maestro 4 is exactly 10 vCPU and the memory fits, so the *only* refusal was
profile identity. In a CI topology where builders produce the app and maestros
run its test shards, every build serializes against every test wave, in both
directions, forever.

Profile identity, like platform identity, is not itself a resource.

## Decision

Add `macosBurst.mixedProfileCohorts`, default false. When enabled:

1. **Coexistence first.** `planMacOS` attempts to spawn the chosen profile
   beside the live foreign cohort before considering any drain. The spawn path
   is the ordinary one — exact vectors, the physical total net of every live
   reservation, the measured residual, profile `maxActive`, repository caps,
   and the elastic envelope's aged/young distinction all apply unchanged — so a
   coexisting spawn can never overcommit the machine.
2. **Drain-and-switch survives as the fallback.** When the chosen profile does
   not fit beside the foreign cohort, the existing path runs exactly as before:
   idle foreign instances are drained and the spawn depends on those drains; a
   busy foreign instance is never touched. Coexistence adds an option, never an
   interruption.
3. **Growth loses only the identity veto.** `macProfileCanGrow` keeps counting
   the target profile against `maxActive`; a live foreign profile no longer
   vetoes by existing. The envelope is the veto.

Default false preserves the single-cohort behavior byte-for-byte, including
the drain-and-switch choreography and every dependency edge.

### Amendment 2026-08-05: the remainder pass chooses a target, not a cohort

Rule 3 says the veto is the envelope. `macProfileCanGrow` implements exactly
that, and `planMacOS` asks it about the profile the QUEUE wants. `fillMacRemainder`
— the complementary pass that admits macOS work in the envelope a Linux tick
leaves over ([ADR 0029](0029-remainder-admission-behind-a-reservation.md)) —
asked it about the profile that happens to be LIVE:

```go
target, active := activeMacProfile(augmented.Instances.Value)
if active && !macProfileCanGrow(augmented, target) { return plan }
```

So a `builder` sitting at its own `maxActive` of one answered "cannot grow" and
the whole pass returned before it ever looked at the queue. Identity was still
the veto there, one call site behind the rule. Deterministic simulation found it
as a liveness wedge — twelve consecutive ticks admitting nothing while a queued
`maestro` fit the four free cores exactly (ADR 0031 property (a); issue #208,
seed 210) — which is the same shape, and the same size, as the 2026-07-30
measurement that motivated this record.

The remainder pass now names its target the way `planMacOS` does: with mixed
cohorts, the highest-priority queued profile that still has room, in the
aged-FIFO order of [ADR 0004](0004-bounded-control-plane-priority.md). A profile
at its `maxActive` ends its own turn rather than the pass, exactly as an
exhausted repository cap already does inside `appendMacSpawns`. Nothing about
admission itself changes: every live instance, every spawn this tick already
plans, any reserved head, `maxActive`, and the repository cap are charged before
a demand is selected.

With `mixedProfileCohorts` off the live cohort is still the only admissible
target and identity still vetoes, so the default remains this record's
"byte-for-byte" promise.

### Amendment 2026-08-09: a target the residual cannot hold ends its own turn

The amendment above moved the target from the live cohort to the queue and made
a profile at its `maxActive` end its own turn rather than the pass. It named the
target on `maxActive` **alone**, and rule 3 above says the veto is the
**envelope**. So the seam kept one more instance of its own bug: a profile with
`maxActive` room but a vector larger than the residual won the pass's single
target, `appendMacSpawns` refused it on the envelope, and `fillMacRemainder`
returned having offered the queue nothing else.

It reached production on 2026-08-09 at ~18:10Z on the Mac mini (Mac16,10: 10
cores, 24 GiB), running v0.1.394+main.d9c8ed23f842 — the build that shipped the
amendment above:

- a live Linux `xl` (6 CPU / 12288 MB) from `rnw-community/rnw-community` was
  busy, leaving four cores and ~11 GiB free;
- the aged global-FIFO head was a second `xl` from that same repository,
  infeasible beside it, so `planLinux` held its reservation and
  [ADR 0029](0029-remainder-admission-behind-a-reservation.md)'s vector
  condition correctly released the residual;
- that repository's cap is 4 with one instance live, so
  [ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)'s slack
  (`4 - 1 - 1 = 2`) let its own macOS work bid;
- three `maestro` demands (4 CPU / 7168 MB) from it, queued 29 minutes, fit
  those four cores **exactly**;
- an older macOS `builder` (6 CPU / 12288 MB), queued 38 minutes with no sibling
  live, took the target and could not fit.

Every documented condition was satisfied and the maestros were refused on every
tick anyway. The cost has the units ADR 0029 stated: 4 cores and 7168 MB idle
for the duration of the blocking job, on a host reporting 58% CPU idle, with the
queue SLO breached.

The target is therefore the highest-priority queued demand the pass can
**actually admit**, judged by every veto `appendMacSpawns` will apply, read
against the same inputs it reads: profile `maxActive`, the aged-or-throttled
envelope for that demand, and the repository cap over live instances plus this
tick's planned spawns. A candidate failing any of them ends its own turn; only
an empty candidate list ends the pass.

Skipping such a candidate does not invert the aged FIFO of
[ADR 0004](0004-bounded-control-plane-priority.md). A demand the residual cannot
hold is not waiting on this pass — it is waiting on the live instances holding
what it needs. That is
[ADR 0017](0017-infeasible-reservation-residual-backfill.md)'s rule, and the
same reason an infeasible reserved head lends its vector rather than stranding
it. Where the older candidate *does* fit the residual, it still takes it, and
that direction is pinned by test.

Nothing about admission itself changes here either, and the single-cohort
default is untouched: it returns before this ordering is reached.

## Consequences

On the production topology, builders and maestros pipeline instead of
serializing: a 6-vCPU build and a 4-vCPU test shard share the 10-core machine
that fits them both. The throughput gain is largest exactly where the SLO
breaches were observed — mixed build-and-test workloads.

Packing two profiles tightens the envelope for everything else; the second
pilot's yield-to-host behavior and the aged starvation guard are what keep that
safe on a shared machine. The exclusive policy of ADR 0014 remains available
for experiments that need an isolated cohort, and takes precedence when set.

The scheduler's drain-safety invariant is untouched: no path introduced here
drains anything, and the fallback drains only idle instances, as it always has.

## Evidence

- `internal/scheduler`: a maestro spawning beside a busy builder with no drains;
  flag-off behavior preserved byte-for-byte; the physical bound holding at
  exactly 10 vCPU; `maxActive` holding under coexistence; a busy foreign cohort
  never drained when the head does not fit; and the idle drain-and-switch
  fallback intact with its dependency edges.
- `internal/config`: flag decode, encode, and round-trip with default-off
  omitted so older strict releases still accept legacy configs.
- `internal/scheduler/scheduler_mixed_profile_test.go`, for the 2026-08-09
  amendment: `TestRemainderLooksPastAProfileTheResidualCannotHold` (the incident
  topology — the oldest maestro admitted past an oversized builder, the
  reservation intact, no drain), `TestRemainderKeepsAgedOrderWhenTheOlderProfileFits`
  (the guard: an older profile the residual *can* hold still takes it, so this is
  not a FIFO inversion), and
  `TestRemainderLooksPastAProfileWhoseRepositoryIsCapped` (the same rule on the
  third axis `appendMacSpawns` already skips by).
- `tests/replay/oversized_remainder_target_incident_test.go`: the incident over
  12 ticks — a control arm where only the oversized builder is queued and
  admitting nothing is correct, the free vector then put to work exactly once,
  the reservation surviving every later tick with no drain planned, and the
  reserved head taking the released vector FIRST ahead of everything still
  queued.
