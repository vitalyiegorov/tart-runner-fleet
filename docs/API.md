# Local admin API (`fleet.v1`)

The admin API is local and served over a Unix socket created with
mode `0600`. It is read-only apart from exactly one guarded mutation
(`POST /v1/operations/discharge`), which is registered only on that private
socket and never on the loopback health listener. Its parent directory is created with mode `0700`; a regular file,
foreign-owned socket, relative path, non-canonical path, or oversized Unix path
fails closed. A stale socket owned by the current user is safely replaced.

`fleet run` keeps the existing loopback health/Prometheus listener for
monitoring, while the operator commands default to the Unix socket.

## Endpoints

- `GET /v1/status`: coherent immutable status envelope.
- `GET /healthz`: liveness probe.
- `GET /readyz`: readiness with bounded reason codes.
- `GET /metrics`: Prometheus text exposition.
- `POST /v1/operations/discharge`: the one guarded mutation; private socket only.

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
    "hostPressure": {
      "availableMemoryMiB": 10240,
      "freeDiskGiB": 203,
      "swapUsedMiB": 0,
      "swapOuts": 0,
      "swapOutRatePerSecond": 0,
      "swapOutRateObserved": true,
      "cpuIdlePercent": 54,
      "loadAverage": 3,
      "admissionAllowed": true,
      "admissionReason": "capacity available"
    },
    "queues": [],
    "instances": [],
    "observations": [],
    "operations": {
      "retrying": 0,
      "dead": 1,
      "failures": [
        {"kind": "deregister", "code": "deregister:runner_busy", "count": 1, "attempts": 835}
      ],
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
  },
  "warnings": []
}
```

`hostPressure.swapOutRatePerSecond` and `hostPressure.swapOutRateObserved` are
the swap guardrail's deciding signal. Admission is refused only when
`swapUsedMiB` exceeds the configured ceiling **and** the host is measurably
paging out, so `swapUsedMiB` alone cannot reproduce the decision, and `swapOuts`
is cumulative and undifferenceable from a single document. `swapOutRateObserved:
false` means the rate could not be measured — no prior sample, a non-advancing
clock, or a counter reset by a reboot — and the guardrail fell back to the level
alone; it never means a quiet host. Both fields are additive and are absent on
daemons that published only the level. The same two facts are exported as
`fleet_host_swapout_rate_pages_per_second` and
`fleet_host_swapout_rate_observed`.

`scopeQueues[].tiers` is the priority-tier breakdown of one scope's queue: the
tier each waiting demand was classified into, its depth, and its oldest enqueue
time, ordered highest tier first. `default` is the tier every unmatched demand
lands in. The array is additive and is absent both on daemons older than the
feature and on a fleet that declares no tier, so an absent key means "no policy
declared", never "no demand" (ADR 0037).

`operations.failures` explains the counts. Each entry pairs an operation kind
with one closed-vocabulary failure code — `<stage>` or `<stage>:<reason>`, for
example `deregister:runner_busy`, `deregister:runner_forbidden`,
`deregister:runner_scope_unresolved`, or `unclassified` — plus how many
operations share it and the worst attempt count among them. Persisted upstream
text is never rendered. The field is omitted while nothing is failing and is
absent on daemons that published only the counts.

`occupancy` reports how long each live instance has held its profile's resource
vector, and `occupancyCheck` is the derived judgement `fleet doctor` renders:

```json
"occupancy": [
  {
    "instance": "trf-xl-05bbe1c83f21fcd6",
    "profile": "xl",
    "repo": "rnw-community/rnw-community",
    "cpu": 6,
    "memoryMiB": 12288,
    "ageSeconds": 4500,
    "budgetSeconds": 2700,
    "warned": true,
    "overBudget": true,
    "starvesQueuedDemand": true
  }
],
"occupancyCheck": {"ok": false, "reasons": ["instance trf-xl-05bbe1c83f21fcd6 ..."]}
```

`budgetSeconds` is `0` for a profile with no configured ceiling; `warned` and
`overBudget` are then always `false`, because an unbounded hold cannot be past a
bound. `starvesQueuedDemand` reports that queued work would fit inside the
vector this instance is holding, and `occupancyCheck` fails only on the
conjunction of `overBudget` and `starvesQueuedDemand`: a long job is allowed to
be long, and a deep queue is allowed to be deep, but an over-budget hold with
work waiting that fits it is the 2026-08-09 incident (ADR 0036). The same three
facts are exported as `fleet_instance_occupancy_seconds`,
`fleet_instance_occupancy_budget_seconds`, and
`fleet_instance_occupancy_starving`, each labelled by profile and instance. Both
fields are additive and absent on daemons that measured no occupancy at all.

`guestSilences` reports every instance whose guest has stopped answering the
node's liveness probe, and `guestLivenessCheck` is the derived judgement `fleet
doctor` renders:

```json
"guestSilences": [
  {
    "instance": "trf-xl-0aacdbcc6653bd8a",
    "profile": "xl",
    "repo": "rnw-community/rnw-community",
    "cpu": 6,
    "memoryMiB": 12288,
    "refusals": 5,
    "silenceSeconds": 120,
    "requiredRefusals": 5,
    "windowSeconds": 90,
    "unresponsive": true,
    "runId": 31939037119,
    "jobId": 93540000001
  }
],
"guestLivenessCheck": {"ok": false, "reasons": ["instance trf-xl-0aacdbcc6653bd8a ..."]}
```

`refusals` and `silenceSeconds` are the measurement; `requiredRefusals` and
`windowSeconds` are the bound this node judges it against, and both are `0` on a
node that probes nothing. Only a refused **transport** is counted: a guest that
answers slowly, and a probe that ran out of its own deadline, both clear the run,
which is what stops a saturated-but-alive guest being probed into a drain.
`unresponsive` is the verdict — both bounds met — and `guestLivenessCheck` fails
on that alone, never on a partial run. `runId` and `jobId` name the job that dies
with the guest; they travel here rather than as metric labels, which are a closed
vocabulary. The measurement is exported as
`fleet_instance_guest_silence_seconds`, `fleet_instance_guest_probe_refusals`,
`fleet_instance_guest_probe_refusals_required`, and
`fleet_instance_guest_unresponsive`, each labelled by profile and instance. Both
fields are additive and absent on daemons that probed no guest at all (ADR 0040).

`reservation` names the aged global-FIFO head the scheduler is standing capacity
by for, and `reservationCheck` is the derived judgement `fleet doctor` renders:

```json
"reservation": {
  "demand": "c/repo/1009/1/500009", "repo": "c/repo", "profile": "xl",
  "cpu": 6, "memoryMiB": 12288, "slots": 1,
  "heldSeconds": 780, "axis": "repository_cap"
},
"reservationCheck": {"ok": true, "reasons": []}
```

`axis` is the operator's whole diagnosis and is a closed vocabulary: `vector`
(the head's resource vector does not fit the starvation envelope, so it waits on
live instances to release), `repository_cap` (the vector fits and the head's own
repository is at its cap, so it waits on one of that repository's instances to
exit and freeing CPU cannot hasten it), `both`, `none` (no axis refuses the head
— the fleet is holding a turn for work it could have started), or empty when the
plan judged nothing because its observation was unusable.

`reservationCheck` fails on `none` held longer than the queue SLO, and on
nothing else. A reservation costs the fleet a turn and one repository slot, never
a vector: ADR 0017 released the vector on the resource axis,
[ADR 0038](adr/0038-a-cap-held-reserved-head-lends-its-vector.md) on the
repository-cap axis, and
[ADR 0045](adr/0045-a-reservation-withholds-order-not-a-vector.md) deleted what
was left of the withholding. `lendsVector` was published here until ADR 0045 and
is gone rather than pinned to `true`.

The same facts are exported as `fleet_reservation_held_seconds` and
`fleet_reservation_vector_cpu` (labelled by profile) and `fleet_reservation_axis`
(labelled by the closed axis vocabulary, every value emitted on every scrape).
`fleet_reservation_lends_vector` is removed for the same reason. The head's
demand key and repository are
deliberately NOT metric labels; they travel in this document only. Both fields
are additive and absent on daemons that published no reservation at all — which
is the condition issue #226 ran in unobserved.

`operations.deadLetters` names the individual operations that have stopped
retrying, because a count is not actionable: discharging one requires its
identity. `parked` reports that nothing will advance the resource without an
operator — no operation for it is pending or claimed, and its owned VM is observed
stopped. `fleet update` discounts a parked resource from its quiescence gate and
nothing else; a running VM always defers a release. The field is omitted while
nothing is parked and is absent on older daemons.

## Guarded mutation

```
POST /v1/operations/discharge
Content-Type: application/json

