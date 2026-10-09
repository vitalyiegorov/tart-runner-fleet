# ADR 0060: Portable Linux overflow preserves architecture truth

## Status

Accepted for issue #358's inventory fix and isolated MacBook validation.
Production activation remains a separate ADR 0015 promotion. Amends ADR 0059's
foreign-platform proof; proposes a bounded exception to ADR 0055 only after the
project and lifecycle evidence below passes.

## Context

The Linux queue is architecture-pinned. A native ARM64 guest cannot satisfy an
AMD64 label, even when both guests run Linux. The existing MacBook Ubuntu ARM64
pilot boots, but its infrastructure smoke tests do not prove project CI.

Replaying an ARM64 Linux binding beside a canonical AMD64 job fails strict
repository inventory reconciliation; the reverse direction fails too. The
simulated repository snapshots lacked foreign-architecture jobs. This is a
world-model blind spot exposing a fleet defect, not an oracle defect.

## Decision

Extend the existing foreign-job predicate to OS **and architecture**, using the
canonical labels already derived and advertised by each binding. Do not add an
architecture field to bindings, another scheduler pass, or a new configuration
flag. Every local binding accepting the repository must prove the requested
capability absent. A same-OS binding without a canonical architecture remains
unknown and fails closed. Same-architecture unknown shapes, ambiguous labels,
and unavailable snapshots remain errors. Ignoring foreign inventory grants no
assignment, acquisition, or lifecycle authority.

The smallest overflow pilot is one existing Tart ARM64 Linux guest on MacBook,
with one validated browser job and the same 2 CPU / 4 GiB contract as Omarchy.
Retain the existing Ubuntu/image provenance and macOS images. No AMD64
emulation, Android/KVM promise, shared database, hub, or new monitor is needed.

Use a job-scoped portable alias where an existing general alias could admit
unvalidated release or native jobs. Both nodes retain their truthful canonical
labels; browser/native caches include architecture. A workflow change affects
future jobs only. Existing AMD64 jobs cannot be retargeted by provisioning a Mac.

Label federation is an opportunistic pool, not an Omarchy-first guarantee.
Initial overflow is explicitly enabled for one canary after fresh primary
capacity and Mac eligibility evidence. Automatic primary preference is not
implemented without a demonstrated gap and deterministic evidence. Existing
resource floors, shared vectors, macOS protection, broker ownership, and
at-least-once delivery stay intact.

## Promotion evidence

Before production routing: validate the exact project source and lockfile,
pinned Node/pnpm/Playwright, complete shard selection and unchanged assertions
on Chromium and WebKit; compare duration and CPU/RAM/disk/paging with AMD64.
Missing preview access or fixture credentials is a blocker, not permission to
skip required tests. Preserve failed traces, screenshots, and reports.

Then prove current bootstrap helper and runner identity, systemd scope and
shutdown contracts, image capabilities, observe/shadow inventory, truthful
capacity, one GitHub-confirmed ephemeral job, runner disappearance, and rollback
under ADR 0015. Withdraw future capacity without stopping healthy jobs. Expand
to Studio/Mini separately after MacBook proves useful throughput and no macOS
regression. The vsock failures recorded in ADR 0055/#264 remain a canary risk to
exercise, not a historical problem to assume fixed.

## Evidence

### Go CI routing, 2026-10-09

The five Go CI jobs use the existing `linux-go-2x4` and `linux-go-4x8`
aliases instead of AMD64-only labels. They execute the same checks and retain
their deadlines. Go and lint caches remain disabled, so this change cannot
restore binaries from the other architecture. Architecture-specific release
and Android/KVM workflows retain their existing labels.

The MacBook's ARM64 guest passed full `make ci` and a controller-managed
ephemeral GitHub canary before this routing change. Production configuration
activation is a separate guarded updater transaction on the released controller;
this workflow change neither installs services nor changes controller authority.
The first PR run exercises the new routing with the current toolchain. Existing
queued AMD64 jobs keep their original assignment.

Unit replay fails on the parent revision for both architectures. Simulation
adds 24 seeds in each architecture direction beside the existing 48 OS cases,
proving one local attribution and no foreign attribution. Pure predicate cases
cover missing canonical labels, mixed local architectures, unaccepted
repositories, and unknown local shapes. No database migration, `fleet.v1`
change, service installation, or authority promotion is part of the code fix.
