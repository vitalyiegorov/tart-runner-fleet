package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// hostBudget is the operator's static ceiling on the TOTAL admission envelope,
// every platform charged against it. It exists for a node the fleet shares with
// somebody else's work — the Mac Studio serving maestro overflow inside 4 vCPU
// and 10 GiB of a much larger machine. It composes with, and never replaces, the
// pressure guardrails: those read whole-host signals and protect co-tenants
// dynamically, while the budget is the ceiling that holds even on a fully idle
// host.

// budgetHost is a wide, quiet machine: 10 physical cores and 24 GiB, all of it
// measured as available. Nothing but the budget can narrow admission here, which
// is exactly what makes these tests about the budget and nothing else.
func budgetHost() domain.Observation[domain.Host] {
	return domain.Fresh(domain.Host{
		Capacity:  domain.Resources{CPU: hostPhysicalCPU, MemoryMB: hostPhysicalMemoryMB - 1_024, Slots: 4},
		Available: domain.Resources{CPU: hostPhysicalCPU, MemoryMB: hostPhysicalMemoryMB - 1_024, Slots: 4},
		Pressure: domain.HostPressure{AvailableMemoryMB: hostPhysicalMemoryMB - 1_024, FreeDiskGB: 400,
			CPUIdlePercent: 100, LoadAverage: 0.2, AdmissionAllowed: true, AdmissionReason: "capacity available"},
	}, testNow)
}

// budgetInput queues more work than any budget can hold, so what a plan admits
// is decided by the envelope and never by the demand supply.
func budgetInput(budget domain.Resources, elastic bool, instances []domain.Instance, profile domain.ProfileID) Input {
	demands := []domain.Demand{
		demand("a/repo", 1, time.Minute, profile),
		demand("b/repo", 2, time.Minute, profile),
		demand("c/repo", 3, time.Minute, profile),
	}
	in := input(demands, instances, State{})
	in.Config.MixedPlatformAdmission = true
	in.Config.MixedProfileCohorts = true
	in.Config.ElasticHostEnvelope = elastic
	in.Config.HostBudget = budget
	in.Host = budgetHost()
	return in
}

func TestHostBudgetCapsTheTotalEnvelopeUnderEveryCapacityModel(t *testing.T) {
	tests := map[string]struct {
		budget   domain.Resources
		elastic  bool
		profile  domain.ProfileID
		wantCPU  int
		wantSpec int
	}{
		"elastic model, macOS work inside a 4 vCPU budget": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, elastic: true, profile: "maestro", wantCPU: 4, wantSpec: 1},
		"static model, macOS work inside a 4 vCPU budget": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, elastic: false, profile: "maestro", wantCPU: 4, wantSpec: 1},
		"elastic model, Linux work inside a 4 vCPU budget": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, elastic: true, profile: "medium", wantCPU: 4, wantSpec: 2},
		"static model, Linux work inside a 4 vCPU budget": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, elastic: false, profile: "medium", wantCPU: 4, wantSpec: 2},
		"memory is the binding dimension": {
			budget: domain.Resources{CPU: 8, MemoryMB: 5_000}, elastic: true, profile: "medium", wantCPU: 2, wantSpec: 1},
		"no budget leaves the envelope where it was": {
			elastic: true, profile: "medium", wantCPU: 6, wantSpec: 3},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			in := budgetInput(test.budget, test.elastic, nil, test.profile)
			plan, err := PlanTick(in)
			if err != nil {
				t.Fatalf("PlanTick() error = %v", err)
			}
			admitted := admittedVector(in, plan)
			if len(spawnedKeys(plan)) != test.wantSpec || admitted.CPU != test.wantCPU {
				t.Fatalf("plan admitted %d spawns holding %+v, want %d spawns and %d CPU",
					len(spawnedKeys(plan)), admitted, test.wantSpec, test.wantCPU)
			}
		})
	}
}

