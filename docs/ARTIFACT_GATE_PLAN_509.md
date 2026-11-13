# Implementation plan — issue #509

Design only. See [research](ARTIFACT_GATE_RESEARCH_509.md) and
[draft ADR 0097](adr/0097-a-gate-serves-run-scoped-actions-artifacts.md).
Each step is one focused PR with failing deterministic tests before code,
green make ci, issue progress and its own validated commit. Do not ship a
red-only regression as a completed fix. No fleet-node action in this phase.

## 1. Local protocol, authorization and bounded durable storage

Smallest useful vertical slice: opt-in, one owner, otherwise current proxy.
Resolve runtime JWT contract/index questions first. Implement five JSON RPCs,
Azure block upload/list commit and signed blob download with one verified
repo/run principal. Put pure auth/expiry/budget decisions apart from HTTP and
filesystem I/O; inject clock, keys, signer, available-space probe and index.
Keep runtime tokens out of disk/logs. Local artifact configuration stays off
by default; make no scheduler/domain-state addition.

Files: extend internal/cachegate/{policy,server,counters}.go and their tests;
new focused artifact policy/token/store/protocol files with paired tests under
internal/cachegate (split by responsibility, not a generic framework).
Reuse host reserve semantics from internal/executor/host.go through a named
predicate if extraction is needed; do not call broad host admission for each
upload. Update cache blob routing to share budget reservations; include npm
writes in the guard when enabled. Extend deploy/actions-cache gate service
and internal/cachegate/deploy_test.go for absolute data path/caps, leaving the
cache-server version unchanged. Update ADR 0078's routing description,
ADR 0097 status, docs/{OPERATIONS,SECURITY,TESTING}.md in the same PR.
If reserve enforcement against sidecar commits cannot fit this slice safely,
keep local serving disabled until that guard is complete; do not call caps a
physical disk guarantee.

Deterministic red tests (real test keys + fake clock, no production tokens):

| Rule | Unit red test | HTTP integration red test |
| --- | --- | --- |
| Forged token never authorizes | Modified payload/signature, alg=none, wrong key/issuer, expired/missing exp, future nbf, malformed/ambiguous Results scope rejected | Each RPC denied; zero store/peer/GitHub calls, no capability issued |
| Other-repo token cannot read/write target | A object under repo A, signed repo B token with A body IDs rejected or not found | Create/finalize/list/sign/delete A object with B token; A unchanged, no bytes disclosed |
| PR cannot read another run | PR run R2 versus object R1 in same repo denied, including forged body and guessed ID/name | List/get-signed/delete/create/finalize R1 using R2 token; no URL/data/effect; no fallback |
| PR own-run writes allowed, cache rule unchanged | Same-run PR create/finalize allowed; ac PR cache save denied | PR ZIP uploaded/finalized/downloaded by sibling job; cache save still 403 |
| Producer job versus consumer job | Create/finalize must match caller; list authenticates caller; sign/delete accept stored producer in same run | Two job tokens in one run transfer and overwrite; producer-job spoof from other run denied |
| Blob capabilities cannot widen access | Expiry/object/repo/run/verb/domain tampering denied | No runtime bearer on valid blob PUT/GET; altered IDs, paths, method, expiry, read-to-write conversion denied |
| Auth/key availability fails closed | Unknown kid bounded refresh, approved-source-only discovery, stale keys policy, unavailable key source | JWKS httptest stub; no jku/redirect/attacker fetch; failure is error, never empty list/fallback |

Protocol/store tests: use httptest.NewServer, t.TempDir and t.Cleanup in the
style of server_test.go and npmcache_test.go. Signed test JWTs must not use
policy_test.go's deliberately junk signature fixture. Keep an immutable
wire transcript or generated JSON fixtures from pinned v2 bundles. Offline
Node compatibility fixture drives the actual artifact v2 SDK against test
server, not live GitHub/Azure; a Go round-trip alone cannot prove Azure SDK
compatibility. Pin dependencies in that future fixture, do not install now.

- All five RPC paths, POST/content-type/errors; snake_case, wrappers,
  RFC3339 timestamps, decimal int64 IDs; reject bad/oversized bodies.
- Create -> parallel/retried/out-of-order block PUT -> XML ordered block-list
  -> finalize -> filtered list -> signed GET; compare ZIP SHA-256 and extracted
  file bytes. Include >8 MiB multi-block input and a tiny file. Exercise name,
  ID, all/pattern downloads, merge-multiple and overwrite deletion.
- Missing/duplicate blocks, invalid XML/base64 IDs, path traversal/symlink,
  unbounded block count/body, wrong size/digest, duplicate finalize, partial
  upload disconnect, crash/restart between each publication boundary.
- No partial/staged object listed; restart retains finalized objects and stable
  IDs; delete idempotency/retry and name reuse never expose old bytes.
