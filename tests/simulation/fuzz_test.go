package simulation_test

import (
	"flag"
	"testing"
)

// The fuzz driver is bounded by flags so one binary serves every budget:
//
//	go test ./tests/simulation                                       # make unit / make race
//	go test ./tests/simulation -run TestSimFuzz -seeds=80  -sim-ticks=200   # pull request
//	go test ./tests/simulation -run TestSimFuzz -seeds=500 -sim-ticks=320   # nightly
//
// The defaults are deliberately small, because `make unit`, `make race`, and the
// coverage gate all run this package unqualified and the race detector amplifies
// a simulation run by roughly twenty-five times. The pull-request gate widens the
// sweep in a separate, un-instrumented step; the nightly widens it much further.
var (
	seedsFlag = flag.Int("seeds", 8, "number of simulation seeds TestSimFuzz explores")
	ticksFlag = flag.Int("sim-ticks", 120, "reconciliation ticks per simulation run")
	seedBase  = flag.Int64("seed-base", 1, "first seed TestSimFuzz explores")
)

// TestSimFuzz is the seed sweep. Every seed is an independent world history; a
// violation is shrunk to a minimal event trace and reported with the plan and
// inputs that produced it, so the reproduction is a command line rather than a
// story.
func TestSimFuzz(t *testing.T) {
	t.Parallel()
	sweep(t, "TestSimFuzz", defaultWorld())
}

// TestSimFuzzBudgetedHost is the same sweep on the shared Mac Studio of issue
// #136: a machine far larger than the share the fleet is allowed, held to an
// explicit 4-vCPU / 10 GiB ceiling. It runs as its own arm rather than as a case
// inside TestSimFuzz because the seed stream is a function of the world, so one
// binary exploring two worlds must explore each of them fully.
//
// Property (g) is what this arm exists to exercise. Its resource ceiling is the
// budget, not the machine, so a committed vector that fits the hardware but
// exceeds the declared share is a conservation violation here and nowhere else.
func TestSimFuzzBudgetedHost(t *testing.T) {
	t.Parallel()
	sweep(t, "TestSimFuzzBudgetedHost", budgetedWorld())
}

// TestSimFuzzContainerNode is the same sweep on the x86 Linux node of issue
// #139: every Linux profile, no macOS profile, twelve cores. It is the
// single-platform world docs/MULTI_NODE_PLAN.md reserves for chunk 2d, and it
// runs as its own arm for the same reason the budgeted host does -- the seed
// stream is a function of the world.
//
// It exercises what the other two arms cannot: the `planLinux` path with no
// two-platform admission logic present at all, which is the arrangement ADR 0034
// §1 calls "heterogeneity expressed as single-platform configurations per node".
func TestSimFuzzContainerNode(t *testing.T) {
	t.Parallel()
	sweep(t, "TestSimFuzzContainerNode", containerNodeWorld())
}

func sweep(t *testing.T, name string, cfg worldConfig) {
	t.Helper()
	for offset := range *seedsFlag {
		seed := *seedBase + int64(offset)
		trace := generateTrace(seed, *ticksFlag, cfg)
		findings := runTrace(t, cfg, trace)
		if len(findings) == 0 {
			continue
		}
		minimal, reduced := shrink(t, cfg, trace, firstKind(findings))
		t.Fatalf("seed %d violated %s\nreproduce: go test ./tests/simulation -run %s -seeds=1 -seed-base=%d -sim-ticks=%d\n%s\n%s",
			seed, firstKind(findings), name, seed, *ticksFlag, reduced[0], minimal)
	}
}

// knownFinding names the defects this repository currently documents as open in
// the scheduler rather than in the harness. ADR 0031 records why each one is
// here and what would close it; nothing else is tolerated.
//
// Each is pinned by its own characterization test in findings_test.go, so the
// sweep tolerating it cannot let it silently change shape.
//
// FINDINGS 1, 2, 3 and 7 are deliberately absent: they are fixed, so
// sigRespawnLiveIncarnation, sigMacOSIgnoresRepositoryCap,
// sigControlPlaneOvertakesAgedWork and sigReservedHeadHeldByARepositoryCap now
// fail the sweep like any other violation of properties (e), (g), (b) and (a).
// Each signature itself is kept so a regression is reported by name rather than
// as an anonymous refusal.
//
// FINDING 7 is the one to read this list by. It was tolerated here, then removed
// as "retired pending #226" when no seed reproduced it any more, and the reason
// no seed reproduced it was that an oracle refinement had blinded the harness to
// it rather than that the scheduler had stopped producing it (ADR 0031, ADR
// 0038). A signature that stops firing is evidence about the ORACLE until
// somebody proves it is evidence about the scheduler.
func knownFinding(item finding) bool {
	switch item.Signature {
	case sigCrossPlatformResidualArbitration, sigCountMaximizationOvertakesAgedWork:
		return true
	}
	return false
}

// TestSimIsDeterministic is the harness's own contract: the same (seed, config)
// pair must produce the same history twice, or nothing else in this package
// means anything.
func TestSimIsDeterministic(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	trace := generateTrace(20_260_803, 60, cfg)
	first := newWorld(t, cfg, trace)
	defer first.close()
	first.run()
	second := newWorld(t, cfg, trace)
	defer second.close()
	second.run()
	if len(first.observations) != len(second.observations) {
		t.Fatalf("replay diverged in length: %d vs %d", len(first.observations), len(second.observations))
	}
	for index := range first.observations {
		left, right := first.observations[index], second.observations[index]
		if left.Plan.ID != right.Plan.ID || left.Applied != right.Applied || len(left.Demands) != len(right.Demands) {
			t.Fatalf("replay diverged at tick %d:\n%s\n%s", left.Tick, first.dumpPlan(left), second.dumpPlan(right))
		}
	}
}

// TestGeneratedTraceExercisesTheWholeWorld guards the generator itself: a sweep
// that never delays a message or never wedges a drain proves nothing about the
// composition, and a silently narrowed generator is the classic way a
// simulation suite rots into a smoke test.
func TestGeneratedTraceExercisesTheWholeWorld(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	seen := map[eventKind]int{}
	for seed := int64(1); seed <= 40; seed++ {
		for _, event := range generateTrace(seed, 200, cfg).Events {
			seen[event.Kind]++
		}
	}
	required := []eventKind{eventArrive, eventStopArrivals, eventSilentCancel, eventLoudCancel,
		eventBrokerDelay, eventBrokerDuplicate, eventBrokerDrop, eventBrokerReorder, eventStatisticsGap,
		eventRESTLag, eventHostTenant, eventHostProbeStale, eventTartUnavailable, eventSlowBoot,
		eventLongJob, eventOverrunJob, eventStalledRunner, eventWedgedDrain, eventUnstoppableGuest,
		eventSilentGuest, eventSaturatedGuest, eventMisreportedPower, eventSiblingReassign, eventSiblingSubstitute}
	for _, kind := range required {
		if seen[kind] == 0 {
			t.Fatalf("the generator never produced %s across 40 seeds", kind)
		}
	}
}
