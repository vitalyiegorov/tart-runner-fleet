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

**Only evidence a job cannot produce may hold work. A log a job can write may name a
reason, and nothing else.**

An earlier version of this ADR let the refusal line hold a platform once two guests
agreed. Review reproduced two jobs forging it: evidence that a guest took a job can
arrive after the read, and missing broker events defeat the check on both guests. That
design is gone rather than patched.

### 1. Bound the image on never-assigned exits

A demand's deleted incarnations are counted from the durable rows, per image generation.
While it is still queued, that count is exactly the instances that left without its job:
demand rank only moves forward (ADR 0026), and `alignRunnerDemand` rebinds a runner to
whatever GitHub actually handed it. **A job cannot manufacture one, because running a job
means being assigned.** A broker event that went missing can at worst slow launches a
little, and the next job to bind clears the hold.

Two bounds read that count:

- **Per demand**: tolerates 2 exits, then waits 5, 10, 20, then 30 minutes after each
  exit, named `demand_empty_exit_backoff`. It is the bound on one job no runner will take
  while the image is fine, and on a node with a single job queued.
- **Per image generation**: exits since that generation last had a job assigned, summed
  across queued demands. At 4 exits across at least 2 demands the whole queue on that image
  waits 15, then 30 minutes, named `image_empty_exit_backoff`. A refused image charges each
  of many demands one exit at a time, so the per-demand bound alone still launched 28 guests
  after a first batch of 4.

Both are a retry cadence: they slow a failing queue and do not cap its total. They end by
time alone, so neither can strand a job. A job GitHub starts dispatching again waits at
most 30 minutes.

**What does not trigger it.** An exit on a job no longer queued is absent from the sum, so
another runner taking the job first, or a cancellation, counts for nothing. Fewer than 4
exits, or exits on one demand alone, hold no other job.

**What clears it.** GitHub naming a runner of that generation on a job's row ends the run
at once: the next tick has nothing to sum. A bare `JobAssigned` names no runner, so it
waits for the `JobStarted` that does. Exits on another generation never count, so a rebuilt
image starts clean on the first tick after its restart. A generation is the image reference
plus its declared runner version, stamped on the instance row. A rebuild under the same
reference and declared version looks like the same generation, which is the declaration's
one way to lie (ADR 0041), and its hold still ends within 30 minutes. Rows older than the
field are an empty generation no node asks about, so upgrading can only forget a hold.

### 2. Retries are the probes, and nothing else is needed

Once a held image's time comes, one guest launches. Which demand is the scheduler's
decision: each candidate is offered to `PlanTick`, the job that has cost the least first,
so one that keeps failing cannot take every retry from one a runner would take, and a job
that cannot fit never blocks one that can.

The launch spends the interval by itself, because the hold is dated from the later of the
last exit and the generation's last launch, both durable. So a guest that never reports
cannot let another through a minute later. No instance state is read, so nothing stranded
can suppress a launch: a `Planned` row that never became a guest keeps its own demand out of
the queue and nothing else, and its age runs out like any other launch's.

### 3. The refusal line is advisory

Beside the first stop of a teardown, a container node reads the one fixed, complete line
from the guest's `runner.log`. Only a version matching the grammar leaves the read, so no
other guest text reaches a log or a status document. A Tart node has no reader (#478, ADR
0080).

**A job runs as the user that owns that log and can write this line, so no plan reads it.**
A reading changes no launch decision, delays nothing, suppresses nothing, and never touches
`ready` or `healthy`. It is logged once per reading as advisory. Only while launches from
that platform's image are already held does it reach `runnerVersionCheck`, as
`runner_version_refused (advisory: a job can write the log this is read from): ...`, and
`runnerImages[]` as `refusedVersion` and `refusedAt`. On its own, forged or not, it reaches
no check. `belowFloor` and `fleet_runner_image_below_floor` stay the floor verdict alone.

### 4. Read the calendar

`runnerVersionCheck` also fails once the oldest stable release newer than an image is past
GitHub's stated 30-day window. On the incident's image that is 2026-09-25, 41 days before
the refusal, at a floor that said nothing. The daemon reads GitHub's public release list
every six hours, anonymously, so it spends none of the App's budget. A draft or prerelease
starts no clock, a failed read keeps the last answer, and a list never read judges nothing.
The finding says "may refuse at any time", not a date. The floor keeps its meaning, a hard
bar an operator states.

