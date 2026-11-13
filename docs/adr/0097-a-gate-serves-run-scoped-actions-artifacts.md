# ADR 0097: A gate serves run-scoped Actions artifacts

## Status

Draft for issue #509. Research and proposed decision only; no production
implementation, deployment or authority promotion. Amends ADR 0078 decision 2
and proposes a narrow exception to ADR 0034's no-cross-node-artifact-path rule.
[Research and immutable upstream evidence](../ARTIFACT_GATE_RESEARCH_509.md).

## Context

GitHub artifact GB-hours block CreateArtifact even after objects are deleted.
The E2E Build -> seven E2E lanes handoff must work without GitHub storage and
without changing normal upload-artifact@v4/download-artifact@v4 steps. Cache
storage is not artifact storage. Pinned falcondev v9.8.0, latest release v10.0.0
and current upstream HEAD forward artifacts; none implements ArtifactService.

A run may span nodes/sites; a fallback cannot download local-only bytes from
GitHub. A roaming or macOS guest still using GitHub cannot see gate-only data.
GitHub REST clients and its web UI do not consult the gate. These are
correctness constraints, not performance details.

## Decision

### 1. Add a minimal artifact store to the existing gate

Keep falcondev unchanged for cache. Serve JSON Twirp CreateArtifact,
FinalizeArtifact, ListArtifacts, GetSignedArtifactURL and DeleteArtifact in
fleet cache-gate. Implement just the Azure block PUT / XML block-list commit
and signed GET protocol used by artifact v2, not a general Azure service.
Use a distinct `/.trf/artifacts/blobs/` namespace; existing cache blob routes
keep their owner. Logs and other results methods continue to proxy to GitHub.
Disabled by default, with an explicit artifact directory enabling the feature.

Proposed root: `~/.local/share/tart-runner-fleet-artifacts` on Linux, outside
fleet.db; a private metadata index plus staged blocks and finalized ZIPs.
Keep opaque generated object paths, never workflow names in filesystem paths.
Use durable metadata and atomic publication; acknowledge finalize only after
bytes and metadata survive restart. Enforce unique names per run, stable
numeric IDs safe for JavaScript, deterministic list ordering/filtering,
idempotent retry outcomes and overwrite via DeleteArtifact. Hidden partial
uploads get a short bounded expiry and count toward capacity. Leased active
transfers cannot be evicted; restart reclaims orphaned partials safely.
Index choice (existing SQLite dependency or atomic manifests) remains open
until the restart/concurrency spike; do not add a second general database API.

### 2. Verify authorization before any artifact access

Introduce a real runtime JWT verifier with injectable clock/key source. Pin
issuer and algorithm allowlist, verify signature, require exp and validate
nbf when present. Resolve keys only from operator-approved GitHub discovery/
JWKS URLs, never JWT jku/x5u/iss-selected network locations. Bound fetches,
key size/cache lifetime and unknown-kid refresh; unavailable verification
refuses the operation, not an empty artifact list or GitHub fallback.
Audience contract is a blocking open question below; do not invent one from
an unrelated OIDC example. Do not disable verification in production.

A verified principal contains repository_id and exactly one well-formed
Actions.Results:<backend-run>:<backend-job> scope. Reject absent, ambiguous or
malformed scopes. Namespace every index lookup and capability by signed repo
ID + backend run ID. A token for repository B cannot access repository A even
if it supplies A's object ID/name/backend IDs. Unknown/non-enabled repositories
are denied local creation rather than implicitly admitted.

Create/finalize require the request run and job to match that principal.
List requires the current consumer's run/job. GetSignedArtifactURL and delete
may name the **producer** job returned by a same-run list; verify it belongs
to that same repo/run/object, not that it equals the consumer job. Never trust
request IDs or mere possession of a database_id. Same-run sibling jobs may
read and overwrite/delete artifacts as required by v4 compatibility.

