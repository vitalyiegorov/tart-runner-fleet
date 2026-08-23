# Operate the fleet

Use the `fleet` binary from the installed immutable generation. Humans and
agents should not scrape `launchctl`, open SQLite, or infer an empty queue from
an unavailable daemon when the versioned API can answer the question.

```sh
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
FLEET="$ROOT/current/fleet"
ENDPOINT="unix://$ROOT/state/fleetd.sock"
```

`current` is an atomically replaced convenience link to the committed immutable
generation. Launchd continues to execute the exact versioned release path. The
link is therefore safe for interactive and agent use without weakening
rollback or executable identity. Confirm it against
`state/installed-generation.json` when auditing an update.

## Daily cockpit

```sh
"$FLEET" status --endpoint "$ENDPOINT"
"$FLEET" status --endpoint "$ENDPOINT" --require-ready --output json
"$FLEET" queues --endpoint "$ENDPOINT" --output json
"$FLEET" instances --endpoint "$ENDPOINT" --output json
"$FLEET" operations --endpoint "$ENDPOINT" --output json
"$FLEET" observations --endpoint "$ENDPOINT" --output json
"$FLEET" doctor --endpoint "$ENDPOINT" --output json
```

Exit `4` means unavailable. Exit `5` means coherent but degraded. Neither means
zero demand and neither authorizes cleanup.

`queues` reports two views. The per-profile rows are the aggregate across every
scope bound to that profile; the per-scope rows attribute the same demand to the
scope and scale set that own it:

```
PROFILE  JOBS  OLDEST
builder  5     10m

SCOPE        PROFILE  SCALE SET  JOBS  OLDEST
budgie-org   builder  1          4     10h
sudoku-repo  builder  1          1     30s
```

Ask the second table first during an incident. A host serving several scopes can
report a healthy-looking `builder 5` while one scope contributes four jobs that
have never been served -- the aggregate cannot distinguish an idle scope from a
busy one sharing its profile. `--output json` returns `{profiles, scopes}` when a
breakdown is present and the bare profile array when it is not, so older
consumers keep parsing.

A fleet that declares priority tiers gets a third table naming the tier each
scope's waiting demand landed in, and the same breakdown appears as an additive
`tiers` array on every scope row of `--output json`:

```
SCOPE          PROFILE  TIER     JOBS  OLDEST
vitalyiegorov  builder  release  2     1h5m
budgie-at      builder  default  3     1h21m
```

`default` is the tier every unmatched demand lands in. The section and the JSON
key are absent when no tier is declared, so a fleet with no policy renders and
publishes exactly what it always did. See
[ADR 0037](docs/adr/0037-a-declared-tier-orders-a-band-escalation-bounds-it.md)
for what a tier does and does not change about admission.

## Operational objective

Operate for maximum **useful** utilization: the highest sustainable completion
throughput within the configured CPU, memory, slot, repository, profile, disk,
and host-pressure limits. A high VM count is not success if capacity is
overcommitted, one repository monopolizes the host, or old large work starves.
Conversely, an idle resource vector while compatible work is queued is a
scheduling defect worth investigating.

For young work, dominant resource share is the generic completion-cost proxy,
so smaller vectors are considered before larger vectors and exact packing
maximizes the admitted job count. Aging eventually overrides that optimization
with global FIFO. This deliberately improves throughput without turning a
continuous stream of small jobs into starvation for a builder or other large
job.

## Resource model

A runner label states the shape it delivers — `trf-<os>-<arch>-<cpu>x<ramGiB>`,
derived from the profile's configured vector so it cannot drift from the VM you
get ([ADR 0032](docs/adr/0032-resource-explicit-runner-labels.md)). The example
configuration exposes this variant matrix:

| Canonical label | vCPU | Memory | Maximum concurrency | Alias |
| --- | ---: | ---: | ---: | --- |
| `trf-linux-arm64-1x2` | 1 | 2 GiB | constrained by the 8-vCPU/16-GiB Linux envelope | `linux-small` |
| `trf-linux-arm64-2x4` | 2 | 4 GiB | constrained by the 8-vCPU/16-GiB Linux envelope | `linux-medium` |
| `trf-linux-arm64-2x8` | 2 | 8 GiB | constrained by the 8-vCPU/16-GiB Linux envelope | — |
| `trf-linux-arm64-4x8` | 4 | 8 GiB | constrained by the 8-vCPU/16-GiB Linux envelope | `linux-large` |
| `trf-linux-arm64-6x12` | 6 | 12 GiB | constrained by the 8-vCPU/16-GiB Linux envelope | `linux-xl` |
| `trf-linux-arm64-8x16` | 8 | 16 GiB | 1 — it is the whole Linux envelope | — |
| `trf-macos-arm64-6x12` | 6 | 12 GiB | 1 | `macos-builder` |
| `trf-macos-arm64-4x7` | 4 | 7 GiB | 2 | `macos-maestro` |

