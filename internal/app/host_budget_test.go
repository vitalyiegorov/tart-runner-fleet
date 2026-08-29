package app

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// A host budget is a promise the operator makes about a machine `fleet config
// validate` has never seen: the CLI decodes a file, it does not probe a host. So
// the one place the promise can be checked against reality is the probe itself,
// and a budget the machine cannot honour is a configuration error the fleet must
// refuse to run on rather than quietly ignore.
func TestHostObservationRejectsABudgetTheMachineCannotHonour(t *testing.T) {
	tests := map[string]struct {
		budget     domain.Resources
		physCPU    int64
		physMemory int64
		elastic    bool
		wantReason string
	}{
		"budget inside the machine is honoured": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, physCPU: 10, physMemory: 24_576, elastic: true},
		"budget inside the machine is honoured under the static model": {
			budget: domain.Resources{CPU: 4, MemoryMB: 10_240}, physCPU: 10, physMemory: 24_576},
		"budget above the physical cores is refused": {
			budget: domain.Resources{CPU: 16, MemoryMB: 10_240}, physCPU: 10, physMemory: 24_576,
			elastic: true, wantReason: "cores"},
		"budget above the physical cores is refused under the static model": {
			budget: domain.Resources{CPU: 16, MemoryMB: 10_240}, physCPU: 10, physMemory: 24_576,
			wantReason: "cores"},
		"budget above the usable memory is refused": {
			budget: domain.Resources{CPU: 4, MemoryMB: 40_960}, physCPU: 10, physMemory: 24_576,
			elastic: true, wantReason: "memory"},
		"budget above physical memory net of the reserve is refused": {
			budget: domain.Resources{CPU: 4, MemoryMB: 24_576}, physCPU: 10, physMemory: 24_576,
			elastic: true, wantReason: "memory"},
		"an unobserved physical total imposes no bound": {
			budget: domain.Resources{CPU: 16, MemoryMB: 40_960}, elastic: true},
		"no budget is never rejected": {
			physCPU: 10, physMemory: 24_576, elastic: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			snapshot := productionSnapshot()
			snapshot.PhysicalCPU = test.physCPU
			snapshot.PhysicalMemoryMB = test.physMemory
			observation := hostObservation(snapshot, configuredCapacity(), productionGuards(), test.elastic, test.budget, false)
			if test.wantReason == "" {
				if !observation.Usable() {
					t.Fatalf("observation unusable: %#v", observation)
				}
				return
			}
			if observation.State != domain.ObservationUnavailable {
				t.Fatalf("observation state = %v, want unavailable for a budget the machine cannot honour", observation.State)
			}
			if !strings.Contains(observation.Reason, test.wantReason) ||
				!strings.Contains(observation.Reason, "budget") {
				t.Fatalf("observation reason = %q, want one naming the host budget and %q", observation.Reason, test.wantReason)
			}
		})
	}
}

// TestBuildSchedulerConfigCarriesTheHostBudget wires the setting the whole way:
// a value in fleet.json has to reach the one function that produces an admission
// envelope, or the ceiling exists only on paper.
func TestBuildSchedulerConfigCarriesTheHostBudget(t *testing.T) {
	cfg := config.Default()
	cfg.HostBudget = config.Resources{CPU: 4, MemoryMiB: 10_240}
	built := BuildSchedulerConfig(cfg)
	if built.HostBudget != (domain.Resources{CPU: 4, MemoryMB: 10_240}) {
		t.Fatalf("scheduler host budget = %+v, want 4 CPU / 10240 MB", built.HostBudget)
	}
	if omitted := BuildSchedulerConfig(config.Default()); omitted.HostBudget != (domain.Resources{}) {
		t.Fatalf("omitted host budget = %+v, want the zero vector that imposes no bound", omitted.HostBudget)
	}
}