PR jobs may create/finalize/read/delete **only within their own signed run**.
No branch/default-run artifact visibility. This deliberately differs from
ADR 0078's cache rule: PR cache writes remain denied while PR artifact writes
are necessary for build-to-test handoffs. Trusted jobs also get only their
own run in the initial design. No runtime-token cross-run API is added.

Signed blob URLs are short-lived bearer capabilities, bound to opaque object,
repo/run, upload-or-download permission and expiry; no raw JWT is persisted.
Use separate signing domain/key from gate identity. Read capability is a
transferable credential (as Azure SAS is); run isolation is enforced at
issuance, not by a nonexistent bearer header on anonymous blob GETs. Never
log capability query strings. Changed paths, verbs, expired signatures and
cross-object reuse fail. Publish only checked committed ZIP size/hash.

### 3. Bound storage and preserve physical disk reserve

Expiry is min(client expires_at, created_at + 72 hours); missing expiry means
72 hours, already-expired requests fail. Client retention-days=0 means default,
not unlimited. Reads never extend absolute retention. LRU eviction among
unleased finalized objects applies at a configured byte cap; ties break by
stable ID. Account for temporary blocks, reservations, metadata overhead and
inodes; byte cap and TTL are independent. A proposed default is 20 GiB per
gate, subject to operator budget validation alongside the existing 60 GiB
cache cap. Expired/deleted URLs no longer expose data.

One write-budget guard must cover artifact and cache writes (and npm writes
when enabled) on the same filesystem. Use fresh available-space/inode
observations plus synchronized in-flight byte reservations before each
bounded block/commit. Deny new writes when the physical reserve cannot be
proved, continue safe reads/deletes. Cover existing falcondev blob routes at
the gate, not just Create/Finalize; committed-copy peaks must be reserved.
No direct guest access to the cache sidecar. Use an injected filesystem probe
and one named budget predicate rather than a second scheduler policy.

ADR 0006's guest diskGb floor remains intact; the host floor is the existing
minFreeDiskGb admission reserve, not a virtual-disk size. Admission does not
stop running writers. A gate guard cannot reserve space against unrelated
host writers atomically; headroom and filesystem hard quotas/reservations
must be evaluated before claiming a strict host-wide guarantee. Stale probe
or exhausted reserve cannot become an authorization to delete fleet VMs.

### 4. Make ownership and fallback explicit

Prefer one artifact owner per repo/backend-run and same-node/site consumers;
site A's Mini/MacBook Linux guests can already share Omarchy's owner. No new
cross-node scheduler, placement pass or run-affinity promise in this change.
GitHub chooses among advertised matching scale sets; site A's ~2 MB/s uplink
makes remote reads a correctness fallback, not the fast path.

Before allowing a split-site run to create local artifacts, every participating
client must reach the owner directly or through a configured authenticated
owner-fetch path. Proposed minimal extension: bounded operator-configured
peer gates, no service discovery/roster/replication service. Peers verify the
original runtime principal themselves over authenticated encrypted transport;
local gates never forward runtime JWTs to arbitrary redirect targets. Preserve
repo/run checks, owner-local IDs and capability expiry. Reject loops, bound
fan-out/deadlines/concurrency; unavailable owner is an explicit unavailable
result, never an empty authoritative list. Exact run-to-owner mechanism is a
blocking open question; mixed local/GitHub ID spaces must never collide.

On a genuine local/owner miss, GitHub lookup is allowed for an artifact that
was actually stored there. An auth failure never triggers fallback. GitHub
cannot recover a local-only artifact or rescue a partially completed local
upload. Choose backing service for an upload before CreateArtifact, never
switch at finalize. Until complete owner discovery and merged-list semantics
are proven, enable only coherent single-owner cohorts; other cohorts keep
GitHub for the **whole producer/consumer handoff**. Zero GitHub bytes across
arbitrary sites is not achieved by a local store alone.

### 5. Preserve direct clients and choose failure-report behavior

