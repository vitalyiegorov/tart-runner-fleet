# Implementation checkpoint — issue #509

Branch: feat/509-artifact-gate. No push, PR, node access or deployment.
[ADR 0097](adr/0097-a-gate-serves-run-scoped-actions-artifacts.md) supersedes
this plan's earlier multi-owner alternatives. Ubuntu is the only owner;
Omarchy forwards artifact RPCs/blobs. Failure reports stay local, no viewer.

## Completed in the owner/forwarding implementation

- Added failing deterministic owner security/handoff, restart, budget and
  forwarding tests before production implementation. Added adversarial key,
  malformed wire, corruption and I/O fault cases as behavior was completed.
- internal/cachegate/artifact_auth.go verifies pinned-issuer RS256 JWTs with
  bounded fixed-source JWKS refresh, exp/nbf, configured repository allowlist
  and a single signed Actions.Results run/job scope. Unsigned ac filtering
  remains solely the cache refusal rule.
- artifact_store.go keeps metadata and bounded blob blocks together using the
  existing SQLite dependency, FULL-synchronous commits, connection-safe foreign keys/page limits, private paths and a
  hard page cap. No fleet.db change or new database framework. No tokens or
  generated signing keys are persisted.
- artifact_http.go serves all five JSON RPCs plus Azure block PUT/XML commit
  and signed ZIP GET. Own-run PR writes and sibling downloads work; guessed
  other-repo/run IDs cannot widen access. Size/hash checks precede finalize.
- artifact_budget.go and artifact_disk.go share fresh free-byte/inode checks
  and concurrent write reservations across owner, cache upload/commit and npm
  publication. Existing cache routes cannot bypass the guard when enabled.
- artifact_config.go adds mutually exclusive opt-in owner/forward flags.
  Forward mode accepts a literal loopback SSH-forward endpoint, makes no
  artifact auth/storage decisions and refuses redirects/outages explicitly.
  Existing disabled behavior and shipped units remain unchanged.
- Expiry/staging cleanup, stable LRU ties, active read leases, maintenance
  shutdown, bounded transfer progress and loopback metrics are covered.
- ADRs 0034/0078/0097 and OPERATIONS.md record the single-owner exception,
  paths/budgets, transport prerequisites, UI/report limits and rollback.

## Test evidence and reproducible checks

The checked-in tests in internal/cachegate/artifact*_test.go cover:

| Boundary | Evidence |
| --- | --- |
| Forged/expired/wrong issuer/nbf/scope tokens | Signed test keys and JWKS httptest server; every unauthorized RPC refused |
| Repo isolation | Two enabled signed repos with identical run/job/name remain independent |
| PR run isolation | PR R2 cannot list/sign/delete/finalize R1; sibling jobs in R1 exchange bytes |
| Blob capabilities | Method/path/expiry tampering, deleted objects, partial uploads, broken streams |
| Protocol | >8 MiB ordered multi-block transfer, duplicate PUT, filters, overwrite/delete, size/hash |
| Durability | Close/reopen SQLite preserves finalized bytes/IDs; partial objects stay invisible |
| Retention/caps | Absolute expiry, 72-hour cap, activity-based staging expiry, LRU/lease/count bounds |
| Unknown observation | Key/store/stat failures refuse operations, never successful empty state |
| Storage faults | Real SQLite with injected driver I/O failures at publication/read/eviction boundaries |
| Transport | Two forwarding gates, seven consumer lanes, owner outage and redirect refusal |
| Lifecycle | Default off, invalid startup, full Execute owner/forward lifecycle, joined maintenance |

The gate replay uses already-placed jobs at two simulated sites and one real
in-process owner; no scheduler affinity or external services are assumed.
Existing tests/simulation remains part of make ci and exercises unchanged
controller invariants. No broader scheduler world model was added for a
feature that changes no scheduler or VM policy.

Additionally ran the actual published @actions/artifact **2.3.2** SDK offline
against the owner: ZIP upload, list, signed download with digest and extracted
byte validation, then delete/list-empty. It passed. This was a temporary
compatibility probe, not a new npm dependency in the repository; package
extracts/cache and probe source were removed. No GitHub artifact API was used.
The persistent Go protocol/security tests require no Node installation.

Required command uses the installed project toolchain, not the mise shim:

```sh
export PATH="$HOME/.local/share/mise/installs/go/$(grep '^go ' go.mod | cut -d' ' -f2)/bin:$HOME/go/bin:$PATH" GOTOOLCHAIN=local
make fmt
GOMAXPROCS=4 make ci
git diff --check
```

Final validation: make fmt, make ci and git diff --check passed. Overall
coverage was **99.054924%**; cachegate was 99.3% in the CI report. Lint, CPD,
dead-code, vulnerability, full race and build gates were green. Artifact
race tests took 7.729s; fleet simulation took 38.033s in coverage and 302.343s
under race. The cachegate tests also cross-compiled for Darwin/ARM64.

Developer-loop follow-up issue proposal (not published): profile the existing
whole-fleet race simulation at a fixed worker budget; 302.343s dominates this
local CI loop. Preserve seeds, coverage, race and all safety properties. No
performance/check change is included in this feature.
No deadline, coverage, race, permissions or fail-closed checks were weakened.

## Remaining work: separate operational promotion

1. Provision/supervise an SSH local forward over existing cloudflared-backed
   fleet SSH from Omarchy loopback to Ubuntu loopback. Pin host keys, fail on
   forward setup errors, use server-alive limits. No existing artifact tunnel
   is documented; forwarding stays unconfigured until it exists and is tested.
2. Verify physical volume identity, cache copy peak and node reserve budgets;
   choose explicit repository IDs/private owner path. No blanket prod write
   authorization follows from this code commit.
3. Prove guest origin resolution on Podman and Linux Tart; preserve #446
   roaming fallback and direct macOS behavior. Direct/GitHub consumers cannot
   read gate-only data. Reports/UI/gh run download remain unavailable locally.
4. Canary a knee-doctor PR build -> seven E2E lanes with byte identity and zero
   upstream artifact creates and listings, measure cross-site transfer, then
   prove rollback without breaking in-flight handoffs. This session performs
   none of these live operations.

Owner's three questions: one process/store removes discovery and peer fetch;
one verified principal and one shared budget reduce per-route rules; tests
exercise the new authorization/storage/transport boundary deterministically.
No fleet.v1 migration, fleet.db mutation or observe/shadow/authority promotion.
The existing x/sys module is now a direct import for filesystem observations;
no new dependency/version was added.
