# ADR 0097: One gate owns the fleet's run-scoped Actions artifacts

## Status

Accepted for issue #509, with council corrections tracked by PR #510.
Implementation is default off; deployment and promotion are separate tasks.
Amends ADR 0078 decision 2 and ADR 0034's no-cross-node-artifact-path rule.


## Context

GitHub's accrued artifact GB-hours can block every CreateArtifact. The E2E
Build -> seven test lanes handoff needs storage independent of that quota.
Falcondev v9.8.0 (the pinned image), latest release v10.0.0 and researched
upstream HEAD implement cache, not artifact storage. GitHub places sibling
jobs across sites; local scheduling does not provide run affinity.

## Decision

### One owner; forwarding has no artifact state

The gate on **ubuntu-vitalii, site B**, owns the only artifact database.
Omarchy forwards every artifact RPC and blob PUT/GET to that owner. Mini and
MacBook Linux Tart guests already reaching Omarchy follow that route too.
There are no site/node artifact stores, peer lookups, owner discovery, ID
merging, replication, fallback fetches or scheduler changes. Site A's roughly
2 MB/s uplink is an accepted transfer cost, not a reason to choose storage.
This single-owner data path is the narrow exception to ADR 0034. Cache and
npm retain their existing placement and authorization rules.

Serve the five v4 JSON ArtifactService Twirp RPCs and Azure block PUT/block-list
commit/signed ZIP GET protocol in the existing Go gate. Do not replace the
cache backend or add a general Azure API. Use the existing SQLite dependency:
metadata and bounded blob blocks live in one private database, separate from
fleet.db, so finalize is one FULL-synchronous publication transaction without
a filesystem/index split. Stable AUTOINCREMENT IDs are never reused; reject
IDs outside JavaScript's exact integer range. A name is unique per repo/run;
overwrite goes through delete. Retried same-job create, block PUT, block-list
commit and finalize are idempotent; repeated delete returns not-found.

### Authentication and scope

Only the owner verifies runtime JWTs. Forwarders pass bytes/Authorization
without decoding claims or applying cache's PR-write filter. The owner pins
RS256 and issuer https://token.actions.githubusercontent.com and gets keys
from that issuer's fixed /.well-known/jwks endpoint. It requires exp, validates
nbf, bounds key fetching/cache lifetime and unknown-kid retries, refuses
redirects, and ignores JWT-selected jku/x5u network locations. Key-unavailable
or store-unavailable is an error, never an empty list or GitHub fallback.

Use signed repository_id plus exactly one well-formed
Actions.Results:<backend-run>:<backend-job> grant. These are opaque backend
IDs, not numeric GITHUB_RUN_ID. Allow only configured numeric repository IDs.
No new audience requirement is invented: like the pinned cache verifier,
signature/issuer and the service-specific signed scope establish authority;
a generic workflow OIDC token without Actions.Results cannot authorize.
No live token inspection is required. Reruns are isolated by their signed
backend run identity; no promise maps attempts to a numeric GitHub run.

Create/finalize must match the caller's run and job. List authenticates the
consumer job. Sign/delete accept the stored producer job within that same
signed repo/run, because sibling jobs download one another's outputs. Names,
body IDs and database IDs never override that namespace. **PR jobs can write
and read their own run**, with no other-run/default-branch read permission.
This deliberately differs from cache: PR cache writes remain refused.

Blob URLs are transferable capabilities (15 minutes for reads, six hours for
uploads so a 20 GiB site-A transfer can finish), limited by artifact expiry,
object ID and read/write operation. A process-local domain-separated HMAC key
stays in memory; restarting invalidates outstanding URLs, and clients obtain
new URLs by repeating create/list/sign. No runtime JWT, URL signature or
runner credential is persisted or logged. An ID's immutable repo/run binding
prevents capability reuse across namespaces; possessing a read URL remains
possession of a credential, as with Azure SAS. Finalize checks ZIP size/hash.

### Retention and the shared write guard

Honor client expires_at (the SDK translates retention-days); cap at 72 hours
from create, default 72 hours when omitted/0. Reads never extend retention.
Expire abandoned staging after one hour without block activity. A minute
sweep and write-time cleanup reclaim expired records. At a default 20 GiB
logical cap, evict finalized objects by LRU, stable ID breaking ties. Include
staged/unreferenced blocks. Active downloads lease their records; eviction
skips them and delete retries rather than breaking the stream. No full ZIP
is buffered in memory; blocks are at most 16 MiB.

