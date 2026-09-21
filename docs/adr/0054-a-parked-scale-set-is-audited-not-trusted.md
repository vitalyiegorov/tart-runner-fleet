# ADR 0054: A parked scale set is audited, not trusted

## Status

Accepted, and **amended on 2026-09-21**, and **scoped to parked sets only** by
[ADR 0056](0056-a-stranded-bound-scale-set-is-recreated.md), 2026-09-21: nothing
in this record covers a set the auditing node BINDS. The `parked scale sets` row
stays informational exactly as the amendment below decided; a bound set GitHub
has stopped delivering for is a different case with different evidence, and it
fails the `ingest delivery` row instead. The one sentence here that ADR 0056
overrides is the Decision's aside that a bound set is "left uncounted rather
than paid for": a bound set whose listing carried no statistics is now read,
because those counters are the whole evidence for that finding.

The 2026-09-21 amendment is *A node's audit is evidence, the
verdict is the hub's* at the end of this record, which supersedes the parts of
the Decision below that make a node-side reading a failing finding. Closes the
detection half of issue #164 for the federation topology
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) permits, and stands
beside [ADR 0026](0026-queued-demand-expires-on-proven-absence.md), which handles
the opposite direction: demand the fleet believes in that GitHub no longer has.

## Context

GitHub routes a queued job to exactly **one** matching runner scale set. It marks
that set `totalAssignedJobs` / `totalBusyRunners` and then stops offering the job
— including to a different, healthy scale set carrying identical labels in the
same repository. A scale set that exists on GitHub and that no daemon polls is
therefore a black hole for every label it advertises.

Under shared-label federation this is not an exotic state; it is what *parking* a
profile means. A node stops serving a profile by removing its scale sets from
that node's configuration, and the objects stay on GitHub.

Two incidents, both user-visible, neither detected by anything:

- **2026-08-04, issue #164.** `vitalyiegorov/suuudokuuu` run 30918713641: two
  `mobile-e2e` jobs labelled `self-hosted,macOS,ARM64,macos-builder` queued at
  14:36:00Z and still queued **4.5 hours later**. `trf-sudoku-builder` (id 1,
  mac mini, polled, healthy) read assigned 0 / busy 0 and served four other
  builder jobs that afternoon. `trf-sudoku-builder-studio` (id 7, mac studio,
  parked) read **assigned 2 / busy 2**. `fleet queues` read 0, `fleet doctor`
  read PASS, `fleet health` read ready, every observation read fresh, and
  `fleet_queue_oldest_age_seconds` stayed 0.
- **2026-09-13.** The v0.1.549 release job sat **six hours** on the studio's
  withdrawn `linux-1x2` set, in exactly the same shape.

Nothing in the fleet could see either, and not for want of data: every signal a
node publishes is about the scale sets it *serves*. A set nobody serves is
outside the vocabulary.

The second, complete source of queue truth that would have caught this — ADR
0015's REST job inventory — is dormant in production: `github.canonicalJobInventory`
is off on every node, gated on issue #153. An audit that depended on it would
detect nothing on the fleet it was written for, and the per-repository job
inventory path is explicitly not available to this work
([SECURITY.md](../SECURITY.md)).

## Decision

**A scale set GitHub holds is audited against this node's configuration, and a
parked set holding work is a finding.**

