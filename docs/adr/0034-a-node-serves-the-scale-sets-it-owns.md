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

**Amended 2026-08-04 (node names):** this record originally named the fleet's
machines "node A", "node B", and "node C". It is edited throughout to name them
by what they are — **mac-mini**, **geekom**, **mac-studio** — for legibility.
The mapping, for anyone arriving from an issue or commit that still says a
letter: A = mac-mini, B = geekom, C = mac-studio. See the amendment "Node
names, not letters" for why and what did not change.

**Amended 2026-08-04 (shared labels):** §3's refusal to let two nodes advertise
one label is withdrawn on measured evidence that GitHub distributes work across
identically-labelled scale sets by advertised capacity. The architecture is
unchanged; the partition is not. §8's steward triggers are narrowed
accordingly. See the amendment "Two nodes may advertise one label, because
GitHub places" for the evidence and its two preconditions.

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
| mac-mini | Mac mini M4, 10c / 24 GiB | macOS on Apple silicon | macOS `builder` and `maestro` |
| geekom | GEEKOM A9 Max, 12c/24t x86_64 | Linux on x86_64 | every Linux profile, `trf-linux-amd64-*` |
| mac-studio | Mac Studio, 14c / 36 GiB physical, remote, `hostBudget` 6c/16384 MiB | macOS on Apple silicon | every profile it fits, same labels as mac-mini |

mac-mini and geekom have disjoint capability sets, so their partition strands nothing:
no job either can run is a job the other could have taken. This holds once geekom
exists; today, with geekom not yet delivered, mac-mini is the fleet's only Linux
capability and nothing partitions against it.

mac-studio overlaps mac-mini. This record originally partitioned that overlap by
**scope** — mac-studio owning the `maestro` scale set of specific repositories and
mac-mini the rest — because a static scope partition was the only arrangement in
which no job could be offered to two owners, and because whether GitHub
distributes across identically-labelled scale sets was unanswered.

**It is answered, and the answer reverses this paragraph.** Two scale sets in
one scope may advertise identical labels, and GitHub distributes the work
between them, filling each to the capacity it last advertised. The overlap
between mac-mini and mac-studio is therefore **not** partitioned: both are
Apple-silicon Tart hosts, so both advertise the same labels for every profile
each fits — `builder` and `maestro`, and, for as long as geekom is not yet
delivered, the Linux profiles too — and GitHub places by advertised capacity.
See the amendment "Two nodes may advertise one label, because GitHub places"
below, which carries the evidence and the two conditions this depends on. Which
profiles are actually declared where, and why, is placement detail and lives in
`docs/MULTI_NODE_PLAN.md`, not in this architectural record.

### 4. The canonical label carries the architecture, as ADR 0032 intended

ADR 0032 spelled `arch` out in `trf-<os>-<arch>-<cpu>x<ramGiB>` precisely "so a
host of another architecture could never reuse these names." geekom's profiles
derive `trf-linux-amd64-1x2`, `trf-linux-amd64-2x4`, `trf-linux-amd64-4x8`,
`trf-linux-amd64-6x12`, and the shapes its larger core count affords. Nothing
about the derivation changes except that `arch` stops being the constant
`arm64` in `internal/config/labels.go:23` and becomes a property of the node's
configuration, still derived and still unable to lie.