{
  "operationId": "op-ea9b705d234ad29f14e79b6d",
  "instanceId": "trf-maestro-096ffcb3a52d8624",
  "reapInstance": true,
  "confirm": "discharge-dead-letter",
  "reason": "GitHub 422 runner_busy: permanent registration leak"
}
```

```json
{
  "apiVersion": "fleet.v1",
  "kind": "DischargeResult",
  "operationId": "op-ea9b705d234ad29f14e79b6d",
  "instanceId": "trf-maestro-096ffcb3a52d8624",
  "operationDischarged": true,
  "instanceReaped": true,
  "vmDeleted": true
}
```

The confirmation token and reason are part of the wire contract, not client
decoration: the daemon refuses an unconfirmed or unexplained mutation whatever
sent it, refuses it entirely outside authority mode, and audits every attempt with
both identities and the reason. Request bodies are bounded to 8 KiB.

A refusal is a bounded document with one closed-vocabulary code and never any
upstream text:

```json
{"apiVersion": "fleet.v1", "kind": "Refusal", "code": "vm_running"}
```

`unknown_operation` answers 404, `not_authority` 403, `invalid_request`,
`unconfirmed`, and `reason_required` 400, `store_unavailable`,
`vm_state_unknown`, and `vm_delete_failed` 503, and every other code 409. The
complete list, and the ordering guarantee between the durable row and the VM, are
in [`docs/OPERATIONS.md`](OPERATIONS.md).

DTOs are intentionally separate from domain and persistence structs. New
optional fields may be added within `fleet.v1`; removals, renames, changed enum
semantics, or changed units require a new API version. Clients reject unknown
API versions rather than guessing.

`hostMode` is one of `idle`, `linux`, `macos`, or `mixed`. `mixed` means live
Linux and macOS instances share the configured host resource envelope; it does
not weaken ownership, freshness, or host-pressure admission checks.

## Snapshot semantics

The response is copied from one mutex-protected telemetry snapshot. It cannot
mix queue data from one reconciliation tick with instance data being updated by
another goroutine. Profiles and observation names are bounded configuration
values, never repository/job metric labels.

The host-pressure snapshot is the exact credential-free evidence used for the
latest admission decision. Prometheus exports the same bounded values as
`fleet_host_*` metrics without repository, VM, runner, or job labels. A stale
or unavailable probe still fails scheduling closed through the scheduler
observation; it never fabricates a healthy pressure sample.

The first interface exposes aggregate state needed for safe operation. Exact
demand, instance, operation, reservation, and plan read models will be added as
bounded, paginated `fleet.v1` resources after their coherent SQLite read
transaction is implemented. `fleet` will not bypass that work with direct DB
queries.