Aliases are the retired role and tier names. They resolve to the same profile
and are advertised on the same scale set, so a workflow that still asks for
`linux-large` keeps routing while it migrates to `trf-linux-arm64-4x8`.

Linux and macOS share one CPU, memory, slot, repository, and host-pressure
envelope. A 4×7 macOS VM may therefore run beside Linux jobs when their combined
resource vectors fit, and two of them may run together. Values come from
`fleet.json`, so treat the table as the example contract rather than a hidden
default; a profile whose label disagreed with its vector would fail validation.

Each GitHub scope exposes only the variants its `scaleSets` list names, so a
wide matrix costs one scale set per variant *per scope that wants it* rather
than one in every scope.

### Second-pilot mode

By default the envelope above is a static configured vector, which suits a
dedicated CI host. On a Mac that also serves an interactive tenant it cannot
adapt: host CPU load never narrows it, and an idle machine offers no more than a
busy one.

Set `"elasticHostEnvelope": true` in `fleet.json` to size the fleet against the
machine it observes instead. Admission then takes the minimum of three bounds:

- `maxLinuxCpu` / `maxLinuxMemoryMb` as a **Linux-only** cap. In this mode a
  macOS VM no longer spends the Linux budget, so re-derive both values from the
  physical machine rather than reusing a number hand-tuned as a shared total.
- the **observed physical host** (`hw.ncpu`, `hw.memsize` less
  `minAvailableMemoryMb`), charged for every live VM on either platform, so
  aggregate reservations never exceed the real machine.
- the **measured residual**: available CPU is `floor(cores x idle%)` and memory
  is the pressure-derived figure. A saturated host advertises no free cores, so
  the fleet waits for its share instead of competing for it.

Physical facts the probe cannot read are reported as unobserved and impose no
bound, falling back to the configured envelope rather than closing admission.

The idle-derived CPU residual throttles **young** work only. A demand past
`linuxReservationAgeSeconds` escapes that advisory clamp and is bounded by the
hard constraints alone -- physical cores net of live reservations, the memory
residual, and every cap -- so a large job cannot starve waiting for a quiet
moment the host's own tenant never provides. All
pressure guardrails, exact vectors, slot ceilings, repository caps, profile
`maxActive`, and aging guarantees apply unchanged; the flag only changes how wide
the envelope is. It ships off, and enabling it on a host is an operational action
with the usual observe-then-promote evidence. See
[`ADR 0018`](docs/adr/0018-second-pilot-elastic-host-envelope.md).

### Capping a node below its hardware: `hostBudget`

Second-pilot mode makes the fleet *polite*: it yields as the host's own tenant
gets busy. It does not make the fleet *bounded*, because a tenant that is quiet
right now is not a tenant that has gone away, and on a quiet machine the fleet
expands to the whole of it.

`hostBudget` is the missing static ceiling. It caps this node's **total**
admission envelope — every platform charged against it together — below physical
capacity:

```json
"hostBudget": { "cpu": 4, "memoryMb": 10240 }
```

Omit it and nothing changes: the envelope is the physical machine, exactly as
before. Removing the setting is the whole rollback.

[`config/fleet.example.json`](config/fleet.example.json) ships
`"hostBudget": { "cpu": 10, "memoryMb": 23552 }` — the production Mac mini's own
envelope, being its 10 cores and 24576 MiB less the 1024 MiB `minAvailableMemoryMb`
reserve. Stated explicitly like that the budget binds at exactly the physical
bound and changes nothing, which is what makes it the right first value on any
node: it documents the share, and lowering the number later is a one-line change
whose effect is bounded and obvious. Derive it from *your* machine — a budget
above the host is refused at the probe, by design.

- It **composes** with everything else by minimum and can only ever narrow an
  envelope. A budget larger than the machine is a no-op, not a widening.
- It is a **hard** bound, not an advisory throttle. Work past
  `linuxReservationAgeSeconds` escapes the CPU-idle clamp above; it does not
  escape the budget.
- Every **live VM of either platform** is charged against it, so a macOS builder
  and a Linux runner cannot each spend the full budget.
- The **pressure guardrails still apply unchanged**. They read whole-host disk,
  memory, swap, load, and idle and keep failing closed — that is the dynamic
  protection for a co-tenant. `hostBudget` is the ceiling that holds at idle, and
  admission sees the minimum of the two.