SQLite's hard page ceiling includes 1% proportional slack plus 8 MiB metadata
headroom above the logical cap, so overflow pages do not prematurely exhaust
a large store. SQLITE_FULL evicts one eligible LRU object and retries the
block insertion exactly once; other I/O failures remain explicit. Reject a
block larger than the entire cap before any eviction. Allocated free pages
are reused rather than continually vacuumed. Artifact writes reserve a fixed
three-block peak plus 1 MiB metadata, independent of database file size.
Deletion and pruning are not gated by that write reserve: reclamation must
remain possible at low space. Byte cap and physical reserve are distinct.

One synchronized budget per gate observes fresh filesystem available bytes
and the 5% free-inode floor, then accounts for concurrent artifact, signed
cache-upload/commit and npm writes. Unknown space refuses writes. Cache PUTs
stream unchanged using Content-Length-based reservations; unknown-length
bodies have a fixed 128 MiB bound/reservation. This preserves the client's
128 MiB single-shot uploads. The progress deadline covers the streaming
request and releases reservations on a stall. Only block-list commit and
finalize reserve the additional cache copy peak, which must be explicitly
configured to the sidecar's cache size cap (there is no 60 GiB default).
Npm reserves per streamed chunk through publication. Cache sidecars remain
loopback-only. write-budget-path must name the actual common filesystem;
owner/npm paths are checked for the same device. Operator proof that the
sidecar volume is on it remains required.

The default reserve is 60 GiB, at least the node's configured minFreeDiskGb.
ADR 0006's guest virtual capacity floor is unchanged. Admission alone does
not protect running writers. Guarded writes cannot spend their shared reserve;
unrelated host writers can still race filesystem observations, so this is
not a filesystem-wide hard quota. Low-space reads remain available. SQLite
may still refuse a deletion if the filesystem cannot create its journal;
never manufacture success or mutate fleet VM state to reclaim artifact space.

### Transport and guest origins

Existing fleet access is SSH through cloudflared; repository docs explicitly
keep cache payloads off cloudflared. There is no existing documented artifact
transport. The smallest addition is an operator-managed **SSH local forward**
from Omarchy loopback to Ubuntu's artifact-only loopback listener, over that authenticated,
encrypted SSH connection, with known-host verification, ExitOnForwardFailure
and server-alive limits. This is an artifact-only exception to the admin/health
transport restriction, not permission to tunnel npm or Actions cache data.

Forward mode therefore accepts only an explicit HTTP literal-loopback endpoint
with a port, intended for that local forward; no public plaintext upstream.
The owner exposes an additional `--artifacts-listen` loopback listener
(default 127.0.0.1:3021) serving only ArtifactService and signed artifact blob
paths. Every cache, npm, identity, health and other path there returns 404.
The SSH forward must land on this listener, never the whole :3017 gate; local
processes on the forwarding host therefore gain no cross-site cache path.
The gate does not create tunnels or manage SSH credentials. It returns an
explicit unavailable error if the forward is down, refuses redirects, and
never falls back to GitHub. Deployment must provision and verify this tunnel;
no host, unit or credentials have been touched by implementation work.

Signed blob origins must resolve to each guest's gate, not the Ubuntu owner's
private tunnel endpoint. The existing common origin
http://host.containers.internal:3017 works for Podman and the documented Tart
hosts mapping: each gate forwards the same blob namespace. No untrusted Host
or X-Forwarded-* field selects an origin or target. ACTIONS_RESULTS_URL stays
at the gate origin; its path is discarded by artifact v2.

### Clients, failure reports and rollback

