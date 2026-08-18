# ADR 0042: A destructive premise must be corroborated

## Status

Accepted. Closes
[issue #246](https://github.com/vitalyiegorov/tart-runner-fleet/issues/246).

It completes the set of bounds
[ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md) and
[ADR 0040](0040-a-guest-that-stopped-answering-is-not-running.md) established, by
giving the oldest recovery cause in the fleet the bound every later one was
written with. It also amends ADR 0040 with a measurement that was not available
when it was written.

## Context

On 2026-08-18, `trf-large-f7d1fac9141ad580` — running the nightly simulation
sweep — was planned for a destructive recovery drain **eighty-six times in nine
minutes**, one every 6.3 seconds. Every one of those drains was aborted at
execution time. The night before, `trf-xl-01066203f2d908be` was planned for one
**a hundred and fifteen times in eleven minutes**, and aborted every time.

Nothing was wrong with either runner. No runner was cut: the drain executor's
re-verification caught every attempt, which is the safety property
[ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md) exists for and it
held two hundred and one times out of two hundred and one.

And **nothing recorded any of it**. The daemon log has no line for either
instance for the whole window. The cause had to be established by re-deriving the
content-addressed operation identities out of `operations` and matching them
against every combination of the five recovery-cause flags: the enqueued
`op-c0c78b5060aa082213c41956`, `op-f0a28ed31d1d2492a99eaa8f`,
`op-6fdea3fa69bddd027b747911` matched, three generations deep, the row with **no
flag set** — `DrainPhaseStoppedRecovery`, whose only possible premise is
`Power == InstancePowerStopped`.

### The premise was a single reading, and the backend cannot tell it from an error

`internal/app/inventory.go` derives `Stopped` from one field of one
`tart list --format json`. `tart` 2.32.1 computes that field like this
(`Sources/tart/VMDirectory.swift`):

```swift
func running() throws -> Bool {
    guard let lock = try? lock() else {
      return false          // any failure to open or lock config.json
    }
    return try lock.pid() != 0
}
```

Demonstrated on this fleet's own host, on a probe VM cloned from
`linux-runner-base-go`:

```
normal      -> [(True,  'running')]
config.json chmod 000
unreadable  -> [(False, 'stopped')]     <- silently, per VM, no error, no stderr
chmod 644
restored    -> [(True,  'running')]
```

So a transient failure that says nothing about the machine arrives at the fleet
as a confident claim that the machine is off, **indistinguishable from the real
thing**. What made the open fail in production is not determined; guest
saturation, host CPU saturation to load 20.8, concurrent `tart exec`, a guest
kernel panic, and lock contention on `config.json` were each tested on the probe
and none of them reproduced it.

### The asymmetry nobody had noticed

Every other recovery cause states a bound before it acts. The assignment deadline
and the idle-runner deadline require elapsed time and fresh demand evidence. The
occupancy budget requires a configured ceiling. The guest-liveness verdict
requires an unbroken run of refusals over a window. `Power == Stopped` required
**nothing at all** — no count, no window, no corroboration — and it is the oldest
of the six, written before any of the incidents that taught the others to be
careful. It is also the only one whose evidence comes from a source that renders
its own errors as the hostile answer.

The 2026-07-20 incident recorded in `internal/lifecycle/executor.go` — *"a
glitched power reading planned a stopped-recovery drain of a busy runner; the
registration guard refused 23 times, then one transient runner-lookup miss
released the kill"* — is the same defect, caught by a different guard, six years
of fleet-time earlier. The response then was to strengthen the guard. This is the
response to strengthening the premise.

### The churn, and the two deadlines it corrupted

verdict → drain → abort → verdict, every ~6.3 seconds at a five-second poll, for
nine minutes, with no backoff, no suppression and no memory. Nothing counted how
many times a premise had already been disproven, so each tick re-derived the
identical operation from the identical reading.

`DrainExecutor.abort` calls `Advance`, which does
`UPDATE instances SET … updated_at=?`, and `ProductionInventory.Observe` derives
`RunningSince` from `UpdatedAt`. **Every abort therefore reset the idle-runner
clock**, so for the whole storm `lingeringRunner` and `stalledAssignment` could
not fire on that instance whatever GitHub said. Only the occupancy budget
survived, because it reads `CreatedAt`.

## Decision

### The bound belongs at the classification, not at the gate

A stopped power reading becomes `InstancePowerStopped` only once an unbroken run
of readings has met both halves of a bound: **three readings over forty-five
seconds**. Until then it classifies as `InstancePowerUnknown` — the value that
already means "nothing was established" everywhere it appears.

Putting it at the classification rather than at the one gate that plans a kill is
the whole of the second half of this record, and the simulator is what taught it.
A stopped reading does not only authorize a reclaim; it also **stops charging the
host** for the instance (`domain.ConsumesHostResources`), so the scheduler admits
work into a vector it believes came back while the VM is still running and still
consuming it. The first sweep that could generate a misreport produced exactly
that — a conservation violation, five slots against a four-slot ceiling, on
`geekom-linux-amd64` seed 8. A rule at the gate would have left that untouched;
a rule at the classification withholds the reading from every consumer at once.

`Unknown` rather than `Running` is deliberate. The fleet has not established that
the VM is running either, and `ProvenIdle` excludes `Unknown` by construction, so
an uncorroborated instance goes on charging the host. That is the conservative
direction on both axes at once.

**It narrows that violation without closing it, and that is stated here rather
than discovered later.** A misreport that outlasts the bound is believed, the
vector is released to a replacement, and when the reading corrects itself both
instances hold it. That is a second and distinct defect — a released vector
cannot be taken back — and it is tracked on
[issue #247](https://github.com/vitalyiegorov/tart-runner-fleet/issues/247) with
its reproducing seed. It is why `misreported_power` is not yet in the fuzz generator's draw: the
fault is exercised by two pinned traces, and the draw entry lands with the fix
for the defect it surfaces, so the sweep is never knowingly red.

A tearing-down instance is exempt from the bound. There the fleet's own decision
corroborates the reading, and requiring a second source would delay releasing the
vector of a VM everyone agrees is finished, on every teardown. That exemption is
also why **every corpus digest is byte-identical to the merge base**: no trace
that is never handed a misreport sees any behaviour change at all.

### The accumulator is ADR 0040's, reused rather than rebuilt

`domain.GuestLivenessState` and `domain.GuestLivenessPolicy` were never about
guests: they are "an unbroken run of hostile observations, judged against a count
and a window". `internal/domain/corroboration.go` names them for what they are —
`ObservationRun` and `CorroborationPolicy`, Go aliases, so not one existing call
site moves — and `PowerSignal` folds a power reading onto the same three-valued
observation. `Dead` is renamed `Confirmed` for the same reason.

`app.PowerCorroborator` carries the run between ticks, mirroring
`app.GuestLivenessTracker` and inheriting its two disciplines: its memory is
in-process, so a restart forgets every run — fail-open, and forgetting can only
delay a reclaim, never authorize one — and it stamps its instants from the
caller's single observation instant, because a run recorded on one clock and
judged on another is not measurable at all.

### The bound is not configurable

A knob here could only ever be turned back towards acting on a single reading,
which is the behaviour this record exists to remove. And unlike ADR 0040's bound,
this one **cannot end a healthy job on its own**: it can only delay a reclaim the
fleet would otherwise perform immediately. Three readings over forty-five seconds
costs a genuinely powered-off instance well under a minute of extra vector hold,
against an occupancy budget measured in hours.

This is the opposite trade from ADR 0040, and deliberately so. That bound is
destructive and therefore operator-visible with an off switch; this one is
protective, and an off switch on a protection is a footgun with a documentation
page.

### An abort is evidence, and the fleet now keeps it

An aborted stopped recovery is the strongest evidence that exists about a power
reading: the drain re-read the same source at the moment of acting and got the
opposite answer. Discarding it is what turned nine minutes into eighty-six
drains.

`DrainExecutor.abort` returns the instance to `Running` and leaves the drain phase
on the durable row, so `state = running AND drain_phase = DrainPhaseStoppedRecovery`
already **is** the record of the contradiction — no new column, no new query, no
new state. `domain.Instance.PowerRetracted` reads it, and a retracted premise must
hold for `PowerRetractedFactor` (eight) times as long — six minutes — before it may
act again.

One step rather than a growing ladder, because there is nothing a second step
learns: the first retraction already establishes that this instance's readings
cannot be trusted at the ordinary bound, and a ladder would add a counter, an edge
to detect and a knob for a distinction no operator acts on differently.

The arithmetic, stated plainly: the 2026-08-18 storm would have produced **two**
drains instead of eighty-six, and the 2026-08-17 storm two instead of a hundred
and fifteen. It is a bound, not an elimination — the trigger is unknown, so
elimination is not on offer — and both of them would now be **logged**.

### The fleet says it out loud, for every cause

Of six recovery causes, only two have ever produced a log line: the occupancy
budget and the guest-liveness verdict, both added by the incidents that created
them. A stopped recovery, an inactive recovery, a stalled assignment and a
lingering runner have been able to destroy a runner in silence since they were
written.

- **`instance recovery drain planned`**, naming the instance, profile, cause and
  job binding, for every recovery cause, **never rate limited**. Each is a
  distinct decision to destroy a live instance, and a storm of them is precisely
  the artifact an operator needs — a suppressed eighty-sixth line is the one that
  would have named the problem.
- **`instance power premise retracted by its own drain`**, rate limited per
  instance, naming the new bound. An abort is the fleet catching itself about to
  destroy a live runner, and nothing has ever recorded one.

## Consequences

A power reading can no longer destroy an instance, or hand its vector away, on the
strength of one enumeration; and when the fleet disproves its own premise it says
so and remembers.

The costs are real and worth naming.

**A genuinely powered-off VM holds its vector for up to forty-five seconds longer,
and up to six minutes longer if the fleet was once wrong about it.** That is the
whole price, it is two orders of magnitude inside the occupancy budget that
backstops the same instance, and it buys the removal of the only unbounded
destructive premise in the fleet.

**An uncorroborated stopped instance keeps charging the host envelope.** It is
counted as occupying its vector while the fleet decides, which is the conservative
direction: the alternative double-books the machine, and the simulator proved it
does.

**The bound is not an elimination.** A misreport that outlasts the bound still
plans a drain, and that drain still aborts. Two instead of eighty-six is a
different order of problem, not the absence of one — and the trigger that made
`tart` misreport a running VM for nine and eleven minutes remains unidentified.

**A misreport can still over-admit the host.** Once a reading is believed, the
instance's vector is released and the scheduler may commit it to a replacement;
the drain then aborts and both hold it. Narrowed by the bound, not closed, and
tracked on issue #247 with its reproducing seed.

### ADR 0040 is amended: its verdict is unreachable on this fleet

Measured on a probe VM on this host, mirroring `execGuestProbe`'s classification
exactly (`err == nil` → alive; own five-second deadline expired → unknown;
otherwise → refused):

| guest condition | `tart exec <vm> true` | classification |
|---|---|---|
| healthy, idle | 0.02–0.77 s, rc=0 | `alive` |
| saturated (four cores pegged, driven to OOM) | **30.02 s**, control-socket error | `unknown` |
| guest agent stopped (kernel fine, VM pingable) | **30.02 s**, same error | `unknown` |
| guest kernel panicked | **30.02 s**, same error | `unknown` |
| VM not running | **0.02 s**, `VM "…" is not running` | `refused` |

Two findings, pointing opposite ways.

**ADR 0040's discrimination is correct exactly where it was doubted.** A saturated
guest classifies `unknown`, not `refused`. The residual false-positive risk #237
recorded — *"tested against a modelled saturated guest, but not against a real
Gradle-at-full-tilt guest on this fleet"* — has now been measured against a real
one, and it does not exist. The deadline-based rule holds.

**It is correct into inertness.** `tart` 2.32.1 applies a **thirty-second**
internal timeout to the control-socket connect, six times the probe's five-second
deadline, so a *dead* guest classifies `unknown` as well. The only measured fast
failure is `VM "…" is not running`, a condition `Power == Stopped` already covers
with no probe at all. **`GuestLivenessRefused` is therefore unreachable for any
guest tart still calls running**: the run of five refusals can never start, and
the verdict, the warning, the doctor check and the four Prometheus series can
never fire on this fleet.

The base image already carries #237's `kernel.panic=10`, so a panicked guest now
reboots in about fourteen seconds — which is the fix working, and which also means
the ninety-second window can never be filled by a panic either.

Nothing is removed here. The mechanism is fail-open by construction and costs one
`tart exec` per running instance per tick; deleting it on one host's measurement
would be the wrong trade while a second backend (`podman exec`, whose refusal
semantics are not measured) shares the port. What changes is the claim: ADR 0040
is recorded as **not currently able to fire on node A**, and making it able to —
a probe deadline above tart's own connect timeout, or a transport-level check that
fails fast — is tracked on #246 rather than asserted here.

## The three questions

**Can I remove overengineering?** Yes, and this record is mostly that. No new
accumulator, no new policy type, no new configuration, no new durable column, no
new store query, no new remedy: ADR 0040's accumulator is renamed to the thing it
always was and reused, the retraction is read from a drain phase already
persisted, and the reclaim is the one that already existed. The one genuinely new
object is a twenty-line tracker that mirrors the one beside it.

**Can I reduce complexity?** The bound is stated once, in
`domain.CorroboratedPower`, and applied once, where power is classified — rather
than re-derived at each of the gates that read power. That is what the
conservation violation taught: the seam that re-implements a rule per call site is
this repository's oldest source of defects, and a premise checked at one gate is
exactly that shape.

**Can I test this?** The harness could not build the state at all: no fault made
`simTart.List` report a running VM as off, and `drainAbortsNow` hard-coded
`return false` for the stopped recovery on the strength of a comment saying "no
fault powers a VM off" — true, and beside the point, because the incident did not
power a VM off. The `misreported_power` event, the mirror of `VMControl.Running`,
and two pinned traces are part of the fix, and the world model is proved red on
the pre-fix wiring: property **(i) `drain_churn`** fires on `m4-mac-mini` seed 1
with two recovery drains aborted on a three-tick misreport.

Property (i) was correct and unchanged throughout, and its bound
(`DrainChurnN = 1`) is what forced the retraction half of the fix: corroboration
alone still lets a persistent misreport re-fire as fast as the run refills. This
is a **fleet defect** behind a **world-model gap** — not an oracle defect.
