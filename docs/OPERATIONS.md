# Operations

## Promotion contract

`fleet` supports `observe`, `shadow`, `canary`, and `authority`. Canary and
authority acquire a renewable singleton lease, recover expired outbox leases,
and stop on lease loss. Canary additionally requires exact scope/profile flags
and runs at most one lifecycle operation concurrently. Promotion is never
implicit: keep the incumbent installed until a real queued job has completed,
deregistered, stopped, and deleted through the Go controller.

## Self-hosting and bootstrap

For a clean-machine procedure, verified release extraction, first LaunchAgent
installation, updater adoption, and the post-reboot proof, start with
[`INSTALL.md`](../INSTALL.md). For the daily operator cockpit and incident
workflow, use [`USAGE.md`](../USAGE.md).

`vitalyiegorov/tart-runner-fleet` is a first-class target with three concurrent
Linux slots. Its preflight, quality, coverage, race, reproducible-build, release,
and nightly jobs therefore run on ephemeral VMs scheduled by this fleet. There
are no GitHub-hosted fallback runners.

Self-hosting uses a two-generation rule to avoid circular upgrades:

1. Generation N remains installed under launchd and owns scheduling while
   generation N+1 is proposed.
2. Generation N provisions the ephemeral Linux runners that execute N+1's
   complete Required CI and reproducible release build.
3. N+1 is installed as a versioned, immutable candidate outside its source
   checkout. Canonical observe may run beside N because it uses only the
   read-only Actions REST API and opens no scale-set message session.
4. After N stops consuming the affected scale-set sessions, run N+1 shadow
   exclusively. A disposable scale-set canary and rollback drill must pass before an atomic
   launchd authority handoff. Never stop N merely because N+1 was downloaded.
5. Keep the previous binary, launchd plist, configuration, and SQLite files until
   the new generation has survived its soak window. Rollback atomically restores
   that pinned generation; it does not depend on GitHub Actions being available.

If all dynamic runners are unavailable, recovery is deliberately local: restore
the pinned incumbent launchd unit, run `fleet doctor`, and verify one disposable
Linux canary before releasing queued work. A workflow cannot repair a stopped
runner controller because that would recreate the same dependency cycle.

## Observe

Observe validates configuration, migrates/quick-checks the private SQLite WAL,
reconciles durable instance metadata with Tart and host pressure, computes
fail-closed plans, and exposes local health. With
`github.canonicalJobInventory: true`, it also polls the read-only Actions REST
API, persists canonical queued-job evidence, and makes every configured scope's
REST freshness readiness-critical. It opens no official scale-set message
session, acquires no authority lease, and performs no GitHub/Tart mutation, so a
canonical observe candidate may safely run beside the incumbent.

Host admission uses two layers. Exact profile vectors enforce CPU, memory, and
slot envelopes first. A fresh host probe then preserves `minFreeDiskGb` and
`minAvailableMemoryMb`, treats swap as pressured only when swap used exceeds
`maxSwapUsedMb` **and** the host is measurably paging out, and treats CPU as
pressured only when both `maxLoadAverage` is exceeded and idle CPU falls below
`minCpuIdlePercent`. Missing values decode to the safe defaults in
`config/fleet.example.json`. Inspect the evidence through `fleet status` or
the bounded `fleet_host_*` metrics; do not infer pressure from queue age alone.

Both of those guards are conjunctions, and the swap one surprises operators
because the level is the visible half. macOS does not eagerly reclaim swap, so
swap used is closer to a high-water mark than to current pressure: a host can
sit far above its ceiling for days after the burst that filled it, while paging
nothing. ADR 0018 therefore demoted the level to a necessary but insufficient
condition and made the page-out rate — the swapout delta between consecutive
observations — the deciding signal. **Swap used above `maxSwapUsedMb` with
admission allowed is the designed behaviour of a host carrying residue, not an
ignored guardrail.** `fleet status` prints the rate beside the level as
`swap 13593 MiB (paging 0/s)`, and `fleet_host_swapout_rate_pages_per_second`
carries the same figure.

A rate needs two samples. When it cannot be established honestly — no prior
observation, a non-advancing clock, or a counter reset by a reboot — it is
reported as unmeasured (`paging unmeasured`,
`fleet_host_swapout_rate_observed 0`) and the level blocks on its own. An
unmeasured rate is never read as a quiet host.

One consequence is worth checking before promoting a node: on a host whose
steady-state residue sits far above `maxSwapUsedMb`, the level clause is
permanently true and the guard degenerates into "did this host page out at all
since the last tick", which flaps admission on an interactive machine. Size
`maxSwapUsedMb` against the host you are deploying on rather than inheriting it
from another node, so the conjunction keeps both of its halves.

Pressure stops new admission, never an active job. Owned ephemeral runners
still follow durable drain, deregistration, stop, and delete operations with
indefinite bounded cleanup retries. Bases and unknown VMs are never pressure-
deleted. Recover disk by resolving visible owned cleanup failures, not by
blindly pruning Tart state.

```sh
fleet run \
  --mode observe \
  --config "$HOME/Library/Application Support/tart-runner-fleet/fleet.json" \
  --database "$HOME/Library/Application Support/tart-runner-fleet/fleet.db" \
  --health-address 127.0.0.1:9876
```

### Scheduling-class configuration migration

Releases that predate `schedulingClass` use strict JSON decoding and reject the
new field. Upgrade without weakening rollback:

1. keep the running generation and its configuration unchanged;
2. install and independently verify the new immutable release candidate;
3. copy the current configuration to a versioned candidate file and set only
   the controller target to `"schedulingClass": "control-plane"`;
4. validate that file with the new candidate's `fleet config validate`;
5. start or restart only the observe candidate with the versioned file;
6. retain the previous binary and previous configuration as one rollback unit.

Do not add the field to a configuration that an older launchd generation may
need to parse, and do not treat this migration as authority promotion.

## Shadow

Shadow additionally opens one official GitHub Actions Scale Set message session
per configured scope/profile. It commits sanitized job events and the scoped
message cursor atomically
before acknowledgement, computes plans, and writes effects to neither GitHub nor
Tart. Do not point another controller at the same scale-set sessions.

GitHub App metadata belongs in JSON. Prefer a Keychain generic-password item
when its access control permits unattended launchd reads. Create that item
through Keychain Access so the PEM never appears in shell history or a process
argument. Use service `tart-runner-fleet-github-app` and account `controller`
(or configure different names), paste the PEM as the item password, then delete
the source file safely.

If macOS Keychain access requires an interactive prompt in the launchd session,
configure `github.app.privateKeyFile` instead. The file must be a regular,
non-symlink file owned by the launchd user with exact mode `0600`; the loader
opens it with `O_NOFOLLOW`, bounds its size, and never logs or serializes its
contents. Keep it below the fleet state directory's mode-`0700` parent. Example:

```json
{
  "github": {
    "app": {
      "clientId": "Iv23...",
      "privateKeyFile": "/Users/runner/Library/Application Support/tart-runner-fleet/secrets/github-app.pem"
    }
  }
}
```

When `privateKeyFile` and Keychain metadata are both present, the file is the
authoritative source. This deterministic precedence prevents stale Keychain
metadata from reopening an interactive prompt. Removing `privateKeyFile`
restores the existing Keychain loader unchanged.

The multi-scope `github` configuration contains one non-secret App client ID,
one private-key credential reference, named installation IDs, and registration
scopes. Use a repository scope for each personal repository and an organization
scope for organization repositories. Every target belongs to exactly one scope;
every scope lists one scale set for each profile variant *it chooses to expose*,
and at least one. A variant a scope omits simply has no runner there, so list
the shapes that scope's workflows actually request
([ADR 0032](adr/0032-resource-explicit-runner-labels.md)). Numeric scale-set
IDs may collide across installations because durable state uses a scoped key.