// TestHostBudgetChargesLiveInstancesOfEveryPlatform is the property the epic
// asks for: the budget bounds the total, so a live macOS VM must consume Linux
// headroom and vice versa. Under the elastic model LinuxCapacity is a Linux-only
// cap, which alone would let a macOS VM spend nothing of it.
func TestHostBudgetChargesLiveInstancesOfEveryPlatform(t *testing.T) {
	live := domain.Instance{ID: "trf-maestro-1", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	in := budgetInput(domain.Resources{CPU: 6, MemoryMB: 12_288}, true, []domain.Instance{live}, "medium")
	in.Config.RepoCaps["mac-a"] = 2
	plan, err := PlanTick(in)
	if err != nil {
		t.Fatalf("PlanTick() error = %v", err)
	}
	admitted := admittedVector(in, plan)
	if admitted.CPU+live.Resources.CPU > 6 || admitted.MemoryMB+live.Resources.MemoryMB > 12_288 {
		t.Fatalf("live %+v plus admitted %+v exceeds the 6 CPU / 12288 MB budget", live.Resources, admitted)
	}
	if admitted.CPU != 2 {
		t.Fatalf("admitted %+v, want the one 2-vCPU medium the budget leaves beside a live maestro", admitted)
	}
}

// TestHostBudgetBindsAgedWorkToo separates the budget from ADR 0018's advisory
// CPU-idle clamp. Aged work escapes the clamp, because a host with an active
// tenant may never produce a quiet moment and FIFO position is worthless without
// capacity. It must not escape the budget, which is a hard declared ceiling.
func TestHostBudgetBindsAgedWorkToo(t *testing.T) {
	in := budgetInput(domain.Resources{CPU: 4, MemoryMB: 10_240}, true, nil, "medium")
	in.Demands = domain.Fresh([]domain.Demand{
		demand("a/repo", 1, 40*time.Minute, "medium"),
		demand("b/repo", 2, 39*time.Minute, "medium"),
		demand("c/repo", 3, 38*time.Minute, "medium"),
	}, testNow)
	host := budgetHost()
	host.Value.Available.CPU = 0
	in.Host = host
	plan, err := PlanTick(in)
	if err != nil {
		t.Fatalf("PlanTick() error = %v", err)
	}
	if admitted := admittedVector(in, plan); admitted.CPU != 4 {
		t.Fatalf("aged admission held %+v, want exactly the 4 CPU the budget allows", admitted)
	}
}

// TestHostBudgetNeverWidensAnEnvelope proves the setting is a ceiling and not a
// second capacity source: a budget larger than the machine changes nothing.
func TestHostBudgetNeverWidensAnEnvelope(t *testing.T) {
	generous := budgetInput(domain.Resources{CPU: 64, MemoryMB: 262_144}, true, nil, "medium")
	unbudgeted := budgetInput(domain.Resources{}, true, nil, "medium")
	generousPlan, err := PlanTick(generous)
	if err != nil {
		t.Fatalf("PlanTick(generous) error = %v", err)
	}
	unbudgetedPlan, err := PlanTick(unbudgeted)
	if err != nil {
		t.Fatalf("PlanTick(unbudgeted) error = %v", err)
	}
	if admittedVector(generous, generousPlan) != admittedVector(unbudgeted, unbudgetedPlan) {
		t.Fatalf("a budget above the machine changed admission: %+v vs %+v",
			admittedVector(generous, generousPlan), admittedVector(unbudgeted, unbudgetedPlan))
	}
}

// TestHostBudgetComposesWithTheClosedPressureGate keeps the division of labour
// explicit: the guardrails still fail closed on a host in distress, and a budget
// is not a licence to admit through them.
func TestHostBudgetComposesWithTheClosedPressureGate(t *testing.T) {
	in := budgetInput(domain.Resources{CPU: 4, MemoryMB: 10_240}, true, nil, "medium")
	in.Host = domain.Fresh(domain.Host{Pressure: domain.HostPressure{AdmissionAllowed: false,
		AdmissionReason: "host memory pressure"}}, testNow)
	plan, err := PlanTick(in)
	if err != nil {
		t.Fatalf("PlanTick() error = %v", err)
	}
	if len(spawnedKeys(plan)) != 0 {
		t.Fatalf("plan admitted %d spawns through a closed pressure gate", len(spawnedKeys(plan)))
	}
}
