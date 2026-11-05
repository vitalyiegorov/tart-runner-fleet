# ADR 0093: The home site reads npm, and later public git, through the gate

## Status

Accepted, 2026-11-05. Issue [#489](https://github.com/vitalyiegorov/tart-runner-fleet/issues/489).
It extends [ADR 0078](0078-a-node-serves-its-guests-actions-cache-pull-requests-only-read-it.md)
and [ADR 0080](0080-a-guest-reports-the-bytes-its-job-moved.md).

The npm cache ships with this ADR. No node has it: it is off by default, and
turning it on is an operational step. **Git seeds are decided here and ship later
in their own PR.**

## Context

ADR 0078 set a registry mirror aside on the grounds that "the registry is not the
slow part of `pnpm install`". That was about a single install and has since been
measured at fleet scale. Over one hour, Omarchy's CI downloaded **3.9 GB from
`registry.npmjs.org` and 3.7 GB from `github.com`, of 8.3 GB of CI internet
traffic**. The home uplink is the shared bottleneck (ADR 0078), and those are the
same bytes fetched again and again.

Git seeds were measured on rnw-community with a seed 30 commits stale:

| Fetch | Without a seed | With a seed |
| --- | --- | --- |
| `depth=1` | 12.5 MB | 1.2 MB |
| full | 150 MB | 7.4 MB |

ADR 0078's other objection to a registry mirror was the write credential each
seeding job would need. A read-through cache needs none: nothing is seeded, because
the first read fills it.

## Decision

### 1. Home site only

Omarchy hosts it. **Omarchy, the Mac mini and the MacBook consume it over the home
LAN.** Ubuntu and the Mac Studio are at the office and go straight to the internet.
The MacBook goes direct when it is away from home, because the launch-time proof
fails there.

**No cache data crosses cloudflared.** The tunnel stays admin and health only. A
cross-site fetch would cross the office downlink and the home uplink, which is
slower than npmjs and is the very traffic being removed.

### 2. One gate address, explicit routes

Guests keep one address and one proof. The gate's address space is:

| Route | Where it goes | Who may write |
| --- | --- | --- |
| `/.trf/gate-identity` | answered by the gate (ADR 0078) | nobody |
| `/npm/` | Verdaccio, on loopback | nobody |
| `/git-seeds/` (planned, section 8) | a seed store, on loopback | nobody |
| Actions cache paths | the cache server (ADR 0078) | branch and tag runs |
| everything else | GitHub's results service (ADR 0078) | as on GitHub |

A gate with no `--npm-upstream` answers `/npm/` with 404. Before this route existed,
such a path went to GitHub with whatever it carried. No other route changes.

### 3. The npm route is read-only at the gate

Verdaccio runs as a separate rootless container, in the same pattern as the Actions
cache server (`deploy/npm-cache/`). It publishes `127.0.0.1:3020` only, and
`--npm-upstream` refuses anything that is not a literal loopback URL, so neither its
port nor its storage is reachable from a guest. Its container is outside the `trf-`
namespace (issue #415).

`cachegate.DecideNPM` decides **independently of Verdaccio's own config**, so a
mistake there cannot open a write path:

- **Only `GET` and `HEAD`.** Every other method gets 403, which covers publish,
  unpublish, login, adduser, token and audit writes, whatever the path.
- **Only these reads:** metadata, tarballs, `-/ping` and `-/package/NAME/dist-tags`.
  Everything else gets 404, including `-/whoami`, `-/user/*`, `-/npm/v1/*`,
  `-/v1/*`, `-/verdaccio/*` and `_session`.
- **Matched on the escaped path.** No percent-encoding except a scope's `%2f` reaches
  the backend, so the gate and Verdaccio cannot read one request two ways, and a dot
  segment cannot match.
- **Only a direct LAN address.** A `Host` that is not an RFC 1918 or unique-local
  literal, loopback, or `host.containers.internal` gets 403, as does any request
  carrying Cloudflare's headers. Verdaccio builds tarball URLs from this `Host`, so
  this also stops a tunnel's hostname from ever being advertised.

Only seven read headers reach Verdaccio. Nothing else is forwarded: no
`Authorization`, `Cookie`, `Npm-*`, query string or body, and no header nobody
thought of. A refusal logs the route's prefix and never the path, because
`npm logout` puts the token in the path. A redirect from the backend becomes 502,
and `Set-Cookie` is dropped.

`deploy/npm-cache/verdaccio.yaml` is the second lock: registration off, publish and
unpublish denied to everyone, an anonymous uplink to npmjs alone, tarballs advertised
under `/npm/`, and metadata cached for at most 10 minutes. Audit is off.

### 4. A guest gets its registry only on that launch's proof

`cacheGate.npmRegistry` is optional and **off by default per node**. Before each
launch, with the proof ADR 0078's amendment introduced:

1. The host asks the gate to sign a fresh nonce, with no proxy and no redirect.
2. Whatever the answer, the host removes any `~/.npmrc` this fleet wrote, because a launch
   can retry on the same guest and the file may name a gate this launch did not prove.
   **A failed removal stops the runner from starting**, exactly as a failed results rewrite
   does, and so does a file the script cannot read, since it cannot say whose that is.
3. If the gate proved itself, the host asks `/npm/-/ping` the same way, because a verified
   gate over a dead Verdaccio would otherwise fail every install in the job.
4. If that answers, a second script asks the route again **from inside the guest**, with no
   proxy, no redirect and plain HTTP only, and only an exact 200 counts: with no
   `--location`, curl calls a redirect success. The host's proof says nothing about the
   guest's network, such as a NAT, and no client falls back once its registry is set.
5. Only then does it write `~/.npmrc`, atomically and marked as ours.

Anything else leaves no setting, and no setting means npmjs. Removal and write are separate
scripts so that their failures cannot be confused: the removal has already succeeded by the
time the write runs, so a failed write, even a lost `exec`, only costs a cold job. An image's
own `~/.npmrc` is left alone and that guest stays direct, because nothing merges a file this
fleet did not write.

**Why `~/.npmrc`, and not an environment variable.** It is the lowest layer that every
Node version on a guest reads. `setup-node` changes the global prefix, so a global
`npmrc` would vanish with it. An environment variable would be the highest layer: it
would override a project's `registry=` and `setup-node`'s `registry-url`, and send
private packages to a cache that cannot have them. At the user level, all of those
still win.

**Why this works on Podman.** `resultsFallbackScript` rewrites only
`ACTIONS_RESULTS_URL` in `.env`, and a Podman guest's results URL is image `ENV`, so
that mechanism does not apply. Config validation also refused a gate on a podman node
outright. Both are resolved by making the registry a separate choice that takes the
same path on every guest, since both backends start guests through `exec`:

| Node | `cacheGate.url` (what the host proves) | `cacheGate.npmRegistry` (what a guest may be given) |
| --- | --- | --- |
| Mac mini, MacBook (Tart) | `http://192.168.1.64:3017/` | `http://192.168.1.64:3017/npm/` |
| Omarchy (Podman) | `http://127.0.0.1:3017/` | `http://host.containers.internal:3017/npm/` |

A podman guest cannot use the host's own LAN address under pasta, and its host has no
need to. A podman node may configure a gate only to set `npmRegistry`. A failed proof
there rewrites nothing, because its Actions cache was never conditional on the check,
and the doctor row says so.

Validation holds both addresses to the direct LAN: plain HTTP, an RFC 1918 or
unique-local literal (or, for a podman host, loopback), an explicit port, no
credentials, query or fragment, and exactly the gate's own host and port. A tunnel
hostname cannot be configured.

### 5. What each client does

Proven by `tests/integration/npm-cache-compat/run.sh`:

| Client | Goes through the cache? | What was proven |
| --- | --- | --- |
| pnpm 11.22.0, 11.28.3, 12.14.0, `--frozen-lockfile` | Yes | Cold and warm from an empty store. It asks for tarballs directly, and the lockfile is unchanged and never names the gate. A tarball that does not match the lockfile's integrity still fails, and `trustPolicy: no-downgrade` still fails on a lockfile that holds a downgrade |
| pnpm, without a frozen lockfile | Yes | `trustPolicy: no-downgrade` and `minimumReleaseAge` fail exactly as they do direct, so Verdaccio keeps the evidence they read, and a tampered tarball fails |
| npm 10.9.2, 12.2.0, `npm ci` | Yes | npm rewrites a lockfile's `registry.npmjs.org` URLs to the cache, keeping scheme and path, and leaves the lockfile alone |
| npm, `npm install` | Yes | The audit step's refused call only loses its summary. A lockfile npm **generates** names the gate |
| `npx`, `npm exec` | Yes | Fetches through the cache |
| `npm install --global` | Yes | Fetches through the cache |
| Yarn 4.17.1 | **No, direct** | It ignores `~/.npmrc`, and no fleet setting is added for it |
| Bun 1.3.1 | **No, direct** | It ignored `~/.npmrc`, and no fleet setting is added for it |
| Yarn 1, Corepack | Not tested | Treat as unknown |

**Private scopes keep going direct and send the gate nothing.** A scope's
`@scope:registry` and its `//host/:_authToken` are bound to that host by npm and by every
pinned pnpm: a recording proxy in front of the gate saw neither the package's name nor
the token, while the private registry did receive the token. A token bound to
`registry.npmjs.org`, a bare `_authToken`, and `setup-node`'s `NPM_CONFIG_USERCONFIG` all
stayed off the gate too. The gate strips whatever it does receive and never logs it, but
that is a backstop and not the guarantee.

### 6. Metrics

The npm route uses the existing shape: `fleet_cache_gate_bytes_total{upstream="npm",
direction="to_guest"|"from_guest"}`. A gate with no `--npm-upstream` does not export
these series at all, so its scrape is exactly what it was, and an absent series means the
route is off, not that it moved nothing. `from_guest` stays at 0, because nothing is
forwarded. The WAN estimate over a window becomes guest receive minus `cache/to_guest`
minus `npm/to_guest`. A miss still crossed the WAN once, between Verdaccio and npmjs,
and the gate cannot see that: it counts what guests read, not what the uplink carried.

### 7. Disk

**The shipped deployment neither caps nor prunes this storage.** Verdaccio does not prune,
the quadlet sets no size limit, and any job can make it cache any public package. Left
alone it grows until admission's reserves refuse guests (ADR 0083). The figures below are
**operational thresholds an operator acts on, not limits anything enforces**:

- **Threshold: 40 GiB on Omarchy.** A full wipe costs one cold round, about what one hour
  of CI already downloads, so the figure is deliberately small beside the node's disk.
- **Reserve: prune at that threshold, or when free space falls below the node's
  `minFreeDiskGb` plus 40 GiB.** The node's own disk and inode floors stay the backstop,
  and they stop admission, not the cache's growth.
- **Pruning** deletes old tarballs weekly and, past either limit, the whole `data`
  directory. The run proved both are safe: the next read fetches again. Weekly pruning
  reclaims only part of it, because metadata is the larger half: three real lockfiles left
  1.56 GiB of metadata beside 0.93 GiB of tarballs. Only the wipe reclaims that.

The recipe is in `docs/OPERATIONS.md`. Scheduling it is an operational step of the rollout,
and no unit ships for it here, so until someone does, nothing watches these figures.

### 8. Git seeds: decided, and shipped later

- **Mechanism.** A bounded, optional job-started hook imports verified objects into a
  locally built `.git` with a valid detached `HEAD`, under `GITHUB_WORKSPACE`. It never
  copies hooks, config, alternates or `refs/replace`, and the job **always fetches the
  requested revision from GitHub**. A seed can only save bytes. It can never decide
  what the job runs. Any failure leaves an ordinary empty checkout.
- **An allowlist of public repositories.** suuudokuuu, budgie-at/budgie and
  rnw-community are public and may be seeded. knee-doctor, hotel-provence and
  pony-labirinth are private and stay direct.
- **Private repositories stay direct because gate identity is not guest authorization.**
  The gate proves itself to a node, and nothing proves a guest to the gate.
  `internal/cachegate/policy.go` parses unsigned claims, and only ever to refuse a
  write, so it cannot authorize a read. Any fork job on these runners could read a
  private bundle. The hook therefore holds no token and needs none.

### 9. Losing the gate mid-job

**A gate lost mid-job fails an npm install, and that needs a rerun.** There is no
transparent fallback. The choice is made once, at launch, and a client cannot change
its registry mid-run. In the run, pnpm retried for about three and a half minutes and
then failed, and the rerun succeeded once the gate was back. This is the same
accepted limit as ADR 0078's amendment. A laptop that leaves home mid-job has the same
failure.

## Consequences

- **A job that needs the registry itself must opt out.** Publish, `npm audit`,
  `pnpm audit`, `npm audit signatures` and `npm search` fail against the gate, by
  design and visibly. Such a job uses a project `registry=` line or `setup-node`'s
  `registry-url`, or sets both `NPM_CONFIG_REGISTRY` and `pnpm_config_registry`, because
  pnpm 11 and later ignore the first. rnw-community's release runs on `ubuntu-latest`
  and is unaffected.
- **A job that commits a generated `package-lock.json` must opt out too**, or it may
  record the gate's address. pnpm's lockfile never does.
- **A version npmjs removes stays cached** until it is pruned, because Verdaccio merges
  what the uplink says into what it has. A frozen lockfile still checks integrity, but
  a non-frozen install can resolve it. The runbook has the per-package purge.
- **A resolve can be up to 10 minutes stale**, so a just-published version may be
  invisible for that long.
- **The traffic is plain HTTP on the LAN**, as ADR 0078's amendment records. A lockfile's
  integrity still protects frozen installs from a tampered response. A non-frozen
  install trusts the cache as it would trust npmjs over TLS, with less protection.
- **The cache shares fate with Omarchy.** A launch after Omarchy dies goes direct. A job
  already running on another Mac fails its next install and needs a rerun.
- **npm warns on every run.** `~/.npmrc` carries both `noproxy`, which npm and pnpm 12 read,
  and `no-proxy`, the only spelling pnpm 11 reads. npm 12 prints `Unknown user config
  "no-proxy"` and says that will stop working in its next major version. Drop that line
  once consumers leave pnpm 11, and before a guest image gets that npm.
- **The doctor's `cache gate` row can now also fail with `npm cache not ready`.** It
  fails no health check, because guests going direct is working as designed.

## Rollout and rollback

Each step leaves every node working, so a step can stop anywhere:

1. Release the binary. With no `npmRegistry` nothing changes. An older binary refuses a
   config that has the key, so the release comes first.
2. On Omarchy, install the quadlet and config, then the gate drop-in, and restart the
   gate. Guests still have no setting.
3. Add `npmRegistry` to the Mac mini and restart its daemon. `fleet doctor` must read
   `npm cache`. Run one job and watch the counter, on Omarchy, where the gate keeps it.
4. Then Omarchy, then the MacBook while it is at home.

**Rollback: remove `npmRegistry` from a node's config and restart its daemon.** New
guests get no setting, and existing Actions-cache behaviour is unchanged, because
nothing else moved. Running guests keep what they have until they end, so leave the
route up until they do. Removing the drop-in and restarting the gate removes the route
itself.

## Evidence

Unit tests pin the route's whole policy table, the header allowlist, the log, the
loopback-only upstream, the deploy files' security properties, the launch selection on
both backends, validation, the doctor row and the additive API fields. The launch's real
guest scripts run in a throwaway `HOME`: a removal or read that fails stops the runner, a
write that fails carries on direct, and real `curl` gets a registry only from an exact 200.

**A real compatibility run** (`tests/integration/npm-cache-compat/run.sh`, ubuntu-vitalii,
2026-11-05): the shipped Verdaccio config on the pinned Verdaccio 6.11.1 image, rootless
with no capabilities, behind the real gate, with a recording proxy in front of it to prove
what clients send. Each client ran in a clean environment with explicit store directories.

- **Cold and warm `pnpm install --frozen-lockfile`** on pnpm 11.22.0, 11.28.3 and 12.14.0,
  each from an empty store.
- **The cache really answers on its own.** With the uplink pointed at nothing, a package
  never seen answered 404 and a frozen install of cached packages still succeeded.
- **pnpm's checks still fail when they should.** From a fixture uplink serving a package with
  a trust downgrade, one published a minute ago and one with a tampered tarball,
  `trustPolicy: no-downgrade`, `minimumReleaseAge` and integrity each fail through the cache
  as they do direct, on all three pnpm versions. On a frozen lockfile, the trust downgrade
  fails through the cache as it does direct. `minimumReleaseAge` does not fail a frozen
  lockfile on any of the three, direct or through the cache, so the cache changes nothing
  there. A tampered lockfile integrity also fails through the cache.
- **Real lockfiles**, fetched cold and then warm through the gate, with every tarball
  accepted by the route:

| Lockfile | pnpm | Cold | Warm | Through the gate, warm |
| --- | --- | --- | --- | --- |
| rnw-community | 11.22.0 | 63 s | 7 s | 277 MB |
| suuudokuuu | 11.28.3 | 74 s | 24 s | 524 MB |
| budgie | 12.14.0 | 119 s | 34 s | 733 MB |

  Those warm bytes are what stays off the uplink for each repeated job. The times are from
  the office site's fast downlink, so they do not predict the home site's.
- **Disk.** The three lockfiles together left 2.6 GiB and 7,406 inodes in the cache: 1.56 GiB
  of metadata and 0.93 GiB of tarballs.
- **A lost gate** failed the install after 211 s with no fallback, and the rerun succeeded
  once the gate was back.

## Not proven

- **Nothing ran on a real guest.** Neither a Tart VM nor a fleet Podman container has
  run this. The compatibility run used the real gate, the real Verdaccio and real
  clients on ubuntu-vitalii, with a `~/.npmrc` written as the script writes it, not one
  written by a launch. The launch scripts themselves ran only in unit tests, in a throwaway
  `HOME`, with real `curl` against a fixture and stand-ins for the steps made to fail.
- **Real LAN and pasta addressing.** The run reached the gate on loopback. It asserted
  the advertised URLs for `192.168.1.64` and `host.containers.internal` with a `Host`
  header, not from a guest on those networks. In particular, a macOS guest behind Tart's
  NAT reaching `192.168.1.64` is unproven, though the guest-side probe sends it direct
  if it cannot.
- **Real savings.** No before-and-after measurement exists, because nothing is deployed.
- **The extra `tart exec` calls on a Tart host.** A launch on a node with `npmRegistry` set
  makes up to two more `exec` calls into the guest, one-shot and not periodic. Repeated vsock
  connections are what trap Tart's VM helper (#478, ADR 0080's 2026-11-05 amendment), and the
  reproduced crash took a probe every 5 s, so this should be far from it, but it has not been
  measured.
- **Long-term disk growth**, and whether the 40 GiB threshold suits real traffic.
- **The installed quadlet.** The run started the image with the same user namespace,
  capabilities and mounts, but not through systemd or Quadlet.

## The three questions

1. **Overengineering.** There is no new daemon, store, state or scheduler rule. Both the
   host ping and the guest probe stay, because each covers a failure the other cannot:
   the ping tells the operator, and the guest probe covers the guest's own network.
2. **Complexity.** One config key, one route and one script. The same path serves Tart
   and Podman, which is what removed the podman special case from the registry decision.
3. **Testability.** The route is a pure function, and the launch choice is deterministic
   through the existing runner port. The simulation is not extended: no admission rule,
   state or axis changes, so its corpus digests must not move. What it cannot reach is
   the clients' behaviour, which is why the compatibility run exists.

## Not built

- **Git seeds**, here. See section 8.
- **A storage cap or a pruning unit.** The recipe is documented and scheduling it is a
  rollout step. Nothing enforces the 40 GiB threshold.
- **Yarn and Bun settings beyond what section 5 states.**
- **Per-job attribution of cache bytes**, for the reason ADR 0080 gives.
- **A cache at the office site**, where the uplink is not the bottleneck.
