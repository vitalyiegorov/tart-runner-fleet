# ADR 0041: A base image declares the runner version it carries

## Status

Accepted. Closes the fleet-side half of
[issue #206](https://github.com/vitalyiegorov/tart-runner-fleet/issues/206). The
image refresh it makes visible is
[issue #243](https://github.com/vitalyiegorov/tart-runner-fleet/issues/243).

It is the second declared, validated fact about a guest image, after the
capabilities of
[ADR 0034's amendment 2026-08-04b](0034-a-node-serves-the-scale-sets-it-owns.md),
and it is declared for the same reason: the daemon never opens the image.

## Context

GitHub is enforcing a minimum `actions/runner` version on self-hosted runners.
Brownouts on github.com begin **24 Aug 2026**, 11:00–15:00 ET, ramping through
mid-September; enforcement is permanent from **25 Sep 2026**. Two rules apply:

- the runner must be on **2.329.0 or later** to register at all, and
- *"the runner must stay up to date by installing each new runner release within
  30 days of its publication."*

Brownouts block registration first, then job execution.

### Why this fleet is exposed more than most

Every job here runs on an **ephemeral runner registered from a JIT configuration
at admission time**. There is no long-lived registration to grandfather in. And
every guest registers with `"DisableUpdate":"True"` in `~/actions-runner/.runner`
— read from a live node A clone on 2026-08-17 — so the runner **cannot** climb
out of a stale version by itself. The version sealed into the image is the
version that executes jobs, for every clone it ever boots, until someone rebuilds
the image.

A blocked registration is not a degraded mode. It is every job on every scope
failing, during a four-hour window, with jobs queued and nodes idle and no red
anywhere — which is indistinguishable from the silent starvation class this repo
has spent weeks eliminating (ADR 0036, ADR 0039, ADR 0040).

### What was measured first

Nothing was designed before the fleet was measured. On 2026-08-17:

| Node | Image | `actions/runner` | Method |
|---|---|---|---|
| A (mac-mini) | `linux-runner-base-go` | 2.335.1 | `tart exec` into a live clone; `~/.ci-base-manifest` and `Runner.Listener --version` agree |
| A (mac-mini) | `macos-tartelet-base-go` | 2.335.1 | base disk attached read-only, `bin/Runner.Listener.deps.json` |
| C (mac-studio) | `linux-runner-base-go` | 2.336.0 | throwaway clone booted, read, deleted |
| C (mac-studio) | `macos-tartelet-base` | 2.336.0 | `tart exec` into a live clone |

`v2.336.0` was published 2026-07-20; `v2.335.1` on 2026-06-09. **The two nodes
are a whole release apart**, and node A's 30-day grace on 2.336.0 expires
2026-08-19 — five days *before* the first brownout. The fleet had no surface that
could say any of this.

## Decision

**A base image declares the `actions/runner` version it carries, the fleet states
one floor, and `fleet doctor` fails a named check when a declaration does not
clear it.**

Four parts, and no more than four.

### 1. The declaration

`baseImageRunnerVersion` sits beside `baseImageCapabilities`, top-level for the
Linux image and inside `macosBurst` for the macOS one, because a node has two
images built by two different procedures and they have **already** drifted a
release apart. It is declared rather than probed for the reason ADR 0034's
amendment gives for capabilities: the daemon never opens the image, and reading
the version out of a sealed image means booting it.

### 2. The floor

`DefaultRunnerVersionFloor` is the constant `2.329.0` — GitHub's number, which
nothing this daemon can measure would derive. `runnerVersionFloor` overrides it
per node.

The override is the one new knob in this change and it is justified narrowly: the
30-day rolling rule means the number an operator must act on **moves at least
monthly, on GitHub's calendar**. Without an override, raising the floor would
require cutting and rolling out a fleet release, during a brownout week, to
change one integer. Both node files set it to `2.336.0` today.

### 3. The predicate, stated once

`config.RunnerImage.Reason()` is the only place the rule lives. The daemon
transports its answer into telemetry; `fleet doctor` renders what it was given;
`fleet_runner_image_below_floor` carries the same verdict. Nothing downstream
re-derives it.

That is deliberate. Four scheduler defects in this repo's history came from one
seam re-implementing a rule at each call site, and version comparison is exactly
the kind of rule two implementations get differently: an operator comparing
`2.9.0` against `2.10.0` by eye gets it wrong, and so does a string compare.

### 4. Two absences, judged oppositely

- **An image with no declared version FAILS.** This is deliberately harsher than
  the fleet's usual "absence is a pass" convention, and it is harsher because
  unknown-reading-as-healthy is the entire content of the incident: both nodes sat
  in that state for two months and every surface reported a healthy fleet.
- **A daemon that publishes no `runnerVersionCheck` at all PASSES**
  (`EffectiveRunnerVersionCheck`). That is a fact about the daemon's version, not
  about an image, and the CLI must not invent a compliance answer an older daemon
  never gave. It is the same handoff rule every check in `fleet.v1` follows.

## What was deliberately NOT built

Recorded here because a decision not to build is still a decision.

**Validation does not refuse a below-floor configuration**, though issue #206
asked for it. A declaration below the floor decodes and the node keeps running.

The reason is the failure mode it would create. The floor rises on GitHub's
calendar and this daemon **restarts itself on every auto-update**. A refusing
decode would therefore take a node that is still successfully serving jobs
completely off the air, at an arbitrary moment, over a condition GitHub had not
yet begun enforcing — converting a warning into a fleet-wide outage, which is a
strictly worse version of the outage this whole change exists to prevent. It
would also make the doctor check unreachable in production: a config that cannot
load produces a generic "config invalid", not a named finding, and an unfirable
check is not a check ([AGENTS.md](../../AGENTS.md), question 3).

`fleet doctor` is the loud channel. It exits 5 and names the image, what it
carries, and the bar.

**The cross-node parity rule does not compare runner versions.** Two nodes
advertising `linux-xl` must declare equal capabilities, because a capability is a
promise about what a label *means*. A runner version is not: it is a compliance
fact judged per node against an absolute bar, and two nodes both above the floor
are both fine at different releases. Adding it to parity would also have turned
today's real fleet state into a permanently red contract test, blocking every
pull request over a condition no pull request can fix.

**The contract test asserts the declaration exists, not that it clears the
floor.** Same reason, one level down: a declaration compliant this week is stale
next week without anyone touching the repository. Compliance is a live condition
and belongs to the node that is running.

**No auto-upgrading runner distribution was built.** The simple floor was not
shown to be insufficient — it was never tried. The rebuild procedure stays the
documented, manual one in `docs/BASE_IMAGE.md` and `docs/LINUX_BASE_IMAGE.md`.
The recurring-staleness half — noticing that a new release started a new 30-day
clock — is genuinely unsolved by this ADR and is filed separately rather than
guessed at here.

## How the truth can go stale

The declaration is the operator's word, and it can be wrong in exactly one way:
an image is rebuilt and the config is not updated. That is the same staleness
ADR 0034's amendment names for capabilities, and it has the same eventual
backstop — the sealed guest manifest at
`/usr/local/share/tart-runner-fleet/image-capabilities.json`, which
`internal/guestbootstrap` already reads inside every clone before the runner
starts.

Extending that manifest to carry the runner version, so a guest can refuse a
declaration its own image disproves, is the honest completion of this design and
is **not** in this change. Two facts argue for deferring it rather than rushing
it seven days before a brownout: node A's Linux base carries no capability
manifest at all today (node C's does, sealed 2026-08-05), so the backstop would
be half-blind on the node that actually needs it; and the manifest schema is
version-gated, so extending it is an image-rebuild-coupled change, which is the
very thing that is slow right now.

The narrower staleness window is worth stating plainly: **an image rebuilt
without updating its declaration is invisible to this check until someone runs
the measurement by hand.**

## Testability

The version comparison, the floor predicate, the telemetry setter, the daemon
projection, the doctor check in both output modes, and the older-daemon handoff
are all unit-tested. The contract test decodes every real node file.

**The deterministic simulation harness ([ADR 0031](0031-deterministic-simulation-testing.md))
is deliberately not extended for this, and its corpus digests must not move.**
That is not a skipped step, it is a scoped one. The harness generates scheduler
worlds and judges scheduler invariants; this change adds no admission rule, no
state, and no axis. It is an observation and configuration concern — the fleet
learning a fact it did not know and reporting it — and the only thing a
simulation could assert about it is that a constant is carried unchanged from
config to renderer, which the unit tests assert directly and more precisely.

If a corpus digest moves on this branch, something is wrong: the change does not
touch `internal/scheduler`.

## Consequences

The version in service is readable from `fleet status --output json` and named by
`fleet doctor` on every run, passing or failing — an operator no longer SSHes
into a guest to answer "what runner is this". `fleet_runner_image_below_floor`
carries the verdict with a closed `platform` label; the version strings stay out
of labels, because a label that changes on every image rebuild churns the series
and makes "is this node behind?" depend on knowing what the answer used to be.

**Node A is declared below its own floor on purpose, so `fleet doctor` fails
there today.** That is the intended state, not an oversight: the fleet is
non-compliant, and it is the first time it has been able to say so.

The costs are real. The declaration is a hand-maintained fact that a rebuild can
silently invalidate. The floor is a hand-raised number, so a fleet whose operator
stops watching GitHub's release feed will pass its own check while failing
GitHub's — this change makes the condition *visible*, it does not make it
*self-correcting*. And an image is now judged by one absolute bar, so a node
running work GitHub's enforcement does not reach would still be told it is
behind; that is the conservative direction and it costs a false alarm, not a
lost job.
