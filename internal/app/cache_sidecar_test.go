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
	name := shippedCacheSidecarValue(t, "ContainerName")
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
		{Name: name, Power: domain.InstancePowerRunning},
		{Name: "trf-unknown-job", Power: domain.InstancePowerRunning},
	}}
	instances, _ = inv.Observe(context.Background())
	if instances.Usable() {
		t.Fatal("an unknown runner must still require reconciliation")
	}
}

// Replay of the ubuntu-vitalii restart loop (1347 restarts in a day): Podman
// 5.7's Quadlet generator dropped the closing quote of a HealthCmd whose
// argument nested double and single quotes, so /bin/sh rejected every probe
// with "unterminated quoted string", HealthOnFailure=kill killed a healthy
// server, and systemd restarted it. Podman 6.1 on Omarchy kept the quote,
// which is why one node served the cache and the other never did. A probe
// with no quoting at all reads the same under every generator.
func TestShippedCacheSidecarHealthCheckNeedsNoQuoting(t *testing.T) {
	probe := shippedCacheSidecarValue(t, "HealthCmd")
	if !strings.Contains(probe, "http://127.0.0.1:3000/health") {
		t.Fatalf("health probe %q must target the server's own /health endpoint", probe)
	}
	if strings.ContainsAny(probe, "\"'\\$`;|&()<>") {
		t.Fatalf("health probe %q relies on shell quoting or metacharacters that Quadlet versions render differently", probe)
	}
}

// shippedCacheSidecarValue reads one key from the Quadlet unit the repository
// actually ships, so a test cannot drift from a second copy of it.
func shippedCacheSidecarValue(t *testing.T, key string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "actions-cache", "tart-runner-fleet-actions-cache.container"))
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	for _, line := range strings.Split(string(body), "\n") {
		if found, ok := strings.CutPrefix(line, key+"="); ok {
			value = strings.TrimSpace(found)
		}
	}
	return value
}