`fleet scale-sets audit` lists, for every configured scope, the scale sets that
exist in that scope's runner group, and classifies each as **bound** (this node's
configuration names it — by the id it serves, or by name only for a set whose id
it has not yet persisted) or **parked** (it exists and this node does not name
it, which includes a set GitHub recreated under a configured name with a new id,
because the node's session still polls the old one). It reports GitHub's own statistics per set and exits `5` when
a parked set holds assigned jobs or busy runners, and `0` when none does. Every
other way an audit can fail to produce a trustworthy result — an unreachable
GitHub, a missing credential, an answer too uncertain to classify — is `4`.
**Unavailable is never a pass**: "GitHub did not answer" and "no set is parked"
are the two states issue #164 was lost between. Only a malformed request is the
usual `2`.

The authority daemon runs the same audit on a slow cadence
(`github.parkedScaleSetAuditMinutes`, default 15, `0` disables) and publishes
`parkedScaleSets` in `fleet status`, a `parkedScaleSetCheck`, the metrics
`fleet_parked_scale_set_assigned_jobs` / `_busy_runners`, and a `parked scale
sets` doctor row.

**The finding is worded from what this node can honestly claim.** It cannot read
a sibling's configuration, so a parked set is very often legitimately the
sibling's — that is ADR 0034's ordinary case, and an idle parked set is therefore
informational and silent. A parked set *with assigned or busy work* is reported
regardless — unless GitHub reports a registered runner, which is itself proof of
a listener (a sibling's, under the shared labels) — and says so: *nothing can be
listening to this set.* That sentence is the whole epistemic position; a
stronger one would be false.

**Three states, never two.** A daemon that predates the check renders `not
reported by this daemon`. A daemon that has never completed an audit — every
observe-mode node, which has no GitHub App authority and never audits — renders
`not audited`. Only a completed audit that found nothing renders `no parked scale
set holds work`. Collapsing the middle state into the third is precisely the PASS
that ran for 4.5 hours.

**It reuses the client and the credential `scale-sets provision` already uses.**
The runner scale-set admin API is the same `_apis/runtime/runnerscalesets`
resource the fleet creates and looks sets up at, reached with the same GitHub App
installation authority. The vendored `actions/scaleset` client exposes only the
name-filtered form of that call and asserts that its transport is an
`*http.Transport`, so the listing is obtained through the client's own
retryablehttp hooks: the request hook removes a sentinel name, making the call
the group listing, and the response hook keeps the answer and hands the client a
well-formed empty listing. This is deliberate. The alternative was a second
implementation of the Actions-service admin handshake, which could drift from the
one the fleet provisions with or come to hold a different authority.

## Consequences

- **API cost, bounded by construction.** One listing per scope per audit, plus
  one read per parked set whose listing carried no statistics, and no polling
  loop anywhere. At the default cadence a two-scope node spends a handful of
  admin-API calls an hour. A faster cadence would buy nothing: the condition
  persists for hours by nature.
- **The remedy is manual, and the ADR says so rather than implying otherwise.**
  An operator cancels and re-runs the workflow, or binds the parked set on a
  node. Neither is automatable here yet.
- **Not addressed: automatic cancel and re-run.** Cancelling a stranded job
  through the API requires repository `actions: write`, an authority no node in
  this fleet holds and none should: a token that can cancel any workflow run in
  every scope a node is installed on is a far larger blast radius than the
  stranding it would repair. Re-routing the work properly needs the hub (ADR
  0036) to move a binding between nodes, which does not exist.
- **Not addressed: recovery on a set this node DOES own.** Issue #164's second
  ask — generating JIT for an assignment with no `runnerRequestId` — is a
  separate change on a different lane, and this one deliberately stays read-only.
- **Parking still means "leave the object on GitHub".** This ADR makes that state
  visible rather than illegal. Deleting a scale set on park would also close the
  hole, and would take with it every job already assigned to it; a set that is
  audited can be un-parked instead.
- **An observe node is unaffected.** It never audits and publishes no rows, which
  its status document states as an absence and its doctor row reads as `not
  audited`.

## Amendment, 2026-09-21: a node's audit is evidence, the verdict is the hub's

**Status.** Accepted, amending the Decision above. The detection stays; the
`finding` becomes evidence.

### What the live fleet showed

The first cadence runs after #324/#325/#326 landed produced, on 2026-09-21, a
set of readings that the Decision above cannot tell apart from issue #164:

- The mac mini had real jobs **QUEUED** — behind its own capacity, with no runner
  booted yet — on three sets it **binds**: fleet-repo `trf-fleet-large` (2 jobs),
  suuudokuuu `trf-sudoku-builder` (2), budgie `trf-budgie-builder-2` (1). From
  node-b those sets are parked, and each read `assigned=2 busy=2 registered=0
  acquired=0 running=0`. `Stranded()` was therefore true for all three, and
  node-b's doctor FAILED on the mini's ordinary backlog.
- node-b's **own bound** set, budgie `trf-budgie-linux-amd64-2x4` (id 16), read
  `assigned=4 busy=4 registered=0` while node-b's queue for it held **0** jobs
  and the only budgie run in progress was on macOS. GitHub's per-set counters are
  stale or incoherent even for a set the reading node serves.
- The genuinely stranded sets seen on 2026-09-14 (rnw `maestro-studio`, the
  budgie builders) carried **the same signature** as those false positives.

The registered-runner exception added in #326 does not rescue the predicate: a
job GitHub has assigned but for which no runner has yet booted reads
`registered=0` for as long as the boot takes, and a node behind its capacity can
sit there for many minutes. The signature is shared by a real stranding, a
sibling's backlog, and a stale counter.

### Decision

**A node-side audit produces evidence. Only a fleet-wide view produces a
verdict.** One node knows its own configuration and GitHub's per-set counters.
It cannot read a sibling's configuration, a sibling's queue, or the age of the
counters it was handed. "No node listens to this set" is a statement about the
whole fleet, and the hub (issues #175/#218, ADR 0036) is the only place that can
make it.

Concretely, and replacing the corresponding sentences above:

1. The `parked scale sets` doctor row is **informational and never FAILS**.
   `parkedScaleSetCheck.ok` is `true` whenever an audit has completed, and its
   `reasons` carry one evidence line per parked set holding work with no
   registered runner. The row renders `no parked scale set holds work` when there
   is none, and otherwise `evidence: <n> parked set(s) hold work (…); a sibling
   may be serving them — confirm with `fleet scale-sets audit` on every node
   before acting`. The three states are untouched: `not reported by this daemon`,
   `not audited`, and a completed audit.
2. `fleet scale-sets audit` exits **`0` by default** with its findings printed as
   evidence and a footer naming the confirmation step, and exits `5` only with
   the new **`--strict`** flag — for an operator or script that has already
   audited every node and accepts the false-positive risk. Exit `4` is unchanged:
   unavailable is still never a pass.
3. `Stranded()` keeps its name and is documented as *the strongest signal one
   node can read*; `Reason()` says the set **may be stranded**, never that
   nothing can be listening.
4. The Prometheus gauges are unchanged. The runbook alert becomes advisory: page
   only when the same `(scope, scale_set)` pair shows work from EVERY node's
   audit.

### Why not tighten the predicate instead

Two tightenings were considered and rejected as unavailable from one node:

- **Require `acquired == 0 && running == 0`.** The mini's queued jobs read
  exactly that. It removes no false positive and would hide a stranding whose
  counters happen to be non-zero.
- **Wait for the same reading to persist across N audits.** A backlog behind a
  saturated node persists for hours; that is precisely the shape of the incident
  it would have to be distinguished from. A timer cannot separate them; a second
  node's answer can, in one reading.

The honest fix is a second observer, and the fleet does not have one yet. Until
it does, the node reports what it saw and names who must confirm it.

### Consequences

- **The 4.5-hour silence of issue #164 is still visible**, in the status
  document, the metrics, the `scale-sets audit` output and the doctor row's
  detail. What changed is that the surfaces no longer claim more than one node
  can know, and no longer fail a node for a sibling's healthy traffic.
- **A FAIL nobody can trust is worse than a PASS with evidence.** A doctor row
  that fails whenever a sibling is busy is a row an operator silences, and a
  silenced row detects nothing at all.
- **This is a blocking argument for the hub.** The cross-node correlation this
  amendment defers is the smallest useful thing the hub must do (issues
  #175/#218).
- **Not addressed here: a set this node SERVES that GitHub has stopped
  delivering for.** That is issue #336 and
  [ADR 0056](0056-a-stranded-bound-scale-set-is-recreated.md). The facts this
  amendment found missing — whose queue, whose instance, how old the reading is
  — are all this node's own when the set is bound, which is why a verdict is
  available there and not here.
- **Not addressed: a node-to-node audit exchange.** Nodes do not talk to each
  other; an admin socket is local and unauthenticated by design. Correlation
  happens where the operator or the hub stands, not between daemons.
