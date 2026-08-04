# ADR 0018: Second-pilot elastic host envelope

## Status

Accepted, default off. Amends [ADR 0012](0012-shared-cross-platform-capacity.md)
where the admission envelope was a single static configured vector. Promotion on
any host is an explicit operational action, not a consequence of this change.

## Context

The controller shares its Mac with an interactive tenant. The host is not a
dedicated CI worker: it runs its owner's editor, browser, and agents, and the
fleet is meant to be a second pilot on it, using capacity the host is not using
and yielding when the host wants it back.

The capacity model could not express that. `Host.Available.CPU` was populated
from `capacity.CPU` -- the configured constant -- so the CPU dimension of the
"fresh host observation" half of ADR 0012's envelope was a tautology. Three
consequences followed:

1. **The fleet could not yield.** Host CPU consumption never reduced the
   envelope. CPU entered admission only as a binary cliff in the pressure
   guardrail (`load > max AND idle < min`), so the fleet ran at its full
   configured width until the host was in distress, then stopped entirely.
2. **The fleet could not expand.** The envelope could never exceed the
   configured constant, so an idle 10-core machine offered exactly the same
   capacity as a saturated one.
3. **A single macOS VM consumed the Linux budget.** Because `linuxFree`
   subtracted every live instance from `LinuxCapacity`, the field named
   `maxLinuxCpu` was really a shared cross-platform total. On the production
   host a 7-vCPU macOS builder left 1 vCPU of an 8-vCPU envelope, so no queued
   profile could ever fit beside it.

Measured on the production Mac mini (Mac16,10: 10 cores, 24 GiB) during the
2026-07-25 incident: 13 jobs queued with the oldest at 37 minutes, one live VM,
`queue_slo_breached`, and the daemon's own probe reporting 62-71% CPU idle,
11-12 GiB available, and `fleet_host_admission_allowed 1`. Roughly seven cores
were idle while work that fit them waited.

Memory was already truthful, via the kernel memory-pressure signal adopted for
the host probe. That signal is a *residual* measurement -- already net of
everything running, VMs included -- and it worked. The gap was that no
equivalent existed for CPU, and that the configured envelope clamped the
measured figure anyway.

## Decision

Add an opt-in capacity model, `elasticHostEnvelope`, that sizes the fleet against
the machine it observes instead of a configured constant. Three bounds apply
together, and admission takes the minimum:

1. **A per-platform configured cap.** `maxLinuxCpu` and `maxLinuxMemoryMb`
   become what their names say: a Linux-only cap that only Linux instances
   consume. A macOS VM no longer spends the Linux budget. macOS concurrency
   remains governed by its own `maxActive` plus the bounds below.
2. **The observed physical total,** charged for every live instance regardless
   of platform. The probe reports `hw.ncpu` and `hw.memsize`; memory is reduced
   by the configured host reserve. This is what prevents aggregate reservations
   from exceeding the real machine, including during a boot burst before new
   VMs' load registers anywhere else.
3. **The measured residual,** `Host.Available`. CPU is `floor(cores x idle%)`,
   truncated so a partially busy core is never advertised as free and a
   saturated host advertises none. Memory stays the pressure-derived figure. The
   residual is already net of running work, so live instances are never charged
   against it a second time.

A dimension the probe could not read is reported as zero and imposes no bound;
consumers fall back to the configured envelope. An unobserved physical total
must never masquerade as a measurement of a zero-resource machine, because that
would silently close admission fleet-wide.

Every existing hard constraint is untouched and still applies: the pressure
guardrails and their fail-closed disk, memory, swap, and load checks; exact
resource vectors; the four-slot Linux ceiling; repository caps; profile
`maxActive`; aged-before-young ordering and the aging starvation guard; the
round-robin cursor; fresh-observation and ownership gates. This decision only
changes how wide the envelope is, never what may pass through it.

Default false preserves ADR 0012 behavior byte-for-byte, including the
observation shape: in the static model no physical capacity is advertised and
CPU availability continues to echo configuration.

## Consequences

The fleet becomes elastic in both directions on a shared host. As the host's own
tenant takes cores, advertised availability falls and admission narrows toward
zero without ever reaching the distress cliff; as the host goes quiet, the fleet
expands up to the physical machine rather than a hand-tuned constant. On the
incident state it admits work into the idle capacity instead of stranding it,
while keeping aggregate vCPU inside the real core count.

