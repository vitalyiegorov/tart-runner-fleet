package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// This file is the 2026-10-11 Mac Studio incident at the planning edge (ADR
// 0061). A macOS guest booted and answered on the network, but `tart exec` into
// it hung, so bootstrap failed and the provisioning operation retried it against
// the same VM 395 times over seven hours. The instance sat in Reachable the
// whole time; every recovery cause was scoped to Assigned or Running, so none
// could release it, and the node's macOS queue waited behind it.

func stalledProvisioning(state domain.InstanceState, since time.Duration) domain.Instance {
	return domain.Instance{ID: "trf-macos-4x7-7afca955a01a9e1e", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium",
		Route: "tiered", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}, State: state,
		Power: domain.InstancePowerRunning, Demand: domain.DemandKey{Repo: "a/repo", RunID: 7, Attempt: 1, JobID: 11},
		ProvisioningSince: testNow.Add(-since)}
}

func TestAProvisioningStuckPastTheDeadlineIsReclaimed(t *testing.T) {
	for _, state := range []domain.InstanceState{domain.InstanceCloning, domain.InstanceBooting, domain.InstanceReachable} {
		t.Run(string(state), func(t *testing.T) {
			stuck := stalledProvisioning(state, 16*time.Minute)
			plan := PlanTick(input([]domain.Demand{demand("b/repo", 9, time.Minute, "small")}, []domain.Instance{stuck}, State{}))
			if len(plan.Operations) != 1 {
				t.Fatalf("plan = %#v, want exactly the reclaim", plan.Operations)
			}
			got := plan.Operations[0]
			if got.Kind != OperationDrain || got.Instance != stuck.ID || !got.Recovery || !got.ProvisionStalled ||
				got.StalledAssignment || got.OccupancyExceeded || got.GuestUnresponsive || got.ConfirmedInactive || got.LingeringRunner {
				t.Fatalf("reclaim = %#v, want a provisioning-stalled recovery and no other cause", got)
			}
			// The operation names the demand so the operator's log line says which
			// job is being re-provisioned.
			if got.Demand != stuck.Demand {
				t.Fatalf("reclaim demand = %#v, want %#v", got.Demand, stuck.Demand)
			}
		})
	}
}

// The tripwire counterpart. A provisioning step younger than the deadline is
// still working; Planned has no VM yet and may wait on another operation;
// Registering may not drain; an unknown entry time and an unconfigured deadline
// both fail closed.
func TestTheProvisioningDeadlineLeavesEverythingElseAlone(t *testing.T) {
	for _, test := range []struct {
		name     string
		instance domain.Instance
		deadline time.Duration
	}{
		{name: "young step", instance: stalledProvisioning(domain.InstanceReachable, 14*time.Minute), deadline: 15 * time.Minute},
		{name: "planned", instance: stalledProvisioning(domain.InstancePlanned, time.Hour), deadline: 15 * time.Minute},
		{name: "registering", instance: stalledProvisioning(domain.InstanceRegistering, time.Hour), deadline: 15 * time.Minute},
		{name: "unknown entry time", instance: func() domain.Instance {
			i := stalledProvisioning(domain.InstanceReachable, 0)
			i.ProvisioningSince = time.Time{}
			return i
		}(), deadline: 15 * time.Minute},
		{name: "no deadline configured", instance: stalledProvisioning(domain.InstanceReachable, time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := input(nil, []domain.Instance{test.instance}, State{})
			in.Config.AssignedTimeout = test.deadline
			if plan := PlanTick(in); len(drainedIDs(plan)) != 0 {
				t.Fatalf("the provisioning deadline drained a protected instance: %#v", plan.Operations)
			}
		})
	}
}
