# Local admin API (`fleet.v1`)

The admin API is local, read-only, and served over a Unix socket created with
mode `0600`. Its parent directory is created with mode `0700`; a regular file,
foreign-owned socket, relative path, non-canonical path, or oversized Unix path
fails closed. A stale socket owned by the current user is safely replaced.

`fleetd` keeps the existing loopback health/Prometheus listener for monitoring,
but `fleetctl` defaults to the Unix socket.

## Endpoints

- `GET /v1/status`: coherent immutable status envelope.
- `GET /healthz`: liveness probe.
- `GET /readyz`: readiness with bounded reason codes.
- `GET /metrics`: Prometheus text exposition.

All other methods return 405 and all unknown paths return 404. Responses are
`Cache-Control: no-store`. Status includes an `ETag` backed by a monotonic
in-memory revision so future watch clients can use conditional polling.

## Status envelope

```json
{
  "apiVersion": "fleet.v1",
  "kind": "Status",
  "generatedAt": "2026-07-12T20:00:00Z",
  "revision": 42,
  "data": {
    "controllerVersion": "v0.1.0",
    "controllerMode": "shadow",
    "hostMode": "linux",
    "lastLoopTick": "2026-07-12T20:00:00Z",
    "lastSuccessfulTick": "2026-07-12T20:00:00Z",
    "live": {"ok": true, "reasons": []},
    "ready": {"ok": true, "reasons": []},
    "queues": [],
    "instances": [],
    "observations": [],
    "operations": {"retrying": 0, "dead": 0}
  },
  "warnings": []
}
```

DTOs are intentionally separate from domain and persistence structs. New
optional fields may be added within `fleet.v1`; removals, renames, changed enum
semantics, or changed units require a new API version. Clients reject unknown
API versions rather than guessing.

## Snapshot semantics

The response is copied from one mutex-protected telemetry snapshot. It cannot
mix queue data from one reconciliation tick with instance data being updated by
another goroutine. Profiles and observation names are bounded configuration
values, never repository/job metric labels.

The first interface exposes aggregate state needed for safe operation. Exact
demand, instance, operation, reservation, and plan read models will be added as
bounded, paginated `fleet.v1` resources after their coherent SQLite read
transaction is implemented. `fleetctl` will not bypass that work with direct DB
queries.
