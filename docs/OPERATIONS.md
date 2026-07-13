# Operations

## Current promotion ceiling

`fleetd` accepts `observe` and `shadow`. `canary` and `authority` are hard-coded
off until a disposable real-VM lifecycle canary and rollback drill pass. The
incumbent shell manager remains production authority.

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
per profile. It commits sanitized job events and the message cursor atomically
before acknowledgement, computes plans, and writes effects to neither GitHub nor
Tart. Do not point another controller at the same scale-set sessions.

GitHub App metadata belongs in JSON; the PEM private key belongs in a Keychain
generic-password item. Create that item through Keychain Access so the PEM never
appears in shell history or a process argument. Use service
`tart-runner-fleet-github-app` and account `controller` (or configure different
names), paste the PEM as the item password, then delete the source file safely.

The `github` configuration requires `configUrl`, `owner`, `clientId`,
`installationId`, `keychainService`, `keychainAccount`, and exactly one
`scaleSets` entry for every enabled runner profile.

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
