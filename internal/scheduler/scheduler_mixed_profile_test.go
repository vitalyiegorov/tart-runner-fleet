package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ADR 0012 relaxed platform exclusivity: Linux and macOS coexist when their
// exact vectors fit. Profile exclusivity WITHIN macOS remained: planMacOS spawns
// a profile only after every foreign-profile macOS instance is drained, and a
// busy foreign instance blocks the spawn outright. One macOS profile cohort at a
// time, however much room the machine has.
//
// Measured on the production host on 2026-07-30: one 6-vCPU builder running,
// two 4-vCPU maestros queued, 4 of 10 physical cores and 10 GiB idle, queue SLO
// breached. builder 6 + maestro 4 = exactly 10 vCPU and the memory fits -- the
// only refusal was the cohort rule. Every E2E build serializes against every
// E2E test shard this way, in both directions.
//
// mixedProfileCohorts lets macOS profiles coexist under the same law as
// everything else: exact vectors, the physical bound, profile MaxActive, and
// drain safety. Default false preserves the single-cohort behavior
// byte-for-byte.

// mixedProfileConfig is the production shape: 10 physical cores, a 6-vCPU
// builder capped at one, 4-vCPU maestros capped at two, elastic envelope on.
func mixedProfileConfig() Config {
	cfg := elasticConfig()
	cfg.ElasticHostEnvelope = true
	cfg.MixedProfileCohorts = true
	cfg.Profiles["builder"] = domain.Profile{ID: "builder", Platform: domain.PlatformMacOS,
		Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1}
	return cfg
}

