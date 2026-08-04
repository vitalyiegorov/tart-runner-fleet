package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// secondPilotConfig is the live fleet.json shape from the production Mac mini,
// parameterized only by the elastic-envelope flag.
func secondPilotConfig(t *testing.T, elastic bool) config.Config {
	t.Helper()
	flag := ""
	if elastic {
		flag = `"elasticHostEnvelope":true,`
	}
	raw := `{
      "baseVm":"linux-runner-base-go", "vmPrefix":"trf-linux",
      "pollSeconds":5, "maxLinuxWhenMacosIdle":4,
      "maxLinuxCpu":8, "maxLinuxMemoryMb":16384,
      "linuxReservationAgeSeconds":300,
      "minFreeDiskGb":60, "minAvailableMemoryMb":1024,
      "maxSwapUsedMb":2048, "maxLoadAverage":9, "minCpuIdlePercent":5,
      "pressureMemoryAccounting":true,` + flag + `
      "linuxProfiles":[
        {"id":"small","label":"linux-small","cpu":1,"memoryMb":2048,"diskGb":50},
        {"id":"medium","label":"linux-medium","cpu":2,"memoryMb":4096,"diskGb":50},
        {"id":"large","label":"linux-large","cpu":4,"memoryMb":8192,"diskGb":50}
      ],
      "macosBurst":{"enabled":true,"baseVm":"macos-tartelet-base-go","vmPrefix":"trf-macos",
        "mixedPlatformAdmission":true,
        "builder":{"id":"builder","label":"macos-builder","cpu":7,"memoryMb":12288,"maxActive":1},
        "maestro":{"id":"maestro","label":"macos-maestro","cpu":4,"memoryMb":7168,"maxActive":2}},
      "targets":[
        {"type":"repo","slug":"a/repo","maxActive":4},
        {"type":"repo","slug":"b/repo","maxActive":4},
        {"type":"repo","slug":"mac/a","maxActive":2}
      ]
    }`
	cfg, err := config.Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if cfg.Guards.ElasticHostEnvelope != elastic {
		t.Fatalf("ElasticHostEnvelope = %v, want %v", cfg.Guards.ElasticHostEnvelope, elastic)
	}
	return cfg
}

// incidentProbe is the host reading fleetd reported during the incident: a
// 10-core / 24 GiB machine that is 62.4% idle with 12 GiB available.
type incidentProbe struct{}

func (incidentProbe) Snapshot(context.Context) executor.HostSnapshot {
	return executor.HostSnapshot{
		Freshness:         executor.Fresh,
		ObservedAt:        time.Unix(1000, 0).UTC(),
		AvailableMemoryMB: 12_288,
		FreeDiskGB:        152,
		SwapUsedMB:        574,
		SwapOuts:          42_392,
		CPUidlePercent:    62.4,
		LoadAverage:       4.19,
		PhysicalCPU:       10,
		PhysicalMemoryMB:  24_576,
	}
}

