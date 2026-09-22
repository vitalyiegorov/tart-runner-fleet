# ADR 0058: A node declares the execution technology it has

## Status

Accepted 2026-09-22. Completes [ADR 0055](0055-linux-work-runs-on-the-linux-node.md),
which decided that Linux work runs on the Linux node but left the Macs unable
to say so in their own configuration files. Applies
[ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md)'s "a node has exactly
one execution technology" to the platform axis. Corrects one planner rule
established alongside [ADR 0012](0012-shared-cross-platform-capacity.md) and
[ADR 0024](0024-mixed-macos-profile-cohorts.md), which was unreachable while
every node declared Linux profiles and becomes a macOS-only node's steady
state.

## Context

ADR 0055 moved every Linux job to node-b. Seven days later the evidence is
unambiguous: both Macs have booted **zero** Linux guests, no consumer workflow
names a Mac's Linux label, and what remains on each Mac is inventory rather
than capability — Linux profiles nothing routes to, parked Linux scale sets
nobody polls, and an 8 GiB `linux-runner-base-go` image that has cloned
nothing.

An operator who tried to delete that inventory could not. `Config.Validate`
demanded a Linux base VM of every node:

```
linux base VM and prefix are required
```

So `fleet config validate --mode authority` refused a macOS node with
`linuxProfiles: []` and no `baseVm`. The fleet could reach the end state ADR
0055 designed, but could not describe it. `docs/MULTI_NODE_PLAN.md` recorded
the constraint as a known mechanical obstacle to the retirement.

Three facts discovered while removing it shaped the decision.

### `baseVm` cannot answer "does this node boot Linux guests?"

The schema keeps a Linux base VM name in **every** node's file. An
observe-only Linux node carries one it will never clone, and so does a podman
container node (ADR 0034). A predicate on the field calls node-b a Tart Linux
host. The question a node can actually answer from its own file is whether it
declares any Linux **profile**: a profile is the only thing a Linux job can be
placed on — the scheduler admits against one, a scale set routes to one, a
label resolves to one.

### `maxLinuxCpu` is not a Linux setting

`maxLinuxCpu`, `maxLinuxMemoryMb`, and `maxLinuxWhenMacosIdle` are named for
Linux and are not Linux-only. Under ADR 0012 — the default, and what both Macs
run — they are the node's **shared cross-platform admission envelope**, and a
macOS guest is charged against all three (`scheduler.staticFree`). Under ADR
0018's elastic envelope they still seed the bound before the physical total
narrows it, and `physicalBound` takes a minimum, so a zero there is a zero
here too.

