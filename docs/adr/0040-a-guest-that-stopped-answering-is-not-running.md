# ADR 0040: A guest that stopped answering is not running

## Status

Accepted. Closes the fleet-side half of
[issue #236](https://github.com/vitalyiegorov/tart-runner-fleet/issues/236).

It adds a sixth recovery cause to the five
[ADR 0036](0036-an-instance-may-not-hold-its-vector-forever.md) left, and it is
the second whose premise a busy runner does not disprove — so it inherits ADR
0036's stated exception to the abort rule of
[ADR 0033](0033-a-runner-is-bound-to-the-job-github-gave-it.md), and it reaches
the guest through the ladder
[ADR 0039](0039-a-drain-that-cannot-stop-its-guest-escalates.md) built. It is the
first recovery cause the fleet derives from a host-side measurement of its own
rather than from GitHub or from the durable ledger, which is what makes it the
first one that can act before GitHub does.

## Context

On 2026-08-16, eight jobs of `rnw-community/rnw-community` took their runners
down with them, across two profiles and three runs. Each ended with GitHub's
`The self-hosted runner lost communication with the server`, sixteen to eighteen
minutes after the job started, with `Start redroid container` left `in_progress`
and **no log uploaded for any of the eight** — `BlobNotFound` on every one.

The mechanism was reproduced host-side, on a dedicated probe VM sized to the
failing profile, twice, first attempt each time. The whole of it is one write:

```
Kernel panic - not syncing: sysrq triggered crash
CPU: 4 UID: 0 PID: 2131 Comm: init Not tainted 6.17.0-23-generic
Call trace:
 vpanic+0x364/0x408
 panic+0x6c/0x78
 sysrq_handle_crash+0x28/0x30
 __handle_sysrq+0xf8/0x298
 write_sysrq_trigger+0x128/0x228
 proc_reg_write+0xd4/0x160
 vfs_write+0xe0/0x3a8
```

The job's `--privileged` redroid container could not open `/dev/binder`, Android
`init` shut its "device" down one second in, and 300 seconds later `init`'s
reboot watchdog — which can never succeed inside a container — escalated through
`/proc/sysrq-trigger` to `c`. The guest kernel panicked. `kernel.panic` was `0`,
so it hung there forever.

### What the fleet saw, and why it was nothing

- `tart list` reported the VM **`running`**, and went on reporting it.
- The `tart run` process was alive at **0.0% CPU**.
- The runner agent's TCP connection was **never closed** — no FIN, no RST. That
  is literally what "lost communication" means here: the agent was not
  terminated, its kernel was.
- No userspace ran again, so no `if: failure()` step, no `ERR` trap, and no
  artifact upload could ever fire. **The class is self-concealing by
  construction, not by bad luck.**
- `tart exec` was refused within seconds — the one host-side signal that
  something was wrong, and nothing in the control plane consulted it.

The durable record of one of the eight, read from this node's
`transition_history`:

```
09:27:37  planned  -> cloning
09:28:25  assigned -> running
09:45:51  running  -> draining   (operation event-drain-trf-xl-0aacdbcc6653bd8a)
09:45:53  stopping -> deleted
```

The drain at 09:45:51 is the reaction to **GitHub failing the job**. The guest
had been dead since roughly 09:40. And there is **no daemon log line for the
instance at all** — not a warning, not a metric, not a doctor finding.

That is structural rather than a missing log. `assignmentRecoveries` could
recover a `running` instance from a stopped VM or from GitHub-derived evidence,
and a panicked guest inside a VM Tart still reports as running produces neither.
Every existing cause is a statement about **whether work is happening**; not one
of them is a statement about **whether the machine is alive**.

### What isolation cannot do

Four containments were tested on the probe, so these are measurements rather
than proposals:

| Containment | Result |
|---|---|
| `kernel.sysrq=0` | **No effect.** The guest already had it; a write to `/proc/sysrq-trigger` bypasses the sysctl by design, which governs only the keyboard path. |
| `-v /dev/null:/proc/sysrq-trigger` | **Refused by runc:** `cannot be mounted because it is inside /proc`. |
| Dropping `--privileged` | **Not available to this workload.** `--cap-add SYS_ADMIN --security-opt apparmor=unconfined` makes the container exit immediately. |
| `kernel.panic=10` in the guest | **Works.** Same panic at +305 s, then `Rebooting in 10 seconds..`; the guest was unreachable for **14 seconds** and came back. |

For the record, the exposure really is exactly the privileged grant: an ordinary
container shows `proc /proc/sysrq-trigger proc ro,...` in `/proc/mounts`, and a
`--privileged` one shows no mask entry at all.

**A job granted `--privileged` can panic its runner's kernel at will, and the
fleet cannot take that away while granting privileged at all.** The invariant the
issue asks for — "a job's own workload must not be able to terminate the runner
agent" — is not achievable by isolation for privileged jobs. It has to be met by
making the death **detectable, bounded, and attributable** instead.

## Decision

### The fleet asks its guests whether they are alive

Each tick, every `running`, powered-on instance's guest is asked to execute a
trivial command: `exec <instance> true`, the same verb readiness already polls at
boot, on the same neutral `executor.CommandRunner`. The port is not widened —
`execGuestProbe` sits beside `execReadiness` in the wiring package, and a second
backend changes one line rather than the contract
([ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)).

It is deliberately restricted to `Running`. An instance that never reached
`Running` has no guest worth this question, and the boot timeout and the
assignment deadline already own that ground. An instance whose VM is stopped or
absent is already reclaimable through a gate that needs no probe at all.

### The observation is three-valued, and that is the entire safety argument

```
alive     the guest executed the command
refused   the transport was refused: nothing in that guest is running
unknown   nothing was established
```

**Only `refused` counts.** An answered probe clears the run, and so does an
inconclusive one — a probe that ran out of its own deadline against a guest
running a monorepo Gradle build at full tilt has established nothing, and a
mechanism that can end a running CI job must not reach its threshold through
observations that measured nothing.

The classification is made from the probe's **own deadline** rather than from
anything the backend said. A command that returned before the deadline and failed
could not reach the guest; a command that ran out of it was too slow to tell.
Reading a backend's error text to separate them would put a Tart string in a
layer that must not know which machine it is on.

The cost is stated plainly: a guest whose probe is *permanently* inconclusive is
never declared dead. ADR 0036's occupancy budget remains the backstop for that
instance, exactly as it was before this record existed. That is the fail-open
side of the trade, and it is the right side to be on.

### A verdict needs a count and a window, and both are configuration

`domain.GuestLivenessPolicy` requires **both** an unbroken run of N refusals and
an elapsed window W. They bound different mistakes: the count bounds a momentary
refusal — a guest agent restarting, a control socket replaced — and the window
bounds a control loop that happens to tick quickly, so no amount of fast probing
can convert seconds of silence into a verdict.

The shipped default is **five refusals over ninety seconds**: about a hundred
seconds to a verdict at a twenty-second poll, against the sixteen to eighteen
minutes GitHub took. The floors are three refusals and thirty seconds, because
fewer than three is one agent restart away from a false positive and a window
under thirty seconds is shorter than a tick on some nodes. `guestLivenessRefusals: -1`
disables the mechanism outright and survives being written back: a destructive
bound must be as easy to turn off as it was to leave alone.

The bound is on by default, unlike every recent feature flag in this fleet. The
reason is that the failure it prevents is deterministic, silent, and eight for
eight, and the mechanism's own failure mode is fail-open by construction.

### The accumulator's clock is the scheduler's clock

`app.GuestLivenessTracker` carries the per-instance run between ticks, probes
concurrently, and forgets an instance the tick no longer reports. Its memory is
in-process rather than durable: a daemon restart forgets every run and starts
again, which is fail-open — the same dead guest is re-declared within one window,
and a restart can never inherit a verdict it did not observe.

Its clock is injected, and it **must be the same clock the scheduler plans on**.
This is not a stylistic point. `ProductionInventory.Observe` stamps its
observation from the wall clock while the scheduler judges on the tick's instant;
under the deterministic simulator those are thirteen days apart, the run is not
measurable at all, the verdict fails closed, and the property would have gone
green against a mechanism that never fires. The simulator caught exactly that.

### The reclaim is the sixth cause, and it outranks only the budget

`guestUnresponsive` joins `confirmedInactive`, `stalledAssignment`,
`lingeringRunner`, and `occupancyExceeded` in `assignmentRecoveries`. It yields
to every cause that re-verifies its premise at execution time, for the reason
ADR 0036 gives: those are evidence that no work is happening, and each aborts
when that turns out to be wrong.

It **outranks the occupancy budget**, and that is the one ordering worth arguing.
Both end a job GitHub still believes is running. A budget breach claims only that
a hold is long; a refused transport is fresh evidence that the guest executing
the job is gone. When both apply, the reclaim an operator reads must be the true
one.

Like a budget reap, and unlike every other drain, the durable operation names the
job — the repository, run, attempt, and job that died with the guest. Every other
cause implies there is no job left to name.

### The premise is re-verified, which no other cause of this kind can do

`DrainPhaseGuestUnresponsive` stops the guest **before** deregistration, for the
reason ADR 0036 established: GitHub refuses to remove a runner it considers busy,
and it will go on considering this one busy until its own grace timer expires —
the sixteen to eighteen minutes this exists to beat. Deregistering first can only
retry with the vector still held. A `runner_busy` refusal therefore does not
abort it.

But it differs from the budget in the way that matters most. **The budget has
nothing to re-verify; this does.** The drain probes the guest once more at the
moment of acting:

- **answered** — abort to `Running`. The guest came back, whatever the
  accumulator said a tick ago, and the same answer has already reset it.
- **refused** — proceed.
- **inconclusive, or no probe wired** — fail the guard and retry. Evidence the
  fleet could not read is never permission to end a job.

The stop climbs ADR 0039's ladder, so the vector comes back on a bound the fleet
owns even when the panicked guest also refuses to power down.

### The fleet says it out loud

This is half the issue, and the half that was completely absent. Three log lines,
each carrying the instance, the vector, the probe timeline, and the job binding:
one when a guest goes quiet, one when it is declared dead (rate limited per
instance and per state, so the escalation is never swallowed by the warning that
preceded it), and one — never rate limited — naming the job the reclaim is about
to end.

`scheduler.GuestSilences` is the one projection the warning, the metric, the
doctor check, and the reclaim all read, so they cannot disagree about a silence.
`fleet doctor` gains a `guest liveness` check that fails **only on the verdict**:
a partial run of refusals is the fleet watching, and a check that fires on that
is unreadable within a week. Four Prometheus series publish the measurement
beside the bound it is judged against, because two refusals is a hiccup or a
verdict depending only on which bound. Both `fleet.v1` fields are additive with an
`Effective*` accessor, so a new CLI against an older daemon reports an unspecified
check rather than a measured pass.

### The base image stops hanging, and starts leaving a trace

Two guest changes, both in `docs/LINUX_BASE_IMAGE.md`, which is this repository's
executable recipe:

- **`kernel.panic=10` and `kernel.panic_on_oops=1`.** Measured on the probe: the
  identical failing arm panicked at +305 s, printed `Rebooting in 10 seconds..`,
  and the guest was unreachable for fourteen seconds before coming back. One
  sysctl converts "a VM Tart reports as running, forever, holding 6 CPU and 12 GB"
  into a bounded reboot. The runner does not survive it — but the failure becomes
  a fact rather than a silence, and the host slot is not held hostage.
- **`console=hvc0` on the guest kernel cmdline.** The panic above was captured
  over netconsole because the guest's cmdline carries `console=ttyAMA0` while the
  VM exposes `hvc*`, so `tart run --serial` captures nothing. Fixing the cmdline
  makes the panic reach the VM's own console, and an opt-in
  `linux.serialLogDirectory` makes the adapter pass `--serial-path` so it lands in
  a host-side file. The knob is **off by default** and the flag is unverified on
  this fleet's tart build; a node enables it after checking `tart run --help`.

Neither is live until the image is rebuilt and rolled out, which is an operational
task with its own checklist on the issue. **This record does not claim they are
in production.**

## Consequences

A guest death is now a bounded, attributable event. The vector comes back in
about two minutes on the fleet's own measurement instead of sixteen to eighteen
on GitHub's, and every stage of it is visible: a WARN while it is happening, a
`fleet doctor` finding, four metrics, a named drain phase, and a durable operation
that carries the job.

The costs are real and worth naming.

**This is the second mechanism in the fleet that can end a running CI job, and
the first that acts on a measurement the fleet takes itself.** Five things bound
it. It requires a configured bound; it counts only hard transport refusals, never
slowness and never an unreadable probe; it requires both a count and an elapsed
window; every safer reclaim cause acts first; and the drain re-probes and aborts
on a guest that answers. A guest it cuts has refused every probe for longer than
any guest-agent restart takes, and has been asked once more at the moment of
acting.

**A permanently inconclusive probe means a guest that is never declared dead.**
That is the deliberate fail-open direction, and the occupancy budget still bounds
that instance.

**The probe is I/O in the observation path.** It runs concurrently, only against
running instances, under a five-second deadline, and its failure to run at all is
an unknown observation rather than a refusal — so a node whose probe is broken
loses the mechanism and nothing else.

Two things are deliberately not addressed. The fleet still cannot stop a
privileged container panicking the kernel it shares, and does not try; it bounds
the consequence instead. And nothing here recovers the *job* — a job whose machine
stopped is lost at the instant it stopped, minutes before the fleet can say so.
What changes is that it is lost visibly, and that nobody else's work waits behind
it.
