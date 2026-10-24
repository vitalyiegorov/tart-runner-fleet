package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Replay #415 with the actual shipped sidecar name, not a second copy of it.
// The cache must coexist with owned runners without weakening unknown-runner
// reconciliation: both executor backends enumerate non-runner resources too.
func TestShippedCacheSidecarDoesNotBlockRunnerInventory(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "actions-cache", "tart-runner-fleet-actions-cache.container"))
	if err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, line := range strings.Split(string(body), "\n") {
		if value, found := strings.CutPrefix(line, "ContainerName="); found {
			name = strings.TrimSpace(value)
		}
	}
	if name == "" {
		t.Fatal("cache sidecar must declare its name")
	}
	now := time.Now().UTC()
	inv := ProductionInventory{
		Store: fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Executor: fakeTart{values: []executor.Instance{
			{Name: "trf-small-1", Power: domain.InstancePowerRunning},
			{Name: name, Power: domain.InstancePowerRunning},
		}},
		Host:     fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4},
	}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || len(instances.Value) != 1 || instances.Value[0].ID != "trf-small-1" {
		t.Fatalf("sidecar %q blocked owned-runner inventory: %#v", name, instances)
	}
	inv.Executor = fakeTart{values: []executor.Instance{
		{Name: "trf-small-1", Power: domain.InstancePowerRunning},
		{Name: "trf-unknown-job", Power: domain.InstancePowerRunning},
	}}
	instances, _ = inv.Observe(context.Background())
	if instances.Usable() {
		t.Fatal("an unknown runner must still require reconciliation")
	}
}
