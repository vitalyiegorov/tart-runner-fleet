# Operations

## Current promotion ceiling

`fleetd` accepts `observe` and `shadow`. `canary` and `authority` are hard-coded
off until a disposable real-VM lifecycle canary and rollback drill pass. The
incumbent shell manager remains production authority.

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