Arch-*pinned* consumers name a canonical label and get exactly one architecture.
Arch-*floating* consumers name an alias — `linux-small`, `linux-medium` — which
geekom advertises after mac-mini stops. Migration is therefore a provisioning run
on geekom and a scale-set removal on mac-mini, not an edit to every workflow;
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
    mac-mini.json             mac-mini: overlay + scale-set ownership
    geekom.json               geekom: overlay + scale-set ownership
    mac-studio.json           mac-studio: overlay + hostBudget + scale-set ownership
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
"hostBudget": { "cpu": 6, "memoryMb": 16384 }
```

(the value mac-studio actually runs, a 14-core / 36 GiB machine shared with
other work).

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

`hostBudget` is not only for the remote node. mac-mini and geekom set it too:
an explicit ceiling that an operator chose is better than a ceiling implied by
`maxLinuxCpu` happening to equal the core count.

### 7. Every node is outbound-only

A node requires outbound HTTPS to GitHub and nothing else. It accepts no
inbound connection, exposes no port, and holds no link to another node. The
admin API is a unix socket in the node's own state directory. Operator access
is SSH, which is outside the fleet's contract. The updater is per node,
unchanged, and activates releases independently.

A node behind NAT, on a residential line, in another country, or asleep for a
weekend is therefore an ordinary node. mac-studio's location costs nothing.

### 8. When the steward returns

Issue #99's design is not wrong; it is early. Both of its original trigger
conditions have been dissolved by the amendment below: same-capability sets on
two nodes no longer strand capacity, and arch-floating aliases advertised by
more than one node no longer need a local owner for the ambiguity, because
GitHub resolves it. What remains as a trigger is narrower:

- **Placement must obey a policy GitHub does not implement.** Advertised
  capacity is the only signal a node can send. If the fleet ever needs
  cross-node priority, aging, or repository fairness — rather than "fill each
  node to its truthful capacity" — no header expresses it, and a steward is the
  only way to hold that policy.
- **The distribution behaviour regresses.** It is a preview API. If GitHub
  reverts to preferring one set, the fleet degrades to exactly the scope
  partition this record originally specified, and the trigger becomes the
  measured stranding named in the previous revision of this section.

Its two GitHub-API spikes were the entry gate, and both are now answered
affirmatively — a peer can delete an inherited session and immediately create
its own, and advertised capacity may be lowered while jobs are assigned. Being
feasible is not being necessary; see the amendment.
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
  refusal to guess when a job matches two scale sets. Originally cited for why
  §3 declined to advertise one label from two nodes; the amendment "Two nodes
  may advertise one label, because GitHub places" below is why that refusal no
  longer implies a scope partition — REST-derived demand still must not guess,
  which is exactly the per-set attribution bound that amendment makes a
  precondition.

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
`Reap`, `List` — over an `InstanceSpec` whose `Image` is a Tart base VM on
mac-mini and an OCI reference on geekom. "Clone a base image" and "create a container
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

geekom's execution technology is built: `internal/adapters/podman` implements
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

## Amendment 2026-08-04: two nodes may advertise one label, because GitHub places

§3 refused to let two nodes advertise the same label, on the stated ground that
"this record does not rely on undocumented behaviour." The behaviour has now
been measured, and the refusal costs more than it buys. This amendment removes
it. **Nothing else in this record changes**: no control plane, no steward, no
election, no cross-node RPC, no shared database, no registry, no heartbeat. Each
node still owns its own scale sets, holds its own single session, and keeps its
own SQLite. The only thing that changes is that two nodes' sets may carry the
same labels, and GitHub decides which set each job goes to.

### What was measured

Two scale sets were created in one throwaway repository scope with byte-identical
label lists, `[self-hosted, trf-spike-dup]`, and long-polled concurrently while a
four-job workflow was dispatched repeatedly. Full request and response transcripts
are in
[issue #144](https://github.com/vitalyiegorov/tart-runner-fleet/issues/144#issuecomment-5180794736).

1. **Identical labels are accepted.** Both `POST /_apis/runtime/runnerscalesets`
   calls returned `200`. What GitHub rejects is a duplicate *name* — `400`,
   `RunnerScaleSetExistsException`. Uniqueness is on the name, not on the labels.
2. **GitHub distributes, server-side, before either listener sees the work.**
   Every message was `JobAssigned`, never `JobAvailable` with a race to acquire.
   Each set saw only its own share. The rule is not creation order, not
   round-robin, and not first-acquire-wins: each set is filled to the capacity it
   most recently advertised. With one set advertising `1` and the other `5`, a
   four-job dispatch split exactly `1` and `3`. With both full, the remainder
   stayed `queued`, and raising one set's advertised capacity pulled that backlog
   onto it on the next poll.
3. **Advertised capacity is not a stored property.** `maxCapacity` in a create or
   `PATCH` body is accepted and ignored; `RunnerScaleSet` has no such field.
   Capacity is the per-poll `X-ScaleSetMaxCapacity` header
   (`github.com/actions/scaleset@v0.4.0/client.go:38-40`, `session_client.go:142`),
   fed here from configured `maxCapacity` through `ScaleSetConfig.MaxCapacity`
   (`internal/adapters/githubscaleset/scaleset.go:45, 89`). Lowering it while jobs
   are assigned is accepted and does not revoke them.

The consequence worth stating in one line: **the number this fleet already
computes for ADR 0015's truthful-capacity invariant is the number GitHub uses to
place work across nodes.** Through `budgetedCapacity`
(`internal/config/config.go:898-905`), mac-studio's 6 vCPU / 16384 MiB
`hostBudget` yields a `maestro` (4 vCPU / 7168 MiB) capacity of 1 — the CPU term
floors `6/4` to `1`, the memory term floors `16384/7168` to `2`, and the bound
is the minimum — against mac-mini's larger, unbudgeted machine, and whatever
ratio each node's `hostBudget` yields is the split GitHub applies. The same
budget change that leaves `maestro`'s capacity at 1 is what newly admits
`builder` at all: `12288 MiB` fits inside `16384 MiB` where it never fit inside
`10240 MiB`. Cross-node placement is not new machinery; it is an existing
invariant read by a
new consumer.

### The two conditions this depends on

**Advertised capacity must be truthful.** `canonicalJobInventory` is off in the
live configuration, and while it is off `internal/config/config.go:776` *requires*
`maxCapacity > runtime capacity` as queue lookahead. Two nodes both inflating
would have GitHub split by fiction and hand a node work it must then queue while
its peer idles — the stranding this record set out to avoid, relocated. A scope
whose label is advertised by two nodes must therefore run with
`canonicalJobInventory: true`, which is ADR 0015's model and is already
implemented and validated.

**REST-derived demand must be attributed per set.** Enabling
`canonicalJobInventory` also enables the REST inventory lane, and that lane
attributes *repository-wide queued jobs* to a binding by label match
(`internal/app/demand.go:229-243`). Under a shared label both nodes would
attribute the same queued job to their own set and both would spawn a guest for
it; the loser would hold a runner against work GitHub never assigned it, and the
ghost-demand reclaim of [ADR 0026](0026-queued-demand-expires-on-proven-absence.md)
would have to clear it. The correction is bounded and the signal is already
ingested: a binding's REST-derived demand is capped by that set's own
`statistics.totalAssignedJobs`, which arrives on every message and is already
carried as `Demand.Assigned`
(`internal/adapters/githubscaleset/scaleset.go:97-103`). This is a precondition
of the amendment, not a follow-up — tracked as issue #153.

### Why relying on undocumented behaviour is acceptable here, and only here

The general rule §3 invoked is sound. It is overridden by the shape of the
failure, not by the size of the prize. The API is preview and the placement rule
is not contractual — but if it regresses to preferring one set, the fleet lands
in *exactly the scope partition this record already specified*: one node busy,
one node idle, no job lost, no runner orphaned, no state corrupted, no schema
migrated. Every node keeps its own set, its own session, and its own database
either way. The blast radius of being wrong is a utilisation regression, and the
rollback is deleting one node's scale sets.

That is the test this amendment asks to be held to: an undocumented behaviour may
be depended on when its failure mode is the documented behaviour.

### What this does not license

Repository caps, `maxActive`, and aging stay per node, exactly as §9 says. GitHub
distributes by capacity and knows nothing of this fleet's fairness policy, so a
scope's work may be split in a way no single node would have chosen. That is
accepted: it is the same trade this record already made when it made caps
per-node. If a policy GitHub cannot express becomes necessary, that is the
steward trigger in §8, and it is the only one left.

## Amendment 2026-08-04: node names, not letters

This record, and `docs/MULTI_NODE_PLAN.md`, originally referred to the fleet's
machines as node A (Mac mini), node B (GEEKOM A9 Max), and node C (Mac Studio).
That was adequate while only one machine existed and the other two were rows in
a table. It stopped being adequate the day a second machine — mac-studio — went
into bring-up (issue #140): every status update, log line, and conversation
needed a decoder ring between an opaque letter and the actual box, and the
letter carried no information the name doesn't carry for free.

This document, `docs/MULTI_NODE_PLAN.md`, `docs/BASE_IMAGE.md`, and
`INSTALL-linux.md` are edited throughout to name machines by what they are:
**mac-mini**, **mac-studio**, **geekom**. No decision changes because of this
edit. Where the letters already shipped — issue titles, commit subjects, prior
PR descriptions — that history is not rewritten; the mapping for anyone arriving
from an old link is stated once, at the top of this record: A = mac-mini,
B = geekom, C = mac-studio.

One fact changed alongside the rename, and it is called out so it is not mistaken
for a renaming artefact: mac-studio's physical specification was confirmed on
arrival as 14 cores / 36 GiB, and its `hostBudget` was set at **6 vCPU /
16384 MiB** — larger than the 4 vCPU / 10240 MiB this record originally
estimated before the machine was in hand. Every figure in this document and in
`docs/MULTI_NODE_PLAN.md` uses the confirmed value. geekom remains ordered and
not yet delivered, expected in about two weeks; every statement about it in
this record is prospective until it arrives.

## Not addressed here

**Whether the container adapter works against a real container runtime is not
proved by any automated gate yet.** Its verbs, error classification, wiring, and
sufficiency for the whole runner lifecycle are covered on every commit; that
podman accepts these argument vectors and prints this JSON is covered by
`scripts/podman-smoke.sh`, which CI can only skip because the fleet's own Linux
runners are Tart guests with no container runtime. The geekom bring-up checklist
in `docs/MULTI_NODE_PLAN.md` requires that script to pass before any job is
routed there, and that requirement is the mitigation.

**How a base image reaches a node, and what is in it.** The runner image must
carry the JIT bootstrap helper and start the runner without `systemd-run`, which
a container does not have. That is image work on geekom, and it is per-node
operator work until measurement says otherwise.

**Simulation stays single-node and needs no change.** `tests/simulation` models
one host because ADR 0031 says so, and under this record one host is still one
whole fleet: every property it checks — liveness, bounded starvation, identity
uniqueness, no double admission, conservation, no stranded demand, no drain
churn — is a per-node property with no cross-node counterpart. A second
simulated node would model only GitHub's routing, which is not this codebase's
code. The harness gains one new dimension when geekom ships, and it is the
existing one: a single-platform configuration, which the world config already
expresses.

mac-studio still needs a macOS image, and keeping every node's base image current is
per-node operator work until measurement says otherwise.