An operator retiring Linux by deleting every key with "Linux" in its name
would therefore delete the node's entire admission envelope. The resulting
daemon starts, passes `fleet doctor`, polls its scale sets, and admits
**nothing at all, forever** — an empty queue on a healthy host, which is what
a fault looks like (#230) and what this fleet's history says nobody spots.

### A macOS-only node wedged the planner

`tests/simulation` gained a macOS-only world for this change and found a
liveness wedge on its sixth seed. The counterexample, reduced:

```
live:   maestro (4 vCPU / 7 GiB), running
head:   builder (8 vCPU / 12 GiB), aged 30m — cannot fit the residual
behind: maestro (4 vCPU / 7 GiB), aged 20m — fits exactly, profile MaxActive 2
residual: 4 vCPU / 9 GiB
```

Every rule the fleet has says admit the second maestro. Nothing did, for 12
ticks and counting.

The cause is one arm of the macOS planner's decision. When a macOS head will
not spawn on a host that is busy rather than idle, the planner asks whether
there is Linux work to hand the host over to. With Linux demand present it
performs a bounded handoff drain; with `mixedPlatformAdmission` on it fills
the residual. But when `len(linux) == 0` it returned the attempted plan
unchanged — admitting nothing — because the branch was written for a fleet in
which "no Linux queued right now" was a passing condition.

On a node with no Linux **profiles**, `len(linux) == 0` is not a passing
condition. It is permanent. The arm that admits nothing becomes the node's
steady state, and the arrangement it wedges is precisely the one ADR 0055
names as the point of the exercise: *"two maestro VMs, or a builder and a
maestro."*

This is a **fleet defect**, not a property/oracle defect and not a
world-model defect. The waiting demand is feasible under the node's own
declared policy — envelope, `MaxActive`, repository cap, and cohort identity
all permit it — and the world that produced it is `mac-mini.json`'s live
configuration with its Linux profiles removed. It was invisible until now for
exactly the reason AGENTS.md warns about: the generator could not reach the
state, because no simulated world had ever declared both macOS profiles and
no Linux profile.

## Decision

1. **A node declares the execution technology it has.** `linuxProfiles` may be
   empty or absent and `baseVm` may be absent, provided `macosBurst` is
   enabled. A node with neither is refused: *"a node must have at least one
   execution technology: declare linuxProfiles or enable macosBurst."*

2. **`Config.ExecutesLinux()` is the one predicate**, stated once and asked
   everywhere, and it asks about profiles rather than about `baseVm`. Four
   call sites read it: validation, the guest-console posture, the runner-image
   compliance set, and the scheduler profile map.

3. **`vmPrefix` stays required** on every node. It is a well-formedness key of
   the schema, not a statement about Linux, and leaving it required keeps one
   rule where a relaxation would create two. (It is worth recording that
   neither `vmPrefix` nor `macosBurst.vmPrefix` names a guest at runtime today
   — instance names are minted as `trf-<profile>-…`. Retiring both keys is a
   separate schema cleanup and is deliberately NOT done here.)

4. **The admission envelope stays required**, on every node, whatever its
   platform. `maxLinuxCpu`, `maxLinuxMemoryMb`, and `maxLinuxWhenMacosIdle`
   keep their existing checks for the reason above: accepting a zero there
   ships a node that admits nothing and says it is healthy. They are not
   renamed in this change — a rename is a schema migration across two live
   nodes and belongs in its own decision — so the constraint is stated in the
   validator, in `USAGE.md`, and in the retirement procedure instead.

5. **A scale set a node cannot serve is refused by name.** On a node that
   declares no Linux execution, any scale set whose profile the node does not
   declare fails validation naming the set, its scope, and its profile. The
   rule is scoped to such a node deliberately: on a node that still executes
   Linux an unknown profile is already named by the authority checks, and
   repeating it here would report one mistake twice. Without it, an operator
   who deletes `linuxProfiles` while a Linux scale set is still listed ships a
   daemon that long-polls a set it can never place a job from — the stranding
   of #164, self-inflicted and invisible.

6. **A macOS head that cannot fit no longer blocks macOS work behind it when
   there is no Linux work to hand off to.** The three planner arms that mean
   "the head will not spawn and no drain can help it" — idle host, no Linux
   queued, mixed-platform admission — become one arm running one remainder
   pass. This **removes** a branch rather than adding one, and the remainder
   pass is the existing one, under the same envelope, `MaxActive`,
   repository-cap, and cohort rules as everything else.

7. **The deterministic simulation gains a macOS-only world.**
   `macOSOnlyNodeWorld` declares both macOS profiles and no Linux profile, on
   the mini's ten cores, with both cohort relaxations off as both Macs
   actually run them. It is swept as its own arm because the seed stream is a
   function of the world, and it is in the corpus determinism gate. A shared
   `servingOnly(platform)` helper now keeps the three coupled fields — the
   scheduler's profile map, the bindings derived from it, and the generator's
   draw list — in agreement, so a world whose generator can draw a profile the
   scheduler does not declare cannot be written by accident.

## Consequences

- A Mac can be configured as what it is. `fleet instances`, `fleet queues`,
  and the `fleet_instances{profile=…}` metrics lose their `linux-*` rows
  automatically, because every per-profile row is keyed by the scheduler's
  profile map.
- `fleet doctor` on a macOS-only node reports `guest console: no Linux guests
  booted on this node` and a `runner version` row naming the macOS image
  alone. Neither is an unavailable observation: both absences are **proved
  from the file**, not unread from a host, which is the distinction AGENTS.md
  rule 4 draws. The node still publishes an image row and still publishes a
  console posture; what disappears is a judgement about an image nothing
  routes to.
- The published policy digest (ADR 0053) changes when a node retires Linux,
  which is correct and is what makes the retirement visible to a peer
  comparison. `linuxCapacity` stays in the projection because the node still
  has an envelope.
- The planner change is a **relaxation of a wedge**, so it can only admit work
  that was already feasible and was already being passed over. It is covered
  by four deterministic scheduler tests and by 400 seeds across every
  simulation arm.

## Not addressed

- **Renaming the envelope keys.** `maxLinuxCpu` and friends remain misleadingly
  named. A rename is a schema migration on two live nodes and needs its own
  decision and its own rollback story.
- **Retiring `vmPrefix`.** Both prefix keys are vestigial at runtime. Removing
  them is a schema cleanup, not part of enabling a macOS-only node.
- **A `fleet scale-sets retire` command.** ADR 0055 says the Macs' Linux sets
  are retired "recreate-or-delete per #336's guarded path". That overstates
  what exists: `fleet scale-sets` has `provision`, `audit`, and `recreate`, and
  `recreate` is a delete-**then**-create that always leaves a new GitHub object
  behind and rewrites the config with its new id. There is no command that
  retires a set. The procedure in ADR 0055's amendment therefore deletes the
  GitHub objects by hand, and the minimal command that would replace that step
  is **proposed, not built**, in that amendment.
