# fleet operator contract

`fleet` is the bounded control-plane client for operators and agents. Status
commands use a private Unix socket owned by `fleet` and never read SQLite.
Mutations are limited to explicit, guarded scale-set provisioning,
production-generation adoption/update, and dead-letter discharge; arbitrary Tart,
GitHub, shell, and SQL passthroughs do not exist.

## Commands

| Command | Purpose |
| --- | --- |
| `status` | Complete controller, host pressure/admission, queue, instance, observation, and operation summary |
| `queues` | Jobs and oldest age by bounded profile, by scope, and by priority tier when one is declared |
| `instances` | VM count, vCPU, and memory by bounded profile |
| `operations` | Retrying and dead durable-operation counts, the bounded failure code and worst attempt count for anything not progressing, and the identity of each parked dead letter |
| `operations discharge` | Close one dead-lettered cleanup an operator has established can never complete; optionally retire the phantom instance row and its stopped VM |
| `observations` | Scheduler observation freshness and age |
| `health` | Liveness and readiness probes |
| `doctor` | Deterministic API, liveness, readiness, queue SLO, occupancy, reservation, drain progress, guest liveness, runner version, parked scale sets, policy declaration, and metrics checks |
| `metrics` | Raw Prometheus exposition |
| `config validate PATH...` | Decode and validate one configuration without starting the daemon; with more than one path, additionally check the cross-node rules |
| `config policy PATH...` | Print the load-bearing policy one node runs with; with more than one path, the keys on which they disagree |
| `scale-sets provision --config PATH` | Plan drift-free scoped runner scale sets; explicit guards are required to apply and persist IDs |
| `scale-sets audit --config PATH` | Read the scale sets GitHub holds for each configured scope and classify each as bound or parked; a parked set holding work is printed as evidence, and exits 5 only with `--strict` |
| `update adopt` | Adopt one already-running exact generation and install its reboot-safe automatic updater |
| `update apply-latest` | Idempotently verify and apply the latest forward-only normal production release while idle |
| `version` | CLI build version |
| `api-version` | Machine API compatibility version |

The old `validate-config PATH` spelling remains a compatibility alias.

### Validating more than one node at once

`config validate` accepts several paths. Each is decoded and validated exactly as
a single path is — same checks, same messages, same exit codes — and then two
rules that are knowable only when every node is in hand are applied across the
set ([ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md)):

- **Guest-capability parity.** For any label advertised by more than one node,
  every capability a scale set requires behind that label must be declared by
  every node that advertises it, on the base image for that label's platform. A
  node that advertises a label without carrying what someone else requires behind
  it is the 2026-08-04 incident: a job that fails deterministically on one node
  and passes deterministically on another, which presents as flakiness in a
  repository this fleet does not own.
- **One owner per scale set.** A `(scope, scale-set name)` pair may appear in
  exactly one node's configuration. GitHub enforces the same thing with a `409`,
  but only after two daemons have started evicting each other.

`self-hosted` is excluded from the parity rule: this codebase requires it of
every scale set, so it carries no routing information and comparing through it
would demand that every node in a deliberately heterogeneous fleet declare every
capability in it.

```sh
fleet config validate config/nodes/mac-mini.json config/nodes/mac-studio.json
fleet config validate --output json config/nodes/*.json
```

A single path prints `configuration is valid: PATH` and the JSON object
`{"valid": true, "path": "PATH"}`, exactly as before. Several paths print one
line per configuration plus a summary, and the JSON object carries `paths`
instead of `path`. Cross-node failures are written to stderr, one per line, and
exit `1`.

### Comparing what two nodes are configured with

`config policy` answers a different question from `config validate`: not "is this
file legal" but "what does this node actually decide with". It prints the
bounded, credential-free projection of the effective configuration that
[ADR 0053](adr/0053-a-node-declares-the-policy-it-runs-with.md) defines — the
same object a running daemon publishes as `data.policy` — with `policyDigest`
identifying the set.