Keep ACTIONS_RESULTS_URL at the gate origin for eligible images; issue blob
URLs using an origin the guest can resolve/reach. Preserve signed gate identity
and #446 bootstrap fallback, add guest-origin reachability evidence before
selecting local storage. macOS Tart without the override, off-LAN MacBook,
GitHub-hosted runners and explicit github-token REST downloads keep GitHub.
A host reachability probe alone is insufficient.

Local artifacts do not appear in GitHub UI, REST artifact listings or
`gh run download`. upload-artifact's artifact-url output is hardcoded to a
GitHub URL and will be unusable for local IDs; the gate cannot change it in
its RPC response. Document this compatibility limit explicitly.

Proposed failure-report decision: keep failure reports local too, with console
failure summaries initially; do not add a public viewer in this safety repair.
The gate cannot infer workflow `if: failure()` from a runtime token or RPC.
If durable browser reports are required, an explicit workflow step must opt
into GitHub (restore ACTIONS_RESULTS_URL for that upload) or a separately
reviewed authenticated viewer. That opt-in consumes GitHub quota and is outside
the unchanged-workflow/zero-upload acceptance cohort. Operator acceptance of
this tradeoff is required before rollout; no silent name-based routing.

### 6. Rollback and observability

Default-off gate restores artifact RPC passthrough. For full reversal, restore
previous executor.image or Tart baseVm plus gate binary/config, preserving
#446 direct-GitHub behavior. New uploads use GitHub and may hit its original
quota. Keep old owner storage/read routes available for in-flight runs through
the retention window; do not switch a run midway or delete bytes on rollback.
Drain/complete affected runs before removing the old gate. Operational
promotion remains a separate explicit task, with rollback proof.

Extend loopback metrics: artifact RPC count/latency by bounded method/result,
JWT refusal by bounded reason, upload/download/owner/GitHub bytes, used/staged/
reserved bytes, object count, evictions by ttl/lru, write-guard refusals,
owner unavailable and verification-key refresh failures. No token, signed URL,
artifact name, repo/run/job labels. Document paths, caps, effective expiry,
UI limitations and rollback in OPERATIONS.md when implementation lands.

## Open questions (must resolve before broad enablement)

1. What exact issuer/audience/algorithm and rerun backend-ID guarantees apply
   to this fleet's runtime tokens? Confirm from upstream signed fixtures, not
   printed live tokens; distinguish backend run identity from numeric run ID.
2. How does a gate select/discover the unique owner and preserve artifact ID
   identity across mixed local/peer/GitHub results without per-run coordination?
   A configured peer list alone does not answer that question.
3. Which handoffs include direct macOS/off-LAN/REST consumers, and how can they
   be kept wholly GitHub-backed without changing normal workflow steps?
4. Can cache SDK copy/commit peaks and other host writes fit a shared reserve
   with gate accounting, or is a filesystem quota/reservation required? Agree
   initial artifact budget, partial-upload TTL, capability TTL and headroom.
5. Is losing clickable Playwright reports acceptable for the first cohort?
   If not, choose an explicit GitHub exception or authenticated viewer scope.
6. Which durable index is smallest while proving restart-safe finalize and
   concurrent eviction/download? How are local numeric IDs kept disjoint?

## Consequences

Normal same-run v4 transfer can avoid GitHub storage with byte-identical ZIPs.
The gate becomes an authorization and durability boundary; existing unsigned
cache filtering cannot be reused as artifact authorization. Cross-site fetch
is a new ADR 0034 exception and needs evidence, not a new fleet control plane.
Shorter retention, LRU eviction and owner outages can break delayed consumers;
clients must receive explicit errors. No fleet.v1 field/enum or fleet.db
migration is proposed; a new private artifact index needs its own recovery
contract. Observe/shadow and all VM mutation authority limits remain intact.

Owner questions: remove overengineering by keeping the cache backend and the
existing gate process; reduce complexity by one artifact principal predicate
and one write-budget predicate, no placement pass; test through deterministic
policy, protocol integration and multi-node simulation before promotion.
