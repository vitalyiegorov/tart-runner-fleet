# ADR 0076: A node's residual is offered once, not once per scale set

## Status

Accepted. Amends [ADR 0073](0073-scale-sets-advertise-the-residual-envelope.md):
it replaces that record's rejection of dividing the residual, its definition of
`held`, and its fail-open rule for a tick that is not ready. Issue #397.

Amended by issue #412 (2026-10-23): a set the division leaves at zero is
floored at one when one instance of its vector fits the residual left after
waiting work. GitHub places a job only on a set advertising room when the job
is queued and does not move it later, and a shared-label set counts a REST job
as waiting only once GitHub has assigned it there. On v0.1.725-v0.1.730 the two
amd64 nodes advertised room on about three of their fourteen sets at a time, and
rnw-community and budgie-at jobs queued while their sets read zero stayed queued
for up to three hours with both nodes idle; the later runs of the same branch
waited behind them on their concurrency group. The floor bounds over-offering
to one instance per set -- at most one job per scope on a node with any room --
instead of the whole residual per set that caused #397.

## Context

The Mac mini and the Mac Studio each serve seven scopes. Every scope binds one
maestro set on each Mac, and both sets carry the same labels (ADR 0034 as
amended). GitHub fills each set to the capacity it last advertised and never
moves a job it has placed.

Under ADR 0073 every set advertised `min(configured, held + fit)`, and `fit` was
the whole node's residual, computed separately for each set. A Mac with one free
slot therefore offered that slot to seven scopes. ADR 0073 expected this
over-offer to last one poll. The fleet databases show it is not small:

- From 2026-10-19 00:00 to 2026-10-22 10:00 (82 h), **80** maestro jobs were
  placed on a Mac with no free slot while the other Mac had one. They waited
  30.5 job-hours.
- In the same window a Mac sat idle, with nothing of its own queued, while its
  sibling had jobs waiting, for **22.0 slot-hours** (6.4 a day).

Two more terms made the offer larger than the node:

- `held` counted every non-deleted guest, including failed guests and guests
  tearing down after their job. GitHub no longer counts either as assigned, so
  each one advertised a slot that GitHub then filled.
- A tick that could not plan dropped the view. Every set then advertised its
  configured maximum, at exactly the moment the node could start nothing.

ADR 0073 rejected dividing the residual. Its reason was that with ten scopes and
two free slots, eight sets would advertise nothing and a single-node scope would
wait on an idle machine.

## Decision

**One residual, divided once.** `scheduler.PartitionHeadroom` replaces
`scheduler.Headroom`. It takes one claim per scale set on the node, across every
profile and both platforms, and grants units of each claim's vector one at a
time. Each grant is charged exactly like one more waiting demand: against the
same envelopes the planner admits with, after every queued demand, and within
each macOS profile's MaxActive. So everything the sets are offered beyond what
they hold could start on the node at once. Two profiles that share one budget
(a 2x4 and a 4x8 set, or a maestro and a builder set) also share their offer.

**Fairness is answered by order, not by withholding.** Units are granted round
robin, in two passes:

1. First, to sets whose scope GitHub shows has work queued that this node has
   not been handed (the REST `Observed` count above `Delivered`, which every tick
   already computes for issue #292). Each such set gets up to that number. A
   scope with a job waiting gets the free slot within one REST refresh, whichever
   set the rotation favours.
2. Then the remainder goes to every set, up to its room: its configured capacity
   less what it holds. A set at its ceiling never absorbs a share it could not
   advertise.

Each round starts at a position that rotates every 30 seconds, one REST refresh.
No set is permanently last for a speculative share. With ten scopes and two free
slots, eight sets advertise only what they hold for a moment, not forever, and a
scope with a job queued is served first. The trade ADR 0073 refused is now
smaller than the defect it accepted: a fresh job may wait up to one REST refresh
for a share, against a placement that strands a job for the whole of another
job's run.

**Only work GitHub still assigns is held.** `domain.Instance.HoldsAssignment`
is true for a live incarnation of its demand that is not tearing down. A failed
guest has released its demand. A guest in the teardown chain has finished its
job or never received one, and drain guards prove its runner idle before it
gets there (ADR 0074).

**A tick that cannot plan keeps the last view.** `app.ResidualAdvertisement`,
now shared by the daemon and the simulation, changes only on a ready tick that
produced headroom. A failed tick, a blocked plan, or an unsized envelope keeps
the last view for as long as it lasts. The age bound is removed as well: a node
whose scheduler cannot plan cannot start what it is offered either. The view is
still fail-open before the first ready tick. A node that has never planned
advertises its configured capacity, so a restart does not withdraw it from
every scope.

## What does not change

- `AdvertisedCapacity` is unchanged: `min(configured, held + share)`, never
  below what is held, never above the configured ceiling (ADR 0015).
- Admission is unchanged. The partition decides only what is advertised. The
  simulation corpus digest is identical before and after.
- A set whose statistics cannot be read is still left out and advertises its
  configured capacity, as in ADR 0073. That is a store failure, not a
  scheduling state.
- Residual advertisement still applies only with `canonicalJobInventory: true`.

## Consequences

- A burst across scopes lands on the node that has the room. The simulated
  Studio (`TestIssue397ABurstAcrossScopesTakesTheFreeSlotOnce`) takes one of
  four jobs into its one free slot, and the sibling takes the other three.
  Before this change it took three.
- The mini's 2026-10-20 14:06Z tick is replayed in
  `tests/replay/shared_label_offer_incident_test.go`. It had a maestro and a
  2x4 running and 4 CPU free. Sized one set at a time, its seventeen sets
  offered 68 CPU of work, and GitHub placed a 4x8 and a maestro into room for
  one. The maestro waited 21 minutes while the Studio was idle. Sized together,
  the sets offer one vector that fits.
- Property (t), `overOfferChecker`, bounds the sum of every macOS profile's
  offers by what its MaxActive leaves. It runs in every arm. The federated
  scope's sweep, the one world that binds two sets of one profile, fails it on
  the per-set offer this record removes. The new `mac-studio-shared-labels`
  world binds three sets behind a broker that honours advertised capacity. It is
  pinned by the #397 traces, and its seed sweep waits on issue #405: at seed 1
  that sweep reaches a quiescence defect that is reproduced on `main` in every
  world and is not this change's.
- The Linux 2x4 "assigned then drained without running" rate (9–28% of
  provisions) was a measurement artifact, not a cross-node race. Those guests
  ran jobs of about 8 s that finished between two ticks, so `running` was never
  recorded. The real waste is 0–0.7%, and it is mostly cancellations. This
  record builds nothing for it.

## Open risk

The rotation is stateless and keyed to the tick's clock. A set's poll carries
the value read when the poll started, for up to about 50 s. Around a rotation
boundary, two sets may therefore hold the same speculative unit for at most one
poll. That window is ADR 0073's accepted window, now bounded to one unit instead
of one unit per set.
