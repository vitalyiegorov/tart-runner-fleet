# ADR 0056: A stranded bound scale set is a fleet defect, and the remedy is recreation

## Status

Accepted, 2026-09-21. Closes issue #336. Stands beside
[ADR 0054](0054-a-parked-scale-set-is-audited-not-trusted.md), which handles the
sets this node does NOT serve, and whose 2026-09-21 amendment deliberately made
the parked reading evidence rather than a verdict.

## Context

Three times on 2026-09-21, on two different nodes, a runner scale set the node
**binds** stopped receiving work and nothing in the fleet said so:

| node | set | GitHub's counters | this node's queue | remedy |
|---|---|---|---|---|
| node-b | 17 `trf-budgie-linux-amd64-4x8` | assigned=3 busy=3 registered=0 acquired=0 | empty, no log line for the set all day | deleted 17, provisioned 19, restarted; delivery within ~60 s |
| mac mini | 2 `trf-pony-maestro` | assigned=3 busy=3 registered=0, nothing delivered since 05:33Z | empty | deleted 2, provisioned 7; three jobs delivered within 60 s |
| mac studio | 6 `trf-pony-maestro-studio` | assigned=2 busy=2 registered=0 | node paused | to be recreated when the node resumes |

Jobs sat queued for hours. Every check PASSED, **including** the doctor's
`ingest delivery` row, whose sentence — *every set is being offered the work
GitHub has for it* — was false: GitHub's per-set counters were stale, the
broker session polled healthily and was handed nothing, and the node's own queue
for the set was consequently EMPTY, so no queue-derived rule had anything to
breach. The only remedy that worked was **deleting the scale set and
provisioning a replacement with a new id**, which an operator had to do with a
program compiled in `/tmp` against the `actions/scaleset` SDK.

ADR 0054's amendment had already shown the same reading on a healthy fleet: a
job GitHub assigned but for which no runner has booted yet reads
`assigned>0 busy>0 registered=0` for as long as the boot takes, and node-b's own
bound set 16 read `assigned=4 busy=4 registered=0` with an empty queue while
nothing was wrong. So the counters alone are not a fault, and the amendment
concluded that a second observer was needed. That conclusion holds for a
**parked** set, where the missing facts belong to a sibling. It does not hold
for a set this node **serves**: there, the two facts that separate a wedge from a
boot are this node's own.

## Decision

**A bound set GitHub reports as holding work, with no registered runner, no
instance on this node, for longer than this node's boot timeout, is a fleet
defect. It fails the node's ingest-delivery check, and the remedy is to recreate
the set under guard.**

The predicate is stated once, in `internal/scalesetaudit`, as two named terms:

- `Starving()` — this node serves the set, GitHub reports assigned jobs **and**
  busy runners, no runner is registered, and this node holds **no instance** for
  the set's profile.
- `Wedging(now, bootTimeout)` — a `Starving` reading that has stood longer than
  `timeouts.boot`, the bound the node already declares on how long a runner may
  take to register. No new knob is introduced; the axis already exists.

The clock the second term needs is produced by `Track`, a pure fold of one audit
over the previous one: a qualifying reading keeps the instant it was first seen,
and a reading that stops qualifying — a runner registered, an instance booted,
the work drained — drops its clock entirely. The authority's existing
parked-set auditor keeps that map across its cadence and supplies the instance
count from its own telemetry. A profile the node has published no instance count
for is reported as **unobserved**, never as zero instances: a daemon that has
not completed a tick knows nothing about its own instances, and reading that
silence as "no instance" would invent the finding.

Surfaces:

1. **`fleet doctor`'s `ingest delivery` row FAILS**, naming the set and the
   remedy. That row is where the fault belongs: it is the node's statement that
   work GitHub holds is not reaching it, which is exactly what happened.
2. **The `parked scale sets` row stays informational**, unchanged. ADR 0054's
   amendment is untouched — a bound set is a different case with different
   evidence, not a reason to re-arm a verdict nobody can trust.
3. **`fleet scale-sets audit` exits 5** on a bound finding, with or without
   `--strict`: unlike the parked evidence it is a verdict. The command reads the
   counters itself and asks the daemon on this node for the local half; without
   a daemon to ask it reports the bound sets as **unjudged** rather than
   guessing either way.
4. **`fleet status --output json` carries `strandedScaleSets`**, each with the
   counters, the profile, when the reading began, and the sentence.
5. A bound set whose listing carried no statistics is now **read** (one extra
   admin call per uncounted set per cadence). Its counters are the whole
   evidence; leaving a bound set uncounted, as ADR 0054 did, would leave this
   defect invisible on any scope whose listing omits them.

**The remedy is `fleet scale-sets recreate NAME --config PATH [--scope NAME]
--confirm recreate-scale-set --reason TEXT`.** It deletes the named set on
GitHub, provisions a replacement with the same name, labels and runner group,
and writes the new id into the configuration atomically, through the machinery
`scale-sets provision --write` already uses. It refuses without the exact
confirmation token and a non-empty reason, refuses a name the configuration does
not carry (exit 3), refuses a name more than one scope carries until `--scope`
names one (exit 6), and refuses a replacement that came back with the **same
id** (exit 6) — that means the delete did not take and the set is still
stranded. It prints `old id -> new id` and tells the operator to restart the
daemon.

**Why a replacement and not a repair.** GitHub routes a queued job to a scale-set
**id**. The object's counters are the fault, and no update to labels, group or
runner setting clears them; ADR 0023's in-place repair exists precisely so a
drifted set does NOT lose its id and orphan its queue, which is the opposite
requirement. Recreation is the only action observed to restore delivery, three
times out of three, within a minute.

**Delete is a port the provisioning path does not hold.** `provision.Recreater`
is `provision.Client` plus `Delete(ctx, id)`, and `scale-sets provision` holds
only the `Client`: the ordinary path could not delete a scale set if it tried.
The adapter method takes an id and nothing else — no listing, no name
resolution, no "delete what does not match".

**The command does not restart the daemon.** Binding the new id is a service
action with its own evidence. A command that deletes a GitHub object and
restarts the controller in one breath leaves nobody able to say which half
failed.

## Consequences

- **The 2026-09-21 silence is visible from the node itself**, on the row whose
  sentence was false, with no hub and no second observer. The hub (issues
  #175/#218) is still required for the parked case, and this ADR does not
  reduce that argument: it only removes the cases where one node had the facts
  all along.
- **A false positive costs a recreation.** It requires a set to hold work with
  no registered runner and no instance for longer than `timeouts.boot` (3
  minutes by default), and the finding clears itself the moment GitHub resumes
  delivering: it is a live reading, never a latch.
- **API cost.** One admin read per uncounted set per audit cadence (15 minutes
  by default), on top of ADR 0054's one listing per scope.
- **Not addressed: automatic recreation.** Deleting a GitHub object on a
  daemon's own judgement is a far larger authority than this fleet grants any
  node, and a stale counter is not distinguishable from a GitHub outage at the
  moment it starts. The detector reports; an operator recreates.
- **Not addressed: the jobs already assigned to the deleted set.** They are lost
  with it and must be re-run, exactly as ADR 0054 records for a parked set.
  Cancelling them through the API needs repository `actions: write`, which no
  node holds.
