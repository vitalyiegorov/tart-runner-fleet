# ADR 0093: The home site reads npm, and later public git, through the gate

## Status

Accepted, 2026-11-05. Issue [#489](https://github.com/vitalyiegorov/tart-runner-fleet/issues/489).
It extends [ADR 0078](0078-a-node-serves-its-guests-actions-cache-pull-requests-only-read-it.md)
and [ADR 0080](0080-a-guest-reports-the-bytes-its-job-moved.md).

The npm cache ships with this ADR. No node uses it: it is off by default, and turning it on is
an operational step. **Git seeds are decided here and ship later in their own PR.**

**Amended 2026-11-05: Verdaccio is replaced by a read-through cache in the gate**, after it failed
in production. See [Why not Verdaccio](#why-not-verdaccio). Sections 2, 3, 6, 7 and 9 describe the
design as it now stands.

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
| `/npm/` | the gate's own cache, then npmjs | nobody |
| `/git-seeds/` (planned, section 8) | a seed store | nobody |
| Actions cache paths | the cache server (ADR 0078) | branch and tag runs |
| everything else | GitHub's results service (ADR 0078) | as on GitHub |

A gate with no `--npm-cache` answers `/npm/` with 404. Before this route existed,
such a path went to GitHub with whatever it carried. No other route changes.

### 3. The npm route is read-only at the gate

The gate answers this route itself, with no separate process and no port or storage a guest could
reach directly. `cachegate.DecideNPM` judges every request first:

- **Only `GET` and `HEAD`.** Every other method gets 403, which covers publish,
  unpublish, login, adduser, token and audit writes, whatever the path.
- **Only these reads:** metadata, one version or tag of it, tarballs, `-/ping`,
  `-/package/NAME/dist-tags` and `-/npm/v1/attestations/NAME@VERSION`. The single version is how pnpm 12 fetches its
  own binary (`@pnpm/exe.linux-x64/12.16.1`), which the image's Corepack shim triggers on the first pnpm
  call in a job, so refusing it fails that job. Attestations are where pnpm reads a publish time, and refusing them
  costs a full document instead (see [Consequences](#consequences)).
  Everything else gets 404, including `-/whoami`, `-/user/*`, the rest of `-/npm/v1/*`,
  `-/v1/*` and `_session`, and so does a path past 512 bytes.
- **Matched on the escaped path.** No percent-encoding except a scope's `%2f` reaches
  the registry or a cache key, and a dot segment cannot match.
- **Only a direct LAN address.** A `Host` that is not an RFC 1918 or unique-local
  literal, loopback, or `host.containers.internal` gets 403, as does any request
  carrying Cloudflare's headers.

**The upstream request is built from scratch.** No guest header, query or body reaches npmjs: only
`Accept` and `Accept-Encoding`, each reduced to the few forms that are really different documents,
so a guest's spelling cannot multiply the cache either. A refusal logs the route's prefix and never
the path, because `npm logout` puts the token in the path. A redirect from the registry becomes 502.

### 3a. The cache

Each object is one file in one flat directory, named for the hash of its key: a line of JSON with its
headers, then its bytes.

- **Tarballs are immutable.** Once stored, they are served without asking again.
- **Metadata is the registry's own bytes, for 10 minutes**, and is then revalidated with its ETag. A `304`
  renews the 10 minutes without moving a body. **Nothing parses a document**, so no request can cost more
  than a copy buffer, and trust evidence reaches the client exactly as npmjs wrote it.
- **Stale metadata answers if npmjs fails**: an error, a `429` or `5xx`, a redirect, or a cut body.
- **A `404` or `410` forgets the document**, in every form it is stored, so that no later failure can bring a removed
  package back as stale. A frozen directory cannot unlink, so there the file is emptied, which reads as not there.
- **A failed disk passes straight through to npmjs**, as a stream: a directory it cannot write, an object
  past 2 GiB, or headers that do not fit. A miss on a directory it cannot write sends one request, the pass-through. A
  document it already holds asks first, because a `304` renews it and a failure serves it, so only a document that
  changed while the disk is frozen costs a second request.
- **One upstream fetch per key.** The others wait for it, and it outlives the guest that asked, because the
  next job wants those bytes too.
- **Every upstream request is bounded**, a fill or a pass-through: cut after 60 s without progress and at 15 minutes
  regardless, and a wait for a connection is not a stall. A cut pass-through aborts the guest's connection, so that a
  truncated stream cannot read as a whole object, and it frees its slot.
- **Writes are atomic.** Temp file, `fsync`, rename, so a crash leaves the whole object or none of it.
- **Integrity stays with the client**, which checks what it receives against its lockfile.

**Blast radius.** The gate also carries every job's Actions cache and artifacts, so this path is kept
from reaching them. It has its own transport and connections, and at most 1024 requests at once, past
which it answers `503`. A panic stays in its request: it is recovered there, releases its slot and its
waiters, and the guest gets a `502` it retries. Tests hold the Actions routes responsive while the
registry hangs, while every npm slot is taken, with the disk broken, with the registry gone, and after a panic.

### 4. A guest gets its registry only on that launch's proof

`cacheGate.npmRegistry` is optional and **off by default per node**. Before each
launch, with the proof ADR 0078's amendment introduced:

1. The host asks the gate to sign a fresh nonce, with no proxy and no redirect.
2. Whatever the answer, the host removes any `~/.npmrc` this fleet wrote, because a launch
   can retry on the same guest and the file may name a gate this launch did not prove.
   **A failed removal stops the runner from starting**, exactly as a failed results rewrite
   does, and so does a file the script cannot read, since it cannot say whose that is.
3. If the gate proved itself, the host asks `/npm/-/ping` the same way, because a verified
   gate with no npm route would otherwise fail every install in the job.
4. If that answers, a second script asks the route again **from inside the guest**, with no
   proxy, no redirect and plain HTTP only, and only an exact 200 counts: with no
   `--location`, curl calls a redirect success. The host's proof says nothing about the
   guest's network, such as a NAT, and no client falls back once its registry is set.
5. Only then does it write `~/.npmrc`, atomically and marked as ours.

These scripts run under `bash -c`, not `bash -lc`. Stock Ubuntu's `~/.bash_logout` runs
`clear_console`, which fails with no terminal, and a non-interactive login shell reads that
file on the `exit` builtin, so under `-l` a script's `exit 0` exits 1. That failed every launch
in the first real attempt on Omarchy (2026-11-05), which was rolled back with no job failed.

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
| pnpm, without a frozen lockfile | Yes | `trustPolicy: no-downgrade` and `minimumReleaseAge` fail exactly as they do direct, because the evidence they read is the registry's own bytes, and a tampered tarball fails |
| npm 10.9.2, 12.2.0, `npm ci` | Yes | npm rewrites a lockfile's `registry.npmjs.org` URLs to the cache, keeping scheme and path, and leaves the lockfile alone |
| npm, `npm install` | Yes | The audit step's refused call only loses its summary. A lockfile npm generates does not name the gate |
| `npx`, `npm exec` | Yes | Fetches through the cache |
| `npm install --global` | Yes | Fetches through the cache |
| Yarn 4.17.1 | **No, direct** | It ignores `~/.npmrc`, and no fleet setting is added for it |
| Bun 1.3.1 | **No, direct** | It ignored `~/.npmrc`, and no fleet setting is added for it |
| Corepack | Its own fetch of pnpm goes direct | Observed on the real image: it downloads from `registry.npmjs.org`, and pnpm 12's self-bootstrap then comes through the cache |
| Yarn 1 | Not tested | Treat as unknown |

**Private scopes keep going direct and send the gate nothing.** A scope's
`@scope:registry` and its `//host/:_authToken` are bound to that host by npm and by every
pinned pnpm: a recording proxy in front of the gate saw neither the package's name nor
the token, while the private registry did receive the token. A token bound to
`registry.npmjs.org`, a bare `_authToken`, and `setup-node`'s `NPM_CONFIG_USERCONFIG` all
stayed off the gate too. The gate strips whatever it does receive and never logs it, but
that is a backstop and not the guarantee.

### 6. Metrics

The npm route uses the existing shape: `fleet_cache_gate_bytes_total{upstream="npm",direction=...}`.
`to_guest` is what guests read, and `from_registry` is what the cache really drew from npmjs, so the
two together are the saving, with nothing left to estimate. `from_guest` stays at 0. A gate with no
`--npm-cache` exports none of these, so its scrape is exactly what it was, and an absent series means
the route is off, not that it moved nothing. The WAN estimate over a window is guest receive minus
`cache/to_guest` minus `npm/to_guest` plus `npm/from_registry`.

### 7. Disk

Any job can make the gate cache any public package, so `deploy/npm-cache/` ships an enforcement timer.
About every five minutes `prune-npm-cache.py` holds the directory to these limits:

- **40 GiB, evicting down to 32 GiB.** A full refill costs one cold round, about what one hour of CI
  already downloads, so the limit is deliberately small beside the node's disk.
- **A reserve of 100 GiB free on the filesystem**, which is `minFreeDiskGb` (60) plus 40 GiB. Under it, the
  timer evicts even below the size limit.
- **It evicts the least recently used files.** The next read fetches them again, and a reader that already
  has one open keeps its bytes, so eviction cannot fail a running install. That removes the race the
  Verdaccio design had to accept. The order relies on access time. On a `relatime` mount such as Omarchy's, a repeat
  read moves it at most once a day, so "recent" is that coarse, and on `noatime` the order is roughly age. A wrong guess
  costs one refetch.
- **It deletes only inside its pinned storage.** The path must be exactly the expected one, reached with no
  symlink and with no mount at or under it, and every delete is relative to a directory fd it has verified.
  Anything in that directory the gate did not write stops it. The unit can write only to that storage
  (`ProtectSystem=strict`, `ProtectHome=read-only`, `ReadWritePaths=`).
- **It leaves alone what is active.** It never evicts a file touched in the last 15 minutes, and it looks again
  immediately before each delete. It also removes the temp files a killed gate leaves, once they are that old.
- **It reports only what it measures.** It re-reads free space after evicting, allowing 60 s for btrfs to return
  it, and never credits what it deleted.
- **Any failure to enforce freezes the cache**, by taking write permission from the directory, and confirms it.
  The gate treats that as a failed disk: hits still serve, misses pass through, and nothing grows, so a
  freeze cannot fail a job. The next run that gets back inside the limits thaws it. Exit 1 is a confirmed freeze
  and exit 3 means it could not confirm one, or would not trust the path enough to try. Both read `failed`, and
  the unit's `OnFailure=` starts `tart-runner-fleet-npm-cache-freeze.service`, which also covers a script killed at
  its 300 s timeout or never started.

**It is not a hard quota, and growth between checks has no strict bound.** Runs about every five minutes and
never overlap, so a slow run stretches the gap, and the cache can pass the limit by whatever the uplink delivers
until the next check finishes. A kernel-enforced cap needs root: the storage is its own btrfs subvolume on Omarchy
so that a qgroup limit can be added later with no migration. The node's own disk and inode floors stay the backstop,
and they stop admission, not the cache's growth.

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

**A gate lost mid-job can fail an npm install, and that then needs a rerun.** There is no transparent fallback. The
choice is made once, at launch, and a client cannot change its registry mid-run. pnpm tolerates a gap of about 70 s
(a retry after 10 s, then one after 60 s). So a restart, which brings the gate back in about 6 s, costs a job time
rather than the job: `kill -9` mid-burst left all 4 installs green. A gate that stays down past that fails them, in
about three and a half minutes, and the rerun gets a fresh launch, which goes direct. A laptop that leaves home
mid-job has the same failure. Omarchy dying takes this route with it, as it already takes the Actions cache.

## Why not Verdaccio

The first design ran Verdaccio behind the gate. On Omarchy, over 2.5 hours and 65 guests, it died 11 times, from OOM
kills and heap failures at its default 512 MB, and it failed one real job (suuudokuuu run 42070622217) with
`ERR_PNPM_FETCH_502`. It was rolled back. Metadata requests for large packages took up to 157 s.

Four concurrent cold installs of that same lockfile, on the same machine:

| Back end | Installs OK | Wall time | Crashes | Slowest metadata | Upstream |
| --- | --- | --- | --- | --- | --- |
| Direct to npmjs | 4/4 | 32–42 s | n/a | 16 s | 2.57 GB |
| Verdaccio, 1 GiB, as shipped | **0/4** | 817 s | 7 | 270 s | ≥1.23 GB |
| Verdaccio, 4 GiB, 3.5 GiB heap | 4/4 | 410 s | 0 | 103 s | ≥2.74 GB |
| Verdaccio, 4 GiB, warm | 4/4 | 421 s | 0 | 203 s | 0.7 MB |

A bigger heap stopped the crashes and left it ten times slower than going direct, even with nothing to fetch. That
points at local work (parsing and rewriting large documents on one thread), though it was not profiled. A cache
that treats documents as opaque bytes has no such cost.

## Consequences

- **A job that needs the registry itself must opt out.** Publish, `npm audit`,
  `pnpm audit`, `npm audit signatures` and `npm search` fail against the gate, by
  design and visibly. Such a job uses a project `registry=` line or `setup-node`'s
  `registry-url`, or sets both `NPM_CONFIG_REGISTRY` and `pnpm_config_registry`, because
  pnpm 11 and later ignore the first. rnw-community's release runs on `ubuntu-latest`
  and is unaffected.
- **A removed version leaves metadata within 10 minutes**, because the document is replaced whole, and a removed package
  goes at its next revalidation. Its tarball stays until evicted, and only a lockfile already pinning it can ask for it.
- **The route serves attestations, because refusing them cost about 60 MB per cold fill.** Under `minimumReleaseAge`, a
  frozen install needs the publish time of any package changed in the last day. pnpm's `resolvePublishedAt` tries the
  abbreviated document, then local metadata, then `-/npm/v1/attestations/NAME@VERSION`, and only then the full document.
  One install run after the other, from empty, the same lockfile drew 690.6 MB (146 full documents, 65.2 MiB) with the
  route refused, and 629.0 MB (151 full documents, 6.4 MiB) with it allowed. A package with no attestation still gets a
  `404`, which is not stored, and then costs its full document, once per 10 minutes. An attestation is as trustworthy as
  the metadata beside it: both are the registry's own bytes over the same LAN hop. `registrySupportsTimeField` has no
  effect on this path.
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

**Before step 3: a job that resolves through the cache writes `tarball:` lines into its lockfile.** Metadata is the
registry's own bytes, so its tarball URLs name `registry.npmjs.org`, which differs from the configured registry, and pnpm
records that (4 lines in a 4-package project). They never name the gate. A later install would then take those tarballs
from npmjs directly, uncached: that follows from the URLs and was not measured. A job that commits a lockfile must opt out,
as one that needs the registry does. A frozen install is unaffected.

Each step leaves every node working, so a step can stop anywhere:

1. Release the binary. With no `npmRegistry` nothing changes. An older binary refuses a
   config that has the key, so the release comes first.
2. On Omarchy, remove what the Verdaccio design installed, install the limits and the gate drop-in, and restart the
   gate. Guests still have no setting.
3. Add `npmRegistry` to the Mac mini and restart its daemon. `fleet doctor` must read
   `npm cache`. Run one job, which also warms the cache, and watch the counter, on Omarchy, where the gate keeps it.
4. Then Omarchy, then the MacBook while it is at home.

**Rollback: remove `npmRegistry` from a node's config and restart its daemon.** New
guests get no setting, and existing Actions-cache behaviour is unchanged, because nothing else moved. Running guests
keep what they have until they end, so leave the route up until they do. Removing the drop-in and restarting the gate
removes the route itself. `docs/OPERATIONS.md` has the full steps, including removing Verdaccio's quadlet, config and
image.

## Evidence

Measured 2026-11-05 on ubuntu-vitalii at `nice -n 15`, never on a node. Each burst is four guests on rootless pasta
networks reaching the gate at `host.containers.internal`, installing suuudokuuu's real lockfile (1,482 entries) with pnpm
11.28.3 and `--frozen-lockfile`, each from an empty store.

| Run | Installs OK | Wall time | From npmjs | Slowest Actions-route answer |
| --- | --- | --- | --- | --- |
| Direct to npmjs | 4/4 | 32 s | 2.57 GB | 0.007 s |
| Gate, cold | 4/4 | 90 s | 747 MB, of which 55 MB was the harness's probe | 0.017 s |
| Gate, warm | 4/4 | 42 s | 0 | 0.18 s |
| Gate, warm, every file aged an hour, so all revalidated | 4/4 | 43 s | 0 | 0.18 s |
| Gate, cold, `kill -9` at 12 s, back 5 s later | 4/4 | 49 s | not comparable | 0.010 s, and one refused probe during the gap |
| Fresh stores after that kill | 4/4 | 41 s | 0 | 0.015 s |

In the cold run three installs took 49 s and one took 90 s, after one `ERR_PNPM_FETCH_TIMEOUT` it retried. The kill left 12
temp files and no partial object, and pnpm's integrity check passed on the next run. The gate's peak memory across
these runs was 56 MB.

Against an upstream throttled to the home site's measured 9 MB/s:

| Run | Installs OK | Wall time | From npmjs | Timeouts |
| --- | --- | --- | --- | --- |
| Direct | 4/4 | 283 s | 4 × 643 MB | 0 |
| Gate, cold (two runs) | 4/4 | 89 s, 88 s | 691 MB | 2 per install, each retried |
| Gate, warm | 4/4 | 93 s, three of them 44–46 s | 0 | 1 |

The shipped prune and freeze units ran under their real confinement on the store that kill left, with only their paths
changed. A good run removed the 12 temp files. An over-limit run evicted 275 files, and an unreachable reserve evicted
1,007 more and froze the directory. `OnFailure=` froze it again when the unit could not start. A frozen cache still
answered, adding no file, and the next good run thawed it. pnpm then installed through what eviction had left with no
integrity failure, refetching 524 MB.

`tests/integration/npm-cache-compat/run.sh` passes 133 of 133, including a real `304` from npmjs, stale service and a frozen
cache. `internal/cachegate` holds 100% coverage and passes under the race detector.

From the Verdaccio design, which is the only real CI this cache idea has seen: Omarchy's first hour served 3.59 GB against
at least 369 MB drawn upstream.

The bursts above ran before the route served attestations. The 629.0 MB under [Consequences](#consequences) came from a
scratch binary with the same pattern, on one install, so expect a cold fill about 60 MB smaller than these tables.

## Not proven

- **No real CI job has used this gate**, and no node has run it. The proxy stays off. All of the above is on
  ubuntu-vitalii, so Omarchy's disk, a Tart guest over the home LAN and a sustained day of real load are all untried.
- **The home link is emulated**: a throttle in front of npmjs, with nothing else competing for the uplink.
- **A single request stalled for 60 s twice**, once in the cold office run and once in the throttled warm run, and pnpm
  recovered by retrying. The second came with nothing drawn from npmjs. It did not recur in 1,968 fetches of a 35 MB object
  at 8 at once through the gate, and those took the same time as a plain file server (342 ms and 360 ms at worst). The cause
  is unknown.
- **The limit of a large cold fill is an estimate.** See [Not built](#not-built).
- **The prune has only run at small scale**: 719 MB with lowered limits, on btrfs. The 40 GiB limit has not been reached, and
  a run killed at its 300 s timeout was not exercised (one that never started was).
- **Why equal cold runs fetched 146 or 161 full documents, with attestations refused.** The difference was about 1 MiB, and
  is probably which packages had changed in the last day.
- **The burst, with attestations served and the later fixes.** The removal, the pass-through bounds and the single request
  on a failed disk are proven by unit tests and the compatibility run, not by a repeat of the bursts.

## The three questions

1. **Overengineering.** Verdaccio's container, image, config and unit are gone, and the gate gains one small file. There
   is no new daemon, state or scheduler rule. Both the host ping and the guest probe stay, because each covers a failure the other cannot:
   the ping tells the operator, and the guest probe covers the guest's own network.
2. **Complexity.** One config key, one route and one script. The same path serves Tart
   and Podman, which is what removed the podman special case from the registry decision.
3. **Testability.** The route is a pure function, and the launch choice is deterministic
   through the existing runner port. The simulation is not extended: no admission rule,
   state or axis changes, so its corpus digests must not move. What it cannot reach is
   the clients' behaviour, which is why the compatibility run exists.

## Not built

- **Git seeds**, here. See section 8.
- **A kernel-enforced storage cap.** It needs root. The timer in section 7 checks growth instead.
- **Streaming a cold object to the guest while it is stored.** The guest waits for the whole object, which is what lets
  one fetch serve many. At 9 MB/s, a burst's largest objects outlast pnpm's
  60 s `fetch-timeout`. The fetch outlives the guest that asked, so a retry finds the file, and all installs still finished
  about three times faster than direct. An object must be stored within pnpm's roughly 250 s of retries, so a cold burst
  of more than about 2 GB at once could run out and fail a job, whose rerun would find what was stored. That is an
  estimate, not a test, and it is why the rollout warms the cache with one job first.
- **Yarn and Bun settings beyond what section 5 states.**
- **Per-job attribution of cache bytes**, for the reason ADR 0080 gives.
- **A cache at the office site**, where the uplink is not the bottleneck.
