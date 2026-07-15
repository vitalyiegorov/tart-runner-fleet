# Operations

## Promotion contract

`fleetd` supports `observe`, `shadow`, `canary`, and `authority`. Canary and
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
   checkout. It starts in observe, then shadow, while N remains authority.
4. A disposable scale-set canary and rollback drill must pass before an atomic
   launchd authority handoff. Never stop N merely because N+1 was downloaded.
5. Keep the previous binary, launchd plist, configuration, and SQLite files until
   the new generation has survived its soak window. Rollback atomically restores
   that pinned generation; it does not depend on GitHub Actions being available.

If all dynamic runners are unavailable, recovery is deliberately local: restore
the pinned incumbent launchd unit, run `fleetctl doctor`, and verify one disposable
Linux canary before releasing queued work. A workflow cannot repair a stopped
runner controller because that would recreate the same dependency cycle.

## Observe

Observe validates configuration, migrates/quick-checks the private SQLite WAL,
reconciles durable instance metadata with Tart and host pressure, computes
fail-closed plans, and exposes local health. It performs no GitHub/Tart mutation.

Host admission uses two layers. Exact profile vectors enforce CPU, memory, and
slot envelopes first. A fresh macOS probe then preserves `minFreeDiskGb` and
`minAvailableMemoryMb`, defers while swap exceeds `maxSwapUsedMb`, and treats
CPU as pressured only when both `maxLoadAverage` is exceeded and idle CPU falls
below `minCpuIdlePercent`. Missing values decode to the safe defaults in
`config/fleet.example.json`. Inspect the evidence through `fleetctl status` or
the bounded `fleet_host_*` metrics; do not infer pressure from queue age alone.

Pressure stops new admission, never an active job. Owned ephemeral runners
still follow durable drain, deregistration, stop, and delete operations with
indefinite bounded cleanup retries. Bases and unknown VMs are never pressure-
deleted. Recover disk by resolving visible owned cleanup failures, not by
blindly pruning Tart state.

```sh
fleetd run \
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
4. validate that file with the new candidate's `fleetctl config validate`;
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
every scope has exactly one scale set for each enabled profile. Numeric scale-set
IDs may collide across installations because durable state uses a scoped key.

Provisioning is explicit, drift-failing, and plan-first:

```sh
fleetctl config validate fleet.json
fleetctl scale-sets provision --config fleet.json
fleetctl scale-sets provision --config fleet.json --apply --write \
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
fleetctl update adopt \
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
`fleetctl`. If the updater is already loaded, a distinct retrying handoff job
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
reports exit status 0, the program path is the committed immutable `fleetctl`,
and bounded updater logs contain no newer error. Process absence alone is not a
failure and process presence alone is not readiness.

After every commit, compare all three generation identities: the installed
manifest, the updater plist on disk, and `launchctl print`'s loaded `program`.
Exit status 0 does not make a stale loaded program healthy. A one-time migration
from a release that predates updater-job reload may require a controlled
`bootout`/`bootstrap` after verifying the new plist; subsequent updates must
maintain parity automatically.

During an update, a short-lived
`com.vitalyiegorov.tart-runner-fleet.updater-handoff` job is expected. It fails
closed while the update journal exists, retries replacement failures, and is
healthy only when the normal updater's loaded program matches the installed
generation. Do not delete it to hide a parity failure.

To run the same idempotent check manually:

```sh
fleetctl update apply-latest \
  --mode authority \
  --confirm automatic-release-update
```

After every install or reboot, require both LaunchAgents and exact daemon
readiness rather than treating process presence as health:

```sh
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.authority
launchctl print gui/"$(id -u)"/com.vitalyiegorov.tart-runner-fleet.updater
fleetctl status --require-ready --output json
fleetctl doctor --output json
```

```sh
set -eu
RELEASE_DIR="$HOME/Library/Application Support/tart-runner-fleet/releases/$VERSION"
STATE_DIR="$HOME/Library/Application Support/tart-runner-fleet/state"
PLIST_DIR="$HOME/Library/Application Support/tart-runner-fleet/launchd/$VERSION"
mkdir -p "$PLIST_DIR"
cd "$RELEASE_DIR"
./render-launchd.sh observe "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/observe.plist"
./render-launchd.sh shadow "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/shadow.plist"
./render-launchd.sh canary "$RELEASE_DIR" "$STATE_DIR" "$PLIST_DIR/canary.plist" fleet-repo small
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
so an ordinary `linux-small` job in the same repository cannot be mutated.
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

## Health

- `GET /healthz`: event-loop liveness.
- `GET /readyz`: successful recent tick plus fresh critical observations.
- `GET /metrics`: bounded-cardinality Prometheus text.

The health server rejects non-loopback TCP listeners. Never expose it directly.

The operator API uses a private Unix socket. With the launchd template it is
`__STATE_DIR__/fleetd.sock`:

```sh
fleetctl status --endpoint unix://__STATE_DIR__/fleetd.sock
fleetctl doctor --endpoint unix://__STATE_DIR__/fleetd.sock
```

The socket is `0600`, is unlinked on clean shutdown, and only a stale socket
owned by the current user may be replaced. `fleetctl` never opens `fleet.db`.

## Recovery

1. Stop admission; do not delete uncertain VMs.
2. Preserve `fleet.db`, `fleet.db-wal`, and `fleet.db-shm` together.
3. Run `PRAGMA quick_check` through a normal `fleetd` startup.
4. Reconcile Tart inventory, GitHub runners/jobs, ownership, and outbox leases.
5. Resume observe, then shadow. Never jump directly to authority.