Each path is **either** a node configuration **or** a `fleet status --output
json` document, recognised by the presence of `data.policy`. That is what makes
the comparison possible without SSH and without file access: copy each node's
status document out and diff them.

```sh
fleet config policy ./state/fleet.json
fleet config policy ./state/fleet.json /path/to/peer-fleet.json
fleet config policy ./node-a-status.json ./node-b-status.json
```

One path prints the policy as JSON and exits `0`. Two or more print one row per
disagreeing key — the key path, then one column per node — and exit `5`; nodes
that agree print `no policy drift across N nodes` and exit `0`. Anything that
cannot be read or parsed exits `2`, because a diff that could not read one side
has not established agreement.

```
KEY                                 ./state/fleet.json  /path/to/peer-fleet.json
macosBurst.mixedPlatformAdmission   true                false
1 policy key disagrees across 2 nodes
```

A key one node does not project at all reads as `absent`, never as `false`: a
missing key and a stated `false` being indistinguishable is the whole of issue
#304. Within one release they cannot differ — every projected key is always
published — so `absent` means the two nodes are running different releases.

Human `fleet status` prints `policy <digest-prefix>` and `fleet doctor` carries a
`policy` row with the same digest. The row is informational and never fails on
its own: a digest is an identity, not a judgement, and no single node can know
whether its own policy is the right one.

## Common flags

- `--endpoint unix:///absolute/path/fleetd.sock`: local admin endpoint. Connection
  flags may be placed before or after a remote command; the default uses the
  private platform configuration state directory.
- `--timeout 5s`: bounded request timeout; maximum 30 seconds.
- `--output table|json` or `-o`: human table or stable machine JSON.
- `--require-ready`: make `status` return exit 5 when readiness is false.
- `--require-healthy`: make `status` return exit 5 when the daemon is not
  healthy — ticking, store writable, observations fresh or stale only because
  this node withdrew its sessions (ADR 0052). A node under host pressure fails
  `--require-ready` and passes this; it is what the update transaction gates on.
  Against a daemon older than ADR 0052 it reads `ready`, which is stricter.

The HTTP compatibility endpoint is accepted only for literal loopback IPs.
HTTPS, DNS names, remote IPs, URL credentials, query strings, and fragments are
rejected.

## Output rules

- stdout contains requested data only; stderr contains diagnostics only.
- JSON timestamps are UTC RFC3339 and all list fields are arrays, never `null`.
- Human rows and JSON arrays are deterministically sorted by bounded profile or
  observation name.
- Human status shows free disk, reclaimable memory, swap with the page-out rate
  that qualifies it, CPU idle, load, and the latest admission decision; JSON and
  Prometheus expose the same units. Swap reads `13593 MiB (paging 0/s)` — the
  level alone never decides admission — or `(paging unmeasured)` when there is
  no second sample yet and the level blocks on its own.
- The JSON compatibility promise is identified by `apiVersion: fleet.v1`.
- Tables use compact ages for reading; automation must use numeric age fields.
- No token, private key, JIT configuration, operation payload, or unbounded
  backend error is part of the API.

### Auditing the scale sets GitHub holds

