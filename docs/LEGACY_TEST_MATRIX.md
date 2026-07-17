# Incumbent test migration matrix

The shell smoke suite's 42 assertions are retained as production-function tests,
grouped below. Names changed where the Go policy intentionally supersedes a
legacy heuristic.

| Incumbent behavior | Go proof |
|---|---|
| target limits, tier labels, legacy/transitional route isolation | `internal/config`, `internal/scheduler` configuration/route tests |
| aged reservation; young safe backfill; FIFO; repo saturation | scheduler exact-reservation, DRR, cap, and bounded-starvation tests |
| matching idle reuse; wrong-tier/route isolation | scheduler compatible-idle and wrong-route tests |
| PR priority and oldest PR across repositories | scheduler event ordering plus global age arbitration tests |
| four medium, two large, mixed weighted packing | exact allocator, resource-vector, and four-slot property tests |
| oversized head does not starve fitting work | exact-selection and infeasible-head incident replay |
| live VM count and over-cap turnover | current-live-resource and lifecycle overlap property tests |
| macOS start/drain/wait and direct handoff | symmetric same-plan Linux/macOS dependent handoff tests; issue 13 durable one-shot aged-small drain backfill replay |
| builder priority | intentionally replaced by global aged FIFO and DRR; mixed-platform no-starvation replay |
| Maestro max two and cross-repo second slot | Maestro cap/fair-spread tests |
| API uncertainty | typed Fresh/Stale/Unavailable fail-closed tests |
| scale-set capacity head-of-line | authority queue-lookahead validation and production incident replay |
| profile keep/switch/no-preemption | busy-profile, idle-switch, and no-extra-tick tests |
| builder/Maestro host resource envelopes | host-vector admission and configuration tests |
| UTF-8 runner locale | `operations.RunnerLocaleEnvironment` test |
| ephemeral listener cleanup/preserve cases | `operations.DecideListenerCleanup` table test |

The Go suite also adds cases absent from the shell suite: queued siblings after a
workflow becomes in-progress, full pagination, hard deadlines, redelivery,
commit-before-ack, durable cursor/inbox replay, dependency cycles, crash recovery,
lease fencing, panic recovery, stale ownership, malicious names, SQLite failure
injection, deterministic fuzz/property tests, and race detection.
