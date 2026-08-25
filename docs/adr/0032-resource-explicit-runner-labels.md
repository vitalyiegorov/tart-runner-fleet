# ADR 0032: A runner label states the shape it delivers

## Status

Accepted. Extends the naming rule of
[ADR 0012](0012-shared-cross-platform-capacity.md) — "profile names are
deployment details and must not encode priority" — by giving profiles a name
that encodes their resource vector and nothing else. It changes no admission
rule: [ADR 0004](0004-bounded-control-plane-priority.md)'s lanes,
ADR 0012's shared envelope, [ADR 0017](0017-infeasible-reservation-residual-backfill.md)
and [ADR 0029](0029-remainder-admission-behind-a-reservation.md)'s residual
passes, [ADR 0030](0030-a-reserved-head-holds-one-repository-slot.md)'s slot
arithmetic, and [ADR 0024](0024-mixed-macos-profile-cohorts.md)'s cohort rules
all read the configured vector, never a label, and continue to do so.
[ADR 0006](0006-per-profile-disk-floors.md)'s per-profile disk floor is
unchanged and is deliberately not part of the label.

## Context

The fleet named its shapes `small`, `medium`, `large`, `xl`, `builder`, and
`maestro`. Those names are opaque in the place they are actually read — a
`runs-on` line in a repository that does not own the fleet configuration. A
workflow author choosing between `medium` and `large` is choosing between two
adjectives whose meaning lives in a file they cannot see, and the answer changes
whenever the operator re-sizes a profile. The README has to carry a translation
table, and the table has already drifted from the configuration it describes:
it lists the macOS builder as 6 vCPU while `config/fleet.example.json` shipped
8 and `USAGE.md` documented 8.

The vocabulary is also not generative. Every new shape needs a new adjective,
and the adjectives run out immediately: a 2-vCPU/8-GiB memory-heavy variant is
neither "medium" nor "large", and an 8-vCPU/16-GiB variant that fills the host
alone has no name at all above `xl`. That is why the variant matrix stopped at
four Linux shapes even though the host packs many more, and it is why
right-sizing advice today can only be prose in a README.

Commercial fleets solved the same problem the same way. BuildJet, Namespace, and
Depot all name a runner by its shape (`buildjet-4vcpu-ubuntu-2204`,
`nscloud-ubuntu-22.04-amd64-4x8`, `depot-ubuntu-22.04-4`), so a pipeline picks a
size without a lookup table and a new size is a new number rather than a new
word.

Two constraints shaped the design:

- **Labels must stay priority-free.** ADR 0012 decided that explicitly, and the
  reason survives: the scheduler orders young work by dominant resource share of
  the *configured vector*, and aged work by global FIFO. A resource-explicit
  label is a description of that vector, not an instruction about it. Nothing in
  the scheme lets a workflow ask to be scheduled sooner.
- **A name must not be able to lie.** A label typed by hand into a configuration
  file is a second source of truth about a shape, and it drifts — as the README
  table already proved.

A third constraint is operational. Every profile exposed in every scope is one
GitHub scale set, one broker session, and one message queue. Validation used to
*require* every enabled profile in every scope, so the cost of the variant
matrix was multiplicative in the number of scopes. Widening the matrix under
that rule would have been the dominant cost of this change.

### What GitHub permits

A runner scale set carries a **list** of labels, not one. The official
`github.com/actions/scaleset` client models `RunnerScaleSet.Labels` as
`[]Label`, preserves caller-supplied labels verbatim, and only falls back to a
single label equal to the scale-set name when the caller supplies none
(`ensureLabels`). GitHub's own ARC documentation describes the common case —
"you can use the installation name as the value of `runs-on`" — but the API
underneath is a label list, and this fleet has been shipping multi-label scale
sets in production for months: the `budgie-at` Linux large set advertises
`self-hosted`, `linux-tiered`, `linux-large`, `linux-ci`, and `linux-burst`, and
all five route. Aliases are therefore free: they are extra entries in a list
that already exists.

