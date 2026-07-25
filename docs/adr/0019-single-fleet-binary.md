# ADR 0019: One `fleet` control-plane binary

## Status

Accepted with a two-generation migration. Narrowly amends
[ADR 0011](0011-atomic-production-updates.md) only where it names `fleetd` and
`fleetctl` as distinct executables. Prerequisite for the multi-host fleet decision that follows it.

## Context

The repository builds three executables. `fleetd` is the daemon, `fleetctl` is
the operator interface, and `tart-runner-fleet-bootstrap` is the guest helper
installed inside every base VM image.

The daemon/CLI split is commonly justified by capability separation. That
justification does not hold here. The measured dependency graphs overlap almost
entirely:

| Package | `fleetd` | `fleetctl` |
| --- | --- | --- |
| `internal/credentials` | yes | yes |
| `internal/adapters/tart` | yes | yes |
| `internal/adapters/githubscaleset` | yes | yes |
| `internal/scheduler`, `app`, `reconcile`, `config` | yes | yes |
| `internal/adapters/sqlite` | yes | no |
| `internal/lifecycle`, `internal/telemetry` | yes | no |
| `internal/autoupdate`, `internal/provision` | no | yes |

`fleetctl` links keychain access, the Tart adapter, and the official scale-set
client. It is not a thin client: `scale-sets provision`, `update`, `adopt`, and
`finish-updater-handoff` perform guarded local mutation without passing through
the daemon. The split therefore buys exactly one structural guarantee — the CLI
does not link the SQLite store, so "never reads or mutates the database
directly" is enforced by the dependency graph.

Two costs are already visible, and both grow once the fleet spans more than
one machine:

- ADR 0011 has the candidate `fleetctl` validate the persisted configuration
  that the candidate `fleetd` will then be activated with. Those are two files
  in one release directory that must agree. Nothing proves they were built from
  the same source.
- Release verification is per artifact: dual reproducible builds, byte-identical
  binary and CycloneDX SBOM comparison, allowlisted archive contents, and
  SHA-256 manifests.

A multi-host fleet makes control-plane version skew a live operational hazard
rather than a theoretical one, because an operator on any host may hold a CLI
from a different release than the daemon it is inspecting.

## Decision

Build two executables.

### `fleet`

The daemon and the operator interface become one binary with subcommand
dispatch. `fleet run` is the daemon; `fleet status`, `fleet doctor`,
`fleet update`, `fleet scale-sets provision`, and the rest are the operator
surface. Command semantics, output contracts, deterministic ordering, and exit
codes are unchanged.

The single structural guarantee is preserved by lint rather than by linkage: a
`depguard` rule forbids the CLI packages from importing
`internal/adapters/sqlite`. Runtime mutation stays in the daemon and the private
`0600` admin socket remains the only path to daemon state.

### `tart-runner-fleet-bootstrap`

The guest helper stays a separate, minimal executable and **keeps its current
name and installation path**. Two reasons, in order:

1. **Minimize the untrusted zone.** The guest executes arbitrary CI workloads.
   The artifact resident in that image must remain three packages that can be
   audited in full, not a control-plane binary containing a gRPC server, a
   GitHub client, a scheduler, and keychain access. Auditability of the
   untrusted-zone artifact outweighs artifact-count minimalism.
2. Renaming would force a base-image rebuild on every host in every region for
   no functional gain. The verified release/helper version pairing required by
   [ADR 0010](0010-ephemeral-guest-shutdown.md) is unaffected.

### launchd

launchd identity is per job label, not per executable. The daemon job invokes
`fleet run`, the automatic-updater job invokes `fleet update`, and the transient
retrying handoff job invokes `fleet finish-updater-handoff`. All three remain
distinct jobs, so ADR 0011's rule that an updater must never `bootout` its own
launchd job is unaffected. Because each generation is staged into its own
immutable `releases/<tag>/` directory with plists naming absolute paths, no
update ever replaces a running executable in place.

### Migration

The installed updater of generation *N* rewrites the updater plist to the
`fleetctl` of generation *N+1* and validates configuration with it. A release
that omits `fleetctl` is therefore not installable by an already-deployed
updater.

Migration is a **one-time manual re-adoption**, not an automatic update. The
order matters: `fleet update adopt` records an *already-running* generation. It
reads the canonical plist and refuses unless that file already names the
candidate's `fleet` and mode, then requires the daemon to report the candidate
version, mode, and readiness. It writes only the updater plist — never the
canonical one. So the boot plist must be rendered, installed, and the daemon job
restarted **before** adopt, which is exactly the sequence
[`INSTALL.md`](../../INSTALL.md) §4 (render and validate launchd) then §5 (adopt
automatic production updates) already prescribes:

1. drain the fleet through the existing quiescence path;
2. unload the incumbent updater job, so the commit does not have to hand its own
   replacement to the retrying handoff job;
3. install and verify the first `fleet` release beside the incumbent;
4. render the new canonical plist from that release, install it, and restart the
   daemon job (`INSTALL.md` §4);
5. verify the daemon reports the exact new version, the unchanged mode, and
   readiness;
6. `fleet update adopt` the running generation, which enables the updater
   (`INSTALL.md` §5);
7. verify both loaded program names resolve inside the new release directory and
   that the updater's last exit status is zero;
8. retain the incumbent generation as the rollback target.

Rollback is the same sequence run against the incumbent release: restore its
canonical plist, restart the daemon job, then `fleetctl update adopt` from the
incumbent directory, which restores the installed generation, the `current` link,
and the updater plist as one unit.

Automatic updates resume from the following release onward, unchanged.

Compatibility shims were considered and rejected. The launchd updater handoff is
the most defect-prone subsystem in this repository — at least six corrective
changes (#45, #46, #47, #52, #54, #61) — and a shimmed generation would add a
new self-replacement case to exactly that code. A single supervised operator
action on a drained fleet carries far less risk than teaching the highest-risk
path a new trick, and this fleet has one operator.

### Rejected

- **Multi-call dispatch on `argv[0]`.** Symlink-based identity complicates
  macOS code signing and release-archive verification for no gain over explicit
  subcommands.
- **A CLI framework dependency.** The existing standard-library dispatch is
  dependency-free and adequate.
- **Merging the guest helper.** See above.

## Consequences

- The validator and the daemon are provably the same code, removing a skew class
  from the ADR 0011 update transaction.
- Control-plane version skew between an operator's CLI and a daemon becomes
  impossible within a host and, under ADR 0020, detectable across hosts.
- Release verification work halves: one control-plane artifact to build twice,
  compare byte-for-byte, generate an SBOM for, and manifest.
- This is a breaking operator change. LaunchAgent `ProgramArguments`, the
  release archive contents allowlist, `RELEASE_VERSION` verification,
  [`INSTALL.md`](../../INSTALL.md), [`docs/CLI.md`](../CLI.md),
  [`docs/OPERATIONS.md`](../OPERATIONS.md), and
  [`docs/AGENT_RUNBOOK.md`](../AGENT_RUNBOOK.md) all change.
- The `adapters/sqlite` isolation guarantee degrades from unlinkable to
  lint-enforced. The rule is a build gate, not a convention.
