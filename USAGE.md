# Operate the fleet

Use the `fleetctl` binary from the installed immutable generation. Humans and
agents should not scrape `launchctl`, open SQLite, or infer an empty queue from
an unavailable daemon when the versioned API can answer the question.

```sh
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
FLEETCTL="$ROOT/releases/<installed-version>/fleetctl"
ENDPOINT="unix://$ROOT/state/fleetd.sock"
```

## Daily cockpit

```sh
"$FLEETCTL" status --endpoint "$ENDPOINT"
"$FLEETCTL" status --endpoint "$ENDPOINT" --require-ready --output json
"$FLEETCTL" queues --endpoint "$ENDPOINT" --output json
"$FLEETCTL" instances --endpoint "$ENDPOINT" --output json
"$FLEETCTL" operations --endpoint "$ENDPOINT" --output json
"$FLEETCTL" observations --endpoint "$ENDPOINT" --output json
"$FLEETCTL" doctor --endpoint "$ENDPOINT" --output json
```

Exit `4` means unavailable. Exit `5` means coherent but degraded. Neither means
zero demand and neither authorizes cleanup.

## Resource model

The example configuration exposes these bounded profiles:

| Profile | vCPU | Memory | Maximum concurrency |
| --- | ---: | ---: | ---: |
| Linux small | 1 | 2 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| Linux medium | 2 | 4 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| Linux large | 4 | 8 GiB | constrained by the 8-vCPU/16-GiB Linux envelope |
| macOS builder | 8 | 12 GiB | 1 |
| macOS Maestro | 4 | 7 GiB | 2 |

Linux and macOS never overlap. Two Maestro VMs may run together; the exclusive
builder runs alone. Linux mixes sizes up to the configured CPU, memory,
instance, repository, and host-pressure bounds. Values come from `fleet.json`,
so treat the table as the example contract rather than a hidden default.

## Scheduling principles

1. Fresh observations are mandatory; unavailable data fails closed.
2. Aged work wins global FIFO and cannot starve.
3. Young control-plane work may receive one bounded priority quantum so the
   manager can build its successor.
4. Compatible small Linux work may use one durable backfill budget while a
   macOS handoff is draining; it cannot postpone the handoff indefinitely.
5. A single deterministic state machine owns each instance from planned clone
   through registration, assignment, drain, deregistration, stop, and deletion.
6. External effects are durable, leased, idempotent, and retried with bounded
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
"$FLEETCTL" update apply-latest \
  --repo owner/tart-runner-fleet \
  --mode authority \
  --endpoint "$ENDPOINT" \
  --confirm automatic-release-update
```

`production generation is current` is a successful no-op. A busy-fleet refusal
is also safe: the five-minute updater retries after the fleet becomes quiescent.
Never bypass readiness, checksum, version, or mode checks to force an update.

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
[`docs/CLI.md`](docs/CLI.md) for the complete command contract, and
[`AGENTS.md`](AGENTS.md) for the coding-agent safety rules.