GitHub documents no cap on the number of scale sets per repository or
organization. The published self-hosted limits are 10,000 runners per runner
group and 1,500 runner registrations per five minutes per scope — neither is
approached here. The real cost of a scale set is local: one long-poll session
per scale set per scope, each with its own recovery bounds.

## Decision

### 1. The canonical label is derived, not written

The canonical runner label of a profile is

```
trf-<os>-<arch>-<cpu>x<ramGiB>
```

for example `trf-linux-arm64-4x8` and `trf-macos-arm64-4x7`. It is computed from
the profile's configured vector, so it cannot disagree with the VM the fleet
boots. `os` is `linux` or `macos`; `arch` is `arm64`, because Tart builds on
Apple's Virtualization framework and runs only on Apple silicon — the component
is spelled out so a host of another architecture could never reuse these names.
Memory is stated in whole GiB, and a profile whose memory is not a whole
multiple of 1024 MiB is rejected: a label that rounded would be a label that
lies. Every shape the fleet has ever shipped is already GiB-aligned.

The label states resources only. It carries no lane, no class, and no ordering,
as ADR 0012 requires.

### 2. Role and tier names become aliases

`label` in configuration and the new `aliases` list are alias names. They
resolve to the same profile and are advertised on the same scale set, so
`runs-on: [self-hosted, linux-large]` and
`runs-on: [self-hosted, trf-linux-arm64-4x8]` reach one scale set and one queue.

Validation proves the configuration is not lying: any alias that *matches the
canonical grammar* must equal the profile's derived canonical label. An alias
that does not match the grammar — `linux-large`, `linux-burst`, `macos-builder`
— is an ordinary name and is carried through untouched. That is precisely why
every configuration written before this decision keeps validating: none of its
labels claim to be canonical. Two profiles may never answer to the same name,
and a scale set may never advertise another profile's canonical label.

### 3. The variant matrix is first-class, and each scope opts in

The shipped matrix is the set of shapes the host can pack, not a ladder of
adjectives:

| Variant | Profile id | Canonical label | Alias |
| --- | --- | --- | --- |
| Linux 1×2 | `linux-1x2` | `trf-linux-arm64-1x2` | `linux-small` |
| Linux 2×4 | `linux-2x4` | `trf-linux-arm64-2x4` | `linux-medium` |
| Linux 2×8 | `linux-2x8` | `trf-linux-arm64-2x8` | — |
| Linux 4×8 | `linux-4x8` | `trf-linux-arm64-4x8` | `linux-large` |
| Linux 6×12 | `linux-6x12` | `trf-linux-arm64-6x12` | `linux-xl` |
| Linux 8×16 | `linux-8x16` | `trf-linux-arm64-8x16` | — |
| macOS 4×7 | `macos-4x7` | `trf-macos-arm64-4x7` | `macos-maestro` |
| macOS 6×12 | `macos-6x12` | `trf-macos-arm64-6x12` | `macos-builder` |

`2×8` exists for memory-bound work that wastes cores on `4×8` — bundlers, type
checkers, JVM tools. `8×16` exists so one job can take the whole Linux envelope
instead of leaving a stranded remainder.

A scope now exposes exactly the variants its `scaleSets` list names. The former
rule — every enabled profile requires one scale set in every scope — is
withdrawn; what remains is that each listed variant exists, appears once, and
carries a label that resolves to it, and that a scope lists at least one. The
count math is the point:

```
before:  scale sets = scopes x enabled variants        (5 x 8 = 40)
after:   scale sets = sum over scopes of exposed variants
```

A plausible live allocation — 3 variants for the control-plane repository, 3 for
`knee-doctor`, 4 for `budgie-at`, 2 each for `hotel-provence` and `suuudokuuu`
— is 14 scale sets and 14 sessions instead of 40. Widening the matrix from 5
variants to 8 becomes cheaper than the 5-variant matrix was.

Profile ids stay opaque routing keys. They are written into durable instance
records, so renaming one while instances are live is an operational act, not a
config edit; the canonical label, not the id, is the name consumers use.

### 4. Adoption is a provisioning run

