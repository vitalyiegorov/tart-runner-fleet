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

The example configuration exposes these bounded profiles:

| Profile | vCPU | Memory | Maximum concurrency |
| --- | ---: | ---: | ---: |
| Linux small | 1 | 2 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| Linux medium | 2 | 4 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| Linux large | 4 | 8 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| macOS builder | 8 | 12 GiB | 1 |
| macOS Maestro | 4 | 7 GiB | 2 |

Linux and macOS share one CPU, memory, slot, repository, and host-pressure
envelope. A Maestro VM may therefore run beside Linux jobs when their combined
resource vectors fit. Two Maestro VMs may run together; the 8-vCPU builder
exhausts the configured CPU envelope and consequently runs alone. Values come
from `fleet.json`, so treat the table as the example contract rather than a
hidden default.

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
bound, falling back to the configured envelope rather than closing admission. All
pressure guardrails, exact vectors, slot ceilings, repository caps, profile
`maxActive`, and aging guarantees apply unchanged; the flag only changes how wide
the envelope is. It ships off, and enabling it on a host is an operational action
with the usual observe-then-promote evidence. See
[`ADR 0018`](docs/adr/0018-second-pilot-elastic-host-envelope.md).

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
5. Compatible small Linux work may use one durable backfill budget while a
   macOS handoff is draining; it cannot postpone the handoff indefinitely.
   This bounded backfill applies only to the default shared policy; exclusive
   macOS cohorts do not admit Linux backfill.
6. A single deterministic state machine owns each instance from planned clone
   through registration, assignment, drain, deregistration, stop, and deletion.
7. External effects are durable, leased, idempotent, and retried with bounded
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