Clients unable to reach a gate (unconfigured macOS Tart, roaming MacBook via
#446) keep GitHub unchanged. Explicit github-token downloads also bypass the
gate through REST. A producer on one storage system cannot supply a consumer
on the other; operators must verify handoff reachability before rollout.
There is no automatic bridge or fallback between those storage systems.

**Failure reports stay local too. No GitHub exception and no viewer.** Local
artifacts do not appear in GitHub UI, REST or gh run download. The action's
hardcoded artifact-url output is unusable for these IDs. This accepted limit
includes Playwright reports. Ordinary gate-reaching v4 workflow steps need no
change; GitHub REST tooling is not transparently compatible.

Default off preserves artifact passthrough. Removing owner/forward mode makes
leftover artifact-related flags inert with a warning, allowing rollback
without removing every associated option. Rollback: complete/drain affected
runs before disabling forwarding/owner or restoring prior executor.image /
Tart baseVm and gate config. Preserve owner storage and the serving routes
until existing consumers finish; changing routing mid-run loses access.
New direct uploads may encounter GitHub's original quota. No automatic
cutover, authority enablement, service installation or fleet.db migration.
Loopback metrics separate artifact transfer bytes from cache/results/npm;
operator capacity/eviction/write-refusal metrics document storage pressure.
No repo/run/job/name/signature metric labels.

## Deployment evidence still required

Provision the SSH local forward and verify its identity, routing, throughput
and failure behavior; agree byte budgets against real sidecar volumes/node
floors; prove guest-resolvable origins and the PR build -> seven lanes
handoff with byte identity and zero upstream artifact uploads. Verify rerun
behavior with pinned client versions. Test actual rollback. These are isolated
operational evidence tasks, not missing owner discovery or a new auth design.

## Consequences and owner's three questions

Remove overengineering: one store, existing process and SQLite dependency;
no discovery, peer fetch, viewer or new scheduler. Reduce complexity: one
verified repo/run predicate and one shared budget. Testability: deterministic
signed-token, protocol, persistence, expiry/LRU, forwarding and disk-pressure
tests exercise the new boundary; runtime/fleet safety policy is unchanged.
Observe/shadow and VM mutation authority limits remain intact.

## Council constraints and consequences

Network request bodies and RPC response writes never hold the SQLite mutex.
Capabilities are checked before bounded body reads, with no disk reservation
while waiting for the network, then object state/expiry is rechecked under
the mutex before persistence. Finalize hashes immutable committed blocks
outside the mutex under a lease; a finalized replay compares stored size/hash
without streaming. Slow transfers do not serialize unrelated run operations.

A non-blocking, store-wide 16-slot semaphore bounds buffered upload bodies
across both owner listeners: at most 256 MiB of block payload (16 MiB per
slot, excluding allocator and SQLite overhead). Block-list reads share the
same conservative slot weight. Acquire before reading/allocating a body and
hold through persistence, including time queued for the store mutex. Full
capacity returns 503 without reading or waiting, allowing SDK retry. Busy
HTTP/1 connections are closed so net/http does not drain an unread trickling
body before flushing the rejection. Failed
reads, rejected state/budget checks and successful writes all release their
slot. This memory bound is independent of the disk write reserve; a valid
write capability cannot admit unbounded trickling PUT buffers.
Storage gauges are refreshed at startup and on the existing minute prune
tick, not by full-table aggregates on each HTTP request.

There is deliberately **no per-run eviction quota**. Global LRU can evict an
active run's finalized artifact under cap pressure once no transfer lease
protects it. Size the owner cap for concurrent runs; own-run authorization
does not promise retention against eviction by other runs. No scheduler or
new run state is introduced to mask that tradeoff.

JWKS keeps a 30-second refresh limit with one immediate bonus refresh when
an unseen kid appears after keys were loaded. The bonus is bounded per
refresh window, not per arbitrary attacker-supplied kid. Existing verified
keys keep their original expiry on a failed refresh.

## Evidence and validation

Pinned cache image: v9.8.0, source `c866ae42315af2cb5e165a23a414228fd2fc035f`;
its [catch-all](https://github.com/falcondev-oss/github-actions-cache-server/blob/c866ae42315af2cb5e165a23a414228fd2fc035f/routes/%5B...path%5D.ts)
forwards artifacts. Latest researched v10.0.0
[`c829dd6f58b28b785e090dd938ec1e4ee7c477a2`](https://github.com/falcondev-oss/github-actions-cache-server/blob/c829dd6f58b28b785e090dd938ec1e4ee7c477a2/proto/cache.proto)
also implements only CacheService.

The v4 upload action snapshot
[`ea165f8d65b6e75b540449e92b4886f43607fa02`](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L3515-L3580)
locks artifact v2.3.2: JSON Twirp at ACTIONS_RESULTS_URL's origin with the
runtime bearer token, CreateArtifact -> Azure block PUT/XML block-list ->
FinalizeArtifact. Download snapshot
[`d3f86a106a0bac45b974a628896c90dbdf5c8093`](https://github.com/actions/download-artifact/blob/d3f86a106a0bac45b974a628896c90dbdf5c8093/dist/index.js#L2299-L2340)
lists with consumer IDs, signs with producer IDs and fetches ZIP bytes.
Explicit github-token inputs choose REST and bypass the gate. A real v2.3.2
SDK offline upload/list/download/digest/delete probe passed during initial
implementation; it introduced no repository npm dependency.

`internal/cachegate/artifact_council_test.go` records deterministic regressions
for #510's council findings, including stalls, large cache PUTs, page pressure,
reclamation, rollback, gauges and the artifact-only listener. Existing security,
SQLite fault, restart and split-site seven-lane replay tests remain in place.
No fleet.v1 or fleet.db migration and no observe/shadow authority promotion.
