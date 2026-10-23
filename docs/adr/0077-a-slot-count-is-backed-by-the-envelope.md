# ADR 0077: A slot count is backed by the envelope, not fixed at four

## Status

Accepted 2026-10-22 (issue #393). Amends
[ADR 0018](0018-second-pilot-elastic-host-envelope.md) and
[ADR 0012](0012-shared-cross-platform-capacity.md), which list "the four-slot
Linux ceiling" among the hard constraints, and
[ADR 0073](0073-scale-sets-advertise-the-residual-envelope.md), whose headroom
read the Linux envelope through that ceiling. Every other hard constraint those
records list is unchanged.

## Context

On 2026-10-21, knee-doctor queued 62 jobs at once and every Linux node sat at
exactly four guests. node-b has 24 threads and 60 GiB with a 16 CPU / 32 GiB
budget; four 2x4 guests used 8 CPU / 16 GiB of it while seven jobs queued on its
own scale sets. The slot count, not CPU or memory, bounded the node, and nothing
an operator could write would change it:

- `internal/config` refused `maxLinuxWhenMacosIdle` outside 1..4;
- `planLinux` and `agedLinuxEnvelope` clamped free slots to four, the latter
  for the stated reason that "the exact Linux allocator deliberately has a tiny,
  auditable search envelope".

The second reason is real, and it is the only one. `exactSelect` is an
exhaustive search: over N queued candidates it visits O(N^k) subsets of size up
to k, and k was the free slot count. With 60 candidates and k = 16 it does not
terminate (measured: a 16-slot `exactSelect` over 60 candidates had not returned
after 15 s; at k = 4 it returns in milliseconds).

But the clamp bounded the *free* slots of one tick, not the node's total.
A node with more slots would already have filled them four per tick. The
search needed a bounded depth; the node never needed a bounded size.

## Decision

1. **The search depth is a per-pass pace.** `admissionsPerTick = 4` bounds how
   many Linux guests one `planLinux` pass admits. `exactSelect` applies it
   itself, so no caller can hand it an unbounded depth; `planLinux` applies it to
   its young and aged envelopes, and its aged loop stops once it has admitted
   four, so it never reserves the next head against slots only the pace
   withheld. A node with more free slots fills them over the next ticks. One
   tick can run a second Linux pass, the mixed-platform remainder after a macOS
   handoff's single-job backfill, so a tick's ceiling is the pace plus that one
   job. Every admission in that pass is still charged against the slots, CPU and
   memory the first pass took, so the pace can never exceed capacity.
2. **Every published or judged envelope keeps its true slot count.**
   `agedLinuxEnvelope` is deleted; its callers read `linuxFreeAged`. Headroom
   (ADR 0073) therefore advertises what the node can start rather than what one
   tick can admit, and `fleet status` shows the real free slots. Judging one
   vector against a slot count that is at least one gives the same verdict
   whether or not that count is clamped, so a reservation judged against the
   unclamped envelope and admitted against the clamped one cannot disagree.
3. **A slot count above four must be one the envelope can fill.**
   `maxLinuxWhenMacosIdle` may be at most
   `max(4, fillable)` and never more than 16, where `fillable` is the most
   guests of one Linux profile that fit `maxLinuxCpu`/`maxLinuxMemoryMb`
   narrowed by `hostBudget`. A slot no guest can occupy is capacity the node
   would advertise and could not serve (ADR 0015).
   - Four stays declarable on every node, whatever its vectors, because every
     production file was written under the old rule. The Mac Studio declares
     four slots on an 8-CPU envelope of 4-CPU guests. A release that refused the
     configuration its node runs could never be adopted: the updater validates
     the existing config with the candidate binary.
   - macOS profiles do not back extra slots. Apple's Virtualization framework
     runs at most two macOS guests and `maxActive` bounds each profile, so a
     macOS guest never needs a fifth slot.
   - Disk is not a term. A guest's disk is a sparse floor, not a reservation
     (ADR 0006), so adding it to an additive envelope would refuse capacity
     that exists. The host's free-disk floor is re-checked fresh at every
     admission and still fails closed.
   - Sixteen is a safety ceiling on what one controller drives at once
     (lifecycle workers, guest boots, runner registrations), not a bound the
     allocator needs.

Configurations at or below four slots are unaffected: their free slots never
exceeded four, so neither clamp ever bound and every plan is byte-identical.

## Consequences

- An operator raises a node's slot count by raising its envelope. A node's
  scale sets advertise `min(profile capacity, scope's repository caps)` under
  canonical inventory, so a scope only gains concurrency on the node once its
  targets' `maxActive` is raised too; `fleet config validate` names the exact
  capacity every set must then declare.
- A burst of N admissions on an idle wide node takes ceil(N / 4) ticks rather
  than one. At a 5-20 s poll interval that is under a minute for sixteen slots,
  and it spreads the guest boots.
- The allocator's cost is unchanged: its depth is the same constant as before.

## Evidence

- `internal/config/slot_ceiling_test.go`: a ten-slot node-b shape validates;
  slots the CPU, memory, or budget cannot fill are refused with both figures;
  seventeen is refused on any envelope; the Studio's four-slot shape still
  validates and a fifth unbacked slot does not; macOS guests do not back slots.
  Red on the parent: `linux max instances must be between 1 and 4`.
- `internal/scheduler/scheduler_wide_node_test.go`: headroom and the published
  envelope report all ten slots (red on the parent: 4); young and aged bursts fill
  ten slots as 4, 4, 2 with the reservation carried between ticks and no
  reservation minted while free slots remain (red before the aged loop stopped
  at the pace: 4, 1, 4, 1); `exactSelect` admits at most four with sixteen free (red
  on the parent: did not terminate within the 15 s test deadline).
- `tests/simulation`: a new `node-b-wide` world (24 threads, 60 GiB, a
  20 CPU / 44 GiB budget, ten slots) runs as its own sweep arm,
  `TestSimFuzzWideLinuxNode`, under every property, including (g)'s slot
  conservation against the configured ten. 300 seeds at 320 ticks are clean.
  `TestWideLinuxNodeHoldsMoreThanFourGuests` fails if the generator stops
  reaching more than four live guests in that world, so the arm cannot quietly
  turn into a re-run of the four-slot worlds.