Grant every configured installation read-only **Actions** repository
permission. The canonical queue observer lists active workflow runs and jobs;
it does not require repository Administration permission and does not fetch the
runner inventory used by lifecycle authority. A missing permission returns 403,
keeps the last durable queue snapshot, and prevents candidate readiness.

The observer is activated only by `github.canonicalJobInventory: true`. With
the field omitted or false, the release remains backward compatible with the
existing official scale-set stream and delivery lookahead. With the field
true, REST freshness becomes readiness-critical and configuration validation
requires truthful capacity.

Set every scale set's `maxCapacity` to its truthful executable maximum for the
scope: the smaller of the profile/host capacity and the sum of the scope's
repository `maxActive` limits. Under the default 8-CPU/16-GiB envelope and one
repository capped at four, that is `4` for `trf-linux-arm64-1x2`, `4` for
`trf-linux-arm64-2x4`, `2` for `trf-linux-arm64-4x8`, `1` for
`trf-linux-arm64-8x16`, `1` for `trf-macos-arm64-6x12`, and `2` for
`trf-macos-arm64-4x7`. Do not add a delivery-only slot.

A scope carries a scale set only for the variants it lists, so this arithmetic
is done per variant per scope that exposes it, not for the whole matrix in every
scope ([ADR 0032](adr/0032-resource-explicit-runner-labels.md)).

When another node owns a scale set advertising the **same labels in the same
scope**, set `sharedLabels: true` on this node's scale set. GitHub places the
work between the two sets by the capacity each last advertised, but it does not
tell either node about the other, and the REST observer polls the whole scope:
without the declaration both nodes attribute every queued job in that scope to
themselves and each spawns a guest for it
([ADR 0015](adr/0015-canonical-inventory-and-truthful-capacity.md)'s shared-label
amendment). With it, this node claims only the share GitHub gave it, and
`fleet status` reports that share rather than the scope's whole backlog — the
peer reports the rest. The field requires `canonicalJobInventory: true` and
truthful capacity, and configuration validation rejects it without them.

For an existing authority that still uses inflated lookahead, first grant the
read-only App permission and validate a versioned candidate with
`canonicalJobInventory: true` and truthful capacity using the candidate
`fleet`. Prove complete queue visibility in canonical observe while the
incumbent remains authority. Run shadow only after the incumbent no longer
consumes the same scale-set sessions, then run the exact-scope canary. Change
live scale-set capacity only while it owns no active job. Keep the running
authority configuration unchanged until those gates pass.

Provisioning is explicit, drift-failing, and plan-first:

```sh
fleet config validate fleet.json
fleet scale-sets provision --config fleet.json
fleet scale-sets provision --config fleet.json --apply --write \
  --confirm provision-scale-sets \
  --reason "initial GitHub Actions scale-set bootstrap"
```

The command inspects every scope before creating anything, reuses only an exact
name/labels/group match, rejects drift, and atomically writes returned IDs with
mode `0600`. It never prints the App key or JIT material.

## Immutable guest bases

Each Linux and macOS base must contain an unpacked GitHub Actions runner at
`$HOME/actions-runner/run.sh` and the matching released helper installed as
`/usr/local/libexec/tart-runner-fleet-bootstrap`. The host sends the bounded JIT
configuration over `tart exec -i` standard input; it never appears in argv,
logs, SQLite, or the parent environment. Build new `*-go` bases and retain the
incumbent bases unchanged for rollback.

Treat the helper as part of the immutable base contract. Before promoting a
controller release, update each stopped base from that release's platform
artifact, require root ownership and mode `0755`, boot it only for a helper
version/smoke check, then stop it and retain the preceding base. Never patch an
active job VM. The helper keeps a detached supervisor around the ephemeral
listener and requests `sudo -n shutdown -h now` after it exits; verify the base
grants that exact non-interactive operation. A real canary must prove the full
chain: runner exits, guest powers off, fresh inventory observes stopped, and
the durable drain/deregister/delete operation completes before the base is
selected for production.

### The guest capability manifest, and the two ways it fails

A base image also carries its own account of what it provides, sealed at
`/usr/local/share/tart-runner-fleet/image-capabilities.json`:

```json
{"schemaVersion": 1, "image": "macos-tartelet-base", "sealedAt": "2026-08-05T04:00:00Z",
 "capabilities": ["android-build-sdk", "ios-simulator-prewarmed", "jvm", "maestro-cli", "node-runtime"]}
```

The daemon passes the capabilities the assigned scale sets require to the
bootstrap helper, which compares them against that file before it reads the JIT
configuration. A scale set that requires nothing produces exactly the invocation
it always did and reads no manifest. This is detection, not prevention: by the
time it fires, GitHub has already assigned the job. The prevention is the
configuration gate — see `fleet config validate` in
[`CLI.md`](CLI.md) — and this catches the one thing that gate cannot, which is a
declaration that was true until someone rebuilt the image.