// busyBuilder is a builder mid-job: not idle, never drainable.
func busyBuilder() domain.Instance {
	return domain.Instance{ID: "trf-builder-live", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "builder", Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

func mixedProfileInput(cfg Config, demands []domain.Demand, instances []domain.Instance) Input {
	in := input(demands, instances, State{})
	in.Config = cfg
	in.Host = elasticHost(90, 20_480)
	return in
}

// TestMaestroSpawnsBesideBusyBuilder is the RED-first case: a maestro whose
// vector fits beside a busy builder must be admitted without waiting for the
// builder to finish, and without draining anything.
func TestMaestroSpawnsBesideBusyBuilder(t *testing.T) {
	cfg := mixedProfileConfig()
	demands := []domain.Demand{
		demand("mac-a", 1, 3*time.Minute, "maestro"),
		demand("mac-a", 2, 3*time.Minute, "maestro"),
	}
	plan := PlanTick(mixedProfileInput(cfg, demands, []domain.Instance{busyBuilder()}))

	spawns := spawnedKeys(plan)
	// builder 6 + maestro 4 = 10 vCPU: exactly one maestro fits, never two.
	if len(spawns) != 1 {
		t.Fatalf("spawns = %#v, want exactly one maestro beside the busy builder", spawns)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			t.Fatalf("profile coexistence must never drain: %#v", plan.Operations)
		}
		if op.Kind == OperationSpawn && op.Profile != "maestro" {
			t.Fatalf("unexpected spawn profile %q", op.Profile)
		}
	}
}

// TestSingleCohortPreservedByDefault proves the flag-off behavior is today's,
// byte-for-byte: the busy builder blocks the maestro and nothing is drained.
func TestSingleCohortPreservedByDefault(t *testing.T) {
	cfg := mixedProfileConfig()
	cfg.MixedProfileCohorts = false
	demands := []domain.Demand{demand("mac-a", 1, 3*time.Minute, "maestro")}
	plan := PlanTick(mixedProfileInput(cfg, demands, []domain.Instance{busyBuilder()}))
	if len(plan.Operations) != 0 {
		t.Fatalf("flag off must preserve the single-cohort wait: %#v", plan.Operations)
	}
}

// TestCoexistenceRespectsThePhysicalBound proves the relaxation admits only what
// the machine holds: with the builder and one maestro live (10 vCPU), a second
// maestro must wait no matter what the flag says.
func TestCoexistenceRespectsThePhysicalBound(t *testing.T) {
	cfg := mixedProfileConfig()
	liveMaestro := domain.Instance{ID: "trf-maestro-live", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	demands := []domain.Demand{demand("mac-a", 3, 3*time.Minute, "maestro")}
	plan := PlanTick(mixedProfileInput(cfg, demands, []domain.Instance{busyBuilder(), liveMaestro}))
	if spawns := spawnedKeys(plan); len(spawns) != 0 {
		t.Fatalf("10 vCPU are live on 10 physical cores; spawns = %#v", spawns)
	}
}

// TestCoexistenceRespectsMaxActive proves profile concurrency is untouched: a
// second builder is refused by MaxActive even when the flag is on and only one
// other profile is live.
func TestCoexistenceRespectsMaxActive(t *testing.T) {
	cfg := mixedProfileConfig()
	demands := []domain.Demand{demand("mac-a", 4, 3*time.Minute, "builder")}
	plan := PlanTick(mixedProfileInput(cfg, demands, []domain.Instance{busyBuilder()}))
	if spawns := spawnedKeys(plan); len(spawns) != 0 {
		t.Fatalf("MaxActive 1 must hold under coexistence; spawns = %#v", spawns)
	}
}

// TestCoexistenceNeverDrainsABusyForeignCohort proves drain safety survives: a
// head profile that does NOT fit beside a busy foreign cohort keeps waiting --
// coexistence never converts into an interruption.
func TestCoexistenceNeverDrainsABusyForeignCohort(t *testing.T) {
	cfg := mixedProfileConfig()
	// Two live maestros (8 vCPU) leave 2: the 6-vCPU builder cannot fit.
	live := []domain.Instance{
		{ID: "trf-maestro-1", Repo: "mac-a", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
			State: domain.InstanceRunning, Power: domain.InstancePowerRunning},
		{ID: "trf-maestro-2", Repo: "mac-a", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
			State: domain.InstanceRunning, Power: domain.InstancePowerRunning},
	}
	demands := []domain.Demand{demand("mac-a", 5, 3*time.Minute, "builder")}
	plan := PlanTick(mixedProfileInput(cfg, demands, live))
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			t.Fatalf("a busy cohort was drained to make room: %#v", plan.Operations)
		}
		if op.Kind == OperationSpawn {
			t.Fatalf("6 vCPU cannot fit beside 8 live; spawns = %#v", plan.Operations)
		}
	}
}

// TestCoexistenceStillSwitchesCohortsWhenForeignIsIdle proves the existing
// drain-and-switch path survives as the fallback: when the head does not fit
// beside an IDLE foreign cohort, the idle instances are drained and the head
// spawns behind them, exactly as before the flag existed.
func TestCoexistenceStillSwitchesCohortsWhenForeignIsIdle(t *testing.T) {
	cfg := mixedProfileConfig()
	idle := []domain.Instance{
		{ID: "trf-maestro-idle-1", Repo: "mac-a", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
			State: domain.InstanceOnlineIdle, Power: domain.InstancePowerRunning},
		{ID: "trf-maestro-idle-2", Repo: "mac-a", Platform: domain.PlatformMacOS, Profile: "maestro",
			Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
			State: domain.InstanceOnlineIdle, Power: domain.InstancePowerRunning},
	}
	demands := []domain.Demand{demand("mac-a", 6, 3*time.Minute, "builder")}
	plan := PlanTick(mixedProfileInput(cfg, demands, idle))
	drains, spawns := 0, 0
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			drains++
		}
		if op.Kind == OperationSpawn {
			spawns++
			if len(op.DependsOn) == 0 {
				t.Fatalf("switch spawn must depend on the drains: %#v", op)
			}
		}
	}
	if drains != 2 || spawns != 1 {
		t.Fatalf("drain-and-switch fallback broken: drains=%d spawns=%d ops=%#v", drains, spawns, plan.Operations)
	}
}
