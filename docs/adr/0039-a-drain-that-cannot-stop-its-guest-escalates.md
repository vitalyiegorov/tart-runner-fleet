# ADR 0039: A drain that cannot stop its guest escalates

## Status

Accepted. Closes
[issue #233](https://github.com/vitalyiegorov/tart-runner-fleet/issues/233).

It gives [ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md) an
enforcement action that can actually finish, adds the elapsed bound
[ADR 0007](0007-durable-runner-cleanup.md) reasoned about but never had, makes
[ADR 0021](0021-dischargeable-dead-letters.md)'s escape hatch reachable in the
situation it exists for, extends [ADR 0020](0020-diagnosable-drain-failures.md)
from "say why it failed" to "do something different about it", and adds the
first executor verbs since [ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)
fixed the port.

## Context

On 2026-08-10 the instance `trf-macos-6x12-f458a747883b9a0d` was created on the
mac studio at 12:52:41Z for `vitalyiegorov/suuudokuuu` run 31381784301, job
"Build embedded iOS E2E app". **The job succeeded in two minutes**, 12:53:02Z to
12:55:00Z.

The drain then never finished. Read from `transition_history` on the studio, the
instance entered `deregistering` at 12:55:01Z and left it at 14:17:20Z: **4939
seconds**, during which its drain operation recorded 67 attempts, every one
failing identically with `runner lifecycle failed at stop`. It reached `deleted`
nine seconds later.

For those eighty-two minutes the instance held **6 CPU / 12288 MB — the mac
studio's entire budget** — for a job that had been over since minute two. Twelve
jobs queued behind it across eight queues, the oldest waiting 3h06m.

A manual `tart stop` from a shell ended it in about thirty seconds, exit 0. The
fleet then completed the drain, reaped the record, and had a new VM running
within seconds.

### Why the daemon's stop failed where a shell's succeeded

This is the first thing the issue asked to establish rather than assume, and it
is establishable from the code, the CLI, and the durable record together.

`tart stop` takes `--timeout <seconds>`, documented as "Seconds to wait for
graceful termination before **forcefully terminating the VM**", default 30. So a
shell `tart stop` against a guest that ignores the shutdown request waits thirty
seconds and then kills it — which is exactly the "roughly half a minute, exit 0"
the operator measured.

The adapter ran a **bare** `tart stop <name>` inside
`context.WithTimeout(ctx, CommandTimeout)`, with `tartControlTimeoutSeconds: 45`
on the studio. Two consequences, and the second is the defect:

1. The daemon's stop was a race between tart's own escalation and the daemon's
   deadline, with no coordination between the two numbers at all — neither the
   adapter nor the configuration knew tart had an internal timeout.
2. When the deadline won, `exec.CommandContext` SIGKILLed `tart stop` — **the
   very process whose next act would have been to force the guest off**. The
   daemon could therefore never be more forceful than "ask nicely until the
   deadline", however many times it tried, while an operator's unbounded shell
   command escalated and won.

The durable record fits that and nothing else. The drain's retry policy is a
fixed 30-second backoff ceiling with no jitter, reached by attempt 6. Sixty-seven
attempts across 4939 seconds, allowing for the early attempts' shorter backoff,
is **≈45.5 seconds spent inside each attempt** — the command deadline, to within
the measurement. A stop that had failed fast (an invalid name, an ownership
conflict, an absent VM, tart exiting non-zero) would have produced ~30-second
cycles and roughly 165 attempts in the same window. It did not.

What is *not* established, and does not need to be: precisely why that
particular guest needed more than 45 seconds to die. `tart ip --wait 5` returned
"no IP address found" while the `tart run` process was alive, so the guest was
wedged rather than absent. The fix does not depend on the answer, and
deliberately so — a fix that depended on it would be a longer timeout, and a
longer timeout is a guess about a number the next wedge will exceed.

### Four defects, and why three of them are not the first one

**The retry ladder had no rungs.** Sixty-seven attempts, all identical. ADR 0007
is right that owned cleanup must keep retrying rather than abandon an owned VM,
and ADR 0020 made those retries say *why* they failed. Neither made them do
anything *different*. A stop that has failed three times will not succeed on the
sixty-eighth by being asked again.

**Operations never dead-lettered, so the escape hatch was unreachable.**
`fleet operations discharge` refused with `operation_not_dead` after 67 attempts
and 90 minutes; `fleet operations` reported `retrying 1, dead 0` throughout. The
recovery path docs/OPERATIONS.md documents was unavailable in precisely the
situation it exists for. The cause is an arithmetic slip in ADR 0007's own
reasoning: 720 attempts was described as "six hours at the thirty-second backoff
ceiling", which is only true if an attempt costs nothing. Each of these cost 45
seconds, making the real ceiling **fifteen hours**.

**ADR 0036's occupancy budget could not have rescued this, because its remedy is
the thing that was broken.** The budget bounds how long an instance may hold its
vector, and its enforcement action is a drain. Here the drain *was* the failure.
Even had the two-hour macOS budget expired — this was ninety minutes and
climbing — it would have enqueued another drain into the same wall. A budget
whose only enforcement action can itself hang is not a bound at all; it is a
statement of intent.

**Nothing named it.** `fleet doctor` reported `queue_incident,queue_slo_breached`
— the symptom — alongside `PASS occupancy` and `PASS reservation`, both true and
both irrelevant. Every fact needed to diagnose it was already in the operations
and instances tables, and none of it was published. Finding it took SSH, a
hand-copied SQLite file, and a read of the operations table.

## Decision

### The graceful stop names its own window

`tart stop` is invoked with an explicit `--timeout` of one third of the
adapter's command deadline. tart's escalation and the teardown that follows it
then both happen **inside** the deadline instead of being killed by it. The
daemon must never be the thing that kills the process that was about to
escalate.

This alone would probably have ended the incident. It is not the fix, because it
is still a number, and the next wedge is under no obligation to respect it.

### A drain that is already decided climbs a ladder

The executor port gains two verbs, and both are demands on every future node, so
they arrive here rather than in a commit that widens an interface:

| verb | Tart | containers |
|---|---|---|
| `Stop` | `tart stop <name> --timeout <third>` | `podman stop --time <grace>` |
| `Terminate` | `tart stop <name> --timeout 0` | `podman stop --time 0` |
| `Destroy` | `Terminate`, then `tart delete <name>` | `Terminate`, then `podman rm --force` |

`lifecycle.StopEscalation` chooses the rung from how many attempts the drain has
already spent failing **at the stop step**: three graceful, then three forced,
then three destructive, then the ladder is exhausted. At the incident's measured
cost of one command deadline plus one backoff ceiling per attempt, that is
roughly four minutes to stop being polite, eight to remove the guest, and twelve
to publish a dischargeable dead letter, against ninety and climbing.

The attempt count is gated on the closed code of the operation's last failure,
so a drain that has spent forty attempts being refused by GitHub at the
deregister step opens at the gentlest rung. It has not yet asked its guest to
stop even once, and must not buy force with failures that happened somewhere
else.

`Destroy` is **not** a relaxed `Delete`. It keeps every guard `Delete` keeps —
durable ownership, and fresh deletion confirmation that the runner and its jobs
are inactive — and differs only in refusing to wait for a shutdown the guest has
already proved it will not perform.

### Force-deleting a guest is not the forbidden `kill -9`

This needs saying explicitly, because the two are one word apart and the next
reader will otherwise conflate them.

The prohibition this fleet operates under is about **the fleet daemon and the
GitHub message sessions it owns**. A scale-set message session is a lease on
GitHub's side with at-least-once delivery semantics; killing `fleetd` without
letting it commit and release loses acknowledgements and strands sessions, which
is why ADR 0009 exists and why the daemon is never `kill -9`'d.

`Terminate` and `Destroy` signal **the guest**, and only ever a guest whose work
is provably over: the deregistering arm is reached after the runner has been
removed from GitHub and its deletion confirmed. There is no session to lose, no
acknowledgement outstanding, and no job to interrupt — the job finished at
12:55:00Z. Powering off an ephemeral VM whose runner is already gone is the
ordinary end of that VM's life, arriving by a different route.

### The ladder never cuts a guest whose job is running

Escalation is reachable from exactly two arms of `DrainExecutor`, and no other:

- **`deregistering`**, where the runner is gone and its deletion confirmed.
  Every other drain phase re-verifies its premise and aborts on fresh busy
  evidence *before* this point (ADR 0033), so an instance that reaches it has
  already passed the busy guard.
- **ADR 0036's occupancy arm**, where an operator-configured ceiling has already
  judged the hold and the drain is decided.

There is no path by which an attempt count overrules the busy guard. A
recovery drain of a runner that turns out to be executing a job aborts to
`Running` as it always has, at any attempt count.

### ADR 0036's remedy goes through the ladder, which is what gives it teeth

The occupancy arm's stop — the one ADR 0036 deliberately orders *before*
deregistration, because GitHub will not remove a runner it considers busy —
climbs the same ladder. That is the whole relationship between the two records:

- ADR 0036 supplies the **judgement** that a hold has gone on too long. It is
  the only mechanism in the fleet that may end a running CI job, and every
  constraint it places on itself stands unchanged.
- ADR 0039 supplies the **means**. Before it, ADR 0036's reclaim could ask a
  wedged guest to stop and be refused indefinitely, and its next act would have
  been to enqueue the same request again.

A budget is only a bound if the thing it triggers terminates. It now does.

### Both ceilings become real, and a proven-permanent failure parks at once

`RetryPolicy` gains `MaxElapsed`, and `DurableCleanupRetryPolicy` sets it to the
six hours ADR 0007 already reasoned about. Attempts and elapsed time are now both
bounded, and the bound no longer depends on what an attempt happens to cost. An
unknown creation instant disables it rather than expiring the operation: an
unreadable start is not a long one.

Separately, an executor may return `operations.ErrExhausted`, and the worker
parks such an operation immediately. When the ladder has tried every rung and
every rung has failed, no ceiling sized for a fault that might still resolve
itself can discover that in time — and the executor already knows. An ordinary
refusal, `deregister:runner_busy`, which GitHub may legitimately issue for as
long as a six-hour job runs, keeps retrying exactly as ADR 0007 requires.

The persisted text of an exhausted failure is byte-identical to the ordinary
failure at the same stage. Terminality travels as a Go error, never as text, so
`lifecycle.FailureCode` and every operator surface keep reading one closed code.

### The fleet names what is not progressing

A new durable projection, `StalledOperations`, unions the operations still
retrying with the instances still held in a cleanup state. The second half is
not redundant: an instance whose drain has already dead-lettered has no retrying
operation at all, and is exactly the row most in need of a name.

`fleet doctor` gains a `drain progress` check that fails on an operation past six
failed attempts or an instance held in a cleanup state past ten minutes, naming
the instance, the step, the attempt count, and the elapsed time. Six is the point
at which the ladder has already stopped being polite; a healthy drain takes
seconds. `fleet status` gains a `STALLED` table beside the occupancy one.

Both are additive `fleet.v1` fields with an `Effective*` accessor, so a new CLI
against an older daemon reports an unspecified check rather than a measured pass:
a daemon that cannot see a condition must never render as having found none.
Persisted failure text never reaches the operator API — the step is the same
closed vocabulary the failure aggregate and the dead letters publish, and the
drain state is a durable enum, checked rather than trusted because it is rendered
as a metric label.

### The harness learns that a step can fail forever

This is the part the issue called the most valuable, and it is right: **the DST
harness could not model a lifecycle step that fails indefinitely, which is why
this shipped.** Every fault it could generate was bounded by construction —
`wedged_drain`, the nearest thing it had, decays a few ticks after it is armed,
and it holds a drain *before* deregistration in any case, so it never reached
the stop edge at all.

Three changes (ADR 0031):

- A new event, `unstoppable_guest`: a guest that refuses to power itself down,
  and **never decays**. Nothing about a wedged macOS guest gets better because
  time passed. Its level is how much force it finally yields to — a forced
  power-off, or removal. There is deliberately no level that survives removal;
  that is a broken host rather than a wedged guest, and the fleet's answer to it
  is a dead letter and a named `fleet doctor` finding, not a released vector.
- The mirror's `deregistering` edge becomes a real stop step that calls
  **production's own `lifecycle.StopEscalation`**, so the policy under simulation
  is the policy that ships. What the harness supplies is the physics.
- Property (o): once a drain has passed deregistration, the instance releases its
  vector within a bounded number of ticks, whatever the guest does. Its clock is
  the oracle's own, counted from the first tick it saw the instance tearing down;
  it does not read `UpdatedAt`, which the harness itself rewrites.

Property (o) picks the instance up exactly where property (k) puts it down. ADR
0036's oracle stops judging the moment a drain starts, on the stated grounds that
the drain's completion is "the drain's own contract, not the budget's". Issue
#233 is what happens when nothing enforces that contract, and that exclusion is
precisely the blind spot it hid in.

The incident is pinned twice, because a property is only worth having if it is
red on the defect: once with the ladder, where the vector comes back and the
queued job runs, and once with the pre-#233 executor that asked the same way
every time, where property (o) fires.

## Consequences

A drain now terminates. The worst case is bounded by the ladder rather than by
how long an operator takes to notice, and every stage of it is visible: the
`drain progress` check names it within minutes, the dead letter is dischargeable
within about twelve, and `fleet status` prints the step and the attempt count
without an SSH session.

The costs are real and worth naming.

**The fleet will now destroy a guest it previously only asked to stop.** The
blast radius is one ephemeral VM whose runner has already been removed from
GitHub and whose deletion has already been confirmed inactive — the same VM the
next successful stop would have deleted anyway, a few seconds later. What is lost
is whatever the guest might have flushed to disk during a graceful shutdown, and
an ephemeral CI guest that has finished its job has nothing to flush.

**Six hours is a real bound where there was none, and something will eventually
hit it.** A cleanup GitHub refuses for longer than its own maximum job duration
now dead-letters rather than retrying into the next day. That is the intent of
ADR 0007 rather than a change to it, and the outcome — a published, dischargeable
dead letter — is strictly more actionable than an invisible retry.

**Two more verbs is two more things a future backend must implement.** The
contract test pins the surface by reflection so the cost is paid explicitly. Both
are expressible in one flag on both supported backends, which is the bar a verb
in this port has to clear.

Two things are deliberately not addressed. The fleet still does not know *why* a
guest wedges, and does not try to find out; it bounds the consequence instead.
And the provision path keeps its unbounded attempt ceiling — a clone that retries
forever is a real gap, but it is a different failure with a different remedy and
no incident behind it yet.