Instantaneous idle is a reactive signal, and this is deliberately a feedback
loop: admitted VMs consume cores, which lowers idle, which narrows the next
tick. The physical-total bound is what makes that loop safe rather than
oscillatory, since it charges reservations immediately instead of waiting for
load to appear. The probe's existing last-good-reading fallback smooths a single
flaky sample.

The model still accounts by *reservation*, not by measured per-VM consumption. A
VM that reserves 12 GiB but touches 4 GiB is charged the full vector against the
physical bound. Reclaiming that difference needs per-VM usage attribution, which
is a separate decision with its own poisoning and cold-start concerns.

`maxLinuxCpu` changes meaning when the flag is on. An operator enabling it on a
host whose configured envelope was hand-tuned as a shared cross-platform total
is widening two limits at once, and should re-derive both from the physical
machine.

## Swap pressure is measured as a rate, not a level

The same "measure the host, do not assume it" principle applies to the swap
guardrail, which gated admission on `SwapUsedMB` alone. macOS does not eagerly
reclaim swap, so that level behaves closer to a high-water mark than a current
pressure reading: once a burst has paged out, it stays high long after the
pressure ended, and a level-only gate latches the entire fleet off a healthy
host for as long as the residue persists.

Observed on the production host on 2026-07-25: swap used 2134 MiB against a
2048 MiB ceiling, so `fleet_host_admission_allowed` was 0 and nineteen jobs
queued behind it -- while the host was 80% idle with 21 GiB available, memory
pressure reported 86% free, and a 60-second sample measured *zero* swapouts. The
machine was not paging at all.

Exceeding the ceiling is now a necessary but insufficient condition: admission is
refused only when the host is also paging out, derived as the swapout delta
between consecutive observations. The level remains required, so paging while
comfortably under the ceiling is treated as the normal virtual-memory behavior it
is.

This does not weaken the fail-closed contract. A rate needs two samples, so when
it cannot be established honestly -- no prior observation, a non-advancing clock,
or a counter that went backwards because the host rebooted -- it is reported as
unmeasured and the level blocks on its own. An unmeasured rate is never read as a
quiet host.

## The CPU residual is advisory and yields to the aging starvation guard

Production falsified one consequence of the original decision within a day of
enablement. The measured CPU residual is instantaneous, and taking the minimum
with it makes it a hard gate: a demand needing R cores is admitted only at a
tick where idle >= R/cores. A 6-vCPU builder therefore needs a 60%-idle moment,
which a host with an active interactive tenant may never produce. Observed on
2026-07-30: builder jobs starved for 23 hours across two scopes while smaller
profiles kept flowing around them.

That contradicted the fleet's own doctrine twice. The host probe classifies CPU
idle, load, and swap as advisory throttles that must never fail-close admission
indefinitely -- and this decision had silently promoted one of them into a hard
bound. And ADR 0012 promises that "aging promotes old work to global FIFO so
large jobs cannot starve" -- a guarantee that is void when the envelope itself
never opens, because FIFO position is worthless without capacity to admit into.

The repair restores both: a demand past the fairness age escapes the advisory
CPU-idle clamp. Nothing else moves. The Linux-only cap, the physical total net
of live reservations, the measured memory residual, the slot ceiling, repository
caps, and profile `maxActive` all bind aged work exactly as before -- vCPUs
time-share, so admitting an aged job against the physical bound degrades
gracefully, while memory does not time-share and its residual stays hard.

Young work keeps the throttle. The division of politeness is deliberate: fresh
work defers to the host's tenant, old work receives a guaranteed floor. Backfill
behind a reservation also keeps the throttled envelope even for aged candidates;
they are bounded by their head's wait rather than by a quiet moment, which is a
wait with an owner and an end.

## A shared node needs a declared ceiling, not only a measured one

This decision made the fleet polite. It did not make it bounded, and on a node
the fleet does not own outright those are different requirements.

