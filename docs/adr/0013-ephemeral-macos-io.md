# ADR 0013: Optimize disposable macOS runner I/O without weakening diagnostics

## Status

Accepted.

## Context

macOS builder and Simulator jobs run in one-job Tart clones. Their root disks
perform heavy dependency, Xcode, Simulator, app-installation, and log writes,
while test shards also download an app artifact that was produced on the same
physical host. The default fully synchronized virtual disk and remote artifact
round trip trade throughput for durability that an ephemeral clone does not
need.

Disabling guest swap or unified logging would instead turn memory pressure into
test kills and remove failure evidence. Those changes are not acceptable for a
robust CI fleet.

## Decision

macOS burst configuration may select one of Tart's documented root-disk
synchronization modes and may expose one absolute host directory to the guest
as the fixed `ci-shared` VirtioFS tag. Only deterministically named macOS
builder and Maestro instances receive these options; Linux instances do not.

Production disposable clones use `sync=none`. A clone is never resumed or
reused after a Tart, guest, or job failure; normal ownership-confirmed teardown
deletes it. The immutable base remains stopped and is never run with
`sync=none` during image construction or maintenance.

The shared directory contains only per-repository, workflow-run, and attempt
artifact paths. Workflows publish a checksum beside each atomically renamed
artifact and retain the GitHub artifact as the durable cross-host fallback.
The share must never contain credentials, runner JIT configuration, or private
keys.

Audio, clipboard, and host UI windows remain disabled for all headless runner
VMs. Unified logging, diagnostic reports, guest swap, and physical host thermal
management remain enabled.

## Consequences

- Disposable macOS clones can acknowledge writes with less synchronization
  overhead, and same-host shards can avoid repeated remote downloads.
- A host crash can corrupt the active clone. This is acceptable only because
  the clone is disposable and all source, caches, and durable artifacts live
  outside it.
- Operators can A/B `sync=full`, `sync=fsync`, and `sync=none` without code
  changes. Relative shared paths and unknown synchronization modes fail config
  validation.
- Image updates remain idle-fleet operations with a retained rollback base;
  this decision does not loosen ownership, drain, or deletion confirmations.