Provisioning advertises the union of the configured scale-set labels, the
canonical label, and every alias. An operator therefore adopts the scheme by
re-provisioning, not by editing every consumer workflow: the canonical label
appears beside the role name, workflows migrate at their own pace, and the alias
can be dropped from configuration once no workflow requests it. Runtime job
matching reads the same expanded set, so a job requesting either name binds to
the same scale set.

The expanded set is derived at both call sites and is deliberately not written
back into `fleet.json`: derived state stays derived, and the file keeps stating
only what the operator chose.

## Consequences

A workflow author reads `trf-linux-arm64-2x8` and knows what they get. Adding a
shape is adding a row to `linuxProfiles`, not inventing an adjective and
updating a translation table. The README table becomes a listing of vectors that
cannot drift from configuration, because a lying label now fails validation and
the self-hosting test resolves profiles by canonical label rather than by id.

The costs are real and bounded. Labels get longer, and `runs-on` lines get
wordier. `aliases` is a new configuration key, so a release that understands it
must be installed before a file that uses it — configuration decoding rejects
unknown fields by design. The whole-GiB rule forbids shapes like 6000 MiB;
nothing shipped uses one. And relaxing per-scope completeness means a scope can
now be configured without a variant its workflows request: those jobs simply
queue with no scale set to deliver them, which is visible in `fleet queues` but
is no longer a validation error.

## Not addressed here: phase-2 right sizing

The scheme makes right-sizing *expressible*; it does not make it *advised*. A
second phase would recommend a variant per job from evidence. This section
records what that evidence could honestly be, so the advisor is not designed
around a signal that does not exist.

**What Tart exposes.** Very little. `tart list --format json` reports name,
state, and disk sizing; there is no per-VM CPU or memory utilization API, and
Apple's Virtualization framework surfaces no guest-internal counters — no guest
memory pressure, no peak RSS, no load average. Any claim about what a job used
*inside* the guest is not available from Tart.

**What the host can observe without a guest agent.** Each running VM is a host
process. Sampling that process at the existing poll interval yields host-side
CPU time and resident size. Both are proxies with known error:

- CPU time of the VM process is virtualized-CPU busy time including hypervisor
  overhead — an upper bound on guest CPU use, and a sound one for the negative
  claim "this job never came close to using its cores".
- Resident size is the host's footprint of the guest's memory. Under
  Virtualization framework it grows toward the configured size and does not
  shrink, and it cannot distinguish memory a guest needed from memory a guest
  cached. It is an upper bound and must never be reported as a requirement.

**What the fleet already knows exactly.** The configured vector the job ran on;
queue wait; assignment-to-start latency; wall-clock duration; the repository,
workflow, and job identity; and the terminal state, including whether the guest
died rather than completed. This is exact, needs no new plumbing, and is the
strongest available signal: the same job observed across two variants shows
whether duration actually scaled with the vector.

**Shape of the record.** One bounded row per completed job — demand key,
repository, workflow and job name, profile id, canonical label, vector, queued
at, started at, finished at, terminal state, and optionally the sampled host-side
CPU and RSS peaks with an explicit "proxy" marker — in a retention-bounded table
in the existing SQLite state (ADR 0003), written through the same idempotent
outbox as everything else.

**Where the advisor reads it.** Never by opening the database: through a
versioned read-only `fleet.v1` projection over the admin socket, as every other
operator surface does, with the recommendation itself a pure deterministic
function so it can be tested without a host.

**What it may say.** Only claims that survive the proxy error above. "This job
used at most 40% of its four vCPU across 30 runs and its duration did not change
when it moved from 2×4 to 4×8" is defensible. "This job needs 3 GiB" is not, and
the advisor must not say it. Recommendations advise a configuration change; they
never re-route a job or reorder admission, both of which remain governed by the
ADRs listed under Status.

## Evidence

- `internal/config`: canonical derivation for both platforms; a label or alias
  that claims a vector it does not have is rejected; non-GiB memory, invalid
  runner-label tokens, blank aliases, and empty vectors are rejected; two
  profiles claiming one name are rejected; a legacy label becomes an alias with
  the canonical label attached; the shipped example's labels are all derived and
  every retired name still resolves; aliases survive decode, encode, and clone.
