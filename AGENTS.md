# Contributor contract

This controller can terminate VMs and affect every CI queue on the host. Treat
all changes as safety critical.

1. Write a failing test before production code. Production incidents begin as
   replay fixtures.
2. For every reproducible CI or fleet incident, open one focused fix PR and
   develop it test-first: prove the regression red, implement the fix in that
   same PR, and hand it off only when every required check is green. Create a
   concise issue when the operator requests one or tracking adds value, but do
   not block a safety repair on issue publication. A red-only regression PR is
   not a completed fix unless progress is genuinely blocked and the blocker is
   explicit.
3. Keep policy pure and deterministic. Time, I/O, randomness, and process
   execution enter through interfaces.
4. Never represent an unavailable observation as an empty collection.
5. Never perform a destructive action without fresh ownership, runner, job,
   Tart, and host confirmation as applicable.
6. Preserve at-least-once delivery and idempotent effects. Commit work before
   acknowledging Scale Set messages.
7. Never log or persist JIT configuration, tokens, private keys, or generated
   runner credentials.
8. Never assemble shell commands from external values. Use argument vectors and
   context deadlines.
9. Run `make ci`; lint, CPD, deadcode, vulnerabilities, coverage, race, and build gates must pass.
10. Do not enable authority mode in a code change. Promotion is an explicit
   operational action after observe/shadow/canary evidence and rollback proof.

## Repository map

- `cmd/fleet`: the single control-plane executable; subcommand dispatch only.
- `internal/daemon`: process lifecycle and dependency wiring only.
- `internal/cli`: operator interface; never imports the SQLite store (enforced by
  a `depguard` rule, see ADR 0019).
- `internal/domain`: immutable domain values and lifecycle rules.
- `internal/scheduler`: pure deterministic policy.
- `internal/adminapi`: versioned read-only DTOs, Unix socket, and bounded client.
- `internal/executor`: the ports a node's execution technology and host probe
  implement; no layer above them may name a backend adapter (ADR 0034).
- `internal/adapters`: GitHub, SQLite, Tart, container, and host-probe
  implementations. `macos` and `linux` are the two host probes; `noexecutor` is
  the backend of a node that has no execution technology yet (ADR 0034).
- `internal/hostpaths`: where a node keeps its releases, state, and service
  definitions, per platform. The one answer the daemon, the CLI, and the
  renderers all read.
- `internal/operations`: durable operations, leases, retries, and workers.
- `internal/discharge`: the one guarded operator mutation and its ordering rules.
- `internal/telemetry`: coherent status, health, readiness, and metrics.
- `tests/{contract,integration,replay,chaos}`: cross-package safety evidence.
- `tests/simulation`: deterministic simulation of the whole fleet (ADR 0031).
- `docs/adr`: decisions that must be updated when architecture changes.

## Agent inspection recipe

Prefer the versioned JSON interface; do not scrape human tables or open the
database while the daemon is running.

```sh
# macOS; on a Linux node the root is $XDG_DATA_HOME/tart-runner-fleet
# (default ~/.local/share/tart-runner-fleet) and everything below is identical.
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
FLEET="$ROOT/current/fleet"
ENDPOINT="unix://$ROOT/state/fleetd.sock"
"$FLEET" status --endpoint "$ENDPOINT" --output json
"$FLEET" doctor --endpoint "$ENDPOINT" --output json
"$FLEET" queues --endpoint "$ENDPOINT" --output json
"$FLEET" instances --endpoint "$ENDPOINT" --output json
"$FLEET" operations --endpoint "$ENDPOINT" --output json
```

Interpret exit `4` as unavailable and exit `5` as coherent but degraded. Never
turn either condition into an empty queue or an authorization to clean runners.
The full copy-paste monitoring and incident procedure is in
[`docs/AGENT_RUNBOOK.md`](docs/AGENT_RUNBOOK.md).

## Before editing

1. Read the relevant ADR and package tests.
2. Convert every incident or missing behavior into a failing deterministic test.
3. Keep policy changes out of adapters and CLI rendering.
4. Preserve JSON field names and enum values in `fleet.v1`.
5. Never expose generic SQL, arbitrary process execution, or raw Tart/GitHub
   passthrough commands through `fleet`.

## The three questions (owner directive, 2026-08-17)

Before finishing any fix or improvement, answer all three explicitly in the PR:

1. **Can I remove overengineering?** Prefer deleting a mechanism to adding one.
   A rule stated once and enforced by a named predicate beats a rule re-derived
   at every call site; a change that makes the system smaller is presumed better
   than one that makes it larger until shown otherwise.
2. **Can I reduce complexity?** If the diff adds a new pass, a new state, a new
   knob, or a new axis, first show why the existing ones cannot express the
   behavior. Repo history is explicit warning: four scheduler defects came from
   one seam re-implementing feasibility one axis at a time instead of stating
   one rule.
3. **Can I test this?** A change the deterministic simulation cannot exercise is
   a change the fleet cannot trust. If the harness cannot yet generate the
   triggering state, extending the harness is part of the fix, not optional —
   every incident in this repo that reached production did so through a blind
   spot in the generator, and one "green no-op" shipped only because a clock
   bug made a verdict unfirable. When a finding is refuted, label which side
   was wrong precisely: property/oracle defect (the invariant judged wrongly),
   world-model defect (the simulation reached an impossible state), or fleet
   defect (production is wrong). Misattribution has already blinded the harness
   to a live production wedge once (#216/#220/#226).

Every unit of work is tracked: a GitHub issue states the problem and carries
progress comments (the durable-progress convention — one commit per validated
chunk, so any agent can resume from issues + commits after a session limit);
an ADR records every decision that changes a rule, including decisions to
retire or NOT to build something; docs (`README`, `USAGE`, `docs/`, this file)
are updated in the same PR, never deferred.

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

- No direct modification of `fleet.db` or manual deletion of state rows. A parked
  dead letter is discharged through `fleet operations discharge`, never by hand.
- No VM deletion without fresh ownership, runner, job, Tart, and host evidence.
- No printing environment variables, Keychain values, JIT configuration, or
  operation payloads.
- No weakening deadlines, coverage, race tests, socket permissions, or
  fail-closed observations to make a check pass.
- No authority/canary enablement, launchd or systemd unit installation, or
  incumbent shutdown without an explicit operational promotion task.