## What was deliberately not built

- **No verdict from the log, however many guests agree**, for the reason above.
- **No in-flight or probe-allowance state.** Time and durable launch dates do its job,
  without the failures it had: a stranded row suppressing probes, and a probe that spent
  nothing.
- **Nothing stops a refused node attracting work.** Its sets keep advertising, so GitHub may
  keep assigning jobs a sibling could run. Session yield (ADR 0047) is the natural tool, but
  it is node-wide and a hold is per image. It is the next step if this recurs.
- **No read of a stopped Tart disk**, which ADR 0041 did by hand. Attaching a VM's disk on
  every teardown is too heavy a mechanism for a sentence that is only advisory.
- **No guest-side report.** The bootstrap helper is sealed in each image, so it could not
  have helped the images this incident is about.
- **No count across rotated request IDs.** A demand is one request ID. The incident's
  demand was preassigned, whose ID derives from the immutable job identity, and it stayed
  one ID across all 14 log lines that printed it. A job GitHub re-offers under a new ID each
  time starts its own demand's count, though it still adds to the image's.
- **No knob.** The bounds are constants, as `PowerCorroboration` is.

## Testability

`tests/simulation/empty_exit_test.go`, `runner_refused_test.go` and
`internal/app/image_hold_test.go` replay all of it, red first: one job booting 24 VMs in two
hours and holding a slot for 183 of 240 ticks; a refused image launching 28 guests across 12
jobs, now 9, of which at most 3 while held; a forged line moving the plan, now byte-identical
to a world with no line; a stranded `Planned` row suppressing launches; and a swap waiting
out the old generation. Property (w), `launch_hold`, judges every plan against the holds. It
counts a demand's empty exits from the world, so a query that lost rows would be caught
rather than agreed with.

Corpus over 32 seeds at 200 ticks, spawns and findings per arm:

| Arm | Merge base | Branch, faults out of the draw | Branch, faults in the draw |
| --- | --- | --- | --- |
| `m4-mac-mini` | 1157, 0 | 1153, 0 | 1012, 1 |
| `geekom-linux-amd64` | 1266, 0 | 1264, 0 | 1030, 0 |
| `federated-maestro-scope` | 683, 0 | 684, 0 | 662, 0 |
| `tiered-release-priority` | 1132, 0 | 1134, 0 | 1016, 0 |
| `sequence-reset-linux-large` | 1048, 0 | 1049, 0 | 869, 0 |
| `mac-mini-macos-only` | 607, 0 | 607, 0 | 543, 0 |
| `mac-studio-4x10-budget` | 307, 0 | 307, 0 | 271, 0 |

The middle column is a real behaviour change, and its digests differ in five of the seven arms:
existing faults such as a stalled runner already reach three empty exits on one demand, and those
demands now back off. The image bound never fires there, so these digests do not depend on it.

**The faults ship out of `faultThisTick`'s draw**, per `docs/TESTING.md`. Drawn, every arm is
clean at the gate's 80 seeds except seed 19 of `m4-mac-mini`, whose wedge is the state of
[#494](https://github.com/vitalyiegorov/tart-runner-fleet/issues/494): a `builder` Mac handoff
with a feasible `maestro` behind it, both in the plan and neither withheld. #494's own trace
contains neither fault and reproduces on the merge base. A hold only removes the Linux
admissions that used to reset property (a)'s counter. The draw line lands with #494's fix.

## Consequences

A node serving a refused image reads as what it is, not as busy. Reaching the hold takes four
exits, so x86 nodes launch a first batch and a refill first, up to about 30 minutes. They then
retry one guest at a time, at most two an hour, and can reach quiescence for an update
between retries. On a Tart node the same bound applies with no sentence naming the reason:
`doctor` shows the holds and, when present, the calendar finding, and the fleet never asserts a
refusal it did not read.

A job can do no more with the log than change a sentence on a node already held. A missed
broker event can slow launches on a healthy image, up to the 30-minute cap, if four exits
across two queued jobs also land before any job binds.

Once deployed, `fleet doctor` will fail `runner version` on any node whose image is past the
window, even if that image still works. That is intended: it is the warning this incident
lacked. The remedy is a rebuild (`docs/OPERATIONS.md`).
