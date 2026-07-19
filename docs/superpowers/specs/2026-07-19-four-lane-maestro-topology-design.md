# Four-lane Maestro Simulator topology on one M4 Mac mini — design

Date: 2026-07-19
Status: awaiting approval (brainstorm deliverable; no implementation yet)

## Problem

Run 4 parallel Maestro iOS Simulator test lanes on the live M4 Mac mini
(10 cores, 24 GiB, macOS 26.5, Tart 2.32.1) with lower median wall time,
acceptable p95, and no infrastructure-failure regression versus the stable
2-lane control.

## Ground truth (verified 2026-07-19)

### The 2-VM limit is hard and unchanged

"4 Mac mini VMs" on one host is not achievable. The limit of two concurrent
macOS guests is enforced in the kernel (`hv_apple_isa_vm_quota`, per
khronokernel's reverse engineering), stated in Apple's macOS EULA, and
independently confirmed by both Tart (cirruslabs/tart discussion #1054) and
Anka (Veertu FAQ). Nested virtualization (M3+/macOS 15+) is Linux-guest-only.
Apple's Containerization framework is Linux-only. The only bypass is a
development kernel with SIP disabled — not viable. Four lanes on one host
therefore means either 2 macOS VMs × 2 Simulators each, or bare metal.

### Where the prior work actually lives

- **Fleet repo (`tart-runner-fleet`)**: all scheduler-side support is merged
  on main — hidden 2-VM Maestro clamp removed (#62), `sync=none` +
  VirtioFS `ci-shared` + headless VM boot (#60), config-only rollout fix
  (#61), macOS-exclusive admission (#63), scale-set queue lookahead (#58).
- **App repo (`~/budgie`)**: the entire four-lane experiment is on
  `origin/codex/macos-vm-performance-experiments` (16 commits, ending at
  `a1a37de58`). Its draft PR #599 was **closed without merge on
  2026-07-19T05:24Z**. Nothing from it — the 2×2 lane matrix, VirtioFS +
  SHA-256 handoff, headless AWT, staggered lane start, rewritten deep-link
  priming, flow-timing TSVs — is on `main`. Main still runs 2 single-lane
  shards with an ad-hoc modulo-2 flow split.
- **Not found anywhere on this machine**: the image builder with two-device
  Simulator prewarm, Metal verification, and 180-second first-boot waits
  described in the experiment notes. If it exists it is on the CI host
  itself. The notes' 3,072-MiB swap guard is also not in the fleet repo
  (repo default/example is 2,048 MiB). Both must be located or rebuilt
  before they can be relied on.

### Why the four-lane runs failed (root-cause chain)

1. Run `29651602642` proved genuine 4-driver overlap; failures were a test
   wrapper bug (optional deep-link priming) — fixed in `a36cac46d`/`a1a37de58`.
2. Run `29654440855` (post-fix): one lane hit the 180 s driver startup
   timeout; three lanes failed first-flow accessibility waits while the
   expected screen was visibly rendered. Both guests were under heavy memory
   pressure (~4.0 GiB compressed, ~180 MiB unused, 386–735 MiB guest swap).
3. Image-level cause: the base image has **one** persistent iPhone 17 Pro.
   Each clone `simctl create`d lane 2 cold and paid the first-boot storm
   while lane 1 started XCTest.

External evidence says this failure mode is generic, not ours alone:

- CircleCI documents the same "first-boot indexing storm": a fresh simulator
  spawns 100+ daemons and burns >300 % CPU for 1–2 minutes; their fixes are
  a pre-warmed simulator snapshot and permanently unloading `diagnosticd`
  (`launchctl unload -w`) in the CI image.
- Flink capped at **4 parallel iOS simulators on a 14-core / 54 GiB** Bitrise
  M4 Pro — instability, not RAM, was binding once virtualized. They use
  `simctl clone` of a pre-configured warm device instead of `simctl create`.
- Maestro (official) warns concurrent JVMs on one machine collide on shared
  temp driver files and ports without per-lane isolation.
- Open Maestro issues on iOS/macOS 26 (#3254 driver unresponsive after ~3
  flows, #3318 driver port unreachable on 2nd flow) mean longer startup
  timeouts alone cannot make lanes robust — plan for driver recycling.

### New operational risks surfaced by research

- **Tart quota leak on macOS 26**: cirruslabs/tart #1217 and #967 — the
  2-VM kernel quota sometimes is not released after a clean `tart stop`;
  only a host reboot clears it. The fleet daemon should verify quota release
  after teardown and alarm/reboot rather than assume.
- **Tart stewardship changed**: Cirrus Labs joined OpenAI (~2026-04); Cirrus
  CI shuts down 2026-06-01; Tart/Orchard are relicensed permissively with
  fees dropped but no dedicated team. Fine short-term; pin versions and
  treat major upgrades cautiously.
- **Thermals**: sustained load throttles M4 mini P-cores from ~4.5 GHz to
  ~3.3–3.8 GHz within 10–15 minutes. Judge promotion on p95 of full runs,
  not first-run medians.

## Approaches

### A. Reland the 2 VM × 2 prewarmed-Simulator topology, hardened (recommended)

Keep 4 vCPU / 9,216 MiB per guest. Reland the closed PR #599 branch downstream
of a base image that genuinely contains two settled iPhone 17 Pro devices,
plus guest-image hygiene the research showed we are missing:

1. **Two-device prewarmed base image** (blocking prerequisite). Create both
   devices at image-build time, boot each until settled, boot both again
   after the candidate VM reboot, record UDIDs. Verify the builder exists on
   the CI host or rebuild it; add `simctl clone` from the warm template as
   the lane-2 fallback instead of `simctl create` (never pay a cold create
   in a job again).
2. **Guest image hygiene**: `launchctl unload -w` `diagnosticd` analogs per
   CircleCI guidance; raise `maxproc`/`maxfiles`; keep Spotlight/updates
   disabled; keep unified logging (export-on-failure only, already in #599).
3. **Maestro lane isolation and lifecycle**: per-lane `TMPDIR`/work dirs
   (JVM temp collision), cap each JVM (`-Xmx` ~1 GiB) on top of headless AWT,
   and recycle the XCTest driver per shard (or every N flows) given Maestro
   #3254/#3318 on iOS 26.
4. **Keep from #599**: VirtioFS + SHA-256 handoff with artifact fallback,
   staggered lane start (should become a near no-op with prewarm — keep as a
   guard), failure-only log export, flow-timing TSV summaries,
   `fail-fast: false`.
5. **Fleet-side watchdog**: after each disposable VM teardown, verify a new
   macOS VM can still be admitted (quota-leak detection per tart #1217);
   fail closed to a host reboot alarm. Reconcile the 2,048 vs 3,072 MiB swap
   guard in one authoritative config.
6. **A/B protocol**: identical workflow to `a1a37de58` with only the new
   base image, `fail-fast: false`, promotion on repeated runs: lower median
   wall time, acceptable p95, ≥15 % host memory free, no swap growth, no
   infra failures. Only afterwards test suspend/resume templates (two
   independently identified templates — MAC constraint) or disk caching.

Pros: topology already proven to overlap 4 drivers; all scheduler support is
merged; addresses the actual root cause (cold lane 2 + guest pressure);
smallest new surface. Cons: guests stay memory-tight at 9,216 MiB — if AX
timeouts persist even prewarmed, memory, not startup, is binding (that is
Approach B's cue).

### B. Bare-metal 4 lanes (no VMs for Maestro)

Run 4 runner services on the host; each job creates/clones an ephemeral
simulator, tests, deletes it. Industry precedent: Clutch Engineering runs 3–5
bare-metal runners per Mac; GitLab's hosted macOS runners are bare metal;
per-simulator footprint ~1.3 GiB plus JVM/driver overhead fits 24 GiB with
far more headroom than 2 × 9 GiB guests.

Pros: eliminates double-virtualization memory pressure (the strongest
remaining failure signal), VM quota bugs, and per-VM boot cost entirely;
likely the fastest possible 4-lane wall time on this hardware. Cons: discards
the fleet's disposable-VM isolation and JIT-runner security model; leaked
processes/CoreSimulator corruption become shared-host risks needing aggressive
per-job cleanup (`simctl delete`, daemon resets); a rogue PR job owns the
whole host. Reasonable as a measured fallback lane-density experiment, not as
the default direction given the fleet's architecture investment.

### C. Scale out: second M4 mini, 2 hosts × 2 VMs

Linear, boring, supported. Each lane returns to the proven single-lane-per-VM
resource envelope (or 2×2 across both hosts for 8 lanes). The fleet daemon
already models per-host admission; Orchard exists if multi-host scheduling is
wanted. Pros: best per-lane stability and p95; no new failure modes. Cons:
hardware cost (~$600–800); does not answer the single-host optimization
question; second host to operate/update.

## Recommendation

**A now, C when the suite or team grows, B only as an experiment if A still
shows AX timeouts after prewarming.** Concrete next steps for A, in order:

1. Locate or rebuild the two-device image builder on the CI host; add
   `simctl clone`-from-warm-template fallback and `diagnosticd` unload.
2. Reopen/reland the `codex/macos-vm-performance-experiments` branch (do not
   let the closed PR #599 rot) with per-lane TMPDIR, JVM heap caps, and
   driver recycling added.
3. Add the fleet quota-release watchdog and reconcile the swap-guard value.
4. Rerun the exact `a1a37de58` A/B with the new image; promote only on the
   stated criteria; then evaluate suspend/resume templates as a separate A/B.

## Hypervisor migration assessment (Lume, MacVisor, Lima) — 2026-07-19

Question: do alternative Virtualization.framework wrappers solve our problems
(2-VM quota, quota-release bug, Simulator cold boot, warm pools)?
Answer: **no — do not plan a migration.** Every candidate sits on the same
Apple Virtualization.framework and the same kernel 2-macOS-VM quota; our real
pain points are Apple-level (quota, quota-leak bug) or Simulator-level
(cold-boot storm), not Tart-level. Verified verdicts:

- **Lume (trycua/cua, MIT)** — reject now, monitor. Active and well funded
  (YC X25), but built for AI-agent desktop sandboxes, not CI; Cua's own docs
  recommend Tart for CI/CD. No suspend/resume or snapshots at all (issue #15
  open since 2025-02). Two open bugs break our exact JIT-injection pattern:
  `lume ssh` drops piped stdin (#1514) and corrupts binary stdout (#1513).
  No `sync=none` control, no ASIF, no Xcode CI images, own stop-reliability
  bugs (#1184, #2168). Clone is APFS COW — parity with Tart, not a gain.
  Re-evaluate if #15, #1513, #1514 close.
- **MacVisor (ScaleNinja)** — reject. Real product name, but pre-release
  closed-source beta marketed solely through templated "vs X" SEO pages. No
  repo, no Homebrew, no downloadable binary, no versions, zero independent
  validation of its snapshot/rollback claims. Re-check in 3–6 months at most.
- **Lima (CNCF Incubating)** — monitor; Linux guests only. `limactl clone`
  is genuine APFS COW and the project is the healthiest of the three, but
  its vz backend is the same Virtualization.framework, so no density or RAM
  gain, and Linux VMs were never subject to the macOS quota. Migrating only
  the Linux side adds a second backend + new golden-image pipeline for zero
  measured benefit. Apple's `container` (v1.0, 2026-06) is likewise too
  immature. Lima is the designated exit ramp if Tart bit-rots post-Cirrus.

Mitigation for Tart stewardship risk instead of migration: pin Tart 2.32.1,
gate any Tart upgrade behind the fleet A/B validator, keep the adapter
boundary clean (all Tart calls live in `internal/adapters/tart`, 7
subcommands), and record Lima (Linux) plus "fork/patch Tart" (macOS) as the
documented exit ramps.

## Resource allocation (the "best resource" answer)

Per guest: 4 vCPU, 9,216 MiB (host ceiling on 24 GiB), root disk
`sync=none`, `--no-graphics --no-audio --no-clipboard` (paravirtual GPU stays
on), VirtioFS `ci-shared`. Per lane: 1 prewarmed iPhone 17 Pro, 1 Maestro JVM
(`-Djava.awt.headless=true`, `-Xmx1g`, unique TMPDIR), driver startup timeout
180 s, driver recycled per shard. Host: ≥15 % memory free, swap guard single
authoritative value, macOS-exclusive admission during Maestro cohorts.