`scale-sets audit` is the read-only answer to a scale set that exists on GitHub
and that no daemon polls. GitHub routes a queued job to exactly one matching set,
marks it assigned, and then offers it to nobody else — so a parked set holding
work is work that will never run, and no signal a node publishes about the sets
it *serves* can see it ([ADR 0054](adr/0054-a-parked-scale-set-is-audited-not-trusted.md),
issue #164).

```sh
fleet scale-sets audit --config ./state/fleet.json
fleet scale-sets audit --config ./state/fleet.json --output json
fleet scale-sets audit --config ./state/fleet.json --strict
```

```
suuudokuuu	1	trf-sudoku-builder	bound	assigned=0	busy=0	registered=0	idle=0
suuudokuuu	7	trf-sudoku-builder-studio	parked	assigned=2	busy=2	registered=0	idle=0
```

| Exit | Meaning |
| ---: | --- |
| 0 | The audit completed. Findings, if any, are printed on stderr as evidence |
| 4 | The audit was unavailable or could not produce a trustworthy result — GitHub unreachable, the App credential missing, or an answer too uncertain to classify. **Never read as a pass** |
| 5 | `--strict` only: a parked set holds assigned jobs or busy runners with no registered runner |

**This command produces evidence, not a verdict.** Under
[ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md) a sibling node may
legitimately own the set, and this command cannot read a sibling's configuration,
a sibling's queue, or the freshness of GitHub's per-set counters. On 2026-09-21
three sets bound on the mac mini read `assigned>0 busy>0 registered=0` from the
Linux node while their jobs were simply queued behind the mini's own capacity,
and the Linux node's OWN bound set read `assigned=4 busy=4 registered=0` with an
empty local queue. A genuine stranding and an ordinary backlog are the same
reading from one chair, so the default exit is `0`: run the audit on EVERY node
and act only when the same set shows work from all of them. `--strict` exits `5`
on a finding, for an operator or script that has already done that and accepts
the false-positive risk.

The cost is bounded: one listing per scope, plus one read per parked set whose
listing carried no statistics. There is no polling loop. The authority daemon
runs the same audit every `github.parkedScaleSetAuditMinutes` (default 15, `0`
disables) and publishes it as the `parked scale sets` doctor row, the
`parkedScaleSets` status section and the `fleet_parked_scale_set_assigned_jobs`
and `fleet_parked_scale_set_busy_runners` metrics.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | Successful and, when required, healthy |
| 1 | Local operation/configuration failure |
| 2 | Invalid command or flags |
| 3 | Requested bounded resource not found (e.g. an unknown dead letter) |
| 4 | Daemon unavailable, timeout, canceled request, or invalid API response |
| 5 | Coherent degraded/not-ready state |
| 6 | Unsafe or failed precondition, including every guarded-mutation refusal |

## Agent examples

```sh
fleet status --output json
fleet status --require-ready --output json
fleet doctor --output json
```

Exit 4 and 5 are evidence, not permission to assume zero demand or delete VMs.

## Mutation boundary

`scale-sets provision` is plan-only unless `--apply --write`, an exact
confirmation phrase, and a non-empty reason are supplied. `update adopt` and
`update apply-latest` likewise require distinct exact confirmation phrases and
preserve controller mode, readiness, checksums, and rollback.

Every `update` subcommand takes `--root`, `--state-dir`, `--launch-agents-dir`,
`--config`, and `--endpoint`, and their defaults are this node's install layout:
`~/Library/Application Support/tart-runner-fleet` and `~/Library/LaunchAgents`
on macOS, `$XDG_DATA_HOME/tart-runner-fleet` and
`$XDG_CONFIG_HOME/systemd/user` elsewhere. `--domain` names the per-user service
manager, and the release transaction accepts only a launchd target
(`system`, `gui/<uid>`, `user/<uid>`, `pid/<pid>`): it swaps generations with
`launchctl` and lints with `plutil`, so on a node whose service manager is
`systemd --user` it refuses rather than half-applying a generation. That node
uses the manual bridge in [`OPERATIONS.md`](OPERATIONS.md).

`operations discharge` requires `--confirm discharge-dead-letter` and a non-empty
`--reason`, and is refused unless the daemon runs in authority mode. It reaches
exactly one durable row and, only with `--reap-instance`, exactly one owned VM
whose ownership and stopped power state are freshly re-observed. It never stops a
running guest and never removes a VM the controller does not own. It is the one
narrow exception to "no VM deletion", and it exists because a GitHub registration
that can never be released leaves no other permitted remedy — see
[`docs/OPERATIONS.md`](OPERATIONS.md). Generic `exec`, SQL, unqualified VM
deletion, and backend passthrough commands remain permanently out of scope.

```sh
fleet operations --output json | jq '.deadLetters'
fleet operations discharge --operation op-ID --instance trf-ID --reap-instance \
  --confirm discharge-dead-letter --reason "operator reason"
```
