# ADR 0096: A gate reopens only once it has held open

## Status

Accepted, 2026-11-03. Issue [#483](https://github.com/vitalyiegorov/tart-runner-fleet/issues/483).
Amends [ADR 0047](0047-a-node-that-cannot-admit-yields-its-sessions.md) (the
blocked bound tolerates brief openings) and
[ADR 0084](0084-a-gated-node-keeps-no-standing-offer.md) (a gated set stays
gated until the gate has held open, and offers exactly what it holds). Admission
itself, the scheduler and the #432 runner-administration rule do not change.

## Context

On 2026-11-03 ubuntu-vitalii held six jobs bound to its own scale sets, aged 35
to 65 minutes, while the Mac mini had an empty queue and Omarchy and the
MacBook had at most one fresh job each. Non-fleet work held its swap at
8144/8191 MiB and its load at 27/36/48 on 24 cores. Its admission gate flapped
between `swap pressure` and `cpu pressure` and opened for 1 to 23 seconds every
minute or two:

```
14:07:05 closed swap pressure (openFor=23s)
14:07:42 opened (closedFor=37s)
14:07:43 closed swap pressure (openFor=1s)
14:08:44 closed cpu pressure  (closedFor=1m1s)
14:09:05 closed swap pressure (closedFor=21s)
14:09:14 opened (closedFor=8s)
14:09:21 closed swap pressure (openFor=7s)
14:10:13 closed cpu pressure  (closedFor=52s)
```

Both tools the fleet has for this node read the gate tick by tick, and both
failed for that reason.

1. **The advertisement (ADR 0084).** A gated set advertises only what it holds.
   But a tick that lands in a seven-second opening sees a healthy host: every
   set goes back to ADR 0082's standing offer and to the residual that opening
   measured. That view stays in force until the next ready tick, and a long
   poll carries the figure it began with for up to a minute (ADR 0073). So each
   opening invited a job per set, and every job it attracted then waited for a
   node that refused admission almost all the time.
2. **Session yield (ADR 0047).** Withdrawal needs `BlockedFor` (ten minutes) of
   *continuous* refusal. Any allowed tick restarted the streak, so the clock
   never got there. On this node it would not have mattered: it ran guests, and
   live work rightly holds the sessions, because they are also how the running
   guests report completion and deregister (issue #432). The same gate on an
   idle node would have kept its sessions forever, which issue #436 had already
   seen on Omarchy.

## Decision

**A refusal ends only once admission has been allowed continuously for
`sessionYieldHealthyForSeconds` (default 120).** Openings shorter than that
neither end the refusal nor restart it. `app.RefusalEpisode` states this once,
and both tools read it:

- **The advertisement.** `ResidualAdvertisement` folds each ready tick's gate
  (`hostHealthy`, unchanged) into one episode and marks every set gated while
  the episode lasts. A gated set advertises exactly `min(configured, held)`:
  no standing slot and no share of the residual. This also changes a steadily
  gated set: ADR 0084 took away only the standing slot, so a set gated by a
  guardrail that leaves memory free (disk, inodes, CPU) still advertised
  `held + fit`. It now advertises `held`. An update drain still is not a gate.
- **Session yield.** The episode replaces the continuous-refusal streak. An idle
  node withdraws once the episode, less any time with work in flight, reaches
  `BlockedFor`, and only on a tick the gate actually refuses. The episode
  outlives the last refusal by `HealthyFor`, so without that condition a node
  that had just recovered would close its sessions and reopen them a tick later. Live instances and retrying operations still hold the sessions
  and restart that clock, exactly as before. A withdrawn node still rejoins
  after `HealthyFor` of continuous admission, which was already the rejoin rule.
  `sessionYield.since` now dates a refusal from its first refused tick, not
  from the last flap.

The two tools share the episode type, not its input. Yield feeds it the
admission gate, which an update drain closes. The advertisement feeds it
`hostHealthy`, which an update drain does not. That difference predates this
ADR and is kept.

The bound is `sessionYieldHealthyForSeconds`, not a new knob. It already means
"how long admission must hold before this node competes for work again"; the
advertisement asks the same question, so it reads the same answer, whether or
not session yield is enabled.

### Invariants

- A session a live instance or busy operation reports through is never cut
  (#432). A busy pressured node stops attracting work through its
  advertisement, not by losing its sessions.
- A node never withdraws, or stops advertising, on a fact it could not
  establish. Only a ready tick with a fresh host observation and at least one
  measured scale set moves the advertisement's gate (ADR 0073's fail-open rule
  stands), and yield still folds only ticks whose operation count was
  readable.
- A node that is truly healthy rejoins after `HealthyFor`, and its sets regain
  the standing offer and their residual share after the same bound.

## Consequences

- The episode is in memory only. A daemon restart starts with none, so a node
  whose gate is flapping offers its standing slot and residual again if its
  first ready tick lands in an opening, for up to one long poll. Starting every
  daemon inside an episode would instead withhold offers for `HealthyFor` after
  each restart, including a fleet-wide update, on nodes that are healthy; that
  cost is the larger one.
- `TestSessionYieldNeverWithdrawsARecoveredNode`: an idle node refused for
  8m5s and healthy from then on never acts on its sessions.
- `TestResidualAdvertisementAnEmptyHeadroomDoesNotMoveTheGate`: three minutes
  of ticks with no measured set neither end a refusal nor change what the set
  advertises.

- `TestSessionYieldWithdrawsThroughBriefGateOpenings`: a gate refusing 60 s of
  every 65 s withdraws an idle node exactly at `BlockedFor`. It never withdrew
  before.
- `TestSessionYieldKeepsABusyFlappingNodesSessions`: the same gate with two
  live instances never withdraws, publishes the refusal from its first tick,
  and withdraws a full `BlockedFor` after the guests finish.
- `TestResidualAdvertisementHoldsTheGateThroughBriefOpenings`: with two live
  guests, a one-tick or seven-second opening advertises the two held jobs, not
  the standing slot or the measured residual; two minutes of continuous
  admission restores held plus fit.
- `TestIssue483ABusyNodeWhoseGateFlapsAttractsNoJob` (simulation): a job queued
  just after a brief opening goes to the idle sibling, and the job queued after
  the gate has held open is assigned here and runs.
- Every #388, #397, #422, #428 and #439 simulation is unchanged. Their worlds
  keep a zero reopen bound, which follows the gate tick by tick.
- **Cost.** After a real recovery, a node's sets regain the standing offer two
  minutes later than before. In the #422 shape (every matching node full or
  gated at once) a job queued in those two minutes can be assigned to no set.
  That window is the one ADR 0084 already accepts while a gate is closed,
  extended by `HealthyFor`, and it applies only to a node that was gated, never
  to one that was merely full or draining.

## Rejected

- **Withdraw the bindings that hold no live instance.** It would stop intake on
  the idle sets, but the busy sets would keep their standing offer through
  every opening, and a busy set is exactly where the next job for its scope and
  profile lands. It also adds a per-binding yield policy beside ADR 0063's
  ladder. The advertisement already reaches every set and keeps every session.
- **Count a refused fraction over a window.** It needs a ring of samples and a
  threshold, and an operator cannot read "84 % refused" off the gate log.
  Continuous admission for a bound is the rule the rejoin side already uses.
- **Cancel open polls when the gate closes.** ADR 0073 rejected this for the
  session lifecycle cost, and it would not help: the poll that matters began
  during an opening, which the held gate no longer advertises.
- **Hysteresis in the admission gate itself.** Admitting during an opening
  starts this node's own bound jobs sooner, which is what those jobs need. The
  gate decides what may start; this ADR decides only what is offered.
