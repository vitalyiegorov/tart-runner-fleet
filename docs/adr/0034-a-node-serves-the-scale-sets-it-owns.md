# ADR 0034: A node serves the scale sets it owns, and no node knows another

## Status

Accepted. Supersedes the unbuilt multi-host design staged in
[issue #99](https://github.com/vitalyiegorov/tart-runner-fleet/issues/99) for
the current fleet shape, and reverses its one rejected option — *partitioned
scale sets per host* — on evidence that did not exist when it was written.
Reaffirms [ADR 0003](0003-sqlite-outbox.md)'s "SQLite is the single-host durable
authority" by making the host boundary the fleet boundary. Reuses the session
exclusivity established by [ADR 0002](0002-official-scale-set-protocol.md) and
classified by [ADR 0009](0009-recover-github-message-sessions.md) as the only
cross-node arbiter. Realizes the architecture component that
[ADR 0032](0032-resource-explicit-runner-labels.md) reserved but could not use:
`arch` in the canonical label. Amends
[ADR 0012](0012-shared-cross-platform-capacity.md) and
[ADR 0018](0018-second-pilot-elastic-host-envelope.md) — the shared envelope and
the elastic envelope are now per node, and gain a static ceiling below physical
capacity. Changes nothing in [ADR 0031](0031-deterministic-simulation-testing.md):
the simulated world stays one node, because a node is still one fleet.

Amended 2026-08-04 (**c**): nodes that advertise the same label must provide
equivalent guest capabilities. Recorded after a production failure caused by two
images behind one label; adds an invariant and a proposal, and changes no
mechanism.

## Context

The fleet runs on one Apple-silicon Mac mini (10 cores, 24 GiB). Two more
machines are arriving: a GEEKOM A9 Max (AMD Ryzen AI 9 HX 370, 12 cores /
24 threads, x86_64, Linux, KVM) and a Mac Studio at a different physical
location, shared with its owner's other work, where the fleet may use at most
about 4 cores and 10 GiB.

### The single host is provably the constraint

A live reading, taken while this record was written, on a host with exactly
`hw.ncpu = 10`:

```
QUEUES                    INSTANCES
PROFILE  JOBS  OLDEST     PROFILE  COUNT  CPU  MEMORY MiB
builder  2     56m43s     maestro  1      4    7168
xl       1     1h41m5s    xl       1      6    12288
```

One Linux `xl` guest (6 vCPU) and one macOS `maestro` guest (4 vCPU) hold all
ten cores. The macOS `builder` needs six and cannot have them, so two builder
jobs have waited nearly an hour, and a third Linux `xl` job has waited longer
still behind the guest that is already running. No scheduler change fixes this;
`docs/FLEET_ARCHITECTURE_PLAN.md` said so at line 722 — "when offered work
exceeds one Mac mini's non-preemptive capacity, reliability requires shorter
jobs or another host, not more control-plane complexity."

Seven days of instance starts show the split that matters:

| Repository | Linux starts | macOS starts |
| --- | ---: | ---: |
| `vitalyiegorov/tart-runner-fleet` | 364 | 0 |
| `vitalyiegorov/suuudokuuu` | 157 | 235 |
| `vitalyiegorov/knee-doctor` | 109 | 0 |
| `rnw-community/rnw-community` | 46 | 99 |
| `budgie-at/budgie` | 35 | 30 |
| **Total** | **711** | **364** |

Two thirds of every boot on this host is a Linux guest that is on Apple silicon
only because that is the only machine there was.

### The capabilities are disjoint, so there is no shared queue to lose

Issue #99 chose a fleet-wide steward because it assumed a homogeneous fleet:
a Mac mini and a Mac Studio, both Apple silicon, both able to run any profile.
Under that assumption its rejected-options table is right — "partitioned scale
sets per host ... abandons the shared queue, which is the requirement."

That assumption does not survive contact with an x86_64 node:

- macOS guests require Apple's Virtualization framework. The GEEKOM cannot run
  one, ever.
- `reactivecircus/android-emulator-runner` with `arch: x86_64`, which
  `rnw-community` already commits to `master`, requires `/dev/kvm` on an
  x86_64 kernel. The Mac mini cannot provide one, ever. That workflow is
  failing today for exactly this reason.
- Google ships no arm64 Linux NDK. `suuudokuuu` therefore builds its Android
  APK on the scarce macOS `builder` — its own comment says so: "The APK builds
  on macOS: Google ships no arm64-linux NDK (the linux package is x86_64-only),
  while the darwin NDK is arm64-native." An x86_64 Linux node dissolves that
  constraint.

When two nodes cannot run each other's work, a shared queue between them is not
a capability that partitioning abandons. It is a fiction. The unit of work
allocation is already the scale set, GitHub already refuses a second session on
one, and the honest arrangement is one owner per scale set.

### What GitHub actually enforces

`internal/adapters/githubscaleset/session.go:83-105` classifies a `409` as
terminal — "a 409 means another session acquired the scale set" — and the
recovery path discards and immediately recreates. Two daemons pointed at one
scale-set name in one scope resolve to the *same* server-side object and evict
each other in a loop, each with its own SQLite cursor and its own
`tart-runner-fleet-authority` lease that cannot see the other. There is no
supported topology in which two nodes share a scale set.

Read the other way, this is a coordination primitive that costs nothing: GitHub
already guarantees single-writer per scale set, arbitrates it centrally, and
requires no code of ours to uphold it.

### The past week is an argument about complexity, not about capability

The defects fixed since 2026-07-25 — ADRs 0026 through 0033, plus issues #123,
#129, #131, #132 — are all cross-pass composition inside *one* scheduler on
*one* host: a demand admitted twice in a tick, a reservation that starved its
remainder, a runner bound to the wrong job. [ADR 0031](0031-deterministic-simulation-testing.md)
exists because that class of bug is not reachable by unit coverage. Adding a
gRPC control plane, a steward election, per-host epoch fencing, and a
handoff record would multiply the state space that produced those bugs, before
any of the three machines has served a single job under the new shape.

## Decision

### 1. A node is a fleet

Each machine runs one `fleet` daemon with its own configuration file, its own
SQLite database, its own host envelope, its own credentials, and its own
updater. No node reads, writes, replicates, or is aware of another node's
state. There is no cross-node lease, no cross-node RPC, no shared database, no
node registry, and no fleet-wide readiness gate.

The daemon needs no new mode. A node that serves only Linux profiles simply has
`macosBurst.enabled: false`, and the two-platform admission logic of ADR 0012,
[ADR 0014](0014-opt-in-macos-exclusive-admission.md),
[ADR 0024](0024-mixed-macos-profile-cohorts.md), and
[ADR 0029](0029-remainder-admission-behind-a-reservation.md) is simply not
exercised there. Heterogeneity across the fleet is expressed as
*single-platform configurations per node*, not as a wider scheduler.

### 2. A scale set is served by exactly one node

Ownership is a property of configuration: a `(scope, scale-set name)` pair
appears in exactly one node's `fleet.json`. This is a validated invariant of the
shared configuration layout in §5, not a convention.

GitHub routes a job to a scale set by label, and the owning node is the only one
long-polling that scale set's session. Routing across nodes is therefore
GitHub's existing, already-load-bearing behaviour, and the fleet adds nothing to
it.

### 3. Nodes are partitioned by capability first, by scope second

| Node | Machine | Platform | Serves |
| --- | --- | --- | --- |
| A | Mac mini M4, 10c / 24 GiB | macOS on Apple silicon | macOS `builder` and `maestro` |
| B | GEEKOM A9 Max, 12c/24t x86_64 | Linux on x86_64 | every Linux profile, `trf-linux-amd64-*` |
| C | Mac Studio, remote, budgeted | macOS on Apple silicon | macOS `maestro` overflow for named scopes |

Nodes A and B have disjoint capability sets, so their partition strands nothing:
no job either can run is a job the other could have taken.

Node C overlaps node A. Its partition is therefore by **scope**: node C owns the
`maestro` scale set of specific repositories, node A owns the rest. A static
scope partition can strand capacity — node C idles while node A queues for a
scope node C does not own — and that is accepted for the MVP, because it is the
only arrangement in which no job can be offered to two owners. The measured
stranding is the trigger condition in §8.

Node C does **not** get a label that node A also advertises. Two scale sets in
one scope advertising one label makes GitHub's choice between them load-bearing
and undocumented; this record does not rely on undocumented behaviour. Whether
GitHub distributes across identically-labelled scale sets is Spike 3 in
`docs/MULTI_NODE_PLAN.md`, and until it is answered, ownership is by scope and
consumers change nothing.

### 4. The canonical label carries the architecture, as ADR 0032 intended

ADR 0032 spelled `arch` out in `trf-<os>-<arch>-<cpu>x<ramGiB>` precisely "so a
host of another architecture could never reuse these names." Node B's profiles
derive `trf-linux-amd64-1x2`, `trf-linux-amd64-2x4`, `trf-linux-amd64-4x8`,
`trf-linux-amd64-6x12`, and the shapes its larger core count affords. Nothing
about the derivation changes except that `arch` stops being the constant
`arm64` in `internal/config/labels.go:23` and becomes a property of the node's
configuration, still derived and still unable to lie.

Arch-*pinned* consumers name a canonical label and get exactly one architecture.
Arch-*floating* consumers name an alias — `linux-small`, `linux-medium` — which
node B advertises after node A stops. Migration is therefore a provisioning run
on node B and a scale-set removal on node A, not an edit to every workflow;
per-repository arch audits live in the plan document.

### 5. Configuration is repo-versioned per node, derived from shared definitions

Configuration decoding rejects unknown fields, accepts exactly one JSON
document, and supports no includes, overlays, or environment substitution
(`internal/config/config.go:281-312, 418-426`). Sharing is therefore a build
step, not a runtime feature:

```
config/
  fleet.example.json          unchanged, the observe-mode example
  nodes/
    shared/
      scopes.json             scopes, targets, installations - identical everywhere
      profiles.linux.json     the Linux variant matrix, arch-free vectors
      profiles.macos.json     builder and maestro vectors
    mac-mini.json             node A: overlay + scale-set ownership
    geekom.json               node B: overlay + scale-set ownership
    mac-studio.json           node C: overlay + hostBudget + scale-set ownership
  nodes/rendered/             generated, committed, what each node installs
```

`scripts/render-node-config.sh <node>` composes shared definitions with the
node overlay and writes `config/nodes/rendered/<node>.json`. A contract test
re-renders every node and fails if a rendered file is stale, if two nodes claim
one `(scope, scale-set name)` pair, or if a rendered file does not pass
`config.Validate`. The daemon still loads exactly one plain file; the operator
still edits one place per fact.

### 6. `hostBudget` is a static ceiling below physical capacity

A new optional top-level setting:

```json
"hostBudget": { "cpu": 4, "memoryMb": 10240 }
```

It bounds the *total* admission envelope of the node across every platform,
charging live instances against it exactly as ADR 0018's probed physical total
is charged. Omitted, it is a byte-for-byte no-op.

It composes with the existing gates rather than replacing them, because the two
answer different questions. The pressure gates in
`internal/adapters/macos/guard.go:37-57` read whole-host kernel metrics —
`vm_stat`, `df`, `vm.swapusage`, `vm.loadavg`, `top` — so they already sense a
co-tenant's load and already yield to it. What they cannot express is a share:
on an idle machine they permit the fleet to take everything. `hostBudget` is
that missing share, and it is the *only* one of the four bounds the aged-work
starvation escape of ADR 0018 does not lift, because a configured ceiling is a
constraint and not a throttle.

The setting is enforced in one place, `freeCapacity` in
`internal/scheduler/scheduler.go:498`, which is the sole point at which any
admission pass — young, aged, reserved-head, drain-backfill, Linux, macOS —
obtains its envelope.

`hostBudget` is not only for the remote node. Node A and node B set it too:
an explicit ceiling that an operator chose is better than a ceiling implied by
`maxLinuxCpu` happening to equal the core count.

### 7. Every node is outbound-only

A node requires outbound HTTPS to GitHub and nothing else. It accepts no
inbound connection, exposes no port, and holds no link to another node. The
admin API is a unix socket in the node's own state directory. Operator access
is SSH, which is outside the fleet's contract. The updater is per node,
unchanged, and activates releases independently.

A node behind NAT, on a residential line, in another country, or asleep for a
weekend is therefore an ordinary node. Node C's location costs nothing.

### 8. When the steward returns

Issue #99's design is not wrong; it is early. It becomes the right next step
when, and only when, one of these is measured:

- **Same-capability sets on two or more nodes compete for one scope's work**,
  so that a static partition demonstrably strands capacity. The observable is
  a scope queueing on node A while node C's identical profile sits idle, in
  `fleet queues` on both nodes.
- **Arch-floating aliases must be advertised by more than one node at once**,
  which requires an owner for the ambiguity that §3 refuses to create.

Its two GitHub-API spikes remain the entry gate and are not answered here:
whether a peer can delete an inherited session and immediately create its own,
and whether GitHub accepts a `maxCapacity` decrease while jobs are assigned.
Its ownership-signature migration warning — adding host identity to the value
that authorizes VM mutation must happen on a drained fleet or hosts orphan
their own guests — applies to any future work that introduces node identity
into ownership, and this record deliberately introduces none.

### 9. Non-goals

This record adds none of the following, and a change that adds one is a new
record, not an implementation detail:

- No distributed control plane, steward, election, or consensus.
- No cross-node scheduler, placement, or fairness. Repository caps, `maxActive`,
  and aging are per node, and a repository capped at *n* may hold *n* runners
  on each node that serves it.
- No shared or replicated database. One SQLite per node, as ADR 0003 says.
- No cross-node RPC, message bus, VPN requirement, or service discovery.
- No node registry, roster, heartbeat, or lost-node reclaim. GitHub's own runner
  expiry is the only reclaim for a node that dies, exactly as it is today for a
  single host that dies.
- No new daemon, no aggregation service, and no dashboard. Observability is each
  node's existing `fleet status` plus a shell script that runs it over SSH on
  each node and prints the results together.
- No cross-node artifact path. Every consumer workflow already hands off
  through `actions/upload-artifact` and `actions/download-artifact`; the
  same-host `ci-shared` mount of [ADR 0013](0013-ephemeral-macos-io.md) stays a
  same-node optimization and is never a correctness dependency.

## Consequences

The Mac mini becomes a macOS-only node. `builder` at 6 vCPU and `maestro` at
4 vCPU then coexist in exactly ten cores and 19 GiB of 24, and the contention
photographed in Context — a Linux guest starving the builder for an hour —
becomes structurally impossible rather than scheduled around. The Linux profiles
stay in its configuration file, because `internal/config/config.go:555-563`
requires them, but no scope lists a Linux scale set, so no Linux demand can
arrive. Retiring them from the schema is a later, optional cleanup.

Two thirds of the fleet's guest boots move to a machine with more cores, more
threads, no nested-virtualization tax, and no macOS guest quota. `rnw-community`
gets an Android emulator that can actually start. `suuudokuuu` gets the option
to build its APK on Linux and stop spending the fleet's scarcest resource on it.

The costs are real. Capacity is statically partitioned, so an idle node cannot
help a busy one; the fleet trades utilization for the absence of a control
plane, and §8 names the measurement that would reverse the trade. Repository
caps are per node, so a cap of four across three nodes is a cap of four
*somewhere*, not four in total — for the current partition this is harmless,
because no repository's macOS and Linux caps were ever the same pool. Operating
three machines is three updaters, three databases, three sets of logs, and three
base images to keep current; the plan document treats base-image drift as the
principal recurring burden. And a node that dies is invisible to the others: its
jobs queue until GitHub expires the runners, which is the same failure the fleet
has today with one host, no better and no worse.

Against those: the design adds no new distributed state, no new persistent
lifecycle state, and no new class of partial failure. Every invariant that
`tests/simulation` checks remains a property of one node, and every one of them
still holds.

## Evidence

- `internal/adapters/githubscaleset/session.go:83-105` and `errors.go:58-59` —
  `409` is terminal, "another session acquired the scale set"; the exclusivity
  this record leans on.
- `internal/daemon/runtime.go:154-181, 257, 850-859` — session replacement and
  the database-local `tart-runner-fleet-authority` lease that cannot observe
  another host, showing why shared ownership is unsupported.
- `internal/scheduler/scheduler.go:480-575` — `freeCapacity`, the single
  admission chokepoint that `hostBudget` extends, and `physicalBound`, the
  "zero means unset" primitive it reuses.
- `internal/adapters/macos/guard.go:37-57` — whole-host pressure gates that
  already yield to a co-tenant and cannot express a share. (Since issue #137
  these gates are `Guardrails.Evaluate` in `internal/executor/host.go`,
  unchanged; see the amendment below.)
- `internal/config/labels.go:19-23, 41, 105-114` — the canonical grammar and the
  `arm64` constant that becomes per-node.
- `internal/config/config.go:281-312, 418-426` — strict single-document decoding,
  which forces configuration sharing to be a render step.
- `internal/app/demand.go:101-114, 229-235` — subset label matching, and the
  refusal to guess when a job matches two scale sets, which is why §3 declines
  to advertise one label from two nodes.

## Amendment 2026-08-04: the port is `internal/executor` (issue #137)

The seam this record deferred now exists, and it is named. `internal/executor`
holds two ports and nothing else: `Backend`, which is a node's execution
technology, and `HostProbe`, which is how a node measures the machine it runs
on. Every layer above them speaks only those types, and a lint rule denies
`internal/adapters/tart` and `internal/adapters/macos` to `internal/lifecycle`,
`internal/app`, `internal/scheduler`, `internal/reconcile`, and
`internal/discharge`. Choosing an implementation is `internal/daemon`'s job and
no one else's.

`Backend` is seven verbs — `Create`, `Start`, `Stop`, `Delete`, `Running`,
`Reap`, `List` — over an `InstanceSpec` whose `Image` is a Tart base VM on node
A and an OCI reference on node B. "Clone a base image" and "create a container
from an image" are one verb, which is what makes the partition of §3 an
implementation detail of the port rather than a fork of the lifecycle.

`docs/MULTI_NODE_PLAN.md` proposed `internal/domain` for these types. That
placement does not survive the import graph: `internal/operations` imports
`internal/domain`, so a port taking an `operations.Ownership` cannot live there,
and `domain.Instance` already names the scheduler's live instance. Only the
instance-name grammar moved to `domain`, as `domain.ValidateInstanceName`,
because it is pure and every layer validates names. The plan document is
corrected in place; nothing else about the plan changes.

The extraction changed no behaviour: the persisted operation kind and failure
stage remain the literal string `clone`, the guardrail evaluation is
byte-identical, and the deterministic-simulation corpus of
[ADR 0031](0031-deterministic-simulation-testing.md) — 64 seeds x 200 ticks on
both world arms, three runs each — reduces to the same counts and the same
digest before and after.

## Amendment 2026-08-04: the container backend, and the one contract it moved (issue #139)

Node B's execution technology is built: `internal/adapters/podman` implements
`Backend` over the rootless podman command line, and `internal/daemon` chooses
it. Three decisions in it are load-bearing enough to record here rather than in
the plan document.

**A Linux node's backend is a property of its configuration, not of its kernel.**
`platformFor(goos)` no longer answers "can this node execute" from `goos` alone,
because the same machine is both stages of the bring-up: with no `executor`
block it is Phase 1 Part A, wiring `noexecutor` and observing, and with
`executor.backend: podman` it is the authority node. `executes` therefore became
a question asked of the decoded configuration, and the daemon asks a second one
beside it — a node that names podman must prove podman is installed and rootless
before it starts in any mutating mode. Rootless is not a preference: §"Why
rootless specifically" of the plan puts approved third-party code on this node,
and a root-owned container daemon is root-equivalent.

**A container is created, never adopted.** ADR 0010 makes an ephemeral guest
single-use, and the Tart adapter can honour that while still finding its own
clone after a lost response, because a Tart VM is a directory that outlives the
process that made it. A container is not: one that already holds a name the
scheduler has just minted was made by something else, or by an operation whose
job has already run in it. So `Create` writes durable ownership, then refuses a
name held by a container that does not carry *this operation's*
`trf.operation` label. The label is written by this adapter's create and by
nothing else.

**`/dev/kvm` is a grant to a named profile, derived from the instance name.**
`InstanceSpec` carries no profile — `tests/contract/executor_port_test.go` pins
its field set, and a field there is a field every backend must answer for — so
the adapter reads the profile off the `trf-<profile>-` prefix
`reconcile.Controller` mints, which is exactly how the Tart adapter already
recognises a macOS guest. The list of profiles is `executor.kvmProfiles`, and
naming a profile the node does not declare is a refused configuration rather
than a grant that silently does nothing.

### One contract moved

`internal/lifecycle` validated the base image with `domain.ValidateInstanceName`.
That is the *instance* grammar — `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` — and an
OCI reference's slashes, colons, and digest `@` are precisely the characters it
forbids, so the provisioning path could never have created a container. The
amendment above claimed `InstanceSpec.Image` was already backend-neutral; it was
neutral in its type and not in its validation.

The neutral rule is now `domain.ValidateImageReference`: present, unpadded, no
whitespace or NUL, and not readable as a command-line option. It asserts only
what is true of an image on every backend and knowable without a registry. A
backend narrows it — `internal/adapters/tart` still requires a full instance
name, because a Tart base VM *is* one — and only a registry or a hypervisor can
say whether the image is really there. Nothing else about the port changed: the
seven verbs, the field sets, and the caller-side ports are byte-for-byte as
issue #137 left them, which is what let the container adapter be written against
a target that could not move.

## Amendment 2026-08-04c: a shared label is a promise about the guest

This amendment adds an invariant to the shared-label federation recorded in
*Amendment 2026-08-04b* ([issue #144](https://github.com/vitalyiegorov/tart-runner-fleet/issues/144),
pull request #152), which withdrew §3's refusal to let two nodes advertise one
label. It changes no mechanism, no schema, and no code. It exists because the
fleet violated the invariant in production on the day that federation was
deployed, and because nothing in this record — or in any gate — would have
stopped it.

### What happened

Node A and node C both advertise `[self-hosted, linux-tiered, linux-xl]`, which
is precisely what amendment 2026-08-04b permits: GitHub places, each node fills
to its advertised capacity, no steward. Node C's Linux base image was built the
same day from `docs/LINUX_BASE_IMAGE.md`, a document written by executing it,
verified against every contract this codebase imposes on a guest, and correct in
all of them.

Node A's image additionally carries a prewarmed Redroid container. Node C's did
not, because no artifact this fleet owns says it should: not the legacy build
script, not the daemon's code, not the profile, not the label, not the scale-set
definition, not the configuration schema. The capability exists only in a
consumer's workflow in another repository.

`vitalyiegorov/suuudokuuu`'s Android Maestro job therefore failed on node C and
passed on node A — same commit, same label, same workflow — which is
indistinguishable from flakiness until someone reads the two runs side by side
and notices the runner names differ. The job carried its own guard
(`::error::The guest image lacks the Redroid prewarm`), and that guard is the
only reason the diagnosis took minutes rather than days. Nothing else in the
loop could have produced it: the daemon started the guest correctly, the runner
registered correctly, and the job ran on exactly the resources its label asked
for.

### The invariant

> **Nodes that advertise the same label MUST provide equivalent guest
> capabilities.** A leaner image behind a shared label is not an optimization
> and not a degraded mode. It is a correctness bug whose observable is
> non-determinism in someone else's repository.

This follows from amendment 2026-08-04b rather than qualifying it. Once
placement is GitHub's and a consumer cannot address a node, the label is the
*entire* interface between a job and a machine. §4 of this record already says
the canonical label "cannot lie" about CPU, memory, OS, and architecture — it is
derived, and the derivation is checked. The label lies about everything else for
free, and "everything else" turned out to include whether Android can run at
all.

Two consequences worth stating plainly:

- **`hostBudget` scales capacity; it does not scale capability.** A smaller node
  may advertise a smaller number. It may not advertise a smaller *image*.
- **The emergency mitigation is to stop advertising, never to keep advertising
  and hope.** Removing node C's `linux-xl` scale sets restored correctness at
  the cost of capacity, and that is the correct trade every time.

### Proposed enforcement, not implemented here

Recorded so the next node does not rediscover this by outage. Ordered by what
each catches and what it costs. **None of it is built**; the work is filed as an
issue labelled `three-node`.

**1. Declare the capability, in the two places that already exist.** A
capability is a lowercase identifier — `redroid-android`, `container-runtime` —
and it appears twice. A node declares what its base image provides:

```json
"baseVm": "linux-runner-base-go",
"baseImageCapabilities": ["container-runtime", "redroid-android"]
```

A scale set declares what its label requires, beside the labels it already
carries:

```json
{ "profile": "linux-6x12", "name": "trf-sudoku-xl-studio",
  "labels": ["self-hosted", "linux-tiered", "linux-xl"],
  "requiresCapabilities": ["redroid-android"] }
```

`Config.Validate` then refuses a scale set requiring a capability the node does
not declare. This is a few lines in `internal/config/config.go` beside the
existing profile/scale-set cross-checks, it costs nothing at runtime, it is
covered by `fleet config validate` before the daemon starts, and — being a
configuration error — it is caught by the same gate that already catches a scale
set naming an unknown profile.

**2. Compare the declarations across nodes, where the fleet exists as a whole.**
§5 of this record renders each node's configuration from shared definitions and
tests that the rendering is current. That contract test is the only place where
all nodes are visible at once — no node may look at another, and this does not
change that, because the check runs in CI on the configuration repository, not
in the daemon. The rule: **for any label advertised by more than one node, the
union of required capabilities must be declared by every node that advertises
it.** That is the actual parity invariant, and it is enforceable exactly where
the fleet is a single artifact. This is the recommended core of the work; §1 is
what makes it expressible.

**3. Make the declaration answerable by the image.** A declaration an operator
types is a claim. The image should be able to confirm it: bake
`/usr/local/share/tart-runner-fleet/image-capabilities.json` at seal time (node
C's base already carries the informal ancestor of this, appended to
`$HOME/.ci-base-manifest`), and have the bootstrap helper read it and compare it
against the capabilities the daemon expected. A mismatch fails the instance at
the `bootstrap` stage with a named reason, which
[ADR 0020](0020-diagnosable-drain-failures.md) already knows how to surface, and
which is a categorically better failure than a job dying inside a consumer's
workflow. It is detection, not prevention — the job has already been assigned by
then — so it ranks below §2 and is worth having anyway, because it is the only
check that can catch a *stale* declaration after an image is rebuilt.

**4. What is deliberately not proposed.** No probing of another node, no
capability gossip, no registry, no runtime capability negotiation with GitHub,
and no dynamic label computation. Every one of those reintroduces the
cross-node coupling §9 forbids, and none is needed: the failure is a
configuration mistake, and configuration is where it should be caught.

The honest limit of all four: a capability list is only as good as the audit
that produced it, and consumer requirements live in consumer repositories that
this fleet does not read. §1 and §2 make an operator's knowledge *mechanically
enforced*; they do not make it *complete*. The recurring operator task — read
the consumers of every label a node advertises, before advertising it — remains,
and `docs/LINUX_BASE_IMAGE.md` now carries it.

## Not addressed here

**Whether the container adapter works against a real container runtime is not
proved by any automated gate yet.** Its verbs, error classification, wiring, and
sufficiency for the whole runner lifecycle are covered on every commit; that
podman accepts these argument vectors and prints this JSON is covered by
`scripts/podman-smoke.sh`, which CI can only skip because the fleet's own Linux
runners are Tart guests with no container runtime. The node B bring-up checklist
in `docs/MULTI_NODE_PLAN.md` requires that script to pass before any job is
routed there, and that requirement is the mitigation.

**How a base image reaches a node, and what is in it.** The runner image must
carry the JIT bootstrap helper and start the runner without `systemd-run`, which
a container does not have. That is image work on node B, and it is per-node
operator work until measurement says otherwise.

**Simulation stays single-node and needs no change.** `tests/simulation` models
one host because ADR 0031 says so, and under this record one host is still one
whole fleet: every property it checks — liveness, bounded starvation, identity
uniqueness, no double admission, conservation, no stranded demand, no drain
churn — is a per-node property with no cross-node counterpart. A second
simulated node would model only GitHub's routing, which is not this codebase's
code. The harness gains one new dimension when node B ships, and it is the
existing one: a single-platform configuration, which the world config already
expresses.

Node C still needs a macOS image, and keeping every node's base image current is
per-node operator work until measurement says otherwise.