Two configuration rules follow, and both are checked rather than documented:

- **A profile this node exposes must be able to fit the budget.** A job routed to
  a shape the node can never admit does not queue politely; it queues forever.
  `fleet config validate` rejects it. A profile that is configured but named by
  no `scaleSets` entry is not exposed, receives no jobs, and is not checked —
  which is how a budgeted node keeps the mandatory `macosBurst.builder` in its
  file while serving `maestro` alone.
- **A budget must fit the machine.** `fleet config validate` decodes a file and
  never probes a host, so this one is checked at the host probe: a budget above
  the physical cores, or above physical memory less `minAvailableMemoryMb`,
  reports the host observation unavailable with a reason naming both figures.
  `fleet status`, `fleet doctor`, and `fleet observations` all show it. A
  physical dimension the probe cannot read imposes no bound.

Scale-set capacity is bounded by the budget too, so a node advertises the number
of slots it can actually serve rather than the number its `maxActive` allows.

### Repairing a drifted scale set

`scale-sets provision` creates what is missing and reuses what already matches.
A scale set whose GitHub object has diverged from `fleet.json` -- labels, runner
group, or runner setting -- fails the plan closed with a conflict, because
silently rewriting a live routing object is not something a create-missing run
should do.

Add `--reconcile-drift` to make that divergence repairable. The plan then reports
`update` for the affected set, and applying repairs it **in place**, preserving
the scale-set id so jobs GitHub already routed to it are not orphaned. The write
is verified against desired state afterwards; an accepted-but-still-different
object is reported uncertain rather than provisioned. An exactly matching set is
never rewritten.

```sh
"$FLEET" scale-sets provision --config fleet.json --reconcile-drift
"$FLEET" scale-sets provision --config fleet.json --reconcile-drift   --apply --write --confirm provision-scale-sets --reason "repair drifted labels"
```

Plan first. `--reconcile-drift` is deliberately a separate flag rather than part
of the confirmation token: repairing an existing object is larger authority than
creating a missing one. See
[`ADR 0023`](docs/adr/0023-repairable-scale-set-drift.md).

Capacity is not a drift dimension: the Actions API models no capacity field on a
runner scale set, so a set matching on name, runner group, labels, and runner
setting is exact as far as reconciliation is concerned.

### Mixed macOS profile cohorts

By default the fleet runs one macOS profile cohort at a time: spawning a builder
drains idle maestros first, and a busy maestro blocks the builder entirely. On a
build-and-test topology that serializes every build against every test wave.

Set `"mixedProfileCohorts": true` inside `macosBurst` to let macOS profiles
coexist whenever their exact vectors fit -- the same law ADR 0012 applied to
platforms. A 6-vCPU builder and a 4-vCPU maestro share a 10-core machine instead
of taking turns. Every hard bound is unchanged: the physical total, profile
`maxActive`, repository caps, the elastic envelope, and drain safety (a busy
instance is never touched; the idle drain-and-switch fallback remains for
profiles that do not fit side by side). See
[`ADR 0024`](docs/adr/0024-mixed-macos-profile-cohorts.md).

### Bounding how long one instance may hold the host

Every profile carries `occupancyBudgetSeconds`: a wall-clock ceiling on how long
ONE instance of that profile may hold its resource vector. Omit it and the
platform default applies -- two hours on macOS, one hour on Linux, both sized
above the longest healthy run measured on this fleet and well inside GitHub's
six-hour job maximum. Set it to `0` to exempt a profile entirely; any other
value must be between 300 and 21600 seconds.

An instance past its ceiling is reclaimed through the ordinary graceful drain:
the ephemeral guest is asked to power down and its runner is deregistered, and
the job ends on GitHub as a lost-communication failure. Nothing is killed. Long
before that, `fleet status` shows the hold in an `OCCUPANCY` table, the daemon
warns at three quarters of the ceiling, and `fleet doctor` fails when an
over-budget instance holds a vector that queued work would fit. See
[`ADR 0036`](docs/adr/0036-an-instance-may-not-hold-its-vector-forever.md).

`baseImageRunnerVersion` declares which `actions/runner` release a base image
carries, once per image — top level for the Linux image, inside `macosBurst` for
the macOS one. `runnerVersionFloor` is what those declarations are judged
against; omit it for GitHub's registration minimum of 2.329.0, and raise it when
a new `actions/runner` release starts its 30-day clock. `fleet doctor` prints the
version each image carries on every run and fails the `runner version` check when
one is below the floor — or when an image declines to declare one at all, because
a guest here registers with `DisableUpdate` set and can never upgrade itself, so
an image nobody has vouched for is one nobody can vouch for. See
[`ADR 0041`](docs/adr/0041-a-base-image-declares-its-runner-version.md).

