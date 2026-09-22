package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// A node with no Linux profiles never produces Linux demand, so `len(linux)`
// is permanently zero on it. That turned one branch of the macOS planner — the
// one reached when a macOS head will not spawn on a host that is busy rather
// than idle — into a no-op: the plan was returned unchanged and the feasible
// macOS work behind the head was never offered.
//
// Nothing made that reachable in production while the Macs still carried Linux
// profiles, because a queued Linux job took a different branch. ADR 0055
// retires those profiles, which makes this branch the node's STEADY STATE, and
// the arrangement it wedges is the one ADR 0055 names as the point of the
// exercise: "two maestro VMs, or a builder and a maestro".
//
// tests/simulation found it on seed 6 of the macOS-only world, as a liveness
// wedge that held a feasible maestro for 12 ticks behind a builder that could
// never fit. This is the deterministic statement of that counterexample.

// TestBusyMacHostWithNoLinuxDemandAdmitsFeasibleMacBacklog is the regression.
// A live maestro holds part of the host; the queue head is a builder that
// cannot fit the residual and never will while that maestro runs; a second
// maestro behind it fits exactly, is the same profile as the live cohort, and
// is inside the profile's MaxActive. Every rule the fleet has says admit it.
func TestBusyMacHostWithNoLinuxDemandAdmitsFeasibleMacBacklog(t *testing.T) {
	builderHead := demand("a/repo", 1, 30*time.Minute, "builder")
	maestro := demand("b/repo", 2, 20*time.Minute, "maestro")
	live := domain.Instance{ID: "trf-maestro-live", Repo: "c/repo", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro",
		Resources: testConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning}

	in := input([]domain.Demand{builderHead, maestro}, []domain.Instance{live}, State{})
	// The residual holds a second 4-vCPU / 7 GiB maestro and can never hold the
	// 8-vCPU / 12 GiB builder head.
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 9_216, Slots: 3}}, testNow)

	plan := PlanTick(in)
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{maestro.Key}) {
		t.Fatalf("feasible mac backlog starved behind an infeasible mac head on a busy host "+
			"with no Linux demand: spawns = %#v", got)
	}
}

// TestBusyMacHostWithNoLinuxDemandDoesNotWedgeAcrossTicks is the half that
// makes it a wedge rather than one slow tick: the same input re-planned must
// keep admitting rather than latch a one-shot handoff and stall again. A
// bounded backfill that fires once is how #226 looked from a queue.
func TestBusyMacHostWithNoLinuxDemandDoesNotWedgeAcrossTicks(t *testing.T) {
	builderHead := demand("a/repo", 1, 30*time.Minute, "builder")
	first := demand("b/repo", 2, 20*time.Minute, "maestro")
	live := domain.Instance{ID: "trf-maestro-live", Repo: "c/repo", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro",
		Resources: testConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning}

	in := input([]domain.Demand{builderHead, first}, []domain.Instance{live}, State{})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 9_216, Slots: 3}}, testNow)

	plan := PlanTick(in)
	if len(spawnedKeys(plan)) == 0 {
		t.Fatal("first tick admitted nothing")
	}
	// The handoff latch is for draining a foreign platform out of the way. There
	// is no foreign platform here and nothing was drained, so nothing may latch:
	// a latched backfill is what turns one admission into a re-wedge.
	if plan.Next.MacHandoff != nil && plan.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("a mac-only host latched a backfill it never needed: %#v", plan.Next.MacHandoff)
	}
	next := input([]domain.Demand{builderHead, first}, []domain.Instance{live}, plan.Next)
	next.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 9_216, Slots: 3}}, testNow)
	if got := spawnedKeys(PlanTick(next)); len(got) == 0 {
		t.Fatal("the queue re-wedged on the following tick")
	}
}

// TestBusyMacHostStillRefusesWorkThatDoesNotFit is the floor under the fix. The
// remainder pass must admit what fits and nothing else: a residual that holds
// neither queued profile admits neither, rather than spawning something the
// host cannot hold.
func TestBusyMacHostStillRefusesWorkThatDoesNotFit(t *testing.T) {
	builderHead := demand("a/repo", 1, 30*time.Minute, "builder")
	maestro := demand("b/repo", 2, 20*time.Minute, "maestro")
	live := domain.Instance{ID: "trf-maestro-live", Repo: "c/repo", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro",
		Resources: testConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning}

	in := input([]domain.Demand{builderHead, maestro}, []domain.Instance{live}, State{})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 3}}, testNow)

	if got := spawnedKeys(PlanTick(in)); len(got) != 0 {
		t.Fatalf("admitted work the residual cannot hold: %#v", got)
	}
}

// TestBusyMacHostRespectsProfileMaxActiveWithNoLinuxDemand keeps the other
// bound. `maestro` allows two; with two already live the remainder pass must
// admit nothing, however much residual envelope the host reports.
func TestBusyMacHostRespectsProfileMaxActiveWithNoLinuxDemand(t *testing.T) {
	builderHead := demand("a/repo", 1, 30*time.Minute, "builder")
	maestro := demand("b/repo", 2, 20*time.Minute, "maestro")
	instances := []domain.Instance{
		{ID: "trf-maestro-1", Repo: "c/repo", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: testConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning},
		{ID: "trf-maestro-2", Repo: "c/repo", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: testConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning},
	}

	in := input([]domain.Demand{builderHead, maestro}, instances, State{})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, testNow)

	if got := spawnedKeys(PlanTick(in)); len(got) != 0 {
		t.Fatalf("exceeded maestro MaxActive: %#v", got)
	}
}