- `internal/config`: a scale set advertising another variant's canonical label
  is rejected; a scope exposing a subset of the matrix is accepted; a scope
  exposing nothing is rejected.
- `internal/provision`: a role-named configuration is provisioned with the
  canonical label and aliases attached to the scale-set spec.
- `internal/app`: a binding built from a role-named configuration matches jobs
  requesting either name, and refuses a shape it does not serve.

## Amendment 2026-08-25: the architecture component is declared, not constant (issue #269)

§1 of this record says `arch` "is `arm64`, because Tart builds on Apple's
Virtualization framework and runs only on Apple silicon". That was true of every
node the fleet had when it was written, and it stopped being true the day node B
arrived: a GEEKOM A9 running Linux guests on x86_64 through the container
backend of ADR 0034's *Amendment 2026-08-04*.

[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) §4 already decided what
happens then — the architecture "stops being the constant `arm64` in
`internal/config/labels.go:23` and becomes a property of the node's
configuration, still derived and still unable to lie" — and this amendment
records that it is now built, because it is *this* record's §1 whose text was
wrong until it was.

### What changed

A node declares one optional top-level key:

```json
"guestArch": "amd64"
```

It names the architecture of every guest that node boots, and it is the `arch`
component of every canonical label the node derives. Absent means `arm64`, so
mac-mini and mac-studio derive exactly the labels they already advertise and
their files encode byte-for-byte what they encoded before. Node B declares
`amd64` and derives `trf-linux-amd64-2x4` and `trf-linux-amd64-4x8` — the names
`docs/MULTI_NODE_PLAN.md` has assumed since #139, and which were unwritable
until now: a configured `trf-linux-amd64-2x4` was refused as a label describing
a vector it does not have, which is what issue #269 reported and what
`config/nodes/geekom.json` worked around with plain `linux-amd64-*` aliases.

Nothing else about the derivation moves. The label is still computed from the
configured vector, an alias that matches the canonical grammar must still equal
the derived label, and the scheduler still reads the vector and never a name.

### Two things the key may not say

- **The vocabulary is closed: `arm64` or `amd64`, one spelling each.** An
  arch-pinned consumer asks for a canonical label *by name*, so a node spelling
  the same machine `x86_64` would publish a second name for one architecture and
  no workflow could ask for both. A value outside the vocabulary is a
  configuration error, not a new name.
- **A node that boots macOS guests must declare `arm64`.** Tart's macOS guests
  run on Apple's Virtualization framework and are Apple silicon by construction,
  so `macosBurst.enabled` on a node declaring `amd64` is refused: `trf-macos-amd64-*`
  would name a machine that cannot exist. This is the same rule as the vector
  check — a derived label may not describe something the node does not provision.

### Why declared rather than probed

`runtime.GOARCH` is available in the daemon and is *not* what the label should
read. `fleet config validate` decodes a file and never touches a host, node
configurations are written, rendered, and checked on machines other than the one
that will run them (ADR 0034 §5), and the deterministic simulation has no host
at all. A probed value would make one node's labels underivable anywhere else,
which is exactly the property the render step and the cross-node parity gate of
ADR 0034's *Amendment 2026-08-04c* depend on. The declaration is checked the way
every other declaration in that amendment is: by a gate over `config/nodes/`,
not by asking the machine.

The residual risk is stated plainly: an operator who declares `amd64` on an
Apple-silicon node gets labels that lie about architecture, and no configuration
gate can catch it. It is the same class of risk as `baseImageCapabilities`, and
smaller — a node's architecture is not a thing that drifts after an image
rebuild.

### Evidence

- `internal/config`: an amd64 node derives `trf-linux-amd64-*` for every profile
  and accepts the configured canonical label that used to be refused; an absent
  declaration still derives `trf-linux-arm64-*` for both platforms; a spelling
  outside the vocabulary and a macOS burst on a non-Apple-silicon node are both
  rejected; the declaration survives decode, encode, and clone, and a node that
  declares none writes no key.
- `tests/contract`: every file in `config/nodes/` derives canonical labels in the
  architecture that node declares, which is live over `geekom.json` today and
  goes on holding when ADR 0034 §5's render step writes the rest.
