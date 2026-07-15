# Agent runbook

This is the shortest safe path for a coding or operations agent to inspect and
support a production Tart Runner Fleet. The local versioned API is the source of
truth. SQLite, log scraping, process presence, and an unavailable API are not
substitutes for a coherent snapshot.

## Establish the installed identity

```sh
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
STATE="$ROOT/state"
FLEETCTL="$ROOT/current/fleetctl"
ENDPOINT="unix://$STATE/fleetd.sock"

test -x "$FLEETCTL"
readlink "$ROOT/current"
cat "$STATE/installed-generation.json"
```

`current` is atomically updated only after a candidate becomes ready. It is a
convenience path for operators and agents; launchd remains pinned to an exact
immutable release. The link target and `releaseDir` in
`installed-generation.json` must agree. Never rewrite either by hand.

## Capture one coherent snapshot

```sh
"$FLEETCTL" status --endpoint "$ENDPOINT" --require-ready --output json
"$FLEETCTL" doctor --endpoint "$ENDPOINT" --output json
"$FLEETCTL" queues --endpoint "$ENDPOINT" --output json
"$FLEETCTL" instances --endpoint "$ENDPOINT" --output json
"$FLEETCTL" operations --endpoint "$ENDPOINT" --output json
"$FLEETCTL" observations --endpoint "$ENDPOINT" --output json
```

A command exit status 0 is healthy. Exit 4 means unavailable. Exit 5 means a coherent but
degraded snapshot. Exit 4 or 5 never means zero demand and never authorizes VM
cleanup. Preserve stdout as JSON and diagnostics as stderr.

## Monitor active work

During a rollout, incident, or real-load proof, sample every two minutes and
report only transitions or anomalies. Compare the current snapshot with the
previous snapshot rather than emitting repeated healthy messages. Track:

- controller version, mode, readiness, and observation freshness;
- queued jobs by profile and age, plus admission reasons;
- instances by lifecycle state and owned Tart identity;
- retrying or dead operations and their bounded, redacted error class;
- CPU, memory, swap, disk floor, and Linux/macOS exclusivity;
- updater generation, last exit, and whether cleanup reached zero.

Use `tart list`, GitHub runner/job state, launchd, and bounded logs only to
cross-check a specific anomaly. Never infer idle from a failed GitHub query or
an unavailable daemon.

## Understand the updater

The updater runs once every five minutes and exits. `launchctl` showing the
updater as `not running` is expected when the last exit status is 0 and its
program path belongs to the committed generation. The updater waits for a
quiescent fleet, verifies release identity and checksums, validates config,
uses bounded launchd bootstrap recovery, and gives production activation a
five-minute readiness budget. It atomically commits or restores the complete
generation.

Treat the updater as current only when `installed-generation.json`, the updater
plist, and `launchctl print`'s loaded `program` all name the same immutable
release. Last exit 0 is necessary but not sufficient if launchd still caches an
older executable. A committed update reloads the updater job so this parity is
automatic. Reload is delegated to the separate
`com.vitalyiegorov.tart-runner-fleet.updater-handoff` one-shot; the updater must
never boot out itself. The handoff waits for a cleared transaction, retries
failures, and verifies the exact loaded program before succeeding.

```sh
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater-handoff
tail -n 100 "$STATE/update.stdout.log"
tail -n 100 "$STATE/update.stderr.log"
```

Do not force an update through busy, checksum, mode, or readiness failures.

## Handle a reproducible defect

1. Preserve the coherent JSON, relevant runner/job state, bounded logs, and
   Tart inventory. Do not expose credentials or JIT configuration.
2. Write one deterministic regression test and record it failing.
3. Implement the smallest fix in the same focused PR.
4. Require at least 99% exact coverage plus race, lint, vet, CPD, deadcode,
   vulnerability, reproducible-build, and Required CI gates.
5. Merge only the exact fully-green head. Verify trusted `main`, production
   release notes, binaries, deterministic archive, CycloneDX SBOMs, and
   SHA256SUMS.
6. Let the updater install while idle, then replay representative work and
   confirm drain, deregistration, VM deletion, and host cleanup.

Do not delete ambiguous VMs, edit durable state, stop authority, or weaken a
safety gate merely to clear a queue. Promotion and rollback remain explicit
operator actions described in [`OPERATIONS.md`](OPERATIONS.md).
