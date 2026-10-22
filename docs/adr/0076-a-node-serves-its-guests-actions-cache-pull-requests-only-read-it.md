# ADR 0076: A node serves its guests' Actions cache on the LAN; pull requests only read it

## Status

Accepted for the two podman nodes (omarchy, site A; ubuntu-vitalii, site B).
Issue #401, with the site diagnosis in #391; part of rnw-community/mobile-ci#273.
Tart guests (macOS and the arm64 Linux overflow) are out of scope here; see
"Not decided here".

## Context

Every ephemeral guest starts with no Gradle home, pnpm store or ccache
directory, so every job that wants one round-trips GitHub's Actions cache over
the WAN. Measured on 2026-10-22:

| | site A (omarchy, Mac mini, MacBook) | site B (ubuntu-vitalii, Mac Studio) |
| --- | --- | --- |
| ISP, public uplink | A1 Telekom, ~2.1 MB/s ceiling (1 and 4 streams alike) | T-Mobile AT, 12.3 MB/s |
| Upload to Cloudflare (neutral) | 1.8–2.1 MB/s (omarchy and Mac mini, both wired) | 12.3 MB/s |
| GitHub release-asset download | 6.3–9.0 MB/s | 96.7 MB/s |
| RTT to 1.1.1.1 (min/avg/max) | 16.8 / 34.7 / 325.6 ms | 5.2 / 11.0 / 22.2 ms |
| Path MTU to 1.1.1.1 | 1500 | 1500 |

The uplink is the bottleneck and it is shared: a 2.19 GB Gradle save takes
about 14.5 minutes at site A and about 3.7 minutes at site B, and mobile-ci#257
showed the slow guests in its benchmark were all on omarchy. Whatever a guest
saves at site A competes with every other upload the site makes.

## Options

**(a) A LAN Actions cache server.** `falcondev-oss/github-actions-cache-server`
implements GitHub's cache service (`actions/cache` v4+ and everything built on
it: `setup-node`, `setup-java`, `gradle/actions`, mobile-ci's own actions). The
runner must send cache calls there instead of to GitHub, which needs two
things: `ACTIONS_RESULTS_URL` in the runner's environment, and
`Runner.Worker.dll` patched so the job message's value does not overwrite it.
A patch only survives on a runner that never self-updates. This fleet already
guarantees that: every guest registers from a JIT configuration with
`DisableUpdate` set, and each base image declares the runner version it
carries (ADR 0041). The server verifies every runtime token's signature
against GitHub's JWKS and keeps GitHub's scoping, per repository and per ref.

**(b) Dedicated tool servers**: a Gradle HTTP build-cache node, ccache
`remote_storage`, a registry mirror. Each needs per-tool, per-consumer
workflow changes, and each needs a write credential handed to the jobs that
seed it. A same-repository pull request receives the repository's secrets, so
withholding that credential from pull requests needs a protected environment
in every consumer. It also does not remove the cost that was measured, which
is moving `actions/cache` tarballs: the registry is not the slow part of
`pnpm install`, and none of these replaces the Gradle home or Android SDK
caches mobile-ci saves today.

**(c) Read-only host mounts.** Gradle can read a dependency cache it does not
own (`GRADLE_RO_DEP_CACHE`), but pnpm's store and ccache's directory must be
writable, so they need an overlay per guest. Something outside the guests would
have to seed the host copy, and that is a build service of its own. It does not
reach Tart guests without a second mechanism.

(a) removes the measured cost for every consumer with no workflow change, and
it is the only option whose security model is GitHub's own. (b) remains a
possible complement for mobile-ci#269 (Gradle build cache and remote ccache);
it is not needed to fix #401.

## Decision

1. **One cache per node, colocated.** Each podman node runs the cache server
   as a rootless quadlet (`deploy/actions-cache/`), bound to loopback, with
   its data under `~/.local/share/tart-runner-fleet-actions-cache`. A site-A
   guest reading a site-B cache would cross site A's downlink and site B's
   uplink, through a Cloudflare tunnel. That is no faster than GitHub, so each
   site has its own cache. A node's cache shares fate with its runners: when
   the host is down, so are the guests that would read it.