- Fake clock: requested 1 day, requested >3 days, absent/0, already expired,
  exact deadline; reads do not extend expiry. LRU stable ties, cap including
  staged/in-flight reservation bytes; active read lease/eviction race.
- Fresh/stat failure/low bytes/low inodes; concurrent cache + npm + artifact
  writes cannot individually pass and collectively spend the reserved headroom;
  declared-length lies/chunked requests and commit-copy peaks are bounded.
  Disk-full after partial upload returns an error, preserves existing objects,
  and cleans only owned staging. Reads/deletes remain available. Race suite.
- Token and capability sentinels absent from every captured log/error/metadata;
  metrics have bounded labels, count actual bytes, stay loopback only.
- Feature disabled preserves artifact/log GitHub passthrough, cache routing,
  operator 404s, npm behavior and gate identity. No auth error falls through.

## 2. Owner fetch and coherent run routing

Resolve owner mechanism before code; approve ADR 0034 exception explicitly.
Do not add run affinity to the scheduler as a shortcut. Prefer existing
same-site reachable owner; add bounded configured encrypted/authenticated
peer fetch only when direct reachability is insufficient. If ownership and
ID merging cannot be deterministic without a larger control plane, keep
split-site cohorts GitHub-backed and record the reduced rollout scope.

Files: cachegate artifact lookup/owner transport and tests; gate config and
service wiring in internal/cachegate/server.go and deploy/actions-cache;
internal/lifecycle/cachegate.go and tests only where guest-side proof/whole
handoff selection needs extension; internal/config and daemon wiring only if
new node configuration is unavoidable. docs/adr/{0034,0078,0097} and
OPERATIONS.md document exact placement/fallback semantics.

Red tests: two httptest gates, isolated roots, signed same-run sibling jobs,
fake network/clock; owner miss versus owner unavailable; other-repo/run auth
never forwarded; redirects cannot exfiltrate JWT; peer loop/hop/deadline/body
bounds; ID collisions/local and GitHub mixed lists; same-name overwrite;
GitHub fallback only for known GitHub data, never local-only finalize/fetch.
Offline GitHub stub proves a local upload performs zero GitHub writes.

Extend tests/simulation with two site-local storage owners, split sibling jobs,
owner failure, roaming/direct clients, retention and delayed consumer events.
Assert repo/run isolation, no invisible handoff, explicit unavailable outcomes,
reserve safety and unchanged assignment/ownership authority. Separate oracle,
world-model and fleet defects if properties fail. No new scheduler pass.

## 3. Compatibility, operations and isolated rollout evidence

Files: internal/cachegate/deploy_test.go, internal/lifecycle/cachegate_test.go,
scripts/check-runner-image.sh, deploy/actions-cache/Containerfile.runner only
if new image selection is necessary; docs/LINUX_BASE_IMAGE.md,
docs/OPERATIONS.md, docs/AGENT_RUNBOOK.md and ADR 0097. Do not patch action
code or redirect GitHub REST traffic to manufacture UI compatibility.

Tests: guest-resolvable absolute signed URL; proven host but unreachable guest;
macOS untouched; roaming MacBook rewrites to GitHub before launch and failed
rewrite refuses start; github-token REST clients intentionally bypass gate;
rollback preserves active local reads and sends new coherent cohorts direct.
Choose and document failure-report decision before operator promotion.

After council and green CI, a **separate authorized operational task** may
canary a knee-doctor PR: E2E Build -> all seven lanes, extracted bytes/digests
match, GitHub run artifacts REST listing is 0 and gate/stub evidence confirms
zero upstream artifact creates (a listing alone cannot prove no failed
uploads). Measure cross-site bytes/duration separately, test actual rollback,
and confirm UI/report tradeoff. No production cutover inferred from tests.

## Validation and review for every implementation PR

make fmt; make ci; git diff --check. Record exact measured coverage, race/build/
lint/vulnerability gates, API/index migration impact and unchanged
observe/shadow authority limits. Code council before merge; no council or PR
is requested for this draft-only research commit. Track all progress on #509
when implementation is authorized. No issue comments/API writes in this phase.

Owner's three questions: reuse the gate process rather than add a server;
keep one auth predicate and one budget predicate rather than per-route rules;
exercise real protocol plus multi-site failures deterministically. Prefer
removing a proposed mechanism when the existing one suffices.

## Design-commit validation

Only these three documentation files changed. make fmt and git diff --check
passed. make ci passed module verification/tidy, actionlint, pinned-action
validation, the config self-host test, all 61 Python tests and go vet, then
stopped at lint: `mise ERROR No version is set for shim: golangci-lint`. No
tool installation or check weakening was attempted. Coverage was not reached
and is **not measured** for this commit; no exact percentage can be claimed.
No production invariant, fleet.v1 API, state migration or observe/shadow
authority changed. This is design evidence, not implementation or cutover
evidence. All upstream scratch clones/package extracts were removed.
