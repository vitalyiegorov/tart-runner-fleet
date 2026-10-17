# ADR 0069: Local macOS shares are checked before startup

## Status

Accepted 2026-10-17. Resolves the validation boundary in issue #161; does not
change issue #97's separate shared-directory isolation boundary. Qualifies the
file-only validation descriptions in ADRs 0018 and 0032 for local executable
configuration, preserving ADR 0034's host-independent fleet comparison.

## Context

A Mac inherited another user's `macosBurst.sharedDirectoryPath`. Its configuration
passed validation because the path was absolute, but every macOS VM failed to
boot with an invalid directory-sharing device. The useful path error was lost
behind the eventual readiness timeout.

`Config.Validate` cannot inspect a host: configuration shape and cross-node
comparisons must remain pure, and a Linux node cannot establish whether a path
on a remote Mac exists. The existing backend preflight is not the same rule:
it proves an execution runtime usable outside observe mode, while this check
validates a local configured mount before daemon startup in every mode.

## Decision

One named `app.ValidateLocalHost` validation receives the platform and a stat
function as ports. On Darwin, when macOS is enabled and a share is configured,
it requires a successful lookup of a directory. Missing paths, non-directories,
permission-denied lookups, and other unavailable lookups refuse validation.
Diagnostics name the configuration field, a bounded quoted path, and an
operator action; raw lookup errors are omitted.

The daemon invokes it after decoding and before opening durable state or any
runtime side effect, including in observe mode. Single-file `fleet config
validate` (including `validate-config`) invokes the same validation as a local
executable-configuration check. Several paths mean cross-node shape validation:
the command never stats their host paths. Linux, other platforms, disabled macOS,
and an unconfigured share require no macOS filesystem lookup. Run single-file
validation on each owning Mac as the daemon user before deployment.

No owner-UID equality rule is added: a directory shared through filesystem
permissions may legitimately have a different owner. A successful stat proves
existence and directory type, not guest access, full directory readability, or
continued existence after validation. Lookup permission failures are closed;
runtime filesystem changes and global share isolation remain separate concerns.

## Evidence and consequences

The daemon incident replay fails before the fix in observe, shadow, canary, and
authority: each opens durable state despite the absent path. Direct tests cover
the lookup outcomes and platform/enablement conditions; CLI tests cover the alias
and remote fleet exemption. Seeded simulation startup now supplies absent, file,
directory, permission-denied, and unavailable filesystem evidence before running
the macOS fleet world, closing its assumption that startup always succeeds.
This incident is a fleet defect, not a property/oracle or world-model defect.

There is no schema, migration, JSON API, scheduler, or authority-policy change.
A misconfigured Mac now refuses startup, including observation, with an actionable
error. No VM deletion, filesystem repair, service change, or operational promotion
is performed. Reverting restores the former late failure and needs no migration.
