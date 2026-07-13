# fleetctl operator contract

`fleetctl` is a read-only control-plane client for operators and agents. Its
primary transport is a private Unix socket owned by `fleetd`; it never reads the
SQLite schema and cannot execute arbitrary Tart, GitHub, shell, or SQL commands.

## Commands

| Command | Purpose |
| --- | --- |
| `status` | Complete controller, host-mode, queue, instance, observation, and operation summary |
| `queues` | Jobs and oldest age by bounded profile |
| `instances` | VM count, vCPU, and memory by bounded profile |
| `operations` | Retrying and dead durable-operation counts |
| `observations` | Scheduler observation freshness and age |
| `health` | Liveness and readiness probes |
| `doctor` | Deterministic API, liveness, readiness, and metrics checks |
| `metrics` | Raw Prometheus exposition |
| `config validate PATH` | Decode and validate a configuration without starting the daemon |
| `version` | CLI build version |
| `api-version` | Machine API compatibility version |

The old `validate-config PATH` spelling remains a compatibility alias.

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
| 3 | Requested bounded resource not found (reserved for list/get expansion) |
| 4 | Daemon unavailable, timeout, canceled request, or invalid API response |
| 5 | Coherent degraded/not-ready state |
| 6 | Unsafe or failed precondition (reserved for future guarded mutations) |

## Agent examples

```sh
fleetctl status --output json
fleetctl status --require-ready --output json
fleetctl doctor --output json
```

Exit 4 and 5 are evidence, not permission to assume zero demand or delete VMs.

## Deliberately absent

Drain/retry/pause commands will not exist until canary/authority promotion. Any
future mutation must use an audited API, fresh ownership/version preconditions,
an idempotency key, a reason, and explicit confirmation. Generic `exec`, SQL,
and backend passthrough commands are permanently out of scope.
