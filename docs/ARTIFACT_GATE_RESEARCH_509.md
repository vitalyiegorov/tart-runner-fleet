# Artifact gate research — issue #509

Design only, researched 2026-11-13. Repository baseline:
`469687e` (origin/main). No node inspection or live-service experiment was done.
Issue: https://github.com/vitalyiegorov/tart-runner-fleet/issues/509.
Companion: [draft ADR 0097](adr/0097-a-gate-serves-run-scoped-actions-artifacts.md)
and [implementation plan](ARTIFACT_GATE_PLAN_509.md).

## Upstream server: reuse does not provide artifact storage

The shipped `fleet-actions-cache` image is **v9.8.0**, digest
`sha256:3e685f12100dfb4c9947afb504c85c78c69eb882ce7fa6be2fb3348fd49316ea`
(`deploy/actions-cache/tart-runner-fleet-actions-cache.container:10-20`). The
version is the quadlet's declaration, not a registry provenance attestation.
The v9.8.0 source tag resolves to `c866ae42315af2cb5e165a23a414228fd2fc035f`.

It implements only CacheService's CreateCacheEntry,
FinalizeCacheEntryUpload and GetCacheEntryDownloadURL, plus cache blob
upload/download. None of the five ArtifactService RPCs is implemented. The
[catch-all](https://github.com/falcondev-oss/github-actions-cache-server/blob/c866ae42315af2cb5e165a23a414228fd2fc035f/routes/%5B...path%5D.ts)
forwards unknown paths to DEFAULT_ACTIONS_RESULTS_URL. Its cache-signed blob
routes are not an artifact store or artifact index.

Also checked the newest published version tag **v10.0.0**,
`c829dd6f58b28b785e090dd938ec1e4ee7c477a2`, and current default-branch HEAD,
`a13ebc80174ed1b4b9c908e0a9d88994e1f43f31` (latest means these observed
snapshots, not a floating compatibility promise). Both have only CacheService
in [proto/cache.proto](https://github.com/falcondev-oss/github-actions-cache-server/blob/c829dd6f58b28b785e090dd938ec1e4ee7c477a2/proto/cache.proto),
cache transfer routes and results passthrough in
[v10 routes](https://github.com/falcondev-oss/github-actions-cache-server/blob/c829dd6f58b28b785e090dd938ec1e4ee7c477a2/src/routes.rs)
and [HEAD routes](https://github.com/falcondev-oss/github-actions-cache-server/blob/a13ebc80174ed1b4b9c908e0a9d88994e1f43f31/src/routes.rs).
There is no ArtifactService, artifact metadata, or artifact-owned signed blob
flow. Upgrading the cache image does not solve #509. Recommendation: a minimal
artifact handler/store inside the existing Go gate; retain falcondev for cache.

## Exact v4 client protocol

Inspected moving v4 action refs at immutable snapshots:

- upload-artifact `ea165f8d65b6e75b540449e92b4886f43607fa02`;
- download-artifact `d3f86a106a0bac45b974a628896c90dbdf5c8093`.

Both lockfiles resolve **@actions/artifact 2.3.2**. Evidence below uses their
checked-in bundles, so it describes the shipped v2 client rather than toolkit
main (currently artifact 6.3.1, which is not the target). Also inspected the
published 2.3.2 tarball; no dependencies were installed.

### RPC and identity

Base URL is `new URL(ACTIONS_RESULTS_URL).origin`: path/query prefixes are
ignored. Each RPC is JSON HTTP POST to
`/twirp/github.actions.results.api.v1.ArtifactService/<method>`, authenticated
by `Authorization: Bearer <ACTIONS_RUNTIME_TOKEN>`, using the runtime token,
not GITHUB_TOKEN or a newly requested id-token. See
[config and Twirp transport](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L2720-L2768)
and [origin selection](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L2883-L2899).
The generated JSON wire uses snake_case keys, decimal strings for int64s,
RFC3339 timestamps and proto JSON wrapper values; do not copy internal
camelCase TypeScript objects as an assumed wire schema. See
[generated clients](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L1770-L1875).

The client decodes JWT `scp`, splits space-separated scopes and finds exactly
`Actions.Results:<workflowRunBackendId>:<workflowJobRunBackendId>`. These are
backend identifiers, not the numeric GITHUB_RUN_ID/GITHUB_JOB. Client-side
decode does not verify the signature. See
[backend ID extraction](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L3100-L3170).
The gate must verify first, obtain signed `repository_id`, then bind every
request/object to that repository and backend run. `ac` is a JSON-string cache
scope grant list (Permission bit 2 grants write); it is not the artifact grant.
Do not require `id-token: write` or use workflow-supplied repo/run fields as
proof. Exact audience and optional runtime claims must be established from
upstream fixtures before enforcing a new claim contract; no production token
was fetched or decoded for this research.

### Upload

1. CreateArtifact carries run/job backend IDs, name, version **4**, optional
   expires_at. retention-days becomes an absolute expiry after client-side
   GITHUB_RETENTION_DAYS clamping; unset/0 omits it.
2. Response has ok + signed_upload_url. Client ZIPs files, hashes ZIP bytes
   SHA-256 and calls Azure BlockBlobClient.uploadStream (8 MiB chunks, default
   concurrency 5). It performs PUT to the signed URL with `comp=block` and
   `blockid=<base64 ID>` per chunk, then PUT `comp=blocklist` with XML ordered
   block IDs (`<BlockList><Latest>...</Latest></BlockList>`). Both PUTs expect
   HTTP 201; commit is application/xml. Preserve the signed query parameters
   when adding comp/blockid, accept SDK x-ms-version/request headers and return
   compatible ETag/Last-Modified headers. A single simple PUT endpoint is
   insufficient.
3. FinalizeArtifact carries run/job IDs, name, decimal-string ZIP size and
   optional hash `sha256:<hex>`; returns ok + decimal-string artifact_id.
   Only finalized data may become listable/downloadable. No GitHub REST
   registration happens separately.

Evidence: [upload flow](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L3515-L3580),
[blob adapter](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L3260-L3335),
[block query parameters](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L31435-L31475),
[SDK stream implementation](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L42591-L42630).
[SDK request/response specs](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L34628-L34752)
pin the two PUTs and HTTP 201 responses.
Signed blob requests use the URL capability, not the runtime bearer header;
capabilities need independent expiry, operation and object binding.

### Download and delete

ListArtifacts carries the consumer's backend run/job IDs and optional
name_filter/id_filter. The response must include database_id, name, size,
created_at and the producer's run/job IDs (and digest when supported).
Download by ID lists with id_filter, then GetSignedArtifactURL uses the
**producer job ID from the returned artifact**, followed by an HTTP GET of
signed_url, ZIP extraction and optional digest comparison. A different job
in the same run must work. DeleteArtifact similarly lists by name, selects
newest database_id, and deletes using producer IDs. upload-artifact overwrite
uses deletion first, so DeleteArtifact cannot be omitted.
Evidence: [download internal flow](https://github.com/actions/download-artifact/blob/d3f86a106a0bac45b974a628896c90dbdf5c8093/dist/index.js#L2299-L2340),
[delete flow](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js#L2084-L2115).

Exception: supplying download-artifact's **github-token** selects `findBy`;
the library uses GitHub REST list/download APIs (redirect to ZIP), bypassing
ACTIONS_RESULTS_URL entirely. Cross-run/repository REST access therefore
continues to see only GitHub artifacts. See
[action selection](https://github.com/actions/download-artifact/blob/d3f86a106a0bac45b974a628896c90dbdf5c8093/src/download-artifact.ts#L52-L66)
and [client dispatch](https://github.com/actions/download-artifact/blob/d3f86a106a0bac45b974a628896c90dbdf5c8093/dist/index.js#L1930-L2015).

### Rewriting

Keep a gate **origin**, e.g. http://host.containers.internal:3017/, in
ACTIONS_RESULTS_URL; intercept the five artifact RPCs at their existing root
paths, issue absolute signed URLs reachable from the guest under a distinct
artifact blob prefix. No `/artifact` suffix on the env URL will route it.
Cache blobs must retain their existing prefixes. Continue forwarding log and
unrecognized results traffic to GitHub with its Host header. No generic
rewriting of arbitrary returned URLs, redirects, or bearer forwarding to peers.

## Existing gate and disk boundaries

- `internal/cachegate/policy.go:1-15,26-44,83-114,163` explicitly reads **unsigned**
  ac claims only to refuse cache writes. CacheService and /upload/,
  /devstoreaccount1/, /download/, /health go to falcondev. Other results calls,
  including artifacts, go to GitHub. Operator routes are denied.
- `internal/cachegate/server.go:55-95,141-146` wires reverse proxies; the GitHub
  proxy rewrites Host. `server_test.go:43-110` and `policy_test.go:135-170` pin
  refusal-before-proxy and artifact passthrough. Extend these exact seams.
- Signature validation is in falcondev, **not the gate**. Pinned
  [lib/scope.ts](https://github.com/falcondev-oss/github-actions-cache-server/blob/c866ae42315af2cb5e165a23a414228fd2fc035f/lib/scope.ts#L7-L127)
  (default issuer in
  [lib/schemas.ts](https://github.com/falcondev-oss/github-actions-cache-server/blob/c866ae42315af2cb5e165a23a414228fd2fc035f/lib/schemas.ts#L63))
  discovers jwks_uri from a configured issuer, falls back to
  `<issuer>/.well-known/jwks`, permits an operator JWKS override and uses jose
  jwtVerify with issuer. Default issuer is token.actions.githubusercontent.com;
  the quadlet sets SKIP_TOKEN_VALIDATION=false. It does not explicitly enforce
  an artifact audience or Actions.Results grant. Artifact auth cannot be
  implemented by calling the existing unsigned claimedScopes helper.
- `internal/cachegate/identity.go:21-31` proves the **gate to the node** using
  an ed25519 challenge; this is separate from runtime-token verification.
- Important correction to #509: **ADR 0006 is a guest virtual-disk capacity
  floor**, not a cache write guard. Its host-reserve consequence is implemented
  by `internal/executor/host.go:143-154`: stale host observations refuse
  admission; free disk below MinFreeDiskGB refuses new guests. It does not stop
  an already running cache/artifact writer. `docs/OPERATIONS.md:2174-2182`
  budgets 60 GiB cache per Linux node; no shared gate-level disk write guard
  exists today. A real shared guard across falcondev writes, npm and artifact
  writes is new work; fixed caps alone cannot guarantee a physical reserve.
- Existing metrics `internal/cachegate/counters.go:12-45` count cache/results/
  npm bytes. Extend without token/name/run-ID cardinality or signed URL logs.

## Reachability and placement (documented state, not live verification)

| Guest/node | Gate path today | Implication |
| --- | --- | --- |
| Omarchy Linux Podman, site A | host.containers.internal:3017 -> own gate | Local artifact candidate |
| ubuntu-vitalii Linux Podman, site B | same name -> its own gate | Separate store, no shared site-A bytes |
| Mac mini / MacBook **Linux ARM64 Tart** at home | 192.168.1.64:3017 -> Omarchy; host.containers.internal mapped in guest | Same-site shared owner already reachable |
| MacBook Linux ARM64 Tart roaming | signed gate proof fails; bootstrap rewrites .env to GitHub before runner starts | Keep GitHub; running jobs do not migrate |
| **macOS Tart** guests | No documented artifact-gate image override | Keep GitHub unless separately proven/configured |

Mini/MacBook Linux guests are Tart, not Podman. The original #446 timeout is
now addressed in code/ADR, not an unsolved blanket reachability assumption:
ADR 0078 amendment 2026-11-04 and `internal/lifecycle/cachegate.go:17-25,81-95`
rewrite rather than delete the override (patched workers ignore the original
job message). Host proof is not a guest-network proof; npm already adds an
in-guest probe (`cachegate.go:63-76`), a useful extension for artifact selection.
Evidence: ADR 0078:158-239; #446
https://github.com/vitalyiegorov/tart-runner-fleet/issues/446.

Run jobs are **not pinned to one node or site**. ADR 0034:298-314 explicitly
has no cross-node scheduler or artifact path; ci-shared is same-node only.
Later shared-label federation lets GitHub choose a node's scale set advertising
capacity: ADR 0060:42-47 has no Omarchy-first guarantee; ADR 0082:28-50,65-74
records placement by GitHub when queued. Sibling jobs may land on different
sites or architectures; local scheduler fairness is not run affinity. The
run ID in `internal/domain/domain.go:14-27` does not create such affinity.
Prefer already available same-node/site labels when possible, but do not
claim a scheduler can force this today. Cross-owner discovery and transport
are a new ADR 0034 exception and a rollout prerequisite for split runs.

## Owner decisions after research

The historical alternatives/open questions above are superseded by ADR 0097:
Ubuntu is the single owner, other gates forward all artifact traffic over a
configured loopback SSH forward, and failure reports remain local without a
viewer. No per-site stores, peer fetch, ID merging or owner discovery will be
built. Existing cloudflared transport is admin/health only (OPERATIONS.md:2287
and ADR 0093); a new artifact-only SSH local forward must be provisioned before
enabling forwarding. No node or service was contacted to validate that path.
