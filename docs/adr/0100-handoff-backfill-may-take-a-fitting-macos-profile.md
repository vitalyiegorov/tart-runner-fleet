# ADR 0100: The macOS handoff's one-shot backfill may take a fitting macOS profile

## Status

Accepted, #494. Amends ADR 0005 on mixed-profile-cohort nodes (ADR 0024).

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
cohorts. That profile gets one wave of spawns that fit beside the live work,
without drains. The `BackfillAdmitted` latch is shared, so a handoff still
admits at most one backfill wave, Linux or macOS. Linux and macOS spawns still
never share a plan.

The spent latch also binds `planBehindInfeasibleMacHead`. When the Linux jobs end
while the backfilled wave still runs, the head is blocked only by that wave, and
planning moves to that path. Without the carried latch, overlapping replacement
waves could keep the head's vector from ever freeing.

No new state, knob or pass.

## Consequences

A feasible maestro no longer idles behind a builder that is waiting on busy
Linux. The builder's extra wait is bounded by one backfill wave, which is the
same cardinality bound ADR 0005 accepts for Linux; like ADR 0005 it is not a
duration bound. Single-cohort nodes are unchanged.

Evidence: `TestIssue494MacHandoffLivenessSeed19` (simulation replay, red before
the change) and `TestMacHandoffAdmitsOneFittingMacWaveBesideBusyLinux`
(scheduler, red before the change).
