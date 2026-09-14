# ADR 0054: A parked scale set is audited, not trusted

## Status

Accepted. Closes the detection half of issue #164 for the federation topology
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
configuration names it, by id or by name) or **parked** (it exists and this node
does not name it). It reports GitHub's own statistics per set and exits `5` when
a parked set holds assigned jobs or busy runners, `0` when none does, and `4`
when GitHub could not be reached or the credential was missing. **Unavailable is
never a pass**: "GitHub did not answer" and "no set is parked" are the two states
issue #164 was lost between.

The authority daemon runs the same audit on a slow cadence
(`github.parkedScaleSetAuditMinutes`, default 15, `0` disables) and publishes
`parkedScaleSets` in `fleet status`, a `parkedScaleSetCheck`, the metrics
`fleet_parked_scale_set_assigned_jobs` / `_busy_runners`, and a `parked scale
sets` doctor row.

**The finding is worded from what this node can honestly claim.** It cannot read
a sibling's configuration, so a parked set is very often legitimately the
sibling's — that is ADR 0034's ordinary case, and an idle parked set is therefore
informational and silent. A parked set *with assigned or busy work* is reported
regardless, and says so: *something must be listening to this set and, from here,
nothing is known to be.* That sentence is the whole epistemic position; a
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