// TestSecondPilotEnvelopeUsesIdleHostEndToEnd drives the real decode ->
// guardrail -> host-observation -> scheduler pipeline with the live incident
// facts. Under the static envelope the planner strands the host; under the
// elastic envelope it puts the measured idle capacity to work without ever
// exceeding the physical machine.
func TestSecondPilotEnvelopeUsesIdleHostEndToEnd(t *testing.T) {
	now := time.Date(2026, 7, 25, 5, 36, 17, 0, time.UTC)
	builder := domain.Instance{ID: "trf-builder-35917ac43a789b33", Repo: "mac/a",
		Platform: domain.PlatformMacOS, Profile: "builder", Route: "macos-builder",
		Resources: domain.Resources{CPU: 7, MemoryMB: 12_288, Slots: 1},
		State:     domain.InstanceRunning, Power: domain.InstancePowerRunning}
	demands := []domain.Demand{
		{Key: domain.DemandKey{Repo: "mac/a", RunID: 100, Attempt: 1, JobID: 1}, CreatedAt: now.Add(-37 * time.Minute),
			Profile: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS, Event: domain.EventPullRequest},
		{Key: domain.DemandKey{Repo: "a/repo", RunID: 100, Attempt: 1, JobID: 3}, CreatedAt: now.Add(-27 * time.Minute),
			Profile: "large", Route: "linux-large", Platform: domain.PlatformLinux, Event: domain.EventPullRequest},
		{Key: domain.DemandKey{Repo: "b/repo", RunID: 100, Attempt: 1, JobID: 5}, CreatedAt: now.Add(-11 * time.Minute),
			Profile: "medium", Route: "linux-medium", Platform: domain.PlatformLinux, Event: domain.EventPullRequest},
	}

	// The host observation is produced by the real ProductionInventory path so
	// the guardrails, reserves, and physical accounting are the daemon's own.
	plan := func(elastic bool) (scheduler.Plan, domain.Observation[domain.Host]) {
		cfg := secondPilotConfig(t, elastic)
		inventory := app.ProductionInventory{
			Store: emptyStore{}, Executor: emptyTart{}, Host: incidentProbe{},
			Capacity: domain.Resources{CPU: cfg.Linux.Capacity.CPU, MemoryMB: cfg.Linux.Capacity.MemoryMiB,
				Slots: cfg.Linux.MaxInstances},
			Guards: executor.Guardrails{MinFreeDiskGB: int64(cfg.Guards.MinFreeDiskGiB),
				MinAvailableMemoryMB: int64(cfg.Guards.MinAvailableMemoryMiB),
				MaxSwapUsedMB:        int64(cfg.Guards.MaxSwapUsedMiB),
				MaxLoadAverage:       cfg.Guards.MaxLoadAverage, MinCPUidlePercent: cfg.Guards.MinCPUIdlePercent},
			ElasticHostEnvelope: cfg.Guards.ElasticHostEnvelope,
		}
		_, host := inventory.Observe(context.Background())
		if !host.Usable() {
			t.Fatalf("host observation unusable: %#v", host)
		}
		return scheduler.PlanTick(scheduler.Input{Now: now, Config: app.BuildSchedulerConfig(cfg),
			Demands:   domain.Fresh(demands, now),
			Instances: domain.Fresh([]domain.Instance{builder}, now),
			Host:      host, Prior: scheduler.State{}}), host
	}

	staticPlan, staticHost := plan(false)
	if staticHost.Value.Capacity != (domain.Resources{}) {
		t.Fatalf("static mode advertised physical capacity %+v, want none", staticHost.Value.Capacity)
	}
	if spawns := spawnedProfiles(staticPlan); len(spawns) != 0 {
		t.Fatalf("static envelope spawned %v; the incident state admits nothing", spawns)
	}

	elasticPlan, elasticHost := plan(true)
	if elasticHost.Value.Capacity.CPU != 10 {
		t.Fatalf("elastic Capacity.CPU = %d, want the 10 physical cores", elasticHost.Value.Capacity.CPU)
	}
	if elasticHost.Value.Available.CPU != 6 {
		t.Fatalf("elastic Available.CPU = %d, want floor(10 x 62.4%%) = 6", elasticHost.Value.Available.CPU)
	}
	spawns := spawnedProfiles(elasticPlan)
	if len(spawns) == 0 {
		t.Fatalf("elastic envelope admitted nothing on a 62%%-idle host: %#v", elasticPlan.Operations)
	}
	// Aggregate vCPU must remain inside the physical machine.
	total := builder.Resources.CPU
	for _, profile := range spawns {
		if profile == "builder" || profile == "maestro" {
			t.Fatalf("second pilot must not grow the macOS cohort past MaxActive: %v", spawns)
		}
		total += profileCPU(t, profile)
	}
	if total > 10 {
		t.Fatalf("admitted %v reaching %d vCPU, beyond the 10 physical cores", spawns, total)
	}
	for _, op := range elasticPlan.Operations {
		if op.Kind == scheduler.OperationDrain {
			t.Fatalf("second pilot must never drain to make room: %#v", elasticPlan.Operations)
		}
	}
}

// emptyStore and emptyTart isolate the host observation: the scheduler input
// supplies occupancy explicitly, so the inventory only needs to be usable.
type emptyStore struct{}

func (emptyStore) LiveInstances(context.Context) ([]operations.Instance, error) { return nil, nil }

type emptyTart struct{}

func (emptyTart) List(context.Context) ([]executor.Instance, error) { return nil, nil }

func spawnedProfiles(plan scheduler.Plan) []domain.ProfileID {
	var profiles []domain.ProfileID
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			profiles = append(profiles, operation.Profile)
		}
	}
	return profiles
}

func profileCPU(t *testing.T, profile domain.ProfileID) int {
	t.Helper()
	switch profile {
	case "small":
		return 1
	case "medium":
		return 2
	case "large":
		return 4
	}
	t.Fatalf("unexpected profile %q", profile)
	return 0
}
