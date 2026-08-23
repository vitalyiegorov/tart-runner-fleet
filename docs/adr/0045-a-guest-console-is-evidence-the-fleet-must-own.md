# ADR 0045: A guest console is evidence the fleet must own

## Status

Accepted. Closes
[issue #259](https://github.com/vitalyiegorov/tart-runner-fleet/issues/259).
It extends the observability rule
[ADR 0040](0040-a-guest-that-stopped-answering-is-not-running.md) established —
*a condition the fleet cannot see is a condition the fleet will one day pay
for* — to the guest's own side of the hypervisor boundary.

## Context

Three incidents in eight days ended at *trigger unidentified*, all with the same
shape and all for the same reason.

- [#236](https://github.com/vitalyiegorov/tart-runner-fleet/issues/236): eight
  runners died mid-job, sixteen to eighteen minutes in, no artifact of any kind.
- [#258](https://github.com/vitalyiegorov/tart-runner-fleet/issues/258): a
  release job received a SIGTERM the fleet did not send; the leading hypothesis
  is our own `panic_on_oops=1` hardening under host I/O pressure.
- [#259](https://github.com/vitalyiegorov/tart-runner-fleet/issues/259): three
  consecutive nightly simulation sweeps killed by a dead Linux guest at the same
  minute of the same deterministic workload, on code that had passed the night
  before.

#259's investigation is the clearest of the three because every fleet-owned gate
left a complete record: the liveness probe refused sixteen consecutive times
(instant control-socket failures, the panicked-kernel signature — a slow but
living guest answers Unknown), the corroborator confirmed Stopped before any
drain was planned, the act-time guard aborted on a Running answer six seconds
later, and the reclaim line named the job it was ending. The host's unified log
showed no jetsam kill, no pressure storm, no VZ error. The workload itself was
measured at ~25 MB peak RSS inside an 8192 MB guest — capacity is not the
suspect.

What no record could answer is why the guest kernel died, because the one
artifact that survives inside a dead guest — its console — was being written
nowhere. `linuxSerialLogDirectory` has been implemented end to end since #236
(config → `tart run --serial-path`) and defaults to off; both production nodes
ran with it off throughout.

Silence by configuration is indistinguishable from silence by defect, and an
operator cannot tell which node forgot.

## Decision

**A node that boots Linux guests must say whether their consoles are captured,
and `fleet doctor` fails when the answer is no.**

The daemon publishes its posture once at startup, beside the runner-image set it
already publishes (`GuestConsole`: `bootsLinuxGuests`,
`serialLogConfigured`; additive `fleet.v1` fields). Telemetry judges it once:
boots Linux guests and captures nothing ⇒ FAIL, naming the setting and the three
incidents. Every other combination passes, and an unpublished field renders as
unspecified on the same handoff terms as every check before it.

The judgement is stated once in telemetry, computed from what the daemon already
knows about its own configuration. No new probing, no new durable state, no new
bound.

Enabling the sink itself remains an operational act per
[`docs/LINUX_BASE_IMAGE.md`](../LINUX_BASE_IMAGE.md): verify `--serial-path`
against the node's tart build first, keep `console=hvc0` in the image (shipped
by the #236 rollout), then reload the daemon. This ADR makes forgetting visible;
it does not turn anything on by itself.

## Alternatives considered

**Fail daemon startup on an unconfigured sink.** Rejected: it converts an
evidence gap into an outage, and a node operator may have reasons — a full disk,
a tart build without the flag — that deserve degradation with a name, not a
refusal to serve.

**Preserve console logs at drain time.** Unnecessary mechanism: the sink writes
outside the VM directory, so deletion already leaves the file. Adding capture
code for files nobody configured would be engineering for a state that cannot
occur.

**Move the nightly sweep to a larger profile.** Deferred until evidence names
the trigger. With a 25 MB workload profile, guessing at capacity costs a
base-image cycle per guess and would not have prevented #236 or #258.

## Consequences

Every Tart node now answers *can a dead guest leave evidence here* on every
`fleet doctor` run, in table and JSON. The fourth incident in this class starts
with a kernel log instead of a fifth reproduction attempt.

The check is additive: older daemons omit the field, newer CLIs render that as
an unspecified pass, and nothing in `fleet.v1` changed meaning.

## The three questions

**Can I remove overengineering?** Two booleans, one predicate, one doctor row.
No probe runs, nothing is stored, nothing is re-derived at call sites.

**Can I reduce complexity?** The rule is stated once — *boots Linux guests ⇒
must configure a console* — where every other standing invariant in this fleet
is stated, instead of living in an issue thread and an operator's memory.

**Can I test this?** Red-first: telemetry fails on the unconfigured posture and
passes on every other; doctor carries the finding through table and JSON output
and states its posture even when passing. The handoff case — an older daemon
that never published — is pinned as an unspecified pass, not a failure.
