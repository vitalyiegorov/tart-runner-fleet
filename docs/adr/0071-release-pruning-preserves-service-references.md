# ADR 0071: Release pruning preserves service references

Status: accepted

## Incident

Issue #352 records a Mini whose committed manifest, `current` link and on-disk
plist named v0.1.612 while launchd still cached v0.1.575. Version-only collection
deleted the cached executable outside rollback headroom. The next daemon exit
left the node unable to relaunch. A successful generation commit does not prove
that every already-loaded or future boot definition names that generation.

## Decision

Before release collection, read a fresh, bounded snapshot through the existing
supervisor command port and read the on-disk fleet boot definitions. Retain all
referenced immutable release directories in addition to the running/current,
newest candidate and ordinary rollback generations. The pure selection predicate
accepts these references; it does not perform process or filesystem observation.

Query every supported mode identity independently of boot-file presence: a
loaded definition can outlive its file. Include the updater, handoff and legacy
launchd identities. Missing launchd jobs require its explicit service-not-found
exit; systemd absence requires explicit `LoadState=not-found`. Other command
errors, cancellation, incomplete output, unreadable definitions or unsupported
binary definition encoding defer the entire collection. Unavailable is never an
empty list of references. The updater reports that deferral and continues its
existing release transaction outcome; housekeeping must not block an update.

Reference matching is deliberately conservative: references from helper argv,
retired executable names and other boot-file fields may retain an extra release.
This is preferable to losing a relaunch path. Backups whose filenames are not
boot-definition filenames do not retain generations indefinitely. No new daemon,
watchdog, state, timeout setting or automatic service reload is added.

## Evidence and remaining scope

The operator-command regression recreates the independent cached/committed
identities and fails red before the fix: the cached old release is deleted. A
pure `PrunableReleases` test pins the referenced generation; host tests cover
both supervisors, unreadable and canceled observations, explicit absence and
preservation of on-disk roots. Updater/supervisor state is outside the
scheduler world, so no simulation fault is added.

This does not repair an already-missing binary, establish runtime parity, alert
on absent nodes, or prove production rollout; those remain tracked by #352. No
authority/canary promotion, API or schema migration.
