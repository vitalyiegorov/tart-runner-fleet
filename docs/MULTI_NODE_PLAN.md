# Multi-node fleet plan

The implementation plan for [ADR 0034](adr/0034-a-node-serves-the-scale-sets-it-owns.md):
three independent fleet daemons, one per machine, coordinated only by GitHub's
one-session-per-scale-set rule. This document carries the placement decisions,
the executor choice, the phased delivery, and the bring-up checklists. The ADR
carries the architecture and its non-goals.

The plan began as a two-node design and grew a third node while it was being
written; the file is named for what it now describes.

## Contents

- [The nodes](#the-nodes)
- [Workload placement](#workload-placement)
- [Executor technology for node B](#executor-technology-for-node-b)
- [The executor seam](#the-executor-seam)
- [`hostBudget`](#hostbudget)
- [Configuration layout](#configuration-layout)
- [Phase 1 — node B bring-up](#phase-1--node-b-bring-up)
- [Phase 2 — the x86 executor adapter](#phase-2--the-x86-executor-adapter)
- [Phase 3 — arch-floating labels and per-repository migration](#phase-3--arch-floating-labels-and-per-repository-migration)
- [Node C — Mac Studio](#node-c--mac-studio)
- [Observability](#observability)
- [Updater](#updater)
- [Simulation](#simulation)
- [Sequencing](#sequencing)
- [Open spikes](#open-spikes)
- [Working agreement](#working-agreement)

## The nodes

| | Node A | Node B | Node C |
| --- | --- | --- | --- |
| Machine | Mac mini M4 | GEEKOM A9 Max | Mac Studio |
| CPU | 10 cores, Apple silicon | Ryzen AI 9 HX 370, 12c / 24t, x86_64 | Apple silicon |
| Memory | 24 GiB | 32–64 GiB | shared with other work |
| Location | owner's desk | owner's desk | remote |
| OS | macOS | Linux | macOS |
| Guest tech | Tart (Apple Virtualization) | ephemeral containers | Tart |
| `hostBudget` | full machine, stated explicitly | full machine, stated explicitly | **4 vCPU / 10240 MiB** |
| Platforms | macOS only | Linux/amd64 only | macOS only |
| Status | live | ordered | ordered |

Node A and node B have disjoint capabilities. Node C overlaps node A and is
partitioned by scope, per ADR 0034 §3.

## Workload placement

Derived from seven days of real instance starts on node A (2026-07-28 to
2026-08-04), plus an audit of every consumer workflow's `runs-on`.

### By profile

| Profile | Vector | Today | End state | Canonical label at end state |
| --- | --- | --- | --- | --- |
| `small` | 1×2 | Node A (Linux/arm64) | **Node B** | `trf-linux-amd64-1x2` |
| `medium` | 2×4 | Node A | **Node B** | `trf-linux-amd64-2x4` |
| `large` | 4×8 | Node A | **Node B** | `trf-linux-amd64-4x8` |
| `xl` | 6×12 | Node A — **transitional, retire in Phase 1** | **Node B** | `trf-linux-amd64-6x12` |
| new | 8×16 | — | Node B, one job takes a large slice | `trf-linux-amd64-8x16` |
| new | 12×24 | — | Node B, whole-node jobs | `trf-linux-amd64-12x24` |
| `builder` | 6×12 | Node A | **Node A only** | `trf-macos-arm64-6x12` |
| `maestro` | 4×7 | Node A | Node A + **Node C** for named scopes | `trf-macos-arm64-4x7` |

Node C cannot host `builder`: 6 vCPU and 12288 MiB both exceed its 4 vCPU /
10240 MiB budget. `maestro` fits exactly.

### By repository and workload

| Repository | Workload | 7-day starts | Today | End state | Note |
| --- | --- | ---: | --- | --- | --- |
| `tart-runner-fleet` | CI: preflight, quality, unit, race, build | 364 Linux | A | **B** | Pure Go, `CGO_ENABLED=0`, cross-compiles the darwin/arm64 release from any arch. Arch-floating. |
| `knee-doctor` | Node build, tests, Playwright chromium+webkit | 109 Linux | A | **B** | Playwright is first-class on amd64; arm64 support is the constrained one. Arch-floating, improves. |
| `hotel-provence` | Node build, tests, Playwright | low | A | **B** | Same. Arch-floating. |
| `budgie` | Node/Expo web build, lint, tests | 35 Linux | A | **B** | Arch-floating. |
| `budgie` | Android build on `ubuntu-24.04` | GitHub-hosted | GitHub | **B** | Already x86_64 Linux with `ndk;27.x`. Comes home at zero risk — the arch it wants is the arch node B is. |
| `budgie` | iOS native build / publish | 30 macOS | A | **A** | `builder` / `macos-tartelet`. |
| `suuudokuuu` | Web build, lint, unit, web E2E | 105 Linux | A | **B** | Arch-floating. |
| `suuudokuuu` | Android APK build | inside `builder`'s 126 | **A (macOS builder)** | **B** | Only on macOS because no arm64-Linux NDK exists. The x86_64 Linux NDK is the *supported* one. Frees the fleet's scarcest profile. |
| `suuudokuuu` | Android Maestro E2E — Redroid arm64 | 52 `xl` | **A — transitional** | **B**, KVM emulator | See [Android](#android-is-the-load-bearing-migration). Retire with node A's Linux. |
| `suuudokuuu` | iOS Maestro E2E | 109 macOS | A | **A**, overflow to **C** | Biggest `maestro` consumer; the scope to move to node C first. |
| `rnw-community` | Android Maestro E2E — x86_64 AVD | 46 `xl`, failing | A | **B** | Cannot ever work on node A. The single clearest justification for node B. |
| `rnw-community` | iOS Maestro E2E | 99 macOS | A | **A** | Stays; node C takes `suuudokuuu` instead so one scope moves, not two. |

Every cross-job handoff in every consumer repository already uses
`actions/upload-artifact` / `actions/download-artifact`. The same-host
`ci-shared` VirtioFS mount of ADR 0013 is used by no workflow as a correctness
dependency, so splitting a build from its E2E across nodes is safe.

### Android is the load-bearing migration

The two consumers took opposite routes around the same missing thing — a fast
Android runtime on arm64 Linux — and node B removes the need for both.

`suuudokuuu` runs **Redroid**, Android in a privileged container over the host
`binder_linux` module, at native arm64 speed. Its own workflow comment states
why: "Google ships no emulator for arm64 linux and nested-KVM boots are
unstable on the host, so Android runs as a privileged container ... no
virtualization inside the guest at all." This is a good workaround and it is
**transitional**: it exists only because node A is the only node. It retires
with node A's Linux profiles in Phase 1.

`rnw-community` runs `reactivecircus/android-emulator-runner` with
`arch: x86_64`, `api-level: 34`, `google_apis` — on `linux-xl`, which is arm64.
There is no hardware acceleration for an x86_64 guest on an Apple-silicon host.
The workflow is on `master` and its dispatches fail.

**On node B, Android is the KVM-accelerated x86_64 Google emulator**, not
Redroid. Reasons, in order:

1. `rnw-community`'s workflow already asks for exactly this and needs no edit.
2. It needs one grant — `--device /dev/kvm` — and no privileged container, no
   host kernel module, and no Docker-in-Docker. Redroid inside a container
   runner would need all three.
3. Google ships x86_64 system images as the first-class emulator target. The
   whole reason Redroid was adopted is that arm64 Linux has no emulator; on
   x86_64 that reason is gone.
4. Node B is bare metal, so KVM is first-level. Node A's Redroid path pays a
   nested-virtualization tax that node B does not.

Cost, stated honestly: `suuudokuuu` must build its E2E APK with the `x86_64`
ABI instead of `arm64-v8a` (`ANDROID_E2E_ABI` in `mobile-build.yml`), and that
build moves to node B at the same time. Both are Phase 3 edits in one PR, and
both are net simplifications — the APK build stops occupying `builder`.

### Should node A keep a small Linux profile?

**No.** Three reasons, and one is arithmetic:

- `builder` (6 vCPU) plus `maestro` (4 vCPU) is exactly ten cores on a ten-core
  machine. Any Linux guest at all re-creates the starvation photographed in the
  ADR. A `small` at 1 vCPU makes the builder wait behind it just as surely as
  an `xl` does.
- Node A's own bursts are the fleet's macOS work, which is not Linux work.
- Keeping one Linux profile keeps the two-platform admission paths live on node
  A for no throughput, which is the complexity the partition exists to remove.

Mechanically, node A's Linux profiles stay in `fleet.json` because
`internal/config/config.go:555-563` requires them, but **no scope lists a Linux
scale set**, so no Linux demand can reach the node. After the retirement,
revisit `maxLinuxWhenMacosIdle`, `mixedPlatformAdmission`, and
`mixedProfileCohorts` on node A: with Linux unreachable they describe a
situation that can no longer occur, and their settings should be made to say so.

## Executor technology for node B

**Decision: rootless Podman, one ephemeral unprivileged container per job.**

| Option | Verdict |
| --- | --- |
| **Rootless Podman / containers** | **Chosen.** |
| KVM microVMs (cloud-hypervisor, Firecracker) | Rejected for the MVP. |
| Plain process runners | Rejected outright. |

### Why containers

The existing provisioning verbs map onto container verbs almost one for one.
`lifecycle.ProvisionExecutor` clones a base image, starts it, waits for
readiness, pipes a JIT runner configuration into the guest over stdin, and later
stops and deletes it. That is `podman create` from an image, `podman start`,
`podman exec -i`, `podman rm -f`. The adapter is a CLI-shelling adapter of the
same shape as `internal/adapters/tart`, which is the cheapest possible new
backend and the one whose failure modes the codebase already models. It shipped
as `internal/adapters/podman` rather than the `internal/adapters/container` this
document first proposed: the package shells one runtime's command line, and a
name that hides which one would be a promise of neutrality the code does not
keep. Neutrality lives in `internal/executor`, which is the whole point of it.

Ephemerality is already the design (ADR 0010), and a container is ephemeral by
construction. Per-job isolation, clean filesystem, and no state carried between
jobs come free.

### Why rootless specifically

The fleet repository is public and accepts fork pull requests behind approval.
Approved third-party code will execute on node B. Rootless Podman puts every
container in a user namespace owned by an unprivileged user, with no root-owned
daemon and no `docker` group, which is root-equivalent. It also mirrors node A's
shape: the daemon is a `launchd` **LaunchAgent** on macOS and becomes a
`systemd --user` service on Linux, and rootless Podman is a `systemd --user`
service too. One unprivileged account owns the daemon, the container runtime,
and every container.

The GitHub App private key stays outside that account's container namespace,
file-mode `0600`, readable by the daemon only — never mounted into a runner.

### Why not microVMs

Stronger isolation, and the honest cost of not choosing them: containers are a
weaker boundary than the full Tart VMs node A uses today, so this is a security
regression relative to the status quo. It is accepted because the compensating
controls are real — fork-PR approval gates every third-party execution, node B
holds no data other than the fleet's own credentials, runner registrations are
per-job JIT tokens, and containers run unprivileged in per-container UID ranges.

Against that, microVMs cost: kernel and rootfs images to build and keep current,
TAP networking to configure, no `exec -i` primitive so bootstrap needs vsock or
SSH, no reap/list verbs that map to the existing ports, and a second-level
virtualization tax on the Android emulator, which is the workload node B exists
for. That is weeks, not days, and it re-introduces the nested-virt problem the
move to x86 solves. Revisit if node B ever serves untrusted code without a human
approval gate.

### Why not plain processes

No isolation, no ephemeral filesystem, no clean teardown, and leaked state
between jobs. It contradicts ADR 0010 and is not considered further.

### Known limitations to record now

- A job that wants Docker inside the runner gets nothing. No consumer workflow
  needs one after the Android migration — the only user was `suuudokuuu`'s
  Redroid step. If one appears, the escape hatch is a per-profile privileged
  flag, not a fleet-wide daemon socket mount.
- `internal/guestbootstrap/systemd_linux.go` launches the runner with
  `systemd-run`, which is not present in an ordinary container. **Carried into
  the image contract by issue #139:** the daemon's side of the bootstrap is
  `podman exec -i <name> /usr/local/libexec/tart-runner-fleet-bootstrap` with the
  JIT configuration on stdin, unchanged from node A, so it is the helper *inside
  the runner image* that must detach without `systemd-run`. Building that image
  is node B bring-up work, not adapter work, and the checklist below names it.
- `/dev/kvm` is granted to the Android profile only, not to every profile.
  Issue #139 made that a validated configuration key, `executor.kvmProfiles`,
  and the adapter reads the profile off the `trf-<profile>-` instance-name
  prefix because `executor.InstanceSpec` deliberately carries no profile field.
- **A container is created, never adopted.** ADR 0010 forbids reusing an
  ephemeral guest, and unlike a Tart clone a container that already holds a
  freshly minted name has unknown history. `Create` therefore refuses a name
  held by a container without this operation's `trf.operation` label.

## The executor seam

The audit found the coupling narrower than the package layout suggests.
`internal/provision`, `internal/reconcile`, `internal/operations`, and
`lifecycle.ControlRouter` import no Tart type at all and need no change.
`internal/scheduler` needs no change either, because node B runs a
single-platform configuration and takes the already-proven `planLinux` path.

Four concrete leaks had to close, all in `internal/lifecycle/executor.go` and
`internal/app/inventory.go`:

```go
// internal/lifecycle/executor.go - before issue #137
type VMControl interface {
	Clone(context.Context, tart.Request) error   // adapter type in a port
	Start(context.Context, string, operations.Ownership) error
	Stop(context.Context, string, operations.Ownership) error
	Delete(context.Context, string, operations.Ownership) error
	Running(context.Context, string) (bool, error)
}

// internal/app/inventory.go - before issue #137
type TartInventory interface {
	List(context.Context) ([]tart.VM, error)     // adapter type in a port
}
```

**Built by issue #137.** The ports below are the code as it now stands, in
`internal/executor`. One correction to the original proposal: they are not in
`internal/domain`, because `internal/operations` imports `internal/domain` (so a
port taking an `operations.Ownership` would be an import cycle) and
`domain.Instance` already names the scheduler's live instance. Only the name
grammar went to `domain`.

```go
// internal/executor - the request a backend receives. Replaces tart.Request.
type InstanceSpec struct {
	Name      string             // validated by domain.ValidateInstanceName
	Image     string             // Tart base VM name, or an OCI image reference
	CPU       int
	MemoryMB  int
	DiskGB    int                // ignored by backends with no disk sizing
	Ownership operations.Ownership
}

// Backend is what a node's execution technology must provide. One
// implementation per node type: Tart on macOS, containers on Linux/amd64.
type Backend interface {
	Create(context.Context, InstanceSpec) error
	Start(context.Context, string, operations.Ownership) error
	Stop(context.Context, string, operations.Ownership) error
	Delete(context.Context, string, operations.Ownership) error
	Running(context.Context, string) (bool, error)
	Reap(context.Context, string, operations.Ownership) error
	List(context.Context) ([]Instance, error)
}

// Instance is what List reports. Replaces tart.VM.
type Instance struct {
	Name    string
	Running bool
	Source  string
}

// CommandRunner is one argument vector of a backend's CLI: the primitive both
// CLI-shelling adapters share, and what the readiness probe is typed on.
type CommandRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

// HostProbe lifted internal/adapters/macos's Snapshot/Guardrails contract out of
// the macOS package so a Linux probe reading /proc can satisfy it unchanged.
// HostSnapshot, Guardrails, AdmissionRequest, AdmissionDecision, and
// Guardrails.Evaluate moved with it, unchanged. It returns no error because the
// snapshot carries its own Freshness and Cause: an unreadable machine is a
// stale or unavailable observation, never a zero-resource one.
type HostProbe interface {
	Snapshot(context.Context) HostSnapshot
}
```

`domain.ValidateInstanceName` took over from `tart.ValidateName`, which was the
de facto global instance-ID grammar. Its regex,
`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, is already a legal container name, and
`tests/contract/executor_port_test.go` asserts exactly that against Podman's
grammar.

That contract file is what chunk 2d implements against. It pins the verb set and
signatures, the field sets of `InstanceSpec` and `Instance`, one backend
satisfying all three caller ports (`lifecycle.VMControl`, `app.ExecutorInventory`,
`discharge.VM`), the name grammar, and it drives the real `ProvisionExecutor` and
`DrainExecutor` through a whole runner lifecycle against an in-memory backend —
so "does my adapter work" is a question the test suite answers before a container
ever starts. A `depguard` rule denies `internal/adapters/{tart,macos}` to every
layer above the port, and `internal/daemon` is the only package that names either.

Wiring needs no new plumbing. `internal/daemon/runtime.go` already carries
`newVM`, `newReaper`, `readiness`, `bootstrap`, and `inventory` as injectable
constructors, the readiness probe is typed on `executor.CommandRunner` so a
container command runner drops in without a second copy of the poll loop, and
`ProvisionExecutor.Bases map[domain.Platform]string` is already a platform-keyed
dispatch table.

## `hostBudget`

An optional top-level configuration key:

```json
"hostBudget": { "cpu": 4, "memoryMb": 10240 }
```

Enforced in `freeCapacity` (`internal/scheduler/scheduler.go:498`), the single
point at which every admission pass obtains its envelope, using the existing
`physicalBound(configured, total, live)` primitive so that an omitted budget is
a byte-for-byte no-op. Live instances of every platform are charged against it.
The aged-work escape of ADR 0018 does **not** lift it: it is a configured
constraint, not a throttle.

Files to change, all non-test: `internal/config/config.go` (struct, wire tag as
a pointer so the default stays omitted, `normalizeGuards`, `Encode`,
`Validate` rejecting a budget smaller than the largest enabled profile),
`internal/app/config.go` (`BuildSchedulerConfig`), and
`internal/scheduler/scheduler.go` (`Config` field, `freeCapacity` restructure,
extract `liveResources`). Roughly 55 lines added and 6 removed.

One decision to take with the implementation:
`Config.authorityProfileCapacities` (`config.go:785-798`) derives the truthful
advertised `maxCapacity` per ADR 0015. A budget below `maxLinuxCpu` makes those
advertised capacities inflated. Fold the budget in, which is
ADR-0015-consistent and a breaking validation change for any host that sets
both `canonicalJobInventory` and `hostBudget`. Node C is the only host that
needs a narrow budget, so the blast radius is one file.

Tests: a case in the `internal/config` decode/encode/round-trip/omit table,
a new `scheduler_host_budget_test.go` covering budget-clamps-admission,
unset-is-no-op, binds-macOS-too, binds-aged-work, composes-with-elastic and
with-static, a scenario in `tests/integration/second_pilot_envelope_test.go`,
and the world-config mirror in `tests/simulation/world_test.go`.

## Configuration layout

```
config/
  fleet.example.json                 unchanged
  nodes/
    shared/
      scopes.json                    scopes, targets, installations
      profiles.linux.json            Linux variant vectors, arch-free
      profiles.macos.json            builder and maestro vectors
    mac-mini.json                    node A overlay + scale-set ownership
    geekom.json                      node B overlay + scale-set ownership
    mac-studio.json                  node C overlay + hostBudget + ownership
    rendered/
      mac-mini.json                  generated, committed, installed as-is
      geekom.json
      mac-studio.json
scripts/
  render-node-config.sh              <node> -> rendered/<node>.json
```

Configuration decoding is strict, single-document, and has no includes or
environment substitution, so sharing must happen before the daemon sees the
file. A contract test re-renders every node and fails if a rendered file is
stale, if two nodes claim the same `(scope, scale-set name)` pair — the
invariant of ADR 0034 §2 — or if any rendered file fails `config.Validate`.

Secrets are never rendered. The GitHub App private key stays at a per-node path
referenced by `github.app.privateKeyFile`; node A may keep using the Keychain.

## Phase 1 — node B bring-up

Part A is doable the hour the machine arrives. Part B is gated on Phase 2,
because the daemon cannot provision a Linux runner on x86 without the executor
adapter. Do not start Part B before Phase 2 is green on node B in observe mode.

### Part A — the day the GEEKOM arrives

**Hardware and OS**

- [ ] Record the actual CPU, RAM, and NVMe capacity; confirm 12c/24t and the
      installed memory, which decides how many `4x8`/`6x12` shapes fit.
- [ ] Install Ubuntu Server 24.04 LTS (or Debian 13) — x86_64, minimal, no
      desktop.
- [ ] Enable SVM/AMD-V in firmware. Verify: `test -e /dev/kvm && lscpu | grep -i
      'amd-v\|svm'`.
- [ ] Set the hostname to `geekom`, static DHCP reservation, `unattended-upgrades`
      for security updates only.
- [ ] Confirm outbound HTTPS to `github.com` and `*.actions.githubusercontent.com`;
      confirm **no** inbound port is open except SSH on the LAN.

**Accounts and runtime**

- [ ] Create the unprivileged service account `fleet`; enable lingering:
      `loginctl enable-linger fleet`.
- [ ] `apt install podman uidmap slirp4netns fuse-overlayfs`; verify rootless:
      `sudo -u fleet podman info | grep -i rootless`. The daemon refuses to start
      in any mutating mode against a runtime that is absent, unusable, or
      root-ful, so this is a hard prerequisite and not a recommendation.
- [ ] Add `fleet` to the `kvm` group; verify
      `sudo -u fleet podman run --rm --device /dev/kvm alpine ls -l /dev/kvm`.
- [ ] Set `/etc/containers/registries.conf` to the registries the runner image
      needs and nothing else.
- [ ] Provision the runner base image: Ubuntu 24.04 + Node 22 + Go + Android
      SDK/NDK (`ndk;27.x`) + emulator + `platform-tools` + Maestro, plus the
      `tart-runner-fleet-bootstrap` binary at
      `/usr/local/libexec/tart-runner-fleet-bootstrap`. Tag and pin by digest.
      Start from [`LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md), which is the
      same package set built and verified on arm64 — its "Adapting this for
      node B" section lists what changes for amd64 and for containers.
      The helper must launch the runner and **detach without `systemd-run`**,
      which an ordinary container does not have; this is the one item of Phase 2
      that is image work rather than adapter work.
- [ ] **Run the real-podman acceptance test, and require it to pass:**
      `TRF_PODMAN_SMOKE=required ./scripts/podman-smoke.sh`, with
      `TRF_PODMAN_IMAGE` set to the runner image. This is the gap CI cannot
      close — see below — and on this node `SKIPPED` is not an answer.
- [ ] Confirm the resource limits actually bind. Rootless podman honours
      `--cpus` and `--memory` only where the cgroup controllers are delegated to
      the service account; with lingering enabled on a cgroups-v2 host they are,
      and `podman run --rm --cpus 1 --memory 512m <image> true` printing no
      warning is the check. An unenforced limit is not a correctness failure —
      the scheduler's own envelope is what bounds the node — but it is a fact
      an operator should know before the first Android emulator boots.

**Daemon**

- [x] Build the `linux/amd64` release: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`.
      Issue #138 made it a published release archive with its own SBOM, build
      identity, and entry in the shared `SHA256SUMS`, so there is nothing to
      build by hand — download it.
- [x] Install to
      `${XDG_DATA_HOME:-~/.local/share}/tart-runner-fleet/{current,state,credentials}`,
      mirroring node A's `~/Library/Application Support` layout. Resolved by
      `internal/hostpaths`, so the daemon, the CLI, and the renderer agree.
- [x] Install the `systemd --user` units (the `launchd` plist equivalents) and
      the renderer, shipped in `systemd/` beside `launchd/` and published in the
      release archive. Install the controller unit only; see the correction
      above for why the updater timer stays disabled.
- [ ] Copy the GitHub App private key to
      `~/.local/share/tart-runner-fleet/credentials/github-app.pem`, mode `0600`,
      owner `fleet`.
- [ ] Render and install `config/nodes/rendered/geekom.json` with
      `macosBurst.enabled: false`, the Linux variant matrix, `hostBudget` stated
      explicitly, and **no `github.scopes` entries yet**.
- [ ] Start in **observe** mode. Verify `fleet status`, `fleet doctor`,
      `fleet health` and that the host probe reports plausible CPU, memory,
      disk, load, and swap from `/proc`. `scripts/observe-smoke.sh` is that
      check as one command, and CI runs it on Linux on every commit.

### Part B — cutover, after Phase 2 is green

- [ ] Add `github.scopes` to `geekom.json`, listing the Linux scale sets for
      `fleet-repo`, `knee-repo`, `hotel-repo`, `sudoku-repo`, `budgie-org`,
      `rnw-repo`, with **new names** (`trf-<scope>-<profile>-amd64`) so they
      cannot collide with node A's live sets.
- [ ] `fleet scale-sets provision --config ... ` (dry run), inspect, then
      `--apply --write --confirm provision-scale-sets --reason "node B bring-up"`.
- [ ] Promote node B to authority. Confirm one job end to end on
      `trf-linux-amd64-1x2` via the fleet's own canary workflow.
- [ ] Advertise the arch-floating aliases (`linux-small` … `linux-xl`) on node
      B's sets, so consumers keep working unchanged.
- [ ] **Drain node A's Linux**: remove every Linux scale set from every scope in
      `mac-mini.json`, re-render, re-provision node A. Watch `fleet queues` on
      node A go to zero for `small`/`medium`/`large`/`xl` and node B's rise.
- [ ] Delete node A's Linux scale sets in GitHub once node A reports no Linux
      instances and no Linux demand for a full day.
- [ ] Confirm the arithmetic on node A: `builder` and `maestro` now coexist, and
      `fleet queues` no longer shows `builder` waiting behind a Linux guest.
- [ ] Revisit `maxLinuxWhenMacosIdle`, `mixedPlatformAdmission`,
      `mixedProfileCohorts`, `linuxReservationAgeSeconds` on node A; they now
      describe a case that cannot occur.
- [ ] Rollback at any point: re-add node A's Linux scale sets from git history,
      re-provision, stop node B's daemon. Node A's configuration is a committed
      file, so this is one revert and one provisioning run.

## Phase 2 — the x86 executor adapter

Four chunks, each independently deployable. Effort is focused engineering days
for an agent working with the existing test gates.

| # | Chunk | Deliverable | Effort |
| --- | --- | --- | ---: |
| 2a | `hostBudget` | Node A gets an explicit ceiling; node C is unblocked. Ships as an ordinary release to node A alone. | 0.5 d |
| 2b | Executor port extraction | **Done, issue #137.** `executor.InstanceSpec`, `executor.Backend`, `executor.Instance`, `executor.CommandRunner`, `domain.ValidateInstanceName`, and `executor.HostProbe` lifted out of `internal/adapters/macos`. Refactor only, zero behaviour change; all gates green and the DST corpus identical across three runs per arm; ships as a no-op release. | 1 d |
| 2c | Linux/amd64 host support | **Done, issue #138**, with one correction below. `linux/amd64` release archive and platform-selected updater asset (`autoupdate.Target`), `systemd --user` units and `render-systemd.sh`, XDG paths (`internal/hostpaths`), file-based credentials, a `/proc` host probe (`internal/adapters/linux`), and `platformFor(goos)` wiring `noexecutor.Backend` on a node with no execution technology. **Deliverable met: the daemon runs on any Linux box in observe mode**, asserted on every commit by `scripts/observe-smoke.sh` in CI. | 2–3 d |
| 2d | Container executor adapter | **Done, issue #139.** `internal/adapters/podman` implementing `executor.Backend` over the rootless Podman CLI, an `executor` configuration block selecting the backend, the OCI image, the podman binary, the `/dev/kvm` profile grant and the hold command, `platformFor(goos)` wiring the adapter when a Linux node names it, a fail-closed startup probe, and the executor-port conformance harness extended to drive the real lifecycle through it. **Deliverable met in code: node B can serve `trf-linux-amd64-*` the moment it exists.** | 2–3 d |

Roughly 1,500–2,000 lines of production code and a comparable amount of test
code, across about 25 files. Sequenced so that 2a and 2b land on node A as
ordinary releases before node B exists, and each of 2c and 2d is separately
verifiable.

### What CI covers of chunk 2d, and what it cannot

Stated plainly, because the difference is the whole risk of shipping an executor
for a machine that has not arrived.

| Claim | Covered by | Where it runs |
| --- | --- | --- |
| Every verb's argument vector, every error classification, every re-observation after a failed command, the no-adoption rule, the `/dev/kvm` grant by name prefix | table-driven unit tests over a fake `executor.CommandRunner` | every machine, every commit |
| The adapter is sufficient to provision and drain a whole runner through the real `ProvisionExecutor` and `DrainExecutor` | the executor-port conformance harness in `tests/contract`, which drives every shipped backend | every machine, every commit |
| The wiring: a Linux node with an `executor` block gets Podman in all five constructors, and one without it stays observe-only | `internal/daemon/platform_test.go` | every machine, every commit |
| A configured-but-absent runtime refuses to start the node | `internal/daemon` startup preflight test | every machine, every commit |
| **That podman accepts these argument vectors, prints this JSON, reports these states, carries a secret in over `podman exec -i`, and honours `--device /dev/kvm`** | `scripts/podman-smoke.sh` driving `tests/integration/podman_live_test.go` | **nowhere automatically, today** |

The last row is the gap. The fleet's own CI runs on its Linux scale sets, which
are Tart guests whose image ships no container runtime, and installing one per
job would add a package pull to every commit for a runtime the node under test
does not otherwise need. So CI runs `scripts/podman-smoke.sh` in best-effort
mode: it prints `SKIPPED` and exits 0 today, and becomes a real gate with no code
change the day a runner image ships podman — which is itself a reasonable thing
to do once node B builds the images.

Until then the honest statement is: **the adapter has never been run against a
real container runtime by an automated gate.** The bring-up checklist closes
that with `TRF_PODMAN_SMOKE=required`, and no job may be routed to node B before
it passes.

Not required, and worth stating because the audit expected otherwise:
`internal/scheduler` needs no generalization. Node B runs a single-platform
configuration and exercises `planLinux` only.

### Correction: `launchctl` → `systemctl` is not part of 2c

The plan listed the release transaction's port with the rest of chunk 2c. It is
not there, and the reason is worth recording. `internal/autoupdate.LocalHost`
is a whole transaction expressed in launchd — a plist rendered and linted with
`plutil`, generations swapped with `bootout`/`bootstrap`/`kickstart`, an updater
that rewrites itself through a separate handoff job, and a rollback that undoes
all of it. Porting it means a supervisor port with two implementations and two
sets of failure-path tests, and none of it is needed to run a node in observe
mode, which is what Part A is.

So issue #138 shipped the packaging and stopped there. All three units are
rendered by `render-systemd.sh` from the release, so node B will never need a
hand-written unit; `NewLocalHost` refuses a domain that is not a launchd target,
so `fleet update apply-latest` on node B names the gap instead of failing inside
`plutil`; and the manual `systemctl --user` bridge is documented in
`docs/OPERATIONS.md` and `INSTALL-linux.md`. The systemd release transaction is
a separate chunk, and Part B does not depend on it: a node that is updated by
hand can still be promoted to authority.

### What the darwin-assumption audit actually found

`GOOS=linux GOARCH=amd64 go build ./...` was already clean before issue #138
began — the tree has no `//go:build darwin` constraints and every adapter is a
CLI-shelling adapter that compiles anywhere. The problem was never compilation;
it was what the binary does when it runs. Three findings were real:

- `internal/daemon` wired Tart and `internal/adapters/macos` unconditionally, so
  a Linux daemon would boot and then report an unavailable host observation and
  an unavailable Tart inventory forever.
- `adminapi.DefaultSocketPath` derived the socket from `os.UserConfigDir`, which
  is the installation root on macOS but `~/.config` on Linux, while the operator
  interface's own defaults were the literal Apple paths. A Linux daemon listened
  on a socket its own CLI would not look for.
- The updater's release asset name was the darwin/arm64 archive, unconditionally.

The Keychain was not one of them: `credentials.GitHubAppKey` already prefers
`github.app.privateKeyFile`, and multi-scope authority validation already accepts
the file as a complete credential source. The one constraint to record is that
the *legacy* single-scope authority path still requires Keychain fields
(`config.go:749`), so a Linux node must use the scoped configuration form.

## Phase 3 — arch-floating labels and per-repository migration

Node B's canonical labels are `trf-linux-amd64-*`; the arch component stops
being the `arm64` constant at `internal/config/labels.go:23` and becomes a
node property, still derived and still unable to lie (about 150 lines).

Consumers then fall into two groups.

**Arch-floating** — name an alias, get whatever the owning node is. No workflow
edit at all; the migration is provisioning on node B and removal on node A.

| Repository | Audit |
| --- | --- |
| `tart-runner-fleet` | Go, `CGO_ENABLED=0`, cross-compiles its darwin/arm64 release from anywhere. Float. |
| `knee-doctor`, `hotel-provence` | Node plus Playwright chromium/webkit. amd64 is the better-supported target. Float. |
| `budgie`, `suuudokuuu` web legs | Node/Expo builds. Float. |

**Arch-pinned** — name a canonical label because the architecture is the point.

| Repository | Workload | Action |
| --- | --- | --- |
| `rnw-community` | `android-maestro.yml`, x86_64 AVD | Pin `trf-linux-amd64-6x12`. No other edit; the workflow already asks for `arch: x86_64` and starts working. |
| `suuudokuuu` | `mobile-build.yml` Android APK | Move `runs-on` from `macos-builder` to `trf-linux-amd64-4x8`; the x86_64 Linux NDK is supported where the arm64 one does not exist. |
| `suuudokuuu` | `android-maestro.yml` | Replace the Redroid step with `reactivecircus/android-emulator-runner`; set `ANDROID_E2E_ABI: x86_64` in `mobile-build.yml`; pin `trf-linux-amd64-6x12`. |
| `budgie` | Android build on `ubuntu-24.04` | Move to `trf-linux-amd64-4x8`. Same arch, comes home. |

Migrate one repository per pull request, in that order, most-broken first.
`rnw-community` is first because its workflow is failing today.

## Node C — Mac Studio

- [ ] Build the Maestro-only macOS base locally from the pinned public Cirrus
      image, following [`BASE_IMAGE.md`](BASE_IMAGE.md). Do not transfer node
      A's 91 GB image: it carries an Android toolchain node C cannot use, and
      the copy would saturate node A's uplink for days while it serves jobs.
- [ ] If the node's configuration also carries Linux profiles — the deployed
      `fleet.json` does, which this plan does not anticipate — build its Linux
      base locally too, following
      [`LINUX_BASE_IMAGE.md`](LINUX_BASE_IMAGE.md). The same
      do-not-transfer argument applies and has already been proven the hard
      way: four `rsync` attempts of an 8.6 GB `disk.img` over the home uplink
      failed or truncated silently.
- [ ] Install the same release as node A; `launchd` LaunchAgent, unchanged.
- [ ] Render `config/nodes/rendered/mac-studio.json`:
      `hostBudget: {cpu: 4, memoryMb: 10240}`, `macosBurst.enabled: true` with
      `maestro` only and `builder` capped out of reach, no Linux scale sets.
- [ ] Assign node C the `maestro` scale set of the `sudoku-repo` scope **only**
      — the largest single `maestro` consumer at 109 starts in seven days.
      Remove that one scale set from node A's configuration in the same change.
      No new label, no consumer edit, no routing ambiguity.
- [ ] Verify the budget binds: with one `maestro` guest live (4 vCPU /
      7168 MiB), `fleet status` on node C must show no remaining envelope, even
      when the machine is otherwise idle.
- [ ] Confirm outbound-only: no inbound port, no link to node A or B, operator
      access by SSH outside the fleet's contract.
- [ ] Rollback: move the scale set back to node A's configuration and
      re-provision. One file, one command.

An alias-based overflow lane — node C advertising `macos-maestro-overflow`
across every scope — is the alternative, and it is deferred until Spike 3
answers whether GitHub distributes across two scale sets that advertise one
label. Scope ownership needs no such answer.

## Observability

No new service, no aggregator, no dashboard. Each node keeps its own
`fleet status`, `fleet queues`, `fleet instances`, `fleet doctor`,
`fleet health`, `fleet metrics` over its own unix socket.

Add one script, `scripts/fleet-nodes-status.sh`, which SSHes to each node in a
static list, runs `fleet status --output json`, and prints one combined table:
node, version, mode, host pressure, queue depths, oldest wait, instance counts.
It holds no state, has no daemon, and a node that is unreachable prints as
unreachable rather than failing the run. That is the entire multi-node
observability story for the MVP, and the ADR's non-goals forbid more.

## Updater

Per node, unchanged. Each node runs its own five-minute quiescence-gated,
forward-only updater and activates releases independently. Nothing couples
release activation across machines; a node may sit a release behind
indefinitely.

Node B needed one addition, and issue #138 made it: the release workflow now
publishes `tart-runner-fleet-<tag>-linux-amd64.tar.gz` beside the darwin archive
in the same `SHA256SUMS`, and `autoupdate.Target` selects the asset — and the
service definition a generation must carry a verified copy of — by node type
instead of hardcoding darwin/arm64.

The manual release bridge on node A — swap the plist `Program` path and
`state/installed-generation.json` together, `launchctl bootout`, wait twenty
seconds, `enable` and `bootstrap`, then **verify** `launchctl list` — has a
`systemd --user` equivalent on node B (`systemctl --user daemon-reload`,
`restart`, then `systemctl --user status`). Both are in `docs/OPERATIONS.md`.
On node B the bridge is not a fallback but the only path, until the systemd
release transaction lands; see the correction under Phase 2.

## Simulation

Nothing changes. `tests/simulation` models one host because ADR 0031 says so,
and under ADR 0034 one host is one whole fleet. Every property the harness
checks — liveness, bounded starvation, plans always apply, identity uniqueness,
no double admission, eventual quiescence, conservation, no stranded demand, no
drain churn — is a per-node property with no cross-node counterpart. Simulating
a second node would simulate GitHub's routing, which is not this codebase's
code.

The harness gains one configuration it already supports: a single-platform world
with `macosBurst.enabled: false`, which is node B's shape. Add that to the
world-config sweep in chunk 2d; add nothing else.

## Open spikes

| # | Question | Gates |
| --- | --- | --- |
| 1 | Can a peer delete an inherited scale-set session and immediately create its own? | Carried over from issue #99. Not needed by this plan; needed by any future steward. |
| 2 | Does GitHub accept a `maxCapacity` decrease while jobs are assigned? | Carried over from issue #99. Needed before any elastic cross-node capacity. |
| 3 | Given two scale sets in one scope advertising the same label, does GitHub distribute jobs between them, or prefer one deterministically? | Gates the alias-based overflow lane for node C. Until answered, node C owns scale sets by scope. |
| 4 | Does the `suuudokuuu` Maestro suite pass against an x86_64 emulator at the timings its challenge flows assume? | Gates the `suuudokuuu` Android migration in Phase 3. |

## Working agreement

For every agent implementing a chunk of this plan.

- **Commit each logically complete chunk immediately**, with a conventional
  commit message. Never batch a session's work into one commit.
- **Post a progress comment on the tracking issue at every milestone** — seam
  chosen, tests red, tests green, gates passed, pull request opened, deployed —
  so a fresh agent can resume from issue comments and commits alone after a
  crash or a session limit.
- **When a pull request merges and its deploy step completes**, tick the chunk in
  the tracking epic and close the sub-issue with a final summary comment.
- **Every pull request references its issue**, and states which node it deploys
  to and how to roll back.

Every chunk in Phase 2 and Phase 3 must end *validated and deployed*, not merged
and waiting. A chunk that cannot be deployed on its own is too big and should be
split.
