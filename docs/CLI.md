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
| `queues` | Jobs and oldest age by bounded profile |
| `instances` | VM count, vCPU, and memory by bounded profile |
| `operations` | Retrying and dead durable-operation counts, the bounded failure code and worst attempt count for anything not progressing, and the identity of each parked dead letter |
| `operations discharge` | Close one dead-lettered cleanup an operator has established can never complete; optionally retire the phantom instance row and its stopped VM |
| `observations` | Scheduler observation freshness and age |
| `health` | Liveness and readiness probes |
| `doctor` | Deterministic API, liveness, readiness, and metrics checks |
| `metrics` | Raw Prometheus exposition |
| `config validate PATH...` | Decode and validate one configuration without starting the daemon; with more than one path, additionally check the cross-node rules |
| `scale-sets provision --config PATH` | Plan drift-free scoped runner scale sets; explicit guards are required to apply and persist IDs |
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

## Common flags

- `--endpoint unix:///absolute/path/fleetd.sock`: local admin endpoint. Connection
  flags may be placed before or after a remote command; the default uses the
  private platform configuration state directory.
- `--timeout 5s`: bounded request timeout; maximum 30 seconds.
- `--output table|json` or `-o`: human table or stable machine JSON.
- `--require-ready`: make `status` return exit 5 when readiness is false.

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