`linuxSerialLogDirectory` writes each Linux guest's serial console to a file on
the host while it runs, which is the only artifact that survives a guest kernel
dying mid-job. `fleet doctor` fails its `guest console` check when a node boots
Linux guests with this unset — issues #236, #258, and #259 all ended without a
root cause because it was. Verify `tart run --help` advertises `--serial-path`
on the node first; see [`ADR 0046`](docs/adr/0046-a-guest-console-is-evidence-the-fleet-must-own.md)
and [`docs/LINUX_BASE_IMAGE.md`](docs/LINUX_BASE_IMAGE.md).

`macosBurst.admissionPolicy` controls cross-platform admission. Omit it or set
it to `"shared"` for the behavior above. Set it to `"macos-exclusive"` for an
experiment that must fill one macOS profile cohort without admitting new Linux
VMs. In exclusive mode, busy foreign instances are never interrupted: the
scheduler persists the handoff, drains only idle foreign instances, and makes
same-tick replacement spawns depend on those drains. An active macOS cohort
continues to block new Linux admission until its instances become idle.

## Scheduling principles

1. Fresh observations are mandatory; unavailable data fails closed.
2. Aged work wins global FIFO and cannot starve.
3. Young control-plane work may receive one bounded priority quantum so the
   manager can build its successor.
4. Within each young scheduling lane, lower dominant-resource-share profiles
   are considered first and exact packing maximizes admitted job count.
5. A declared priority tier orders demand INSIDE each of those bands, never
   across them: an aged demand still precedes a fresh one of any tier, and a
   waiting demand climbs one tier per configured escalation threshold, so a tier
   costs everything below it at most rank x threshold of extra waiting
   (ADR 0037). A fleet that declares no tier is unaffected.
6. Compatible small Linux work may use one durable backfill budget while a
   macOS handoff is draining; it cannot postpone the handoff indefinitely.
   This bounded backfill applies only to the default shared policy; exclusive
   macOS cohorts do not admit Linux backfill.
7. A single deterministic state machine owns each instance from planned clone
   through registration, assignment, drain, deregistration, stop, and deletion.
8. External effects are durable, leased, idempotent, and retried with bounded
   backoff; restart resumes state instead of guessing from process presence.

## Automatic updates

Inspect the updater without changing authority:

```sh
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
tail -n 100 "$ROOT/state/update.stdout.log"
tail -n 100 "$ROOT/state/update.stderr.log"
```

Run the same idempotent forward-only check on demand:

```sh
"$FLEET" update apply-latest \
  --repo owner/tart-runner-fleet \
  --mode authority \
  --endpoint "$ENDPOINT" \
  --confirm automatic-release-update
```

`production generation is current` is a successful no-op. A busy-fleet refusal
is also safe: the five-minute updater retries after the fleet becomes quiescent.
Never bypass readiness, checksum, version, or mode checks to force an update.

The updater LaunchAgent is a periodic one-shot process. Between invocations,
`launchctl` may report it as `not running`; that is healthy when its last exit
status is 0 and its loaded program path belongs to the committed generation.
Cross-check that path against both `state/installed-generation.json` and the
updater plist: an on-disk plist update does not prove launchd discarded a cached
older executable.

An already-loaded updater is replaced by a separate retrying
`updater-handoff` LaunchAgent after the generation commit becomes durable. The
updater never boots out itself. Treat a persistent handoff failure or any
manifest/plist/loaded-program mismatch as an update failure, even when the fleet
authority remains ready.

## Incident workflow

1. Capture `status`, `doctor`, bounded logs, runner/job state, and `tart list`.
2. Preserve ambiguous VMs and the SQLite WAL set; stop admission if ownership is
   uncertain.
3. Reproduce the defect with a red deterministic test.
4. Implement the fix in the same PR and require full coverage/race/quality/build
   CI before merge.
5. Verify trusted `main`, the normal production release and all release assets.
6. Let the updater install only while idle, then replay a representative real
   Linux and macOS load and audit complete cleanup.

See [`docs/OPERATIONS.md`](docs/OPERATIONS.md) for first promotion and rollback,
[`docs/CLI.md`](docs/CLI.md) for the complete command contract,
[`docs/AGENT_RUNBOOK.md`](docs/AGENT_RUNBOOK.md) for the agent cockpit, and
[`AGENTS.md`](AGENTS.md) for the coding-agent safety rules.
