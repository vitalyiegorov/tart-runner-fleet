# ADR 0079: A pinned tool the image lacks is a red flag

## Status

Accepted. Issue [#414](https://github.com/vitalyiegorov/tart-runner-fleet/issues/414),
the "fleet-visible tooling-drift check" its 2026-10-24 audit lists as
outstanding. It is the third declared, validated fact about a guest image, after
the capabilities of ADR 0034's amendment 2026-08-04b and the runner version of
[ADR 0041](0041-a-base-image-declares-its-runner-version.md). It follows ADR
0041's design closely, and where it differs this ADR says why.

## Context

Every job here starts in a fresh guest. When the image does not carry the exact
tool version a consumer's workflow pins, the job still succeeds, because the
setup action downloads the tool first. It downloads it again on every run, on
every shard, over the node's uplink. Issue #414 measured this on 2026-10-24:

- The macOS image carried Maestro 2.6.1. The reusable workflow pins 2.10.0, so
  every maestro shard downloaded the 314,828,521-byte archive.
- The Linux containers each fetched a 57 MB Node archive. `setup-node` looks for
  versions in `RUNNER_TOOL_CACHE`, not on `PATH`, so the Node already installed
  in the image did not help.
- Parallel jobs restored a 1.1 GB pnpm store from GitHub's Actions cache, at
  about 83 Mbps of a 150 Mbps home uplink.

Every fleet check read healthy while this happened. A capability says whether
the image has a kind of tool, such as `maestro-cli`. It does not say which
version, and the version is what decides whether the job downloads. Consumers
also change their pins in other repositories, so the gap can open again without
anyone touching this fleet. PR #417 bakes Node 22.22.2, 22.19.0 and 24.22.1 into
the Linux runner image. Without a check, nothing notices when a consumer moves
to a version that is not baked.

## Decision

**Each base image declares the exact tool versions it carries. Each scale set
declares the exact versions its consumers pin. `fleet doctor` degrades (exit 5)
on a named `image tools` row when a booted image lacks a version one of its
scale sets pins.**

1. **The declarations.** `baseImageTools` (top level for the Linux image, inside
   `macosBurst` for the macOS one) and the per-scale-set `requiresTools` are
   lists of `tool@version`, for example `maestro@2.10.0` or `node@22.22.2`.
   Absent means no declaration, and an absent key encodes no key at all, as
   before.
2. **The grammar is exact.** The tool name uses the capability grammar. The
   version is dot-separated decimals. There are no ranges (`22.x`), no `v`
   prefix and no suffixes. A setup action asked for `22.22.2` downloads 22.22.2
   even when 22.22.3 or 24.x is cached, and a newer Maestro does not satisfy an
   older pin. Equality is the only comparison that predicts a download. A
   consumer that pins a range is declared by the exact version the range
   resolves to in the image.
3. **One predicate.** `config.ImageTools` and `ImageTools.Reason` are the only
   place the rule lives. The daemon publishes the result once at startup.
   Telemetry passes it through as `status.data.imageTools` and `imageToolCheck`
   and as the `fleet_image_tools_missing{platform}` gauge. The doctor shows what
   it was given and does not compute the rule again.
4. **Absences.**
   - If no scale set pins a tool, the image passes, whatever it declares. This
     can be proved from the file alone.
   - If a scale set pins a version and the image declares nothing, that version
     is missing and the check fails.
   - A daemon that has not published sends **no** `imageToolCheck`. The doctor
     shows `not reported by this daemon` and does not treat it as a measured
     pass. The telemetry snapshot tracks this with `ImageToolsPublished` rather
     than inferring it from an empty slice (AGENTS.md rule 4). This is one
     step stricter than `runnerVersionCheck`. That check is always present once
     the field exists, so its "unpublished" and "published, nothing to judge"
     look the same.

## Where this differs from a capability

A tool version could have been encoded as a versioned capability
(`maestro-cli@2.10.0`). It was not, because the two facts have opposite failure
modes:

- **A missing capability fails the job.** The guest bootstrap compares the
  required capabilities against the image's sealed manifest and refuses to start
  the runner (`bootstrap:guest_capability_missing`). The cross-node parity
  contract makes two nodes that share a label declare equal sets.
- **A missing tool version only costs bandwidth.** If it went through the
  capability path, a consumer's pin bump would refuse every guest until the
  image was rebuilt. Parity would also force every node that shares a label to
  carry identical tool versions. That turns a bandwidth warning into an outage,
  which is the same mistake ADR 0041 declined to make.

The two declarations therefore sit beside each other with separate predicates.
The new field is a list of strings next to an existing list of strings. It adds
no state, no pass and no scheduling axis.

## What was deliberately NOT built

- **Validation does not refuse an uncovered pin.** The node keeps serving, for
  ADR 0041's reason: refusing at decode time would take a running node off the
  air on its next self-update restart over a cost, not a fault. Decoding refuses
  only an entry it cannot compare, or a requirement whose profile no image
  answers for.
- **No cross-node parity for tools.** Two nodes behind one label may carry
  different tool versions. Each node's doctor reports its own gaps.
- **No measured network signal, yet.** The brief asked for per-instance network
  receive bytes as a warning if it fit cleanly. It does not fit cleanly today.
  Neither the executor ports nor any adapter reads per-instance network
  counters. Podman exposes them through `podman stats`/`inspect`. Tart has no
  per-VM equivalent, so a macOS guest (the Maestro case) would stay unmeasured.
  Adding the signal means a new port method, two adapter implementations (one
  of which cannot answer), a periodic sampler, a threshold knob and a new
  status axis. By question 2 of AGENTS.md that needs its own issue and evidence,
  not a ride-along. The declared check catches the drift the fleet can know
  about. The measured signal would catch the drift it cannot, meaning a consumer
  pin change nobody mirrored into `requiresTools`. That gap is recorded here and
  not hidden.
- **No guest-manifest backstop.** ADR 0041 explains why reading versions back
  out of the sealed `image-capabilities.json` is a change tied to rebuilding the
  image. The declaration is the operator's word. An image rebuilt without
  updating `baseImageTools` stays invisible until someone measures it.
- **No policy-digest change.** Tool declarations decide no admission, so they
  are not part of the `fleet config policy` subset. Every node's digest stays
  where it is.

## How the truth can go stale

Both sides are declarations, and either can go stale. The image side goes stale
when an image is rebuilt and the config is not updated. The consumer side goes
stale when a workflow in another repository bumps a pin. The second case is the
one #414 worries about. This check makes it visible the moment someone writes
the new pin into `requiresTools`, and not before. The runbook therefore asks
for `requiresTools` to be updated in the same change that bumps a consumer pin
on a self-hosted label.

## Testability

The grammar, the coverage predicate (exact match, a newer version does not
count, booted images only, the union across scale sets), the round trip, the
clone, the telemetry setter's refusal of unreadable rows, the status projection,
the metric, the doctor row in pass, fail and not-reported forms, and the daemon
projection's acceptance by telemetry all have unit tests.

The deterministic simulation ([ADR 0031](0031-deterministic-simulation-testing.md))
is deliberately not extended, for ADR 0041's reason. This change adds no
admission rule, state or axis, and does not touch `internal/scheduler`. The
corpus digests must not move.

## Consequences

`fleet doctor` gains an `image tools` row and exits 5 when it fails. A node
whose files declare neither key reads `every pinned version is carried` with
`none declared`. The checked-in node files are unchanged. Declaring the measured
versions there, for example `macosBurst.baseImageTools: ["maestro@2.6.1", ...]`
against `requiresTools: ["maestro@2.10.0"]` on the maestro sets, will turn the
row red on those nodes. That is an operational choice to make with the image
refresh.
