package scheduler

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// A busy instance is one whose runner may be executing a workflow job right
// now: state assigned or running, VM powered on, and no fresh confirmation
// that the runner and its job are inactive. Draining one kills a live CI job
// (2026-07-20 incident analysis, issue #72). Every planner path that can emit
// drains must leave busy instances alone; this suite pins that invariant for
// each path independently so a regression in any one of them fails loudly.

func busyInstance(id string, profile domain.ProfileID) domain.Instance {
	config := testConfig().Profiles[profile]
	return domain.Instance{ID: id, Repo: "a/repo", Platform: config.Platform, Profile: profile, Route: config.Route,
		Resources: config.Resources, State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

func drainedIDs(plan Plan) []string {
	var ids []string
	for _, operation := range plan.Operations {
		if operation.Kind == OperationDrain {
			ids = append(ids, operation.Instance)
		}
	}
	return ids
}

func assertNoDrainOf(t *testing.T, plan Plan, id string) {
	t.Helper()
	for _, drained := range drainedIDs(plan) {
		if drained == id {
			t.Fatalf("busy instance %q was drained; plan operations: %#v", id, plan.Operations)
		}
	}
}

func TestBusyInstancesSurviveEveryDrainPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		state domain.InstanceState
	}{
		{name: "assigned", state: domain.InstanceAssigned},
		{name: "running", state: domain.InstanceRunning},
	} {
		t.Run("exclusive mac handoff with busy linux "+test.name, func(t *testing.T) {
			busy := busyInstance("busy-linux", "medium")
			busy.State = test.state
			in := input([]domain.Demand{demand("a/repo", 1, 0, "maestro")}, []domain.Instance{busy}, State{})
			in.Config.MacOSExclusive = true
			assertNoDrainOf(t, PlanTick(in), busy.ID)
		})

		t.Run("mac profile switch with busy foreign mac "+test.name, func(t *testing.T) {
			busy := busyInstance("busy-builder", "builder")
			busy.State = test.state
			in := input([]domain.Demand{demand("a/repo", 2, 0, "maestro")}, []domain.Instance{busy}, State{})
			assertNoDrainOf(t, PlanTick(in), busy.ID)
		})

		t.Run("linux handoff with busy mac "+test.name, func(t *testing.T) {
			busy := busyInstance("busy-maestro", "maestro")
			busy.State = test.state
			in := input([]domain.Demand{demand("a/repo", 3, 0, "small")}, []domain.Instance{busy}, State{})
			in.Config.MacOSExclusive = true
			assertNoDrainOf(t, PlanTick(in), busy.ID)
		})

		t.Run("assignment recovery without confirmation "+test.name, func(t *testing.T) {
			busy := busyInstance("busy-unconfirmed", "medium")
			busy.State = test.state
			busy.RecoveryReady = false
			in := input(nil, []domain.Instance{busy}, State{})
			assertNoDrainOf(t, PlanTick(in), busy.ID)
		})

		t.Run("idle reclaim pressure with busy "+test.name, func(t *testing.T) {
			busy := busyInstance("busy-under-pressure", "medium")
			busy.State = test.state
			in := input([]domain.Demand{demand("b/repo", 4, 0, "large"), demand("c/repo", 5, 0, "large")},
				[]domain.Instance{busy}, State{})
			// Saturate capacity so any eager reclaim would be tempted to evict.
			in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 1, MemoryMB: 1_024, Slots: 1}}, testNow)
			assertNoDrainOf(t, PlanTick(in), busy.ID)
		})
	}
}

func TestConfirmedInactiveAndStoppedInstancesStillDrain(t *testing.T) {
	// The counterpart guard: recovery MUST still reclaim instances that are
	// provably not running work, or dead VMs leak capacity forever.
	stopped := busyInstance("stopped", "medium")
	stopped.Power = domain.InstancePowerStopped

	confirmed := busyInstance("confirmed-inactive", "medium")
	confirmed.RecoveryReady = true

	for _, test := range []struct {
		instance domain.Instance
	}{{stopped}, {confirmed}} {
		plan := PlanTick(input(nil, []domain.Instance{test.instance}, State{}))
		if len(drainedIDs(plan)) != 1 || drainedIDs(plan)[0] != test.instance.ID {
			t.Fatalf("instance %q must be recovery-drained; got %#v", test.instance.ID, plan.Operations)
		}
	}
}
