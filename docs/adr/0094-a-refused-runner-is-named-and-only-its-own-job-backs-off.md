# ADR 0094: A refused runner is named, and only its own job backs off

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

**Empty exits slow only the job they happened to, and everything else about an image is a
name in `doctor`. A signal is cheap; a hold is not.**

Two earlier versions of this ADR went further, and review reproduced both failing. One let the
refusal line hold a platform once two guests agreed: two jobs forged it. The next held a whole
image generation on never-assigned exits: a healthy node with jobs waiting on another node's
slot idled for 15 to 30 minutes, launching nothing, even for fresh jobs. Exits cannot tell a bad
image from normal dispatch elsewhere, and a job can write the log. Neither may suppress unrelated
work, so neither does.

### 1. Back off the one demand

A demand's deleted incarnations are counted from the durable rows, per image generation. While
it is still queued, that count is exactly the instances that left without its job: demand rank
only moves forward (ADR 0026), and `alignRunnerDemand` rebinds a runner to whatever GitHub
actually handed it. A job cannot manufacture one, because running a job means being assigned.

It tolerates 2 exits, then waits 5, 10, then 15 minutes after each exit, named
`demand_empty_exit_backoff` under the `empty exits` doctor check. This is a retry cadence: it
slows a failing demand and does not cap its total. It ends by time alone, so it cannot strand a
job. **It holds that demand and no other:** a job with no exits of its own launches as it
always did.

**The cap is set by the job that was only waiting.** Two cases were measured in the simulator:

- A job out-competed for runners is never charged, because a guest spawned for it and handed a
  sibling is rebound to the sibling (`TestAJobOutCompetedForARunnerCollectsNoExits`). No exclusion
  is needed.
- A job whose dispatch was held up elsewhere does collect exits, and when it can finally run,
  what is left of its hold stands between it and a runner. That measured **34 minutes at a cap of
  30, and 19 at a cap of 15** (`TestAJobThatWasOnlyWaitingIsNotDelayedLongOnceItCanRun`).

So the cap is 15. On a single job on a Tart node it costs about four VMs an hour rather than two,
against the roughly forty the incident ran at.

**An exit dated ahead of the clock holds nothing**, as after a clock correction across a restart:
it establishes nothing, so it cannot put a retry beyond the bound. Exits count per **image
generation**, the image reference plus its declared runner version, stamped on the instance row,
so a rebuilt image starts clean on the first tick after its restart. A rebuild under the same
reference and declared version looks like the same generation, which is the declaration's one way
to lie (ADR 0041), and its hold still ends within 15 minutes. Rows older than the field are an
empty generation no node asks about, so upgrading can only forget a hold. A Failed row an operator
reaped never reached a runner, so it does not count.

### 2. A refused image still launches at the per-demand rate

**This is the accepted trade-off.** Nothing bounds a refused image as such. Replaying a refusal
across 12 queued jobs on a 4-slot x86 world launches **28 guests in two simulated hours**, the same
count as with no image-wide bound, because each of many demands earns its own holds one exit at a
time. One job alone boots 9 VMs in the same two hours, against 24 with no backoff. A node serving a
refused image stays busy and can delay its own updates. In exchange, no healthy node ever idles on
this logic, and the outage has a name.

### 3. Name it

The `image signals` row of `fleet doctor`, and `imageSignals[]` in status, carry two display-only
findings. They never fail a check, never change `doctor`'s exit, and reach no plan.

- **`image_generation_not_receiving_jobs`**: at least 4 never-assigned exits on queued jobs, the
  last within 30 minutes, and no job assigned on this generation for 30 minutes. It states that a
  refused runner is one cause and dispatch elsewhere can also cause it, and that it changes no
  launch decisions. It needs no guest read, so it works on a Tart node. This is what would have shown
  this outage.
- **`runner_version_refused`**: a guest with no record of taking a job logged the refusal line in
  the last 30 minutes. A job runs as the user that owns that log and can write this line, so it is
  labelled advisory.

