# ADR 0100: The macOS handoff's one-shot backfill may take a fitting macOS profile

## Status

Accepted, #494; extended by #529. Amends ADR 0005 on mixed-profile-cohort nodes (ADR 0024).

## Context

ADR 0005 gives a blocked macOS handoff one backfill: a single aged smallest-tier
Linux demand. That was written for single-cohort hosts, where nothing macOS could
run beside the head's drain.

With mixed cohorts a younger macOS profile can fit beside the busy Linux work that
blocks the head. The nightly simulation found the result on `m4-mac-mini`, seed 19:

- a `builder` head (6 CPU) cannot fit while a `large` and a `small` Linux job run;
- a `maestro` behind it (4 CPU) fits in the 5 CPU left;
- the handoff only offers Linux, so the maestro waits until the Linux jobs end,
  up to the 60-minute occupancy budget when they overrun.

`planBehindInfeasibleMacHead` already lends this capacity when no Linux is live.
The busy-Linux arm was the only path that did not.

## Decision

The handoff's existing one-shot budget may be spent on the next macOS profile
behind the head when no Linux backfill is admitted and the node allows mixed
cohorts. That profile gets one wave of spawns that fit beside the live work:
up to its MaxActive, with no aging requirement, and without drains. The `BackfillAdmitted` latch is shared, so a handoff still
admits at most one backfill wave, Linux or macOS, and that wave is never mixed.
(Other passes in the same tick, such as `fillLinuxRemainder` under mixed-platform
admission, may still add Linux spawns beside it.)

The spent latch also binds `planBehindInfeasibleMacHead`, but only while live
macOS work of ANOTHER profile blocks the head. The head's own profile does not
count: a live or idle instance of it holds the head by MaxActive, not by a
backfill wave. When the Linux jobs end
while the backfilled wave still runs, the head is blocked only by that wave, and
planning moves to that path. Without the carried latch, overlapping replacement
waves could keep the head's vector from ever freeing. With nothing macOS live
the head is simply infeasible, and refusing there would bring back the wedge that
path exists to prevent.

No new state, knob or pass.

## Consequences

A fitting macOS wave no longer idles behind a builder that is waiting on busy
Linux.

#529 extends this to later arrivals. Once the lent wave has ended, with no
other-profile macOS instance live, and Linux still blocks the head, the handoff
lends the next fitting wave. The Linux one-shot backfill stays one-shot. Waves
never overlap: a new one is lent only when the previous one is gone.

The builder is waiting on the Linux jobs either way. When they end while a wave
runs, the spent latch stops `planBehindInfeasibleMacHead` from starting another,
so the builder's extra wait beyond Linux is still bounded by one backfill wave. That is a cardinality
bound like ADR 0005's, but wider: up to MaxActive spawns of one profile rather
than one aged smallest-tier job. Like ADR 0005 it is not a duration bound. Single-cohort nodes are unchanged.

Known limit: the gate asks whether live macOS of another profile exists, not
whether that work is what keeps the head from fitting. If the head could not fit
even with every such instance gone (external load, or a head more than twice the
backfill profile), fitting maestros run in serialized waves instead of
continuously. The wait is bounded by the running wave's duration, because the
gate opens once that work ends, but the simulation's property (a) may still
report it as a wedge. With the
scheduler test profiles (builder 8 CPU, maestro 4 CPU) it cannot occur.

Evidence: `TestIssue494MacHandoffLivenessSeed19` (simulation replay, red before
the change) and `TestMacHandoffAdmitsOneFittingMacWaveBesideBusyLinux`
(scheduler, red before the change).
