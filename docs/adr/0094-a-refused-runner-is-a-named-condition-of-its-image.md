# ADR 0094: A refused runner is a named condition of its image

## Status

Accepted, 2026-11-05. Issue [#493](https://github.com/vitalyiegorov/tart-runner-fleet/issues/493).
It amends [ADR 0041](0041-a-base-image-declares-its-runner-version.md), whose
"recurring-staleness half" it closes, and adds the case
[ADR 0026](0026-queued-demand-expires-on-proven-absence.md) cannot reach.

## Context

At about 08:05Z on 2026-11-05 GitHub began refusing `actions/runner` 2.336.0. Nothing
in the fleet named it. Every signal it had said the nodes were busy.

**Measured on Omarchy.** Three live containers on `2.336.0` each had this in
`runner.log`, and no `Worker_` log, so none ever received a job:

```
Listening for Jobs
An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.
```

Their reclaim was `assignment never started`, the fifteen-minute stalled-assignment
deadline, after which the fleet filled the slot again. `fleet doctor` passed
`runner version`, because the image declared 2.336.0 and the floor was 2.336.0.
Omarchy's updater repeatedly logged `fleet is not quiescent`.

**Measured on the Mac mini.** A `maestro` VM lived about 90 seconds and ended as
`vm powered off` (ADR 0010's supervisor powering off after the runner exits), then
the next was spawned for the same demand: 19 such drains from 08:07Z to 08:52Z, for
a job REST kept corroborating as queued. So the job's row is never absent (ADR 0026
cannot retire it), and the only bound is the 48-hour overdue expiry (#315), about
1,900 VMs at the rate observed.

**Inferred, not measured.** The Mini's loop has the same cause. Its macOS image
declares 2.336.0 and its Linux image on 2.338.0 is healthy, but no log from a macOS
guest was read: the guests delete themselves, and polling a live one with `tart exec`
is what #478 forbids.

**The calendar could have warned.** GitHub's published dates:

| Release | Published | Plus 30 days |
| --- | --- | --- |
| 2.336.0 | 2026-07-20 | |
| 2.337.0 | 2026-08-26 | 2026-09-25 |
| 2.338.0 | 2026-10-06 | 2026-11-05 13:55Z |

GitHub states a 30-day rule (ADR 0041). 2.336.0 kept working for 41 days past the
first date and was refused about six hours before the second. One observation
cannot establish that "second newer release plus 30 days" is the real rule, and
GitHub publishes no cutoff, so this ADR relies on neither as a date.

## Decision

**A runner the broker refuses is a fact about its image: the fleet names it, stops
launching from that image, and probes it on a bound. Where the fleet cannot read
that fact, it still bounds the loop and says what it can see.**

### 1. Read the refusal where reading is safe, and only where a job cannot forge it

Beside the first stop of a teardown, a container node reads the one fixed line from
the guest's `runner.log`. Only a version matching the grammar leaves the read, so no
other guest text reaches a log or a status document. A read that fails is no
evidence. A Tart node has no reader at all (#478, ADR 0080).

That log is writable by the user that runs jobs, so a pull request from a fork could
append the line. A job must not be able to stop a node, so two rules stand between
the text and a hold:

- **A guest that ever took a job is not asked.** A real refusal never receives one.
  "Took a job" is read from what only the broker can write: the instance entered
  Running, or a job's own row names it as the runner. If that cannot be read, the
  guest counts for nothing.
- **A verdict takes two distinct guests**, naming one version, within an hour. One
  guest is only a witness. This covers the one hole left by the first rule, a job
  whose broker messages were lost, because a job can only write to its own guest.

Under a standing verdict one probe's reading renews it, because only one probe runs
at a time. A job that started after a reading clears it, witness or verdict.

The verdict lives per platform, in process, like `GuestLivenessTracker`. A rebuilt
image only reaches a daemon through a restart, so a verdict cannot outlive the image
it names. A restart forgets it and later teardowns learn it again, which costs a few
launches and can never hold back a working image.

### 2. Hold launches, and probe

While a verdict stands, `plannableDemands` receives that platform's whole queue as
withheld. Rows, queue age, the SLO and the job at GitHub are untouched; running work
is left alone; nothing is cancelled. After `RunnerRefusalProbeInterval` (30 minutes)
one guest launches, because enforcement began as brownouts (ADR 0041), so a refusal is
never taken as permanent.

A probe is due only if no guest on that platform can already answer, meaning none is
live, job-less and outside teardown. A guest in cleanup can never answer, and a stuck
cleanup or parked dead letter can stay that way indefinitely.

Which demand probes is the scheduler's decision. Each candidate is offered to
`PlanTick`, oldest first, and the first plan that actually spawns it is kept, so an
unschedulable head cannot hold the allowance. Nothing is spent until a spawn is
selected.

Only a job that **started after** the reading clears it. One already running at the
cutoff says nothing about the image today.

### 3. Name it through the existing check

`config.RunnerImage.Reason` stays the one statement of the rule, and now reads
`runner_version_refused: ...` first. `runnerVersionCheck` carries it, and
`runnerImages[]` gains `refusedVersion`, `refusedAt` and `nextProbeAt`. `belowFloor` and
`fleet_runner_image_below_floor` stay the floor verdict alone, so a refused image the
floor clears reads as what it is. It never
touches `ready` or `healthy` (ADR 0052): a release may be what ends the condition.

### 4. Read the calendar

The same predicate fails once the oldest stable release newer than an image is past
GitHub's stated 30-day window. On the incident's image that is 2026-09-25, 41 days
before the refusal, at a floor that said nothing. The daemon reads GitHub's public
release list every six hours, anonymously, so it spends none of the App's budget. A
draft or prerelease starts no clock, a failed read keeps the last answer, and a list
never read judges nothing. The finding says "may refuse at any time", not a date.
The floor keeps its meaning, a hard bar an operator states.

### 5. Keep the empty-exit backoff, because Tart needs it

A demand's deleted incarnations are counted from the durable rows. While it is still
queued, that count is exactly the instances that left without its job: demand rank
only moves forward (ADR 0026), and `alignRunnerDemand` rebinds a runner to whatever
GitHub actually handed it. No new state or migration. The first two are tolerated,
which covers a sibling taking the job first or a guest crashing once; from the third
the demand waits 5, 10, 20, then at most 30 minutes after its last exit, and is named
`demand_empty_exit_backoff` under a new `empty exits` doctor check.

Exits count against one demand on one **image generation**, stamped on the instance
row as the image reference and its declared runner version. A rebuilt image owes
nothing for its predecessor's exits, so the operator's remedy works on the first launch
after the restart. A rebuild under the same reference and declared version looks like
the same generation, which is the declaration's one way to lie (ADR 0041), and such a
hold still ends within 30 minutes. Rows older than the field are an empty generation no
node asks about, so upgrading can only forget a hold.

A hold ends by time alone, so it cannot strand a job: a job GitHub starts dispatching
again waits at most the cap. It is the only bound on a Tart node, and the only bound
anywhere on a job that one runner after another will not take while the image is fine.
It is not enough alone: the replay below still boots 28 guests on a refused image,
because each of many demands earns its own three strikes.

## What was deliberately not built

- **Nothing stops a refused node attracting work.** Its sets keep advertising, so
  GitHub may keep assigning jobs a sibling could run. Session yield (ADR 0047) is the
  natural tool, but it is node-wide and a refusal is per image. It is the next step
  if this recurs, and it is not guessed at here.
- **No read of a stopped Tart disk**, which ADR 0041 did by hand. Attaching a VM's
  disk on every teardown is too heavy a mechanism for what the calendar and the
  backoff already say.
- **No guest-side report.** The bootstrap helper is sealed in each image, so it could
  not have helped the images this incident is about.
- **No count across rotated request IDs.** A demand is one request ID. The incident's
  demand was preassigned, whose ID derives from the immutable job identity, and it
  stayed one ID across all 14 log lines that printed it. A loop GitHub re-offers
  under a new ID each time would not back off. Nothing has measured one.
- **No knob.** The bounds are constants, as `PowerCorroboration` is.

## Testability

`tests/simulation/empty_exit_test.go` and `runner_refused_test.go` replay both. Red
before the fix: one job booted 24 VMs in two hours and held a slot for 183 of 240
ticks; a refused image launched 28 guests after a first batch of 4. Review of the first
version added four more, each red first: two jobs forging the line stopped a node, a
stuck cleanup blocked every probe, an unschedulable demand held the only allowance, and
a rebuilt image waited out its predecessor's hold. Property (w),
`launch_hold`, judges every plan against the hold. It counts empty exits from the
world, so a query that lost rows would be caught rather than agreed with.

Corpus over 32 seeds at 200 ticks, spawns and findings per arm:

| Arm | Merge base | Branch, faults out of the draw | Branch, faults in the draw |
| --- | --- | --- | --- |
| `m4-mac-mini` | 1157, 0 | 1153, 0 | 944, 0 |
| `geekom-linux-amd64` | 1266, 0 | 1264, 0 | 897, 0 |
| `federated-maestro-scope` | 683, 0 | 684, 0 | 662, 0 |
| `tiered-release-priority` | 1132, 0 | 1134, 0 | 926, 0 |
| `sequence-reset-linux-large` | 1048, 0 | 1049, 0 | 746, 0 |
| `mac-mini-macos-only` | 607, 0 | 607, 0 | 543, 0 |
| `mac-studio-4x10-budget` | 307, 0 | 307, 0 | 271, 0 |

The middle column is a real behaviour change and its digests differ in five of the
seven arms: existing faults such as a stalled runner already reach three empty exits
on one demand, and those demands now back off.

**Both faults are in `faultThisTick`'s draw**, the sweep being green with them at the
gate's 80 seeds, per `docs/TESTING.md`. The first version of this change was not: one
seed reached
[#494](https://github.com/vitalyiegorov/tart-runner-fleet/issues/494), a liveness wedge
whose shrunk trace contains neither fault and reproduces on the merge base. Requiring
two witnesses changed what that seed's full trace does, so the stream no longer reaches
it. **#494 itself is unfixed**, and its shrunk trace still fails.

## Consequences

A node serving a refused image reads as what it is, not as busy. Learning it takes two
teardowns, so a one-slot node may take a second assignment deadline to hold. On a
container node it then spends one guest per half hour instead of every slot, and can reach quiescence for
an update while it waits. On a Tart node the loop falls from about forty VMs an hour
to a retry cadence of two an hour per job (it slows a failing demand and does not cap
its total), and `doctor` shows the backoff beside the calendar finding,
but the fleet never asserts a refusal it did not read.

Once deployed, `fleet doctor` will fail `runner version` on any node whose image is
past the window, even if that image still works. That is intended: it is the warning
this incident lacked. The remedy is a rebuild (`docs/OPERATIONS.md`).
