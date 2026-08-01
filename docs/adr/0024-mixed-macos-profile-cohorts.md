# ADR 0024: macOS profiles may coexist when their exact vectors fit

## Status

Accepted, default off. Completes for macOS profiles what
[ADR 0012](0012-shared-cross-platform-capacity.md) decided for platforms.
[ADR 0014](0014-opt-in-macos-exclusive-admission.md)'s exclusive policy is
unchanged and still available.

## Context

ADR 0012 relaxed platform exclusivity on the grounds that "platform identity is
not itself a resource": Linux and macOS instances coexist whenever their
combined vectors fit the envelope. Profile exclusivity *within* macOS remained.
`planMacOS` spawns a profile only after every foreign-profile macOS instance is
drained, a busy foreign instance blocks the spawn outright, and
`macProfileCanGrow` vetoes growth the moment any other profile is live. One
macOS profile cohort at a time, switched by drain-and-wait.

The invariant simplified handoff reasoning when macOS admission was young. It
is now the dominant throughput limiter on the production host. Measured on
2026-07-30: one 6-vCPU builder running, two 4-vCPU maestros queued, four of ten
physical cores and ten GiB of memory idle, and the queue SLO breached — builder
6 + maestro 4 is exactly 10 vCPU and the memory fits, so the *only* refusal was
profile identity. In a CI topology where builders produce the app and maestros
run its test shards, every build serializes against every test wave, in both
directions, forever.

Profile identity, like platform identity, is not itself a resource.

## Decision

Add `macosBurst.mixedProfileCohorts`, default false. When enabled:

1. **Coexistence first.** `planMacOS` attempts to spawn the chosen profile
   beside the live foreign cohort before considering any drain. The spawn path
   is the ordinary one — exact vectors, the physical total net of every live
   reservation, the measured residual, profile `maxActive`, repository caps,
   and the elastic envelope's aged/young distinction all apply unchanged — so a
   coexisting spawn can never overcommit the machine.
2. **Drain-and-switch survives as the fallback.** When the chosen profile does
   not fit beside the foreign cohort, the existing path runs exactly as before:
   idle foreign instances are drained and the spawn depends on those drains; a
   busy foreign instance is never touched. Coexistence adds an option, never an
   interruption.
3. **Growth loses only the identity veto.** `macProfileCanGrow` keeps counting
   the target profile against `maxActive`; a live foreign profile no longer
   vetoes by existing. The envelope is the veto.

Default false preserves the single-cohort behavior byte-for-byte, including
the drain-and-switch choreography and every dependency edge.

## Consequences

On the production topology, builders and maestros pipeline instead of
serializing: a 6-vCPU build and a 4-vCPU test shard share the 10-core machine
that fits them both. The throughput gain is largest exactly where the SLO
breaches were observed — mixed build-and-test workloads.

Packing two profiles tightens the envelope for everything else; the second
pilot's yield-to-host behavior and the aged starvation guard are what keep that
safe on a shared machine. The exclusive policy of ADR 0014 remains available
for experiments that need an isolated cohort, and takes precedence when set.

The scheduler's drain-safety invariant is untouched: no path introduced here
drains anything, and the fallback drains only idle instances, as it always has.

## Evidence

- `internal/scheduler`: a maestro spawning beside a busy builder with no drains;
  flag-off behavior preserved byte-for-byte; the physical bound holding at
  exactly 10 vCPU; `maxActive` holding under coexistence; a busy foreign cohort
  never drained when the head does not fit; and the idle drain-and-switch
  fallback intact with its dependency edges.
- `internal/config`: flag decode, encode, and round-trip with default-off
  omitted so older strict releases still accept legacy configs.
