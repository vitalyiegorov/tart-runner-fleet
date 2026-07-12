# Contributor contract

This controller can terminate VMs and affect every CI queue on the host. Treat
all changes as safety critical.

1. Write a failing test before production code. Production incidents begin as
   replay fixtures.
2. Keep policy pure and deterministic. Time, I/O, randomness, and process
   execution enter through interfaces.
3. Never represent an unavailable observation as an empty collection.
4. Never perform a destructive action without fresh ownership, runner, job,
   Tart, and host confirmation as applicable.
5. Preserve at-least-once delivery and idempotent effects. Commit work before
   acknowledging Scale Set messages.
6. Never log or persist JIT configuration, tokens, private keys, or generated
   runner credentials.
7. Never assemble shell commands from external values. Use argument vectors and
   context deadlines.
8. Run `make ci`; lint, CPD, deadcode, vulnerabilities, coverage, race, and build gates must pass.
9. Do not enable authority mode in a code change. Promotion is an explicit
   operational action after observe/shadow/canary evidence and rollback proof.

## Repository map

- `cmd/fleetd`: process lifecycle and dependency wiring only.
- `cmd/fleetctl`: thin operator CLI; no direct SQLite or Tart access.
- `internal/domain`: immutable domain values and lifecycle rules.
- `internal/scheduler`: pure deterministic policy.
- `internal/adminapi`: versioned read-only DTOs, Unix socket, and bounded client.
- `internal/adapters`: GitHub, SQLite, Tart, and host implementations.
- `internal/operations`: durable operations, leases, retries, and workers.
- `internal/telemetry`: coherent status, health, readiness, and metrics.
- `tests/{contract,integration,replay,chaos}`: cross-package safety evidence.
- `docs/adr`: decisions that must be updated when architecture changes.

## Agent inspection recipe

Prefer the versioned JSON interface; do not scrape human tables or open the
database while the daemon is running.

```sh
fleetctl status --output json
fleetctl doctor --output json
fleetctl queues --output json
fleetctl instances --output json
fleetctl operations --output json
```

Interpret exit `4` as unavailable and exit `5` as coherent but degraded. Never
turn either condition into an empty queue or an authorization to clean runners.

## Before editing

1. Read the relevant ADR and package tests.
2. Convert every incident or missing behavior into a failing deterministic test.
3. Keep policy changes out of adapters and CLI rendering.
4. Preserve JSON field names and enum values in `fleet.v1`.
5. Never expose generic SQL, arbitrary process execution, or raw Tart/GitHub
   passthrough commands through `fleetctl`.

## Required handoff evidence

```sh
make fmt
make ci
git diff --check
```

Report the exact coverage percentage, changed invariants, migration/API impact,
and whether observe/shadow authority limits remain intact. Do not claim a
production cutover from unit or integration evidence.

## Prohibited actions

- No direct modification of `fleet.db` or manual deletion of state rows.
- No VM deletion without fresh ownership, runner, job, Tart, and host evidence.
- No printing environment variables, Keychain values, JIT configuration, or
  operation payloads.
- No weakening deadlines, coverage, race tests, socket permissions, or
  fail-closed observations to make a check pass.
- No authority/canary enablement, launchd installation, or incumbent shutdown
  without an explicit operational promotion task.