| `code` | Meaning | Action |
| --- | --- | --- |
| `bootstrap:guest_capability_missing` | The image answered, and it does not provide something a scale set requires. | Rebuild the base through the seal step of [`BASE_IMAGE.md`](BASE_IMAGE.md) or [`LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md), or stop advertising the label. The guest names the missing capability on the helper's standard error inside the guest; the scale set's own `requiresCapabilities` names the candidate set. |
| `bootstrap:guest_capability_unverifiable` | The image could not answer: the manifest is absent, unparsable, empty, or a schema this release does not read. | Re-seal the image with a manifest. An image that cannot answer is failed closed on purpose — treating silence as consent would catch nothing. |

### macOS VM quota exhaustion (`host_quota`)

Apple's kernel caps concurrent macOS guests at 2 per host and, on macOS 26,
sometimes fails to release a slot after a clean `tart stop`
(cirruslabs/tart#1217, #967). The tart adapter reports this as error kind
`host_quota` ("exceeds the system limit") and fails closed instead of
retrying. Operator response: verify no macOS VM is actually running
(`tart list`), then reboot the host — a reboot is the only known way to
clear a leaked quota slot. Do not raise `maxActive` or retry admission
until the reboot completes.

## Real canary and authority handoff

Every release carries four immutable launchd templates plus
`render-launchd.sh`. Render them into a generation-specific directory; never
edit a plist in place. The observe template remains the default and uses the
existing `com.vitalyiegorov.tart-runner-fleet` label. Shadow and canary use
separate databases, sockets, health ports, logs, and launchd labels:

### Persistent automatic production updates

After the first authority handoff is healthy and its rendered authority plist
has been atomically installed at
`$HOME/Library/LaunchAgents/com.vitalyiegorov.tart-runner-fleet.plist`, adopt it
once:

```sh
fleet update adopt \
  --release-dir "$RELEASE_DIR" \
  --mode authority \
  --confirm adopt-current-generation
```

Adoption refuses a mismatched plist, mode, version, config, checksum, or
unready daemon. It installs
`com.vitalyiegorov.tart-runner-fleet.updater.plist`, which checks the latest
normal production release every five minutes. Each update verifies the external
archive checksum and every executed artifact, validates the existing config
with the candidate, and waits until queues, VMs, and operations are all empty.
It then atomically replaces the persistent daemon and updater plists, requires
the exact new version and the same controller mode to become ready, and restores
the previous generation on any failure. Updates are strictly forward-only.
Both generated plists use `RunAtLoad`; the daemon additionally uses
restart-on-failure `KeepAlive`, while the updater uses a five-minute
`StartInterval`. Every committed update rewrites the updater to the new immutable
`fleet`. If the updater is already loaded, a distinct retrying handoff job
waits for the durable commit, unloads the old updater, bootstraps the rewritten
plist, and verifies launchd loaded the exact committed executable. The updater
never boots out itself. The plist on disk and launchd's cached
`ProgramArguments` therefore advance together, and a later timer invocation,
login, or reboot cannot regress to an older updater.

Activation and rollback use bounded launchd bootstrap recovery for the brief
unload-to-load transition. Production allows a five-minute readiness budget so
all configured Scale Set sessions can start sequentially on a busy control
plane without accepting an unready generation. A successful commit also
atomically points `$ROOT/current` at the immutable release; LaunchAgents never
execute through that convenience link.

The updater itself is a periodic one-shot process. It is expected to be `not running`
between invocations. Treat that state as healthy only when launchd
reports exit status 0, the program path is the committed immutable `fleet`,
and bounded updater logs contain no newer error. Process absence alone is not a
failure and process presence alone is not readiness.

After every commit, compare all three generation identities: the installed
manifest, the updater plist on disk, and `launchctl print`'s loaded `program`.
Exit status 0 does not make a stale loaded program healthy. A one-time migration
from a release that predates updater-job reload may require a controlled
`bootout`/`bootstrap` after verifying the new plist; subsequent updates must
maintain parity automatically.

If a legacy updater terminated itself after writing a newer plist, leave the
transaction journal and rollback files intact. First require a quiescent fleet,
download `fleet` and `SHA256SUMS` from the latest normal production release,
and verify the binary against that release. Run the verified binary as an
independent operator process with `update apply-latest`; because the updater is
not loaded, the guarded commit can bootstrap the corrected updater without
terminating its caller. Never delete the journal or edit the installed manifest
to force convergence. The update must either publish one complete generation
and remove the journal itself, or roll back to the recorded generation.

During an update, a short-lived
`com.vitalyiegorov.tart-runner-fleet.updater-handoff` job is expected. It fails
closed while the update journal exists, retries replacement failures, and is
healthy only when the normal updater's loaded program matches the installed
generation. Do not delete it to hide a parity failure.

To run the same idempotent check manually:

```sh
fleet update apply-latest \
  --mode authority \
  --confirm automatic-release-update
```

To atomically roll out a configuration-only experiment on the installed
release, write and validate a separate absolute config path, then pass it to the
same guarded command. The updater refuses until every queue, VM, retry, and dead
operation is empty, and rollback restores the prior config/plist tuple if the
daemon does not return ready:

```sh
fleet update apply-latest \
  --mode authority \
  --config /absolute/path/to/fleet-experiment.json \
  --confirm automatic-release-update
```

Never overwrite the active config in place for an experiment. Config-only
activation requires the exact installed version, release directory, mode, and
endpoint; it cannot substitute a same-version binary.

After every install or reboot, require both LaunchAgents and exact daemon
readiness rather than treating process presence as health:

```sh
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
fleet status --require-ready --output json
fleet doctor --output json
```

### The Linux node's release bridge

Everything above is a `launchd` transaction: it lints a plist with `plutil` and
swaps generations with `launchctl bootout` / `bootstrap` / `kickstart`. A Linux
node (geekom, ADR 0034's node B — not yet delivered) has none of those, and
`fleet update apply-latest` refuses there rather than half-applying a
generation — the refusal names the domain, because a `systemd --user` manager
is not addressable as a launchd one. Automatic updates on that node arrive
with the systemd release transaction; its units are already rendered by
`render-systemd.sh` so the node will not need a hand-written file when it
does.

Until then a Linux node adopts a generation by hand, and the ordering rule is
the same one the macOS bridge has: the unit and the recorded generation move
together, and the result is *verified* rather than assumed.

```sh
ROOT="${XDG_DATA_HOME:-$HOME/.local/share}/tart-runner-fleet"
UNITS_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
"$RELEASE_DIR/render-systemd.sh" observe "$RELEASE_DIR" "$ROOT/state" "$ROOT/systemd/$VERSION"
install -m 0600 "$ROOT/systemd/$VERSION/tart-runner-fleet.service" \
  "$UNITS_DIR/tart-runner-fleet.service"
ln -sfn "$RELEASE_DIR" "$ROOT/current.new" && mv -Tf "$ROOT/current.new" "$ROOT/current"
systemctl --user daemon-reload
systemctl --user restart tart-runner-fleet.service
systemctl --user status tart-runner-fleet.service
"$RELEASE_DIR/fleet" status --require-ready --output json \
  --endpoint "unix://$ROOT/state/fleetd.sock"
```

`systemctl --user restart` reports success as soon as the process starts, which
is the same trap `launchctl bootstrap` sets: only `status --require-ready`
proves the exact new version became ready. If it does not, re-render from the
previous release directory and restart again. The full sequence, with the
download and verification steps, is in
[`INSTALL-linux.md`](../INSTALL-linux.md).

```sh
set -eu
RELEASE_DIR="$HOME/Library/Application Support/tart-runner-fleet/releases/$VERSION"
STATE_DIR="$HOME/Library/Application Support/tart-runner-fleet/state"
PLIST_DIR="$HOME/Library/Application Support/tart-runner-fleet/launchd/$VERSION"
mkdir -p "$PLIST_DIR"
cd "$RELEASE_DIR"
./render-launchd.sh observe "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/observe.plist"
./render-launchd.sh shadow "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/shadow.plist"
./render-launchd.sh canary "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/canary.plist" fleet-repo linux-1x2
./render-launchd.sh authority "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/authority.plist"
plutil -lint "$PLIST_DIR"/*.plist
```

Bootstrap shadow first. After its evidence is coherent, boot it out before
bootstrapping canary; only one official scale-set message consumer may own a
scope/profile at a time:

```sh
launchctl bootstrap gui/"$(id -u)" "$PLIST_DIR/shadow.plist"
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.shadow
launchctl bootout gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.shadow
launchctl bootstrap gui/"$(id -u)" "$PLIST_DIR/canary.plist"
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.canary
```

Dispatch `.github/workflows/fleet-canary.yml`. Canary ingestion requires the
dedicated `tart-fleet-canary` job label in addition to the exact scope/profile,
so an ordinary `trf-linux-arm64-1x2` job in the same repository cannot be mutated.
Require the complete sequence:
queued demand, owned Tart clone, readiness, JIT registration, job success,
fresh completed-job guard, deregistration, stop, deletion, and zero owned VMs.
Then remove canary and perform the fail-safe authority handoff below. The
authority plist is already fully rendered and linted; the block restores the
incumbent immediately if launchd cannot bootstrap Go authority:

```sh
set -eu
AUTHORITY_PLIST="$PLIST_DIR/authority.plist"
INCUMBENT_PLIST="$HOME/Library/LaunchAgents/com.github.linux-burst-manager.plist"
launchctl bootout gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.canary
launchctl bootout gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet 2>/dev/null || true
launchctl bootout gui/"$(id -u)"/com.github.linux-burst-manager
if ! launchctl bootstrap gui/"$(id -u)" "$AUTHORITY_PLIST"; then
  launchctl bootstrap gui/"$(id -u)" "$INCUMBENT_PLIST"
  launchctl kickstart -k gui/"$(id -u)"/com.github.linux-burst-manager
  exit 1
fi
launchctl kickstart -k gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
```

Watch one Linux plus one macOS job before releasing normal load. The exact
rollback is local and does not depend on GitHub Actions:

```sh
set -eu
AUTHORITY_PLIST="$PLIST_DIR/authority.plist"
INCUMBENT_PLIST="$HOME/Library/LaunchAgents/com.github.linux-burst-manager.plist"
launchctl bootout gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
launchctl bootstrap gui/"$(id -u)" "$INCUMBENT_PLIST"
launchctl kickstart -k gui/"$(id -u)"/com.github.linux-burst-manager
```

After rollback, drain only Go-owned VMs and retain the failed authority plist,
database, and logs as one immutable incident bundle. Do not delete or rewrite
incumbent state.

## Dead letters

### Discharging a dead-lettered cleanup

A dead letter is an operation that has stopped retrying and is parked awaiting an
operator. It is not in flight: it holds no lease, `Claim` cannot select it, and no
tick will advance it. `fleet operations` names each one.

```sh
ROOT="$HOME/Library/Application Support/tart-runner-fleet"
FLEET="$ROOT/current/fleet"
ENDPOINT="unix://$ROOT/state/fleetd.sock"
"$FLEET" operations --endpoint "$ENDPOINT" --output json |
  jq '{retrying, dead, deadLetters}'
```

```json
{
  "retrying": 0,
  "dead": 1,
  "deadLetters": [
    {
      "operationId": "op-ea9b705d234ad29f14e79b6d",
      "kind": "deregister",
      "code": "deregister:runner_busy",
      "resourceId": "trf-maestro-096ffcb3a52d8624",
      "attempts": 835,
      "parked": true
    }
  ]
}
```

`parked` is the fleet's own judgement that nothing will advance this resource
without you: no operation for it is pending or claimed, and its owned VM is
observed **stopped**. `fleet_operations_parked` publishes the same count for
alerting. A dead letter with `parked: false` needs no action yet — either work is
still progressing, or the fleet cannot currently see the VM's power state, and in
both cases a release update keeps deferring as it should.

**Before discharging, establish that the cleanup genuinely cannot complete.**
Discharging records that a human accepted an effect the fleet will never perform;
it is not a way to skip a slow retry. Check the `code` against the case studies
below, confirm no workflow run owns the runner, and confirm the VM is not running.

Discharge the operation only:

```sh
"$FLEET" operations discharge --endpoint "$ENDPOINT" \
  --operation op-ea9b705d234ad29f14e79b6d \
  --instance trf-maestro-096ffcb3a52d8624 \
  --confirm discharge-dead-letter \
  --reason "GitHub 422 runner_busy: permanent registration leak"
```

Discharge it and retire the phantom instance row and its stopped VM:

```sh
"$FLEET" operations discharge --endpoint "$ENDPOINT" \
  --operation op-ea9b705d234ad29f14e79b6d \
  --instance trf-maestro-096ffcb3a52d8624 --reap-instance \
  --confirm discharge-dead-letter \
  --reason "GitHub 422 runner_busy: permanent registration leak"
```

Both forms require the exact `--confirm discharge-dead-letter` token and a
non-empty `--reason`, which is written to the daemon's audit log with both
identities and the applied effects. The daemon re-checks every guard itself, so a
direct socket caller is held to the same bar, and the mutation is refused outright
unless the controller runs in authority mode.

#### Ordering: the row goes first, the VM second

`--reap-instance` retires the durable row **before** deleting the VM, and that
order is load-bearing. A *stopped* VM belonging to a live instance row is
load-bearing state:

- **VM deleted first, row still live** → for a row outside the cleanup states
  (`draining`, `deregistering`, `stopping`), `internal/app/inventory.go` turns the
  ENTIRE instance observation `Unavailable` with `owned VM <id> missing from
  Tart`, which blocks planning for the whole host. It is unrepairable by
  observation, because the VM that would have proved anything is gone. A cleanup
  row degrades to `absent` power per instance instead of blocking the host (ADR
  0022), but there is still no reason to create that state by hand.
- **Row retired first, VM still present** → the observation is also blocked, with
  `untracked controller VM requires reconciliation`, but that state is trivially
  repairable: the VM still exists, and re-running the same discharge command
  removes it.

So never delete a controller VM by hand while its row is live. If the command
reports `discharge refused: vm_delete_failed`, the durable half already applied —
re-run the identical command until `vm deleted true`. The mutation is idempotent:
a repeat reports `false` for the steps that were already done.

#### Refusal codes

| Code | Exit | Meaning |
| --- | --- | --- |
| `unknown_operation` | 3 | No operation with that ID. Re-read `fleet operations`. |
| `resource_mismatch` | 6 | The operation does not belong to the named instance. |
| `operation_not_dead` | 6 | The operation is still pending, claimed, or already completed. Nothing to discharge. |
| `resource_not_parked` | 6 | Another operation for the same resource is pending or claimed; let it finish. |
| `instance_not_reapable` | 6 | The row is not in a cleanup or terminal state. Retiring it would abandon a live runner. |
| `vm_running` | 6 | Fresh Tart evidence shows the guest is running. `Reap` never stops a VM. |
| `vm_state_unknown` | 6 | Tart could not be read. Fail closed and retry. |
| `not_authority` | 6 | The controller is not in authority mode. |
| `unconfirmed` / `reason_required` | 6 | The confirmation token or reason is missing. |
| `vm_delete_failed` | 6 | The durable half applied; the VM survives. Re-run the command. |
| `store_unavailable` | 6 | The database could not be read or written. |

#### Why a dead letter no longer defers a release update

Until v0.1.282, `fleet update` treated `dead != 0` and any live instance row as
"busy". A cleanup that can never complete therefore made the fleet permanently
non-quiescent, and the automatic updater logged

```
apply production release: prepare update: autoupdate: fleet is not quiescent
```

every 300s for hours while refusing to install the release that bounded the very
wedge blocking it. Now only *retrying* operations, queued jobs, and instances the
daemon cannot prove parked defer activation. A running VM still defers, always.

### Case study: `deregister:runner_busy` — a permanent GitHub registration leak

On 2026-07-25 a scale-set runner registration in the `budgie-at` organization
reached a contradictory state: `status=offline`, `busy=True`, `labels=[]`. GitHub
refused to remove it:

```
DELETE /orgs/budgie-at/actions/runners/3175
-> HTTP 422 {"message":"Bad request - Runner trf-maestro-096ffcb3a52d8624 is
    currently running a job and cannot be deleted."}
```

Established facts, so nobody re-litigates them during the next incident:

- A privileged token **with `admin:org` fails identically**. This is GitHub-side
  state, not a permissions problem, and escalating the token does not help.
- GitHub's own six-hour maximum job duration elapsed — the runner was over nine
  hours old — without the registration being released.
- **No workflow run held it.** Sixty recent runs' jobs were swept for
  `runner_name` with zero hits, `--status in_progress` was empty, and cancelling
  the two candidate stuck runs did not release it. The documented remedy for a
  busy runner, cancelling the owning run, therefore did not exist for this case.
- Conclusion: this registration can never be deregistered. The fleet is right to
  refuse to invent absence, and right to stop retrying at ADR 0020's ceiling.

The remedy is the discharge above, with `--reap-instance`, recording the 422 in
`--reason`. Do not delete the runner registration expectation from configuration
and do not force the deregister to "succeed": a 401/403 must never be read as
absence, or a permissions regression would release the teardown of instances the
fleet cannot actually deregister.

### Case study: a ghost queued job — demand GitHub advertises but no longer has

On 2026-08-01 a `knee-repo`/`large` job was queued at 18:32:52Z, its PR branch
was force-pushed, and GitHub cancelled the superseded run server-side. No
terminal scale-set message ever arrived, so the session kept advertising one
acquirable job and the fleet kept one `JobAvailable` demand row alive for
**10h35m** while the repository's REST scope held zero non-completed workflow
runs.

Established facts, so nobody re-litigates them during the next incident:

- **The broker never retracts.** `runner_demands` ratchets forward by design, so
  a job that dies without a `JobCompleted` message cannot be removed by protocol
  evidence. `Statistics.TotalAvailableJobs` stayed at 1 all night.
- **A restart does not help.** Observations are rebuilt from the same durable
  row. The 2026-08-01 remedy — `launchctl kickstart -k` after proving no busy
  runner existed anywhere — worked only because the operator supplied the
  absence proof by hand.
- The fleet kept spawning `large` VMs that registered online and never went
  busy, and the release gate refused every 300s tick with
  `apply production release: prepare update: autoupdate: fleet is not quiescent`,
  blocking v0.1.304.

Since v0.1.305 the canonical REST job inventory retracts it (see
[ADR 0026](adr/0026-queued-demand-expires-on-proven-absence.md)). A demand row is
expired only when it was corroborated by REST at least once, has then been
missing from at least three complete scope snapshots, and has been missing for at
least fifteen minutes — and it is restored automatically, with its original queue
age, by any later REST snapshot or broker event that contradicts the expiry. Do
not delete demand rows by hand and do not disable the job inventory to "clear" a
queue: without a complete scope observation the fleet has no absence evidence at
all, and the ghost simply returns.

## Health

- `GET /healthz`: event-loop liveness.
- `GET /readyz`: successful recent tick plus fresh critical observations.
- `GET /metrics`: bounded-cardinality Prometheus text.

The health server rejects non-loopback TCP listeners. Never expose it directly.

The operator API uses a private Unix socket. With the launchd template it is
`__STATE_DIR__/fleetd.sock`:

```sh
fleet status --endpoint unix://__STATE_DIR__/fleetd.sock
fleet doctor --endpoint unix://__STATE_DIR__/fleetd.sock
```

The socket is `0600`, is unlinked on clean shutdown, and only a stale socket
owned by the current user may be replaced. `fleet` never opens `fleet.db`.

### An instance holding its vector too long

`fleet doctor` reports `FAIL  occupancy` when an instance has held its profile's
resource vector past that profile's configured ceiling **while queued work that
would fit that vector waits**. Either half alone is not a fault: a long job is
allowed to be long, and a deep queue is allowed to be deep. Together they are the
2026-08-09 incident, where one 6 vCPU / 12 GiB instance held sixty percent of the
host for seventy-five minutes on a job that had already failed, with five jobs
queued behind it and four cores idle (ADR 0036).

```sh
fleet status --endpoint "unix://$ROOT/state/fleetd.sock" --output json |
  jq '.data.occupancy | map(select(.overBudget))'
```

`STATE` in the `OCCUPANCY` table of `fleet status` is the whole judgement:

| `STATE` | Meaning | Action |
| --- | --- | --- |
| `ok` | Inside three quarters of the ceiling. | None. |
| `warn` | Past three quarters of the ceiling. | None yet. The daemon has logged one WARN naming the job. |
| `over` | Past the ceiling, but nothing queued fits the held vector. | None. The fleet reclaims it; no other work is waiting on it. |
| `STARVING` | Past the ceiling with queued work that fits behind it. | The fleet is reclaiming it. Check the job named in the WARN before raising the ceiling. |

A `-` in `BUDGET` is a profile with no ceiling configured, not a ceiling of zero.

The reclaim is not silent. The daemon logs one line naming the instance, the
vector, how long it was held, the ceiling, and the repository, run, attempt, and
job it cut; the durable drain operation's payload carries the same job identity.
A reaped job fails on GitHub with a lost-communication error, which is
indistinguishable from a flake without those two records — so if a job failed
that way and neither record names it, it was a flake and not a reap.

If a profile's legitimate work has grown past its ceiling, raise it rather than
removing it. The ceiling is per profile, `occupancyBudgetSeconds`, and must be
`0` (never reaped for age) or between 300 and 21600 seconds; unstated takes the
platform default of two hours on macOS and one hour on Linux.

### A guest that has stopped answering

`fleet doctor` reports `FAIL  guest liveness` when an instance's guest has
refused a whole run of liveness probes: the VM is still enumerated as `running`,
GitHub still reports its job `in_progress`, and nothing inside it is executing
any more.

This is the 2026-08-16 class (ADR 0040, issue #236). A job's `--privileged`
container wrote to the guest's `/proc/sysrq-trigger` and panicked the guest
kernel; with `kernel.panic=0` the panicked kernel hung forever. Eight runners
died that way, each holding 6 vCPU / 12 GiB until GitHub's own grace timer failed
the job **sixteen to eighteen minutes later**, and the fleet produced no artifact
of any kind — no log line, no metric, no doctor finding.

```sh
fleet doctor --endpoint "$ENDPOINT" --output json |
  jq '.checks[] | select(.name == "guest liveness")'
fleet status --endpoint "$ENDPOINT" --output json | jq '.data.guestSilences'
```

| Field | Meaning |
| --- | --- |
| `refusals` / `requiredRefusals` | Consecutive probes refused, against the bound this node requires. |
| `silenceSeconds` / `windowSeconds` | How long the run has lasted, against the bound. |
| `unresponsive` | `true` once BOTH bounds are met: the fleet has declared the guest dead and is reclaiming its vector. |
| `runId` / `jobId` | The job that died with the guest. |

Only a refused **transport** counts. A guest that answers slowly, and a probe
that runs out of its own deadline against a saturated guest, both clear the run —
so a job using every core cannot be probed into a drain. The check fails only on
the verdict; a partial run of refusals is the fleet watching, and is not
actionable.

The reclaim is not silent and not automatic-and-invisible. The daemon logs one
line when a guest goes quiet and another when it is declared dead, each naming
the instance, the vector, the probe timeline, and the job; a third names the
reclaim itself. The drain re-probes once more before acting, and a guest that
answers that probe is returned to `running` untouched.

**If this fires and the guest was healthy**, the bound is wrong for this node, not
the mechanism. Raise `guestLivenessRefusals` / `guestLivenessWindowSeconds`, or
set `guestLivenessRefusals: -1` to stop probing altogether. The floors are three
refusals and thirty seconds; the shipped default is five over ninety seconds.

**If this fires and the guest really is dead**, the job is already lost: the
machine executing it stopped minutes before the fleet said so. The remedy is on
the consumer side — a `--privileged` container can panic the kernel it shares,
and the fleet cannot take that away while granting privileged at all.

**On this fleet the verdict cannot currently fire at all.** `tart` 2.32.1 waits
thirty seconds on the guest control socket before failing, six times the probe's
five-second deadline, so a dead guest classifies `unknown` exactly like a
saturated one and the run of refusals never starts. Measured on a probe VM on the
Mac mini against a saturated guest, a stopped guest agent and a panicked kernel;
see ADR 0042's amendment. Treat `guest liveness` as informational until a probe
deadline above tart's own connect timeout ships. The discrimination itself was
confirmed correct by the same measurement — a saturated guest really is never
mistaken for a dead one.

### A power reading nobody corroborated

The fleet does not reclaim an instance because one `tart list` said its VM was
powered off. A stopped reading must hold for **three readings over forty-five
seconds** before it becomes a fact the fleet acts on; until then it classifies as
unknown, and an unknown power neither authorizes a reclaim nor releases the
instance's vector back into the admission envelope.

This is the 2026-08-18 class (ADR 0042, issue #246). `tart` reports a VM whose
`config.json` it cannot open as `"Running": false` — its `running()` swallows the
error — so a transient failure that says nothing about the machine arrived at the
fleet as a confident claim that the machine was off. One instance was planned for
a destructive drain **eighty-six times in nine minutes**, and a hundred and
fifteen times the night before; every one was caught and aborted by the drain's
own re-read, and none of it was logged.

```sh
# the fleet destroying something, whatever the cause
grep 'instance recovery drain planned' fleet-authority.stderr.log
# the fleet catching itself about to destroy a live runner
grep 'power premise retracted by its own drain' fleet-authority.stderr.log
```

`instance recovery drain planned` is never rate limited: one line per decision,
so a storm looks like a storm. `instance power premise retracted by its own
drain` is logged once per instance, and means the drain re-read the power at the
moment of acting and got the opposite answer. That instance is not reclaimed for
a power reading again until one holds for **six minutes**.

There is no knob. The bound cannot end a job — it can only delay a reclaim the
fleet would otherwise perform immediately — so an off switch could only ever make
the fleet less safe. If retractions are frequent on a node, the backend's
enumeration is unreliable there and that is the thing to investigate; the fleet
is already declining to act on it.

### A vector that has not come back yet

An instance in `draining` or `deregistering` **still charges the admission
envelope**, even when `fleet instances` shows its VM powered off. That is
deliberate, and it is what an operator sees when a teardown looks like it is
holding capacity it no longer needs.

The vector comes back on one of two facts and never on a reading (ADR 0043, issue
#247):

| the fleet observes | the vector |
|---|---|
| `draining` / `deregistering`, VM reads stopped | still held |
| `stopping` — the fleet's own stop landed and deregistration is confirmed | released |
| any cleanup state, VM absent from the enumeration | released |

The reason is that the first row can be taken back. A drain re-reads its premise
at the moment of acting and can abort the instance back to `running` (ADR 0033),
and an enumeration that called a VM stopped can tell the truth on the next poll —
while the replacement the fleet admitted into the freed capacity is already
cloning. The simulator produced both: five slots against a four-slot ceiling, and
eight vCPU against a four-vCPU budget.

The cost is one lifecycle edge on an ordinary teardown. It is longer for the two
reclaims that power the guest off before deregistering — the occupancy budget and
the guest-liveness reclaim — because GitHub refuses to remove a runner it still
considers busy until its own grace timer expires, measured at sixteen to eighteen
minutes on this fleet. **An occupancy or guest-death reclaim whose deregistration
GitHub is refusing holds its vector for that whole window.** If admission looks
stalled behind a teardown, that is the shape to check:

```sh
fleet instances --endpoint "$ENDPOINT" --output json |
  jq '.data[] | select(.state == "draining" or .state == "deregistering") |
      {id, state, power, drainPhase}'
fleet operations --endpoint "$ENDPOINT" --output json |
  jq '.data[] | select(.kind == "drain" and .lastError != "") | {id, attempts, lastError}'
```

A drain stuck at the deregister stage is bounded by ADR 0007's retries and ends as
a dischargeable dead letter; it is never released by hand.

### A base image whose runner GitHub will refuse

`fleet doctor` reports `FAIL  runner version` when a base image carries an
`actions/runner` release below the configured floor, or when an image declares no
version at all. ADR 0041, issue #206.

This is the only check here whose failure is **not** something the fleet did
wrong and **not** something the fleet can fix. GitHub enforces a minimum runner
version: 2.329.0 to register at all, and every new release must be installed
within 30 days of its publication. Brownouts on github.com began 24 Aug 2026;
enforcement is permanent from 25 Sep 2026. Every guest here registers from a JIT
configuration with `DisableUpdate` set, so **no runner will ever upgrade itself** —
the version sealed into the image is the version that runs jobs until the image
is rebuilt.

A blocked registration is silent by construction: jobs queue, nodes idle, nothing
turns red. It looks exactly like a quiet day.

```sh
fleet doctor --endpoint "$ENDPOINT" --output json |
  jq '.checks[] | select(.name == "runner version")'
fleet status --endpoint "$ENDPOINT" --output json | jq '.data.runnerImages'
```

| Field | Meaning |
| --- | --- |
| `platform` | `linux` or `macOS`. A node has one image per platform. |
| `vm` | The base image that platform boots. |
| `version` | The `actions/runner` release the image carries, as declared. Empty means nobody has vouched for it. |
| `floor` | The version it is judged against: `runnerVersionFloor`, or 2.329.0. |
| `belowFloor` | The verdict. |

The check passes loudly as well as failing loudly: a passing `runner version`
check still prints what each image carries, because the point is that the version
in service is readable without SSH-ing into a guest.

**To find out what an image really carries** — the declaration is the operator's
word and a rebuild can invalidate it:

```sh
# Linux, from any live clone (the manifest and the binary must agree):
tart exec "$INSTANCE" bash -lc 'cat ~/.ci-base-manifest; ~/actions-runner/bin/Runner.Listener --version'

# macOS, from a base image that is not running, without booting it:
hdiutil attach -readonly -nomount ~/.tart/vms/"$IMAGE"/disk.img   # note the APFS Data volume
diskutil mount -mountPoint /tmp/img readOnly diskNsM
python3 -c 'import json;print(json.load(open("/tmp/img/Users/admin/actions-runner/bin/Runner.Listener.deps.json"))["libraries"])' | tr , '\n' | grep Runner.Listener
diskutil unmount /tmp/img && hdiutil detach diskN
```

**When it fires, the remedy is a rebuild, and only a rebuild.** Refresh the runner
payload per [`docs/LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md) or
[`docs/BASE_IMAGE.md`](BASE_IMAGE.md), update `runner_version=` in the image's
`$HOME/.ci-base-manifest`, then update `baseImageRunnerVersion` in that node's
configuration to what was actually sealed. There is no runtime fix and no
restart that helps.

**Raising the floor is an operator action, not an automatic one.** When a new
`actions/runner` release ships, a 30-day clock starts; set `runnerVersionFloor`
to the new release on every node so the check goes red *before* GitHub does.
Nothing in the fleet watches GitHub's release feed — the check makes staleness
visible, it does not make it self-correcting.

**A daemon older than this check publishes no `runnerVersionCheck` at all**, and
that renders as a pass with the detail `not reported by this daemon`. That is a
statement about the daemon, not about the images.

### A dead guest with no console to leave behind

`fleet doctor` reports `FAIL  guest console` when a node boots Linux guests and
`linuxSerialLogDirectory` is unset — meaning a guest kernel that dies mid-job
leaves no evidence of why. Issues #236, #258, and #259 each ended at *trigger
unidentified* for exactly this reason. ADR 0045.

```sh
fleet doctor --endpoint "$ENDPOINT" --output json |
  jq '.checks[] | select(.name == "guest console")'
```

The check passes loudly too: it prints `serial console captured`,
`serial console NOT captured`, or `no Linux guests booted on this node`. The
remedy is operational, not a rebuild: verify `tart run --help` advertises
`--serial-path` on that node's tart build, set `linuxSerialLogDirectory` in
`fleet.json`, reload the daemon, and confirm the next instance start creates a
`.log` file there — see [`docs/LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md) for
the image-side `console=hvc0` half, which the #236 rollout already shipped.

### A drain that is not progressing, and a guest that will not stop

`fleet doctor` reports `FAIL  drain progress` when a durable operation has failed
more than six times, or when an instance has sat in a cleanup state
(`draining`, `deregistering`, `stopping`) for more than ten minutes. The reason
names the instance, the step, the attempt count, and the elapsed time.

```sh
fleet doctor --endpoint "$ENDPOINT" --output json |
  jq '.checks[] | select(.name == "drain progress")'
fleet status --endpoint "$ENDPOINT" --output json | jq '.data.stalled'
```

The same rows render as the `STALLED` table of `fleet status`:

```
STALLED
INSTANCE                          OPERATION                              STEP  ATTEMPTS  RETRYING  DRAIN STATE    HELD
trf-macos-6x12-f458a747883b9a0d   event-drain-trf-macos-6x12-f458a...    stop  67        1h22m19s  deregistering  1h22m19s
```

`STEP` is the whole diagnosis. Read it first:

| `STEP` | Meaning | Action |
| --- | --- | --- |
| `drain_guard` | The fleet cannot read fresh GitHub runner or job state. | Check the `github` observation. Nothing is being killed; the guard is fail-closed. |
| `deregister:runner_busy` | GitHub refuses to remove a runner it considers to be executing a job. | Legitimate for up to GitHub's six-hour job maximum. See the case study below. |
| `confirm_inactive` | Deregistration succeeded but GitHub will not confirm the runner and its jobs inactive. | Check the `github` observation; the fleet retries. |
| `stop` | The guest will not power itself down. | **This section.** |
| `delete` | The guest is off but the backend will not remove it. | Check host disk and the backend's own logs. |

#### A guest that will not stop

A `stop` step that keeps failing means the guest is wedged: its runner is already
deregistered and its deletion already confirmed, so the job is over and all that
remains is to make the VM stop holding the host. The fleet escalates on its own
(ADR 0039) — three graceful stops, then three forced ones, then three that remove
the guest outright — which is roughly four minutes to stop being polite and eight
to remove it. **In most cases the right action is to wait those few minutes and
watch `ATTEMPTS` stop climbing.**

If it climbs past nine, every rung has failed and the operation dead-letters at
once. Confirm the guest really is wedged before doing anything by hand:

```sh
# The VM the fleet cannot stop. On a container node this is `podman ps`.
tart list --format json | jq '.[] | select(.Name == "<instance>")'
tart ip --wait 5 <instance>          # "no IP address found" on a wedged guest
pgrep -fl "tart run <instance>"      # the run process is alive from the host's view
```

A wedged guest may need a manual stop. It is safe here and only here — the
instance is in `deregistering` or `stopping`, so the runner is gone from GitHub
and the job is over:

```sh
tart stop <instance> --timeout 0     # forceful; no graceful wait
```

Never do this for an instance in any other state, and never delete a controller
VM by hand while its row is live (see the ordering note under
[Discharging a dead-lettered cleanup](#discharging-a-dead-lettered-cleanup)).
The fleet completes the drain on its next attempt and reaps the record itself.

#### When discharge is refused

`fleet operations discharge` refuses with `operation_not_dead` while the
operation is still retrying. That is correct — a retrying operation is still
making progress and discharging it would record that a human accepted an effect
the fleet had not finished trying.

On 2026-08-10 that refusal held for ninety minutes because the operation could
not dead-letter at all: the attempt ceiling was 720 and each attempt cost 45
seconds, so the "six hour" bound was in fact fifteen hours. Both bounds are now
real (ADR 0039). An operation dead-letters when **any** of these is true:

- it has failed 720 times;
- it has been retrying for six hours;
- the executor proved the failure permanent — for a drain, the stop ladder ran
  out of rungs, which takes about twelve minutes.

So if `discharge` is refused with `operation_not_dead`:

1. Read `ATTEMPTS` in the `STALLED` table. Under nine, the ladder has not
   finished; wait.
2. Over nine and still `retrying`, the ladder is being re-entered from an
   earlier step — read `STEP` again, because it is no longer `stop`.
3. If the guest is wedged and the queue behind it cannot wait for the ladder,
   perform the manual `tart stop --timeout 0` above. That is the remedy, not
   discharge: discharge closes an operation the fleet can never complete, and a
   guest an operator can stop is one the fleet can complete.

Discharge is for the case where the effect will never happen — a permanently
leaked GitHub registration, the case study below. It is not a way to skip a slow
retry, and it does not release the instance's vector on its own.

### Diagnosing an unavailable GitHub observation

`scheduler ready FAIL: critical_observation_unavailable` names how many
observations are unavailable but not why. Every ingest observation carries a
bounded, credential-free reason in `detail`:

```sh
fleet status --endpoint "unix://$ROOT/state/fleetd.sock" --output json |
  jq '.data.observations | to_entries | map(select(.value.freshness != "fresh"))'
```

| `detail` | Meaning | Action |
| --- | --- | --- |
| `session_expired` | GitHub invalidated the broker session; it is being recreated. | None. Expect `fresh` within one poll interval. |
| `recreated_after_failures` | The failure could not be classified, so the bounded escalation discarded the session. | None, unless it repeats — then treat it as a broker or credential fault. |
| `session_release_failed` | The dead session could not be released yet; recreation is withheld until the bound. | Wait out `githubSessionFailureWindowSeconds`. |
| `session_create_failed` | The replacement session could not be opened. | Check GitHub App installation permissions and rate limits. |
| `message_poll_failed` | Ordinary long-poll failure that no closer classification fits. | None if transient. |
| `rate_limited` | GitHub answered with its primary or a secondary rate limit. | None if occasional. A count that climbs across hours means this node polls too aggressively — widen the poll interval before GitHub widens it for you. |
| `server_error` | GitHub answered 5xx: the fault is GitHub's own, not this node's network or credentials. | Wait it out; escalate only if it persists across regions or outlives GitHub's status page. |
| `demand_commit_conflict` | The durable store refused a broker message. This is not a network condition and redelivering the same message cannot clear it: the fleet holds state the message contradicts. | Read the `binding ingest failure` warning for the scope, profile, and scale set, then see [ADR 0035](adr/0035-a-broker-message-id-is-unique-only-within-its-sequence.md). A restarted message-id sequence resolves itself on the next colliding delivery; anything else is a genuine durable contradiction and needs the demand rows read. |
| `queue_observation_failed` / `queue_observation_stale` | The REST queue inventory is unavailable or aged out. | Check API reachability and rate limits. |
| `queue_reconcile_failed` | The REST queue snapshot could not be persisted. | Check the database and disk. |

The rate-limited `component loop failure` warning now carries the same
`reason=` attribute, so stderr and the admin API agree. It names the component
and nothing else, so a second, binding-scoped warning names the binding:

```
level=WARN msg="binding ingest failure" scope=knee-repo profile=large \
  scaleSet=2 observation=github-8077185082566234948 reason=demand_commit_conflict
```

It is rate limited to one line per binding and reason per minute, on the same
window as the component warning, and it carries no upstream text.

### A broker message-id sequence restarted

GitHub's message ids are unique only inside one broker sequence, and GitHub
restarts the sequence (it did so for scale set `8077185082566234948` on
2026-08-01, which stranded every `linux-large` job in that repository for three
days — [ADR 0035](adr/0035-a-broker-message-id-is-unique-only-within-its-sequence.md)).
The fleet detects a restart on the first delivered message that contradicts its
ledger, adopts a new inbox generation, and keeps ingesting. It reports itself:

```
level=WARN msg="broker message sequence restarted" scope=knee-repo profile=large \
  scaleSet=2 observation=github-8077185082566234948 generation=1 \
  retiredMessageId=100000086 adoptedMessageId=100000004
```

**No operator action is required, and there is no command to run.** The
recovery is durable and idempotent, and it applies equally to a database that
was already poisoned before the fix: the next colliding delivery heals the
binding. Do not delete inbox rows or rewind cursors by hand — the hand-written
SQL that ended this incident is exactly what the generation contract replaces.

Investigate only if the line repeats for the same binding on every delivery,
which would mean detection is oscillating rather than converging.

The scheduler component classifies its own failures the same way. Each token
names a distinct repair, so a reasonless `component=scheduler` warning is now a
bug rather than the norm:

| `reason` | Meaning | Action |
| --- | --- | --- |
| `engine_invalid` | The engine is wired without a store, inventory, controller identity, valid mode, or advancing clock. | A deployment fault. Check the generation and its arguments. |
| `scheduler_state_unreadable` | The durable `scheduler_state` row could not be read. | Check the database and disk. |
| `scheduler_state_reseed_failed` | The row was missing and the cold-start reseed was refused. | Check that the database is writable. |
| `scheduler_state_corrupt` | The stored optimization state did not decode. | Recoverable: the row is reseeded on the next tick. Investigate if it repeats. |
| `demand_unreadable` | Durable demand for a binding could not be read. | Check the database; distinct from a stale-statistics binding, which trickles instead of failing. |
| `queue_summary_unreadable` | The canonical queue summary could not be produced. | Check REST reachability and rate limits. |
| `plan_commit_failed` | The plan was computed but the durable store was unavailable. | Check the database, the authority lease, and operation leases. A constraint violation is no longer reported here — it is a refused plan, not an unavailable store. |
| `plan_commit_contended` | The plan lost an optimistic-concurrency race: the durable state moved between the observation it was built from and its compare-and-set. | None. It self-heals on the next tick, which re-observes. Expected under host saturation; investigate only if it never clears. |
| `plan_commit_rejected` | The durable layer refused the plan as malformed, e.g. an unknown profile, a drain of an instance whose durable state cannot be drained, or a write that violates a schema constraint. | Repeats every tick until the inputs change. Reconcile the inventory and configuration; this is not a database fault. Confirm with `scheduler_state.version` never advancing and `plans` gaining no row — see [ADR 0027](adr/0027-one-tick-admits-a-demand-once.md) and [ADR 0028](adr/0028-a-repeated-decision-is-a-new-attempt.md) for the two plans that reached production this way. |
| `plan_invalid` | The scheduler could not form a usable plan, e.g. a live instance with an unrecognized platform. The durable write was never attempted. | Reconcile the inventory; this is not a database fault. |

A blocked plan is not a loop failure. Fail-closed admission on a stale or
unavailable observation returns no error, so it is visible as a non-`ready` plan
status and in `observations` rather than as a warning.

The warning above is rate limited to one line per component and reason per
minute, so it records *that* a loop is failing and never *how often*. Use the
counter for that:

```sh
fleet metrics --endpoint unix://__STATE_DIR__/fleetd.sock | grep fleet_component_failures_total
fleet_component_failures_total{component="scheduler",reason="plan_commit_rejected"} 412
```

`fleet_component_failures_total` is monotonic for the life of the process and
both labels are closed vocabulary, so `rate()` over it separates a loop that
failed once from one that has not committed anything for half an hour — a
distinction the log cannot make. A rising `reason="unclassified"` means a failure
path is missing from the vocabulary and is worth reporting.

A daemon restart is no longer a recovery step for a wedged session: recovery is
per binding and bounded. `githubSessionMaxIngestFailures` (default 5) and
`githubSessionFailureWindowSeconds` (default 300) tune the bound in `fleet.json`;
both are omitted from a rewritten file while they hold the defaults.

Jobs already queued against a discarded session are not redelivered to its
replacement. GitHub binds a queued job to the session that existed when it was
queued, so after a `session_expired` or `recreated_after_failures` event, any
run that was already `queued` needs an operator-driven cancel and re-run. Those
jobs stay visible in `queues` and still breach the queue SLO, so they are not
silent — but the controller must never cancel a user workflow itself.

### `controller authority lost: renew lease` in the daemon log

The daemon exits and its supervisor restarts it. The restart is the expensive
part: the successor re-establishes every scale-set session, on a host that was
already struggling when the renewal failed.

The renewal itself is a single small `UPDATE` with a five-second budget, and it
now runs on **its own SQLite connection**. The shared connection is capped at
one, so before that change a renewal queued behind whatever the scheduler and
the operations worker were doing and could spend its whole budget without ever
reaching the database (issue #295; the same shape as the 2026-08-01 incident of
three exits in seventy minutes).

If this line still appears, the cause is no longer queueing behind scheduling.
Look for a host so saturated that a small write cannot complete at all, or for
`ErrLeaseLost` — a second controller holding the same lease name, which is a
real fencing loss and not a blip.

### A node draining toward its own update

`fleet status` prints an `UPDATE DRAIN` block above the queues, `fleet doctor`
fails its `update drain` check, and `fleet_update_drain_active` is 1. This is
not a fault: a generation newer than the running one has been sitting under
`releases/` unapplied, and the node has stopped admitting *new* instances so the
live ones can finish and the updater's quiescence gate can pass
([ADR 0048](adr/0048-a-node-arranges-the-quiescence-its-update-needs.md)).

Nothing running is interrupted — that guarantee is ADR 0011's and is untouched.
The queue growing during a drain is the expected cost, not evidence against it.

It ends by itself in one of three ways: the update lands and the process is
replaced; the candidate disappears; or `updateDrainMaxWaitSeconds` (default
7200) elapses, after which the node serves normally for
`updateDrainCooldownSeconds` (default 3600) before trying again.

An operator who needs the capacity back immediately can set
`updateDrainDisabled: true` and restart — a draining node resumes admission
rather than staying stranded. `updateDrainPendingForSeconds` (default 1800)
tunes how long a candidate waits before a drain starts. All four are omitted
from a rewritten file while they hold the defaults.

If a node is drain-looping — draining, timing out, cooling down, draining again
— the candidate cannot be reached at all: something holds an instance longer
than `MaxWait`. Read `OCCUPANCY` for the hold and its budget rather than raising
`MaxWait`.

### A node that withdrew its sessions

`fleet status` prints `SESSIONS WITHDRAWN` above the queues, `fleet doctor`
fails its `session yield` check, and `fleet_session_yielded` is 1. This is not a
fault: the node concluded it cannot admit — the check names the admission
reason — and released the sessions behind its scale sets so GitHub binds new
jobs to a sibling advertising the same labels instead of to a node that will
not run them ([ADR 0047](adr/0047-a-node-that-cannot-admit-yields-its-sessions.md)).

It rejoins by itself once admission has been allowed for
`sessionYieldHealthyForSeconds` (default 120). The operator's job is the cause,
not the withdrawal: read `admission` in `HOST PRESSURE` and clear whatever it
names, usually free disk against `minFreeDiskGb` or load against
`maxLoadAverage`.

Two facts matter while a node is withdrawn. Its bindings report `stale`
observations detailed `session_yielded`, so it reports NOT READY on purpose —
it is holding no session and observing nothing. And jobs bound to its scale
sets *before* it withdrew do not migrate: like a discarded session above, they
need a cancel and re-run. Withdrawal prevents a backlog; it does not drain one.

`sessionYieldBlockedForSeconds` (default 600) and
`sessionYieldHealthyForSeconds` (default 120) tune the bounds in `fleet.json`,
and `sessionYieldDisabled: true` turns the behaviour off for a node that should
hold its sessions regardless. All three are omitted from a rewritten file while
they hold the defaults.

### A breached queue SLO while the host still has free capacity

`queue_slo_breached` with `admissionAllowed: true` and idle cores is normally
head-of-line blocking, not a fault. The aged global-FIFO head reserves its exact
vector, and admission behind that reservation is bounded on purpose. The
reservation is scheduler state and is not exposed by `fleet.v1`, so reconstruct
it: the reserved head is the oldest queued job past the fairness age, and what
is free is the host less every live instance.

```sh
fleet status --endpoint "unix://$ROOT/state/fleetd.sock" --output json |
  jq '{queues: .data.queues, live: .data.instances, pressure: .data.hostPressure}'
```

Compare the head profile's configured vector against the host capacity less the
`live` vectors, then read the two cases:

- The reserved head **does not fit** what is free: it is waiting for a live
  instance to exit, not for the queue behind it to stay idle, so work that fits
  the residual — Linux *or* macOS — is admitted every tick behind it
  ([ADR 0017](adr/0017-infeasible-reservation-residual-backfill.md),
  [ADR 0029](adr/0029-remainder-admission-behind-a-reservation.md)). A queue
  still frozen in this state with a demand that fits is a bug worth reporting;
  that is exactly the 2026-08-02 incident, where a `maestro` fitting the four
  free cores waited over an hour behind an `xl` head that could not use them.
  Work in the head's **own** repository is admitted here too, but only up to the
  slack its cap leaves once the head's own slot is set aside — so a repository
  at its last free slot still lends nothing
  ([ADR 0030](adr/0030-a-reserved-head-holds-one-repository-slot.md)). Read the
  cap and the live instance count for that repository before calling it a wedge.
- The reserved head **does fit** what is free: it is blocked by a non-resource
  gate, usually its repository cap. Its whole vector is then held for it and
  only the leftover is lent out, so a queued job larger than
  `free - reservation` correctly waits. This is policy, not a wedge.

Neither case is repaired by restarting the daemon. Both clear when the blocking
instance finishes; the reserved head is re-checked first on every tick and takes
the first vector large enough for it.

## Recovery

1. Stop admission; do not delete uncertain VMs.
2. Preserve `fleet.db`, `fleet.db-wal`, and `fleet.db-shm` together.
3. Run `PRAGMA quick_check` through a normal `fleet` startup.
4. Reconcile Tart inventory, GitHub runners/jobs, ownership, and outbox leases.
5. Resume observe, then shadow. Never jump directly to authority.
