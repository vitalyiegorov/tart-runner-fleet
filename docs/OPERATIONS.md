# Operations

## Promotion contract

`fleetd` supports `observe`, `shadow`, `canary`, and `authority`. Canary and
authority acquire a renewable singleton lease, recover expired outbox leases,
and stop on lease loss. Canary additionally requires exact scope/profile flags
and runs at most one lifecycle operation concurrently. Promotion is never
implicit: keep the incumbent installed until a real queued job has completed,
deregistered, stopped, and deleted through the Go controller.

## Self-hosting and bootstrap

`vitalyiegorov/tart-runner-fleet` is a first-class target with three concurrent
Linux slots. Its preflight, quality, coverage, race, reproducible-build, release,
and nightly jobs therefore run on ephemeral VMs scheduled by this fleet. There
are no GitHub-hosted fallback runners.

Self-hosting uses a two-generation rule to avoid circular upgrades:

1. Generation N (currently the pinned shell incumbent) remains installed under
   launchd and owns scheduling while generation N+1 is proposed.
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

To run the same idempotent check manually:

```sh
fleetctl update apply-latest \
  --mode authority \
  --confirm automatic-release-update
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
