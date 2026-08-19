# ADR 0044: An unread power state establishes nothing

## Status

Accepted. Closes
[issue #252](https://github.com/vitalyiegorov/tart-runner-fleet/issues/252).

It amends [ADR 0042](0042-a-destructive-premise-must-be-corroborated.md) in two
places: the mechanism that record left open, and one factual claim in it that the
next two incidents disproved. It also amends the narrowing
[ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md) and
[ADR 0040](0040-a-guest-that-stopped-answering-is-not-running.md) each wrote into
their own gate.

## Context

### What happened, and what did not

Two nightly runs of this repository's own simulation sweep died on their
race-detector arm, on `trf-large-f7d1fac9141ad580` (2026-08-18) and
`trf-large-441584681743257d` (2026-08-19), both with GitHub's *"The self-hosted
runner lost communication with the server"*. In the second, ADR 0042's bound and
logging worked exactly as designed and produced the first complete record this
class has ever had:

```
06:24:22 WARN instance recovery drain planned  cause="vm powered off"
06:24:28 WARN instance power premise retracted by its own drain
         power="" stoppedReadings=10  requiredWindow=6m0s
06:28:38 ... stoppedReadings=49
```

**The fleet did not kill its runner.** That was established before anything was
written, because it is the only question whose answer changes what the fix should
be, and the durable ledger answers it four ways:

- Between 04:24:23 and 04:32:34 the instance's `transition_history` contains
  thirty rows and all thirty are `draining → running`. Across every instance on
  the node in that window the only rows reaching a destructive state are the last
  three, at 04:32:35.
- All thirty-one operations completed with `attempts=0` and an empty
  `last_error`; no stage failed and nothing retried.
- Deregistration is idempotent through a fresh `GetRunnerByName`, so had any
  removal landed, the next attempt six seconds later would have found no runner,
  returned nil, and advanced. Thirty out of thirty aborted, so the registration
  was still present and still busy at 04:32:34.
- GitHub's own record puts the job's `completed_at` at **04:32:34Z**; the fleet's
  `event-drain-…` operation row was created at **04:32:35** and the VM deleted at
  04:32:36. The teardown followed the death by one second.

Both deaths sit about ten minutes after the `-race` arm starts (9m31s and
10m00s), and only eight and nine minutes after the storm starts. The runner
agent's own heartbeat expiring under that arm's I/O and memory pressure fits the
first interval and not the second: **load → both**, with the storm as a
co-symptom rather than a cause.

### And the part that should worry us

ADR 0042 recorded that *"the drain executor's re-verification caught every
attempt"*. On these two nights it cannot have, and the counters say so.

`stoppedReadings` climbs 10 → 20 → 29 → 39 → 49 → 59 → 69 → 79 with **no reset**.
`CorroborationPolicy.Observe` zeroes the run on any `running` reading, so all
seventy-nine consecutive inventory readings of `tart list --format json` said
*not running*. The drain's act-time check is `VM.Running` → `find` → **the same
`tart list --format json`**, two seconds later, in the same process. For it to
have caught anything, that identical command would have had to answer "running"
thirty times out of thirty while the inventory's answered "stopped"
seventy-nine times out of seventy-nine, interleaved seconds apart — and ADR 0042's
own probe (`chmod 000 config.json`) shows the misreport is *sustained*, not
flickering.

So all thirty attempts passed the guard, fell through to `Control.Deregister`
(the stopped-recovery arm deliberately skips `SafeToDeregister`), and issued a
runner-removal call against a live, busy runner. GitHub refused it, `RunnerBusy`
answered from the durable demand, and the drain aborted.

**The fleet's last line of defence against this class was GitHub's, not its own.**
The act-time re-verification is not a second source. It is the first source, read
twice.

### The premise, not the window

`power=""` in that log line is the corroborated value: `InstancePowerUnknown`,
because ADR 0042's bound had downgraded a reading it had not yet corroborated.
The RAW reading underneath it was `stopped`, seventy-nine times, and it was
`stopped` because `executor.Instance.Running` is a **bool** and `tart`'s
`running()` answers false for every failure to open a VM's `config.json`.

That is AGENTS.md rule 4 — *never represent an unavailable observation as an
empty collection* — violated by a type. A bool has two values and the backend has
three answers, so the third went into the second, which happens to be the premise
of the oldest destructive recovery in the control plane.

No window can fix it. ADR 0042 raised the corroboration to three readings over
forty-five seconds and #248 raised a retracted premise to six minutes; the
production run was wrong for **nine minutes** and would have outlasted any bound
that leaves the fleet able to reclaim a genuinely stopped VM at all. A bound
answers a reading that is wrong for a while. This reading was not wrong — it was
never taken.

## Decision

### The port carries three values, and every consumer names the third

`executor.Instance.Running bool` becomes `Power domain.InstancePower`, and
`Backend.Running(ctx, name) (bool, error)` becomes
`Backend.Power(ctx, name) (domain.InstancePower, error)`. The enum already
existed — `domain.InstancePower` has had Unknown, Running, Stopped and Absent
since ADR 0022 — so this is the port catching up with the vocabulary the domain
already spoke, not a new axis.

It is a compiler-enforced migration on purpose. Every site that read the bool now
has to say what it does about the third answer, and several of them were quietly
wrong:

| site | was | is |
|---|---|---|
| `app.ProductionInventory` | derived `Stopped` from `!vm.Running` | carries the backend's own classification through |
| `DrainExecutor` stopped recovery | proceeded unless the VM answered "running" | fails the guard on an unread power, as it already does on an inconclusive guest probe |
| `Adapter.Stop` / `Delete` | skipped the stop when `!Running` | skips it only when the VM is *proven* stopped |
| `Adapter.Reap`, `discharge` | refused a *proven running* VM | refuses everything not *proven stopped* |
| podman `container.running()` | unrecognised state ⇒ not running | unrecognised state ⇒ Unknown |

### The tart adapter corroborates the one reading that can be manufactured

`tart` cannot report a VM *running* by failing to read it: the failure path
answers false. Only the hostile answer can be produced by an error, so only the
hostile answer is corroborated. A `"Running": false` for a local VM is checked
against `<tart home>/vms/<name>/config.json` — **the same file `tart`'s own
`running()` opens** — and:

- readable ⇒ `Stopped`. A genuinely powered-off VM is still reclaimed as fast as
  it ever was, and every existing trace is unchanged.
- unreadable ⇒ `Unknown`, with the errno class and the read latency attached.

A cached OCI image has no local configuration and cannot be running, so it is not
corroborated; reporting every base image on the node as unreadable would bury the
signal this exists to raise.

### The fleet says which failure it was, because nobody knows yet

Issue #246 tried five reproductions and failed. ADR 0042 could demonstrate the
mechanism and had to record the trigger as unidentified. The reason is structural:
the one artifact that would answer the question — the errno — is discarded inside
`tart`, every time, and the fleet was never told there had been an error at all.

`domain.PowerReadFailure` carries a closed-vocabulary reason
(`permission_denied`, `descriptor_exhaustion`, `interrupted`, `io_error`,
`timeout`, `missing_configuration`, `unclassified`) and the measured latency, and
the daemon logs one line per instance per reason:

```
WARN instance power state unreadable instance=… reason=descriptor_exhaustion
     readLatency=1.234s outcome="nothing is planned on this reading;
     the instance goes on charging the host"
```

Both numbers are there because the two live hypotheses have different shapes: a
refused open fails in microseconds, a starved one takes as long as the host is
busy. One occurrence with both settles it. The vocabulary is closed rather than
free-form for the reason every classified failure in this fleet is: a bounded
token can be logged, counted and compared without carrying a path or a home
directory into a log line.

Resource exhaustion is the leading hypothesis — descriptor pressure or memory
pressure under a `-race` build, sustained for minutes and then clearing on its
own, invisible to every reproduction that did not run the workload — so it gets
two classes rather than one, because the two point at different remedies.

**Per-process descriptor exhaustion is ruled out.** Measured on this host against
a `tart list` with two VMs genuinely running, at `ulimit -n` of 16, 12, 10 and 8:
every run reported both running VMs correctly. `tart` does not lose the reading to
its own file-descriptor budget, so if descriptors are the trigger it is the
system-wide table (`ENFILE`), not the per-process one. That is the sixth failed
reproduction and, unlike the first five, it removes a candidate.

Reproduced end to end on this host, on a base-image VM directory, with ADR 0042's
own method:

```
chmod 000 linux-runner-base-go-pre-panic-236-20260817/config.json
tart list  -> State: "stopped"                          <- the misreport
adapter    -> power="", reason=permission_denied,
              readLatency=14µs                          <- the correction
```

Fourteen **microseconds**. That is the measurement the latency field exists to
provide: a refused open fails immediately, so the next production occurrence
separates "refused" from "starved" on the first line it prints, without a sixth
investigation.

The corroboration is a **lower bound on detection**, stated plainly: it opens the
configuration read-only, and `tart`'s own lock may open it for writing. A
permission mode that denies write but allows read would fail `tart` and succeed
here, and the reading would classify `stopped` exactly as it does today. Every
resource-exhaustion and I/O failure fails both. Matching the flags exactly would
mean guessing at `tart`'s, and guessing wrong towards write would make a
read-only VM store report every VM unreadable — a fail-closed cost paid on every
tick for a case nobody has seen.

### Two bounds are opened, not closed, and the sweep is why

This is the half of the record that reverses a rule, so it is stated plainly.

`occupancyExceeded` and `guestUnresponsive` each required
`Power == InstancePowerRunning`. Both comments give the same reason: *a stopped or
absent instance is reclaimed by a cheaper gate*. That reasoning does not survive a
third answer. An instance whose power cannot be read has **no** cheaper gate — the
stopped recovery may not act on it, by construction, which is this record's first
half — so leaving it out of both made an unreadable reading a way to hold a whole
vector, indefinitely, with a dead guest on it.

The simulation found exactly that, the first time `unreadable_power` was drawn:
property **(k) `occupancy_budget`** at 133 ticks against a one-hour ceiling on
four federated seeds, and property **(p) `dead_guest_hold`** at twenty-five ticks
against a twenty-four tick release bound on four more. Neither was hypothetical
and neither was reachable before this fault existed.

Both gates now read `!instance.Power.ProvenIdle()` — the predicate ADR 0022
already wrote for "a successful observation established this VM is executing
nothing" — so they exclude a proven stop and a proven absence, and include an
unread reading.

Neither reclaim rests on the unread power:

- The **ceiling** never claimed the runner was idle. It is a wall-clock fact about
  how long a vector has been held, and how long it has been held does not depend
  on whether the enumeration could open a file.
- The **guest verdict** rests on five refused probes over ninety seconds, from the
  guest itself. On this fleet the only fast probe failure is
  `VM "…" is not running` (ADR 0042's measurement table), so the verdict
  corroborates from an independent source exactly the fact the enumeration could
  not read. That is better evidence than the enumeration was ever going to give.

Absence of evidence still authorizes nothing. Evidence from somewhere else does.

### ADR 0042 is amended

> *"No runner was cut: the drain executor's re-verification caught every attempt,
> which is the safety property ADR 0033 exists for and it held two hundred and one
> times out of two hundred and one."*

The outcome stands — no runner was cut on 2026-08-17, 08-18 or 08-19 — but the
mechanism named is not what held. For a **stopped recovery** the re-verification
reads the same enumeration the plan read, so it can only catch a misreport that
flickers, and neither production storm flickered. What held was GitHub's refusal
to remove a busy runner, one layer further out and outside this fleet's control.

That is recorded here rather than quietly fixed, because ADR 0033's safety claim
is load-bearing for four other drain phases where the re-verification genuinely
IS a second source: the stalled assignment and the lingering runner re-read the
durable demand, the guest-liveness reclaim re-runs the probe, and the event drain
asks GitHub. The stopped recovery was the one phase whose re-read had no
independent source, and after this record it does not need one: the premise is
established at classification or it does not exist.

## Alternatives considered

**Raise the window again.** #248 already took it from forty-five seconds to six
minutes and the storm shrank from eighty-six drains to thirty without the
incident changing at all. A premise that is wrong for longer than any bound the
fleet can afford is not a timing problem.

**Make the drain re-read a genuinely different source** — `tart get`, a process
scan, an SSH probe. Every one of them adds a mechanism to compensate for a
classification that is wrong, and the classification is one predicate. Deleting
the misclassification is smaller than any second opinion about it, and it fixes
the six other consumers of the same reading at the same time.

**Keep `Running bool` and add a separate `PowerKnown bool`.** Two booleans is
three states plus one that cannot happen, and nothing forces a consumer to read
the second. The whole defect is a consumer reading a value that did not carry the
answer.

**Drop the stopped recovery entirely.** It is the only fast reclaim of a genuinely
powered-off VM, and the occupancy ceiling that would replace it is measured in
hours. The premise was never the problem; the reading was.

## Consequences

A destructive recovery can no longer be planned, and a drain can no longer act, on
a power state nobody read. When that state occurs the fleet names the errno class
and the latency, which is the evidence three investigations have lacked.

**An unreadable instance holds its vector until a bound the fleet owns expires.**
It goes on charging the host — the conservative direction, unchanged from
ADR 0042 — and it is now reclaimable by the ceiling and by the guest verdict
rather than by nothing. On a node with no configured ceiling and no guest-liveness
bound, an instance whose power is permanently unreadable is held until its job
ends. That is stated rather than discovered: it is strictly better than
destroying it, and both bounds are configured on this fleet.

**The corroboration bound and the retraction factor stay.** They answer a
different fault — a backend that *confidently* reports a running VM as off, which
`misreported_power` still models and which a tart bug or a real stop-and-start
would still produce. This record removes the readings that were never
observations; ADR 0042 bounds the ones that were.

**One extra file read per stopped local VM per tick.** Roughly ten reads of a
280-byte file every five seconds on this node, against a `tart list` that already
walks the same directory.

**Every corpus digest is byte-identical to the merge base until the fault enters
the generator's draw**, which is the evidence that the classification change moves
no trace that is never handed an unreadable reading. See the tables in the pull
request.

## The three questions

**Can I remove overengineering?** Yes, and the change is mostly removal. The
inventory's `switch` that manufactured `Stopped` from a bool is deleted; the
adapters classify their own reading, which is where the knowledge is. No new
policy type, no new configuration, no new durable column, no new query, no second
opinion mechanism, and no new bound — the two gates that changed were narrowed by
a condition that is now expressed by `ProvenIdle`, a predicate that already
existed for exactly this question.

**Can I reduce complexity?** The three-valued reading replaces "a bool plus a
convention about what false means", and the convention was the defect. Every
consumer states its own answer once, checked by the compiler, rather than
inheriting a default from a type that could not express the case. The one addition
— `PowerReadFailure` — decides nothing; it is evidence, and it exists because the
trigger has survived three investigations without it.

**Can I test this?** The harness could not build the state at all, and worse, it
could not have failed. `simTart` flattened everything into the same bool, and
`drainAbortsNow` answered the stopped-recovery re-read from the simulated
hypervisor's own truth rather than through the enumeration — so the harness's
guard caught a misreport that production's guard, wired to one method, cannot.
That is a **world-model defect** of #239's exact shape (ADR 0043: *a simulated
port answers from the source production answers from, or it is testing a different
fleet*), and it is the fourth of them; it is corrected here, and it is what let
this incident happen twice inside a green sweep. The `unreadable_power` fault, the
mirror correction, and one pinned property — *no instance is reclaimed on a
premise the fleet never established* — are part of the fix. The property is proved
red on the pre-fix flattening and stated over the PLAN rather than over the abort,
because a test that asserted "the drain aborted" would have been green through
both nights.

This is a **fleet defect** behind a **world-model gap**. Property (k) and property
(p) were correct throughout, and the two rule reversals above are what their
first true world demanded.