Every bound above is *measured*: the physical machine, the idle residual, the
pressure guardrails. Each is a fact about the host right now, and each narrows
admission as the host's own tenant gets busy. None of them expresses a promise.
On a machine somebody else also works on, the fleet is entitled to a share, and
the share is a number the operator chose — not a number the kernel reports. A
tenant who is quiet at this tick is not a tenant who has gone away, so a purely
measured model lets the fleet take the entire machine every time the tenant
steps out, and hands it back only once the tenant is already contending for it.
That is the correct behavior for the Mac mini, which the fleet effectively owns.
It is the wrong behavior for a remote Mac Studio the fleet is a guest on.

`hostBudget: {cpu, memoryMb}` is that declared ceiling. It caps the TOTAL
admission envelope of one node — every platform charged against it together —
below physical capacity, and composes with every bound above by minimum. It can
only narrow: a budget larger than the machine is a no-op, because the physical
bound keeps binding first.

Three properties make it a ceiling rather than a fourth throttle:

1. **It applies to both capacity models.** The seam is `freeCapacity`, the one
   function that produces an admission envelope, so the bound lands after the
   static model and the elastic model alike. A ceiling that only bound under
   `elasticHostEnvelope` would silently not bind on the nodes that have not
   enabled it.
2. **It binds aged work.** The section above lifts the advisory CPU-idle clamp
   for a demand past the fairness age, because a host with an active tenant may
   never produce a quiet moment and FIFO position is worthless without capacity.
   That reasoning is about an *instantaneous measurement* being unfit as a hard
   gate. It does not extend to a declared share: aged work escapes a throttle,
   never a ceiling.
3. **It is charged for every live instance regardless of platform**, exactly as
   the physical total is. Under the elastic model `maxLinuxCpu` is a Linux-only
   cap, so nothing else would stop a macOS VM and a Linux VM each spending the
   full budget.

The arithmetic is deliberately the same `physicalBound(configured, total, live)`
used for the observed machine, and for the same reason: a budget is a physical
total the operator asserts rather than one the probe measured, so an *undeclared*
dimension imposes no bound exactly as an *unobserved* one does not.

Validation splits by what is knowable where. That a profile can never fit the
budget is knowable from the file, and is refused there — but only for profiles
the node exposes to GitHub through a scale set, since a job routed to a shape
this node can never admit queues forever rather than waiting its turn, while a
profile no scale set names receives no jobs at all. That the budget fits the
machine is not knowable from a file, because `fleet config validate` decodes a
configuration and never probes a host; that check therefore lands at the probe,
which reports the host observation unavailable with a reason naming both
figures. It fails closed on purpose: on a node whose whole purpose is a promise
to a co-tenant, an operator who believes the node offers capacity it does not
have is a belief worth stopping for. A physical dimension the probe cannot read
still imposes no bound, for the reason this decision already gives.

Omitting the setting is this decision unchanged, byte-for-byte, and removing it
is the entire rollback. Deterministic simulation carries the invariant: property
(g)'s resource ceiling on a budgeted node is the budget rather than the machine,
and the independent feasibility oracle behind properties (a) and (b) reads the
same ceiling so a demand the budget correctly refuses is not counted as a wedge.

## Rollout

The flag ships off. Enabling it on a host is an operational action requiring the
usual evidence: observe the advertised `Capacity`/`Available` figures and the
admission decisions before granting authority, and keep the pinned incumbent as
rollback. This ADR does not authorize enabling it anywhere.

## Evidence

- `internal/adapters/macos`: physical totals are read, fail safe to
  not-observed, and never degrade the host observation.
- `internal/app`: the observation reports the physical machine and derives
  available CPU from measured idle; legacy mode is unchanged; unreadable facts
  fall back; nonsense idle is clamped; pressure still fails closed.
- `internal/scheduler`: expansion into an idle host, yielding on a busy host,
  monotonicity across idle levels, the physical CPU and memory bounds, the
  per-platform Linux cap, unobserved capacity imposing no bound, and
  fail-closed behavior on an over-subscribed cap.
- `internal/config`: flag decode, encode, and round-trip with default-off
  omitted so older strict releases still accept legacy configs.
- `tests/integration`: the live incident facts driven end to end through decode,
  guardrails, host observation, and the planner, asserting the static model
  strands the host, the elastic model uses it, and aggregate vCPU stays inside
  the physical cores.
