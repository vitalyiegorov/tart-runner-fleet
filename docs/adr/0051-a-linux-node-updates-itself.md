# ADR 0051: A Linux node updates itself

## Status

Accepted. Extends [ADR 0011](0011-atomic-production-updates.md)'s generation
transaction to the second node type [ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)
gave this fleet, and retires the manual `systemctl --user` bridge documented in
[OPERATIONS.md](../OPERATIONS.md).

## Context

ADR 0011's transaction is a `launchd` transaction. It lints a plist with
`plutil` and swaps generations with `launchctl bootout` / `bootstrap` /
`kickstart`, and it gated itself on the node naming a launchd domain: a
`systemd --user` manager is not addressable as one, so `fleet update
apply-latest` refused on the Linux node rather than half-applying a release.

The refusal was correct and the consequence was not. The Linux node has been
moved forward by hand three times since it arrived, each time by an operator
rendering a unit, installing it, restarting the service, re-pointing the
`current` symlink, and remembering to check `status --require-ready` afterwards.
Every step of that is a step the macOS node does not perform, and the one an
operator is most likely to skip — verifying that the exact new version became
ready — is the one the whole transaction exists for. A node that is updated by
hand is also a node that is updated late, which is how a fleet ends up several
releases behind while its incidents are already fixed upstream.

The units were never the obstacle. Issue #138 already renders all of the node's
`systemd --user` units from the release itself (`render-systemd.sh`), for
exactly this: the shape the port would drive was fixed in advance so that no
node would need a hand-written file at cutover.

## Decision

`internal/autoupdate` gains `SystemdHost`, a phase-for-phase twin of the launchd
transaction, and `NewHost` selects between them on the domain the node names —
`user` for `systemd --user`, a launchd target otherwise. The domain decides
rather than `runtime.GOOS`, because the domain is what the supervisor commands
are addressed to, and a platform switch is a thing the whole test suite would
then have to fake.

Twin means twin. The same quiescence gate, the same checksum-verified generation
(with the authority *unit* as the boot definition a Linux generation must carry,
per `Target.ServiceDefinition`, and the controller checked under the name the
shared manifest gives it, per `Target.ControllerAsset`: a release lists one
`SHA256SUMS` for both node types, every archive unpacks its controller as
`fleet`, and only the Apple binary is the bare `fleet` among the loose assets
the manifest enumerates — a Linux node's is `fleet-linux-amd64`, so a verifier
that looked its `fleet` up under `fleet` refused every real release), the same
durable journal naming the prepared unit and the backups, the same readiness
proof against the candidate's own executable, and the same all-or-nothing
rollback. Both hosts call one
implementation of each of those, so the two transactions cannot drift into
disagreeing about what "busy" or "ready" means.

Two things are genuinely different, and both are simplifications.

**The units are rendered from the release, not written by the updater.** macOS
generates its updater plist in Go. Here the release's own templates are
rendered — the placeholder substitution `render-systemd.sh` performs, plus the
one value that belongs to the generation rather than to the release, its
configuration path. A rendered unit that still contains a placeholder, or a path
carrying a quote or a newline that would escape the templates' quoting, is
refused rather than installed.

**The timer is re-armed; the updater service is never touched.** On macOS the
periodic updater is a single job, so a commit performed *by* that job would have
to reload the process executing it — which kills the caller mid-commit, and is
why a separate, retrying handoff job exists there. systemd splits the schedule
from the work: `tart-runner-fleet-updater.timer` is what carries the five-minute
poll and `tart-runner-fleet-updater.service` is the one-shot it starts.
Restarting the timer while our own `updater.service` runs is safe, because the
timer is not the running process. So the commit issues `systemctl --user enable
--now tart-runner-fleet-updater.timer` and never restarts, stops, or starts the
updater service. There is no handoff dance to perform.

## Consequences

A Linux node is enrolled once, with the same command macOS uses —
`fleet update adopt --release-dir … --mode authority --confirm
adopt-current-generation` — which refuses unless the unit that is already
running names exactly that release and mode, and unless the daemon reports
itself ready as that version. From then on the node polls every five minutes and
brings itself forward, or restores the unit it was running and says why.

`ErrUnsupervised` no longer says "launchd-only"; it says the node names no
supervisor domain this transaction can drive, which is now a much rarer thing to
be told and means something is wrong with the node's configuration rather than
with this fleet's coverage.

Rollback restores the updater service and timer to whatever the transaction
found, including *absent* — a node adopting its first generation has no timer,
and a rollback there disarms the timer while its unit file is still on disk,
because systemd cannot stop by name a unit it can no longer load.

The `--launch-agents-dir` flag keeps its name on both platforms. It has always
meant "the per-user service definition directory", the rendered updater unit
passes it, and renaming a flag that three units already carry would break the
node this record exists for at exactly the moment it updates itself.

## Not addressed here

**`fleet update finish-updater-handoff` is a no-op verification on systemd.**
The release ships a handoff unit, and the subcommand stays, because the launchd
node still needs it and the Linux node may still be asked. What remains of it
there is the assertion the handoff was always for: after a durable commit, reload
and re-arm the timer and require `ActiveState=active`. Nothing is replaced,
because nothing had to be.

**Canary mode remains refused on both hosts.** The canary unit template takes
scope and profile selectors that no generation carries, and a canary is an
operator-driven experiment rather than something an automatic updater should
promote.

**The controller unit is not enabled by an activation.** It is already wanted by
`default.target` from the generation it replaces. Enabling it during a
transaction that is then rolled back would leave a symlink naming a unit the
rollback removed; enrolment is `Adopt`'s job, once.
