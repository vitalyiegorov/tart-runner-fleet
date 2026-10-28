# ADR 0085: A standing offer waits only for Linux work

## Status

Accepted. Issue [#439](https://github.com/vitalyiegorov/tart-runner-fleet/issues/439),
the residual recorded in the last comment on
[#428](https://github.com/vitalyiegorov/tart-runner-fleet/issues/428).
Amends [ADR 0082](0082-a-bound-scale-set-keeps-a-standing-offer-of-one.md): a
Linux set keeps the standing free slot only if its vector could start on the
node once the node's own Linux work has drained.
[ADR 0084](0084-a-gated-node-keeps-no-standing-offer.md) and
[ADR 0073](0073-scale-sets-advertise-the-residual-envelope.md) are unchanged.

## Context

ADR 0082 gives every ungated set `min(configured, max(held + fit, placed + 1))`.
Its reason for the `+1` is that the attracted job "waits in this node's queue
until admission starts it". That assumes the wait is bounded by work this node
will finish: Linux admission, FIFO and aging start a Linux job once the Linux
guests ahead of it end.

On a mixed-platform node that assumption fails. On 2026-10-28 (v0.1.788) the
Mac mini, `hostBudget` cpu 10 / 23552 MiB and `maxLinuxCpu` 10, ran two pony
maestro guests (4 CPU / 7 GiB each). They held 8 of the 10 CPUs from 05:15Z
for about 1h50m, within their 10800 s occupancy budget. The mini was not gated,
only full, so its `linux-4x8` sets kept the `+1`:

| Job | Queued | Placed on | Waited |
| --- | --- | --- | --- |
| knee-doctor Quality Checks | 05:18Z | mini set 18 `knee-repo/linux-4x8` | 37+ min |
| budgie diagnose | 05:53Z | mini set 23 `budgie-org/linux-4x8` | still queued, while the MacBook had room |

No Linux work on the mini was holding those 4 CPUs. Every Linux guest there
could have drained and the 4x8 still could not start before about 07:05Z.
GitHub does not move an assigned job. The same shape appeared on 2026-10-26
20:48Z, with a `linux-go-4x8` job waiting 77 minutes.

## Decision

`scheduler.FitsOnceLinuxDrains(in, profile)` answers one question: could one
instance of this Linux profile start on this node once every Linux guest and
queued Linux demand here has ended? `scaleSetHeadroom` records the negative as
`ScaleSetHeadroom.Stranded`, and `AdvertisedCapacity` withholds the standing
slot from a stranded set exactly as it does from a gated one:

    min(configured, held + fit)                        gated or stranded
    min(configured, max(held + fit, placed + 1))       otherwise

The question reuses the envelope the planner admits against. It does not build
a second model:

- `freeCapacity(in, outlasting, aged=true)`, the function behind `linuxFree`
  and `linuxFreeAged`, asked of the occupancy Linux drain cannot release:
  every live instance that is not Linux. That keeps the Linux-only cap, the
  physical total, and the host budget, in both the elastic and the static model.
- Every queued macOS demand that no guest incarnates yet is charged with
  `exhaust`, the way `PartitionHeadroom` charges queued work. It takes its
  vector before a later Linux job can (the other cohort's reservations).
- The measured residual (`Host.Available`) is left out. It is net of the very
  Linux guests that would drain, so keeping it would strand a node that is
  full of Linux work and reverse #422. A host tenant's pressure is ADR 0084's
  gate, not this rule.

What stays the same:

- **#422's guarantee.** A node full of drainable Linux work still offers `+1`.
  Every #422 and #428 reproduction was a Linux-full or gated node, and each
  keeps the behaviour it had.
- **Never below held, never above configured.** A stranded set advertises
  `min(configured, held + fit)`, so work GitHub already placed is kept, and a
  set configured at 0 advertises 0.
- **macOS sets are unchanged.** For a macOS profile the function is always
  true. Its wait is bounded by its own cohort, which drain-and-switch and
  `MaxActive` manage, and #422's shared-label Mac scenarios depend on it.
- **ADR 0084's gated rule** is unchanged and composes by "or".
- **Linux-only nodes** have no non-Linux occupancy and no macOS demand, so the
  answer is "fits". The one exception is a vector the empty node cannot hold
  at all, which the node could never start anyway.
- Admission, the scheduler, and session yield are unchanged. The rule decides
  only what GitHub may assign.

## Trade-off

The cost is #422's: if every node serving a job's labels is a mixed node whose
macOS guests hold what the job needs, no set offers room and the job is
assigned to no set. GitHub does not reconsider that placement when a guest
exits, so the job stays queued until an operator cancels and reruns it once a
matching set advertises room. The `linux-go-*` aliases are served only by the
two Macs today, so that state is possible while both run maestro suites.
`linux-portable-*` jobs are also served by the amd64 Linux nodes (Omarchy and
Ubuntu ran `linux-portable-4x8` jobs on 2026-10-28), and those Linux-only nodes
keep the standing offer. We accept it for the same reasons as ADR 0084:

1. The other side is measured, not hypothetical. A job placed behind a macOS
   guest waits for that guest's lifetime, up to its occupancy budget, and a
   matching sibling often has room meanwhile (2026-10-26 and 2026-10-28).
2. Withholding is bounded to the shape that causes the long wait. A Linux
   set behind Linux work, and any set whose vector fits beside the macOS
   guests (the mini's 2x4 sets), keeps the slot.
3. The `doctor` alarm for a REST job assigned to no set, owed by ADR 0082 and
   ADR 0084, covers this case too.

## Rejected

- **Withhold the slot whenever `fit == 0`.** That is ADR 0082 reversed, and it
  strands #422's 15:33Z shape (rejected in ADR 0084).
- **Model macOS guests' remaining time.** A guest's remaining runtime is not
  observable, and the occupancy budget is a ceiling, not an estimate. The
  question "could Linux drain alone free it" needs no clock.
- **Charge the measured residual too.** It already includes the Linux guests
  that drain, so a Linux-full node would read as stranded (see Decision).
- **Apply the rule to macOS sets as well.** A macOS job behind a macOS guest
  waits for the cohort it belongs to, and #422's
  `TestIssue422AJobQueuedWhileEveryNodeIsFullIsAssigned` is that shape.

## Consequences

- `TestFitsOnceLinuxDrainsAsksOnlyWhatLinuxWorkCannotRelease`: on a mini
  budget, a 4x8 does not fit beside two maestro guests and a 2x4 does. Linux
  guests, queued Linux demand, and a spent measured residual do not strand. A
  queued maestro does. The static envelope agrees, and macOS profiles and
  Linux-only nodes always fit.
- `TestScaleSetHeadroomStrandsAVectorOnlyMacOSGuestsHold`: beside two maestro
  guests the 4x8 set advertises 0 (1 before) and the 2x4 set 1. A held 4x8 job
  is kept, and a node full of 4x8 Linux work keeps 1.
- `TestAdvertisedCapacityOnlyLowersAndNeverBelowWhatIsHeld`: stranded rows
  never go below held and never above configured.
- `TestIssue439MacOSGuestsLeaveTheLinuxVectorToASibling`: two long maestro
  suites fill the mini. A 4x8 queued beside them goes to the sibling, a 2x4 is
  assigned and starts here, and a 4x8 queued after they end runs here.
- `TestIssue388SmallNodeStopsAdvertisingAVectorItCannotStart` now sends all
  three 4x8 jobs queued beside the MacBook's maestro to a sibling, not two of
  them to the node. That is ADR 0073's original intent for #388, which ADR 0082
  had given back for this shape.
- Every #397, #422 and #428 simulation is unchanged and green.