2. **The guest reaches it through `fleet cache-gate`.** The gate listens on
   `:3017` because rootless podman's pasta network reaches the host as
   `host.containers.internal` through the host's own address, not loopback.
   It sends cache-service calls and the upload/download URLs the server signs
   to the server. It sends every other results-service call (artifacts, logs)
   straight to GitHub, so artifact uploads keep working while the cache server
   is down. It answers the server's management API, metrics and API docs with
   404, so those stay on loopback for the operator.
3. **Pull requests only read.** GitHub gives a pull-request job write access to
   its own `refs/pull/N/merge` scope, so behind the server alone a pull request
   could still store entries. The gate refuses `CreateCacheEntry` and
   `FinalizeCacheEntryUpload`, whatever the method, unless every write scope in
   the token's `ac` claim is a branch or a tag. A token it cannot read is
   refused. It reads claims without verifying them and uses them only to
   refuse. Anything it lets through is verified by the server exactly as if the
   gate were absent, so a forged token is refused here or rejected there.
   `pull_request_target` runs in the base branch's scope and can write, exactly
   as it can on GitHub's own cache; consumers must keep treating it as trusted
   code.
4. **The runner image is the node's existing image plus three things**
   (`deploy/actions-cache/Containerfile.runner`): `zstd`, the
   `ACTIONS_RESULTS_URL` → `ACTIONS_RESULTS_ORL` patch, and
   `ACTIONS_RESULTS_URL=http://host.containers.internal:3017/`. The build fails
   unless the stock name occurs and the patch removes every occurrence, so a
   runner release that moved the variable stops the build instead of shipping an
   image whose cache silently goes back to GitHub. The guest bootstrap keeps
   every image variable except `PATH`, `LANG`, `LC_ALL` and the JIT
   configuration, so the value reaches the runner and its steps.
5. **Bounded and restart-safe.** The server keeps its index in SQLite beside
   the payloads, so it survives restarts. It evicts least-recently-used entries
   past `CACHE_MAX_SIZE_BYTES` (per node, in docs/OPERATIONS.md) and deletes
   entries unused for seven days, as GitHub does. Scoping is per repository
   id; there is no per-repository quota, and only trusted refs write.

No scheduler, admission or daemon behaviour changes. Enabling the cache on a
node is a change to `executor.image` and two user units. Rolling back is the
reverse: restore the previous `executor.image` and restart the daemon. New
guests then use GitHub's cache again; running guests finish as they are.

## Consequences

- A node's first trusted run per key is cold, because the two sites do not
  share entries.
- A pull request no longer saves even to its own scope, so a re-run of the same
  pull request reads what the base branch seeded, not what its own first
  attempt built. mobile-ci already skips pull-request saves for exactly this
  reason.
- The runner patch must be re-checked on every runner bump. The Containerfile
  check makes that a build failure, not a silent fallback.
- The upload IDs the server signs are 10 random digits and the upload URL
  carries no token. A guest that guessed a trusted save's pending ID within
  that save's upload window could write parts into it. The search space is
  10^10 against windows of seconds, the guest is already running code its
  repository approved, and the entry stays scoped to that repository. This is
  recorded as residual risk and not mitigated.

## Not decided here

- **Tart guests.** Patching the macOS and arm64 Linux base images is a base
  image rebuild (BASE_IMAGE.md, LINUX_BASE_IMAGE.md). At site A those guests
  would also need to reach a host other than their own through omarchy's
  firewall. That is a separate change with its own validation on pony-labirinth
  and the iOS lanes.
- **Gradle build cache, remote ccache.** mobile-ci#269.

## Evidence

Unit: `internal/cachegate` pins the refusal table, fail-closed token parsing,
the operator-surface 404s, the cache/results split, and that a refused save
never reaches the server. Integration (omarchy, 2026-10-22, real server
v9.8.0 behind a dev gate, probed from a runner container on the pasta
network): a pull-request-scoped token got `403 permission_denied` from the
gate, a forged trusted token got `401` from the server's signature check, the
artifact service answered from GitHub, and `/metrics` got 404. The production
before/after measurement and the pull-request write test on the live fleet are
in #401.