A container node reads the line beside the first stop of a teardown, from the guest's `runner.log`.
A Tart node has no reader (#478, ADR 0080). The guard on the read is there to keep the sentence
honest and to save an exec per finished job, not for safety: nothing decides on the answer.

**A job can make none of this large.** The line is matched whole, each version component is at most
6 digits, and the guest's command prints one line capped at 256 bytes. A job with `sudo` could replace
those tools, so the host also keeps at most 64 KiB of anything run inside a guest
(`podman.GuestOutputLimit`). The version is the only guest text that reaches a log or status; the
traffic sample is parsed to numbers, and a failed bootstrap carries an exit status only.

### 4. Read the calendar

`runnerVersionCheck` fails once the oldest stable release newer than an image is past GitHub's stated
30-day window. On the incident's image that is 2026-09-25, 41 days before the refusal, at a floor that
said nothing. The daemon reads GitHub's public release list every six hours, anonymously, so it spends
none of the App's budget. A draft or prerelease starts no clock, a failed read keeps the last answer,
and a list never read judges nothing. The finding says "may refuse at any time", not a date. The floor
keeps its meaning, a hard bar an operator states, and `belowFloor` and
`fleet_runner_image_below_floor` are that verdict alone.

## What was deliberately not built

- **No hold from the log, however many guests agree**, and **no image-wide hold from exits**, for the
  reasons above. Do not add either back without evidence that distinguishes a bad image from dispatch
  elsewhere.
- **Nothing stops a refused node attracting work.** Its sets keep advertising, so GitHub may keep
  assigning jobs a sibling could run. Session yield (ADR 0047) is the natural tool, but it is node-wide,
  and acting needs the same evidence this ADR does not have.
- **No read of a stopped Tart disk**, which ADR 0041 did by hand. Attaching a VM's disk on every teardown
  is too heavy a mechanism for a sentence.
- **No guest-side report.** The bootstrap helper is sealed in each image, so it could not have helped the
  images this incident is about.
- **No count across rotated request IDs.** A demand is one request ID. The incident's demand was
  preassigned, whose ID derives from the immutable job identity, and it stayed one ID across all 14 log
  lines that printed it. A job GitHub re-offers under a new ID each time starts its own count.
- **No knob.** The bounds are constants, as `PowerCorroboration` is.

## Testability

`tests/simulation/empty_exit_test.go`, `runner_refused_test.go` and
`internal/app/empty_exit_tick_test.go` replay all of it. Each replayed failure was red first: one job
booting 24 VMs in two hours, now 9; exits on some jobs moving the plan for others; a time ahead of the
clock holding a job past its bound; a line that was only near the refusal text; a flood of log lines
reaching the host. Property (w), `launch_hold`, judges every plan against the per-demand hold, counting
empty exits from the world, so a query that lost rows would be caught rather than agreed with.

Corpus over 32 seeds at 200 ticks, spawns and findings per arm:

| Arm | Merge base | Branch, faults out of the draw | Branch, faults in the draw |
| --- | --- | --- | --- |
| `m4-mac-mini` | 1157, 0 | 1153, 0 | 1026, 0 |
| `geekom-linux-amd64` | 1266, 0 | 1264, 0 | 1079, 0 |
| `federated-maestro-scope` | 683, 0 | 684, 0 | 662, 0 |
| `tiered-release-priority` | 1132, 0 | 1134, 0 | 1021, 0 |
| `sequence-reset-linux-large` | 1048, 0 | 1049, 0 | 894, 0 |
| `mac-mini-macos-only` | 607, 0 | 607, 0 | 543, 0 |
| `mac-studio-4x10-budget` | 307, 0 | 307, 0 | 271, 0 |

The middle column is a real behaviour change, and its digests differ in five of the seven arms: existing faults
such as a stalled runner already reach three empty exits on one demand, and those demands now back off.

**Both faults are in `faultThisTick`'s draw**, the sweep being green with them on every arm at the gate's 80 seeds,
per `docs/TESTING.md`.
[#494](https://github.com/vitalyiegorov/tart-runner-fleet/issues/494) is unrelated to this change and still open:
its shrunk trace contains neither fault and reproduces on the merge base, and this stream does not reach it within
80 seeds.

## Consequences

A node serving a refused image is named as such in `doctor`, by exits on every node and by its guests'
own logs on a container node, and it goes on launching at the per-demand rate until someone rebuilds the
image. A job can do no more with the log than change a sentence. Healthy nodes are unaffected: a job with
no exits of its own launches as it always did, and one with a few waits at most 15 minutes.

Once deployed, `fleet doctor` will fail `runner version` on any node whose image is past the window, even
if that image still works. That is intended: it is the warning this incident lacked. The remedy is a
rebuild (`docs/OPERATIONS.md`).
