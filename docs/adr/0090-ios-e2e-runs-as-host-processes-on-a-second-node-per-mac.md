# ADR 0090: iOS e2e runs as host processes, on a second node per Mac

## Status

Accepted 2026-10-30 as a design and a code path. **Not production-proven**: the
host gates in §8 stand between this record and any job, and nothing here enables
authority or installs a service (AGENTS.md rule 10). Issue
[#454](https://github.com/vitalyiegorov/tart-runner-fleet/issues/454).

Keeps [ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) intact: a node
is one fleet and has one execution technology. Extends
[ADR 0058](0058-a-node-declares-the-execution-technology-it-has.md) with a third
declared technology. Applies [ADR 0012](0012-shared-cross-platform-capacity.md)'s
shared envelope per node and [ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)
§6's static `hostBudget` to split one physical Mac between two nodes. Narrows
the slot reasoning of [ADR 0077](0077-a-slot-count-is-backed-by-the-envelope.md)
for one node type.

## Context

iOS Maestro e2e is capacity-bound by a limit the fleet cannot change: Apple's
Virtualization framework runs at most **two macOS guests per host**.
`docs/BASE_IMAGE.md` records "a constantly full maestro queue"; the `maestro`
profile (`macos-4x7`) is `maxActive: 2` on both Macs. On mac-studio the live
configuration (read 2026-10-30) is `hostBudget` 8 vCPU / 16384 MiB with
`builder` `macos-6x12` and `maestro` `macos-4x7`: two maestro guests fill the
budget, and the builder cannot run beside them.

A Maestro job does not need a macOS guest. It needs one booted iOS simulator,
Xcode's tools, Node, and the Maestro CLI. Paying a 4 vCPU / 7 GiB guest, one of
two per host, for a simulator is the whole bottleneck.

**Spike, mac-studio, 2026-10-30**, while both macOS guest slots were busy (two
`trf-macos-4x7` guests running): four simulators (iPhone 17, iOS 26.2) were
created **on the host** as the interactive user, slimmed with simslim 0.12.2 and
mobile-ci's `profiles/ci.json`, and booted in parallel in **30 s** wall. Each
measured 1.63–1.65 GB `phys_footprint` and 96 processes. Host free memory went
75% → 57%, swap stayed 1148 MB, and settled CPU idle was 71% (the boot burst
briefly spiked load). Not yet measured: CPU while Maestro drives a React Native
app.

**Owner decision (made before this record):** run Maestro jobs as ephemeral JIT
runners that are **host processes on the Mac**, each leasing its own simulator
from a simlock worker on the host
([callstackincubator/simlock](https://github.com/callstackincubator/simlock):
leases, warm slim simulators, a capacity queue; mobile-ci adds
`actions/simlock-lease`, rnw-community/mobile-ci#305). Native builds stay on
Tart guests.

## Decision

### 1. A host-process Mac runs two nodes

The Mac runs a **second `fleet` daemon instance**, a node in ADR 0034's exact
sense: its own `fleet.json`, its own SQLite database, its own admin socket, its
own scale sets, its own `github.sessionOwner`. It declares
`executor.backend: hostprocess`. The Tart node on the same Mac is unchanged.

The alternative, two technologies inside one daemon, was rejected. It would
reopen what ADR 0034 closed: one scheduler arbitrating two execution
technologies with different failure modes, one database for both, and an
`executor.Backend` chosen per instance rather than per node. Every layer above
the port is written assuming the second can never happen. Two daemons share
nothing but the machine, and the machine is what §3 divides.

### 2. The node technology, as declared

```json
"executor": {
  "backend": "hostprocess",
  "image": "/usr/local/share/tart-runner-fleet/actions-runner-template",
  "binary": "/usr/local/libexec/tart-runner-fleet-host-launcher",
  "runnerUser": "ci-runner"
}
```

`image` is the runner template the launcher clones per job, which is what
`InstanceSpec.Image` already means: what an instance is made from. `binary` is
the launcher (default as shown). `runnerUser` is new and means something only to
this backend; it is refused on any other. A host-process node must enable
`macosBurst`, must declare no `linuxProfiles` (a Linux job run as a macOS
process would be a label that lies, ADR 0032), and may not carry container
settings. `macosBurst.baseVm` stays required by the schema and means nothing
there, exactly as `linux.baseVm` on the podman node.

### 3. Capacity: a static split, and each node's own pressure probe

Neither node sees the other's static budget, and per ADR 0034 §1 neither should.
The Mac is divided by two `hostBudget`s whose sum stays below the physical
machine, and each daemon's live host-pressure probe (`vm_stat`, swap, load, CPU
idle, disk) is the fail-safe for the rest, because it reads the whole machine
and so sees the other node's load. That probe is unchanged and is the
`internal/adapters/macos` probe on both nodes.

**First-host target, mac-studio (owner decision 2026-10-30).** 14 cores /
36 GiB, shared with the owner's other work. The Tart node's budget is not
raised.

| | Today | Target |
| --- | --- | --- |
| Tart node | `hostBudget` 8 / 16384, builder `6x12` + maestro `4x7` | `hostBudget` 6 vCPU / 12 GiB, **builder only**; the maestro VM profile is retired from this node once the host-sim lane is proven (§8 G5) |
| Host-process node | — | `hostBudget` 6 vCPU / 14 GiB, profile `1x2` × 5, simlock `ios.maxRunning = ios.maxDevices = 5`, `ramBudget.iosSlimBytesPerDevice` ≈ 2 GiB |
| Owner's other work | — | about 2 cores / 10 GiB |

The expected effect is two concurrent iOS e2e shards becoming about five. That
number must be confirmed by a CPU measurement under real Maestro load before the
maestro VM profile is retired.

**The job's vector is 1 vCPU / 2 GiB**, and that is deliberate. The simulator is
a process of the simlock worker, not of the job, so the job's vector charges the
runner, Node, and the Maestro JVM. Simulator memory is bounded by simlock's own
`ramBudget` and capacity limits, and by the pressure probe, and the host node's
`hostBudget` leaves room for it. macOS has no per-process CPU quota, so a vector
here is an **admission charge, not an enforced limit**; ADR 0012 already treats
vectors that way for the envelope, and the pressure probe is the backstop.

**No VM-slot accounting.** On a Tart node `fillableSlots` ignores macOS profiles,
because Apple's two-guest limit and `maxActive` bound them below the four slots
every node may declare. A host process is not a guest, so on a host-process node
the macOS profiles are counted exactly as Linux profiles are: five 1x2 slots in
a 6 / 14 GiB envelope validate, and the same file on a Tart node is refused.

### 4. The security boundary: the job never runs as the daemon's user

A job runs third-party code. The daemon's user can read the GitHub App key, the
database, and every runner's JIT configuration in flight. So:

- Jobs run as a dedicated, **non-admin** `ci-runner` account.
- The daemon reaches it only through one argument vector,
  `sudo -n -u ci-runner -- /usr/local/libexec/tart-runner-fleet-host-launcher <verb> ...`.
  A sudoers drop-in, validated with `visudo -cf` before install, admits exactly
  that program, as exactly that user, without a password, and sets
  `!log_input, !log_output` for it. There is no shell anywhere on the path
  (AGENTS.md rule 8).
- The launcher is **root-owned**, outside every directory the daemon's user can
  write, including its self-updating release tree. A launcher the daemon could
  rewrite would make the sudoers rule a grant of arbitrary code.
- The launcher reads a **root-owned** configuration
  (`/usr/local/etc/tart-runner-fleet/host-launcher.json`, refused unless owned by
  root and writable by nobody else) that fixes the instance root, the runner
  template, the runner's uid and home, and the capability manifest. It refuses to
  run as any uid but the configured runner's, refuses uid 0, and refuses a daemon
  that is itself the runner user (`SUDO_UID`). Every instance name is checked with
  `domain.ValidateInstanceName`, and every ownership value against a printable
  grammar.
- The runner's environment is **built, not inherited**: HOME and USER of the
  runner account, a fixed PATH, a per-instance `TMPDIR`, a locale, and the JIT
  variable. Nothing sudo kept reaches a job.
- The daemon refuses to start as root, and its preflight runs one `list` through
  the full path. That proves the sudoers rule, the launcher, its configuration,
  and the uid check before any mode above observe.

### 5. The launcher protocol mirrors the port

| Port verb | Launcher verb |
| --- | --- |
| Create | `create <n> <template> <controller> <resource> <operation>` clones the root-owned template (`cp -c -R`, an APFS clone) into a staging directory, writes an ownership marker, and publishes by rename. It is idempotent for the creating operation and a conflict for any other. |
| Start | `start <n> <operation>` spawns a supervisor in a new session (the process-group leader) and returns once it holds the instance. |
| readiness, liveness | `exec <n> true` succeeds iff the instance is running. These are the vectors `execReadiness` and the guest-liveness probe already send. |
| JIT bootstrap | `exec -i <n> /usr/local/libexec/tart-runner-fleet-bootstrap [--require-capabilities=…]` is the vector `lifecycle.StdinBootstrapper` already sends to a guest. |
| Stop, Terminate | `stop <n> <operation> <seconds>`: SIGTERM to the process group, the grace period, then SIGKILL; 0 seconds is SIGKILL alone. |
| Delete, Reap | `delete <n> <operation>` refuses a running instance and removes the tree. |
| Power, List | `list` returns JSON rows of name, image, operation and power. |

Because the launcher accepts the guest's own `exec` vectors, the lifecycle's
readiness, bootstrap and liveness code is reused unchanged, and the only new
lifecycle wiring is in `internal/daemon`. There is no guest filesystem, so the
ADR 0080 traffic sample is off on this node (`guestExec` is nil).

**Instance identity is a directory** under the runner user's
`~/trf-instances/<n>`, mode 0700, created fresh by Create and removed by Delete.
List enumerates only directories with valid instance names and reports the
operation from each ownership marker. Every mutating verb carries the operation,
and the launcher re-checks it against the marker, so the launcher never acts on
an instance the asking operation did not make. The daemon independently
re-checks its durable ownership record first.

**An instance runs once.** The supervisor writes a `booted` marker. Start on an
instance that already ran is a conflict, and a second supervisor refuses
(ADR 0010).

### 6. Power is a lock the job holds, never a guess

The supervisor takes an exclusive `flock` on `control/alive.lock` and passes the
descriptor to the runner, and through it to every process the job starts. The
kernel releases the lock when the last holder exits. So:

- `running` means some process of this instance still holds the lock;
- `stopped` means none does, or the lock was never created;
- any other outcome (a lock that cannot be opened or locked) is **`unknown`**,
  which the adapter reports as `InstancePowerUnknown` with a reason, never as
  stopped (AGENTS.md rule 4, ADR 0044).

A PID file could be fooled by a recycled PID; this reading cannot. A runner that
exits after its one job is, to the fleet, a guest that powered itself off,
which is exactly the premise every drain already reasons about.

### 7. The JIT configuration never touches disk

Daemon → sudo stdin → launcher `exec -i` → a FIFO (mode 0600, in the
instance's 0700 `control/`) → the supervisor → the runner's environment. A FIFO
has no data blocks. The bootstrap opens it non-blocking (so no waiting reader
means failure, not a hang), proves through the open descriptor that it is a
FIFO before writing a byte (a regular file planted at the path never receives
the secret), and the supervisor removes it after one read. The bootstrap checks
capabilities **before** reading stdin, so a capability failure provably cannot
carry the secret. Its exit statuses 3 and 4 are the guest helper's, so the
lifecycle's classifier reads both backends identically. Neither the launcher nor
the adapter ever logs the value (AGENTS.md rule 7). The runner itself writes its
credentials into its own instance directory, as it does in a guest, and Delete
removes them.

### 8. Host gates: what code cannot prove

Each of these must be proven on the real host, and recorded on #454, before the
host node runs any job. Plan rollout on **mac-studio first**; the hosts are
never idle. Scripts run over SSH must name their interpreter (`bash -s`), since
the login shell is zsh.

- **G1: CoreSimulator under `sudo -u` (open question).** CoreSimulatorService
  is a per-user launchd service, looked up in the caller's bootstrap namespace.
  A process that `sudo -u ci-runner` starts from the daemon user's launchd
  session runs as `ci-runner` but in the **daemon user's** per-user domain, so
  `simctl` may fail, reach the wrong service, or be denied. The simlock worker
  must also run as `ci-runner`: the job reaches it with `simlock --local`, and
  the simlock CLI talks only to the daemon of its own macOS user (a different
  user silently starts a private worker). `simlock daemon install` is a
  LaunchAgent, which runs only while that user has a **GUI login session**, and
  on the shared mac-studio that is not a given. **This is not proven.** The
  proof is to boot a device and run `simctl list` and a simlock lease, as
  `ci-runner`, from a process the fleet daemon started.
  *Fallback:* the root launcher variant enters the runner's own per-user domain
  with `launchctl asuser <uid> sudo -u ci-runner …`, and simlock is bootstrapped
  into `user/<uid>`. That needs a sudoers rule that runs the launcher **as
  root**, which widens the boundary, so it is a new decision on this record and
  not a silent switch.
- **G2: simlock as the runner user.** Pin a simlock version, then confirm
  `drivers.ios.deviceSet = default` (mobile-ci fails closed otherwise),
  `ios.defaultMode = slim`, `capacity.config.limits.ios.maxRunning = maxDevices = 5`,
  and `ramBudget.iosSlimBytesPerDevice ≈ 2 GiB` with `simlock doctor` as
  `ci-runner`. Jobs lease with `simlock --local lease --platform ios --detach
  --ttl <ttl> --agent-id <id> --mode slim` and release in an `always()` step.
  A dead job's lease expires by TTL.
- **G3: the sudo path.** `sudo -n -u ci-runner -- <launcher> list`, run as the
  daemon user, prints `[]`. `visudo -c` is clean.
- **G4: the host tool audit (an ADR 0079 analogue).** The host is not a sealed
  image, and its toolchain has drifted from the guests'. mac-studio is macOS
  15.7.4 / Xcode 26.3 / iOS 26.2; mac-mini is macOS 26.5 / Xcode 26.6 /
  iOS 26.5; the guest images pin Xcode 26.4.1. Before a scale set is exposed,
  audit the host against every consumer's pins, and write
  `/usr/local/etc/tart-runner-fleet/host-capabilities.json` from that audit.
  The scale sets' `requiresCapabilities` are checked against it at every
  bootstrap.
- **G5: CPU under real Maestro load.** Measure host CPU with five concurrent
  Maestro shards driving a React Native app. Only then retire the maestro VM
  profile from the Tart node and settle the `1x2` vector and the 5-slot target.
- **G6: a second daemon instance's service.** `fleet run` already takes
  `--config`, `--database`, `--admin-socket` and `--health-address`, so a
  second instance can run under the same user with its own state directory,
  socket and health port. What is not built is its packaging: the launchd label,
  the renderer, and the self-updater all assume one generation per user. The
  second instance therefore starts in **observe** mode under a hand-written
  label (for example `com.vitalyiegorov.tart-runner-fleet.host-sim`, health
  `127.0.0.1:9877`). Making it update itself is follow-up work, gated like
  every other promotion.

## Isolation is weaker than a guest, and this record says so

A Tart guest is a fresh machine per job. A host process is not:

- **Jobs share one uid.** Concurrent jobs on the host node, and the simlock
  worker, all run as `ci-runner`. A job can read, signal, or tamper with another
  concurrent job's files and processes, and with simlock's. Per-instance users
  would close this, at the cost of per-user simlock workers, and are not built.
- **Jobs share a home.** `~/Library` (DerivedData, CocoaPods and npm caches,
  the keychain, CoreSimulator's device set) persists across jobs, so one job can
  poison the next. The host node must therefore serve **only trusted
  repositories' e2e scale sets**, never fork pull requests.
- **A process can escape the group.** A job process that calls `setsid` and
  closes descriptor 3 leaves both the process group and the lock. The fleet
  then sees the instance stopped while the process lives on as `ci-runner`. If
  it keeps the lock but leaves the group, SIGKILL cannot reach it, the instance
  stays running, and the stop ladder escalates and fails visibly. Neither case
  can reach the daemon's user.

None of these crosses the boundary §4 exists for: no job can read the App key,
`fleet.db`, or a JIT configuration that is not its own runner's.

## Consequences

- `internal/adapters/hostprocess` implements `executor.Backend`;
  `internal/hostlauncher` and `cmd/tart-runner-fleet-host-launcher` are the
  launcher; `internal/daemon` chooses the wiring per file on darwin through one
  `eitherPlatform` dispatch, so no constructor can follow the file while another
  does not. Depguard and the contract test forbid the layers above the port from
  naming either package.
- The executor contract harness drives the **real launcher** in process, and a
  second contract test bootstraps a runner through `exec -i` and sees the JIT in
  its environment.
- The deterministic simulation gains `hostProcessNodeWorld` (1x2 × 5 within
  6 / 14 GiB on 14 cores / 36 GiB). It is the only world that can hold more than
  two macOS instances, so it has its own fuzz arm, it is in the corpus
  determinism gate, and a guard test fails if the generator never reaches a
  third. It was swept at 400 seeds × 200 ticks with no finding.
- `deploy/macos-host-runner/` holds the one-shot root setup script, its
  README, and an example node file that a test keeps valid.
- `guestbootstrap` exports `ReadJIT`, `CheckCapabilities` and `HelperPath`, so
  the launcher shares the guest helper's input and capability rules rather than
  copying them.

## Not addressed

- **Shipping the launcher in the release archive.** Release members are pinned
  by tests and the release workflow. Until that change, the launcher is built
  from the tagged source (`CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build
  -trimpath ./cmd/tart-runner-fleet-host-launcher`) and installed root-owned by
  the setup script.
- **The `launchctl asuser` fallback** of G1. It is designed above and not built,
  because it runs the launcher as root and must not exist before G1 shows it is
  needed.
- **Per-instance runner users**, which would close the shared-uid exposure.
- **ADR 0072's host-version gate** still applies to a host-process node, because
  it is evaluated for any macOS-enabled node. It protects macOS guests from
  macOS 27's page-cache defect, which a host process does not have. Narrowing it
  is a separate change; it costs nothing on the two hosts today.
