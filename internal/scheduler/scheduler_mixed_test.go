package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// mixedConfig gives the host enough CPU headroom that two live maestros
// (8 CPU, 14 GiB together) still leave room for a couple of small Linux VMs.
// The default testConfig() is CPU-bound at two maestros (they exhaust the 8-CPU
// LinuxCapacity), which would hide platform coexistence behind a resource wall;
// this config isolates the scheduling decision from the resource wall.
func mixedConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}
	cfg.RepoCaps["mac-a"] = 2
	cfg.RepoCaps["mac-b"] = 2
	return cfg
}

func liveMaestro(id, repo string) domain.Instance {
	return domain.Instance{ID: id, Repo: repo, Platform: domain.PlatformMacOS, Profile: "maestro",
		Route: "macos-maestro", Resources: mixedConfig().Profiles["maestro"].Resources, State: domain.InstanceRunning,
		Power: domain.InstancePowerRunning}
}

// TestMixedAdmissionRedLinuxBesideLiveMaestros is the RED-first scenario for
// mixed-platform admission. Two maestros are live (a full macOS cohort) and a
// macOS builder is the aged priority head that cannot start beside the live
// maestros. A feasible young Linux backlog sits behind it with ample host
// headroom. Today (flag OFF) the platform-exclusive planner admits ZERO Linux:
// the macOS head takes the tick and the bounded handoff backfill admits only
// aged/control-plane smallest-tier work, so a young standard backlog waits.
// With mixed admission (flag ON) the Linux backlog fills the residual envelope
// beside the live maestros.
func TestMixedAdmissionRedLinuxBesideLiveMaestros(t *testing.T) {
	builderHead := demand("mac-a", 1, 20*time.Minute, "builder") // aged macOS head, cannot grow the cohort
	small1 := demand("a/repo", 2, time.Minute, "small")          // young, standard: ineligible for bounded backfill
	small2 := demand("b/repo", 3, time.Minute, "small")
	maestros := []domain.Instance{liveMaestro("maestro-1", "mac-b"), liveMaestro("maestro-2", "mac-b")}

	build := func(mixed bool) Plan {
		in := input([]domain.Demand{builderHead, small1, small2}, maestros, State{})
		in.Config = mixedConfig()
		in.Config.MixedPlatformAdmission = mixed
		in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}}, testNow)
		return PlanTick(in)
	}

	off := build(false)
	if got := spawnedKeys(off); len(got) != 0 {
		t.Fatalf("flag OFF must document today's behavior (no Linux beside a live macOS cohort under a macOS head); got spawns %#v", got)
	}

	on := build(true)
	onSpawns := spawnedKeys(on)
	if len(onSpawns) == 0 {
		t.Fatalf("flag ON must admit Linux beside the live maestros; got no spawns: %#v", on.Operations)
	}
	for _, op := range on.Operations {
		if op.Kind == OperationSpawn && op.Profile == "builder" {
			t.Fatalf("infeasible builder head must not spawn beside the maestro cohort: %#v", on.Operations)
		}
		if op.Kind == OperationDrain {
			t.Fatalf("mixed admission must not drain to make room beside live macOS: %#v", on.Operations)
		}
	}
	// Both feasible young smalls fit the residual envelope (12-8=4 CPU, 24-14=10 GiB).
	if !reflect.DeepEqual(onSpawns, []domain.DemandKey{small1.Key, small2.Key}) {
		t.Fatalf("mixed admission = %#v, want both feasible smalls beside the maestros", onSpawns)
	}
}

func mixedInput(t *testing.T, demands []domain.Demand, instances []domain.Instance, prior State) Input {
	t.Helper()
	in := input(demands, instances, prior)
	in.Config = mixedConfig()
	in.Config.MixedPlatformAdmission = true
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}}, testNow)
	return in
}

// TestMixedIdleMacHeadSpawnsThenFillsLinux covers the containsSpawn leaf: on an
// idle host a macOS head that DOES fit spawns its cohort, and Linux then fills
// the residual envelope in the same tick (no drains).
func TestMixedIdleMacHeadSpawnsThenFillsLinux(t *testing.T) {
	m1 := demand("mac-a", 1, 10*time.Minute, "maestro")
	m2 := demand("mac-a", 2, 9*time.Minute, "maestro")
	s1 := demand("a/repo", 3, time.Minute, "small")
	s2 := demand("b/repo", 4, time.Minute, "small")
	plan := PlanTick(mixedInput(t, []domain.Demand{m1, m2, s1, s2}, nil, State{}))
	if containsDrain(plan.Operations) {
		t.Fatalf("idle mac head + linux fill must not drain: %#v", plan.Operations)
	}
	got := spawnedKeys(plan)
	if !reflect.DeepEqual(got, []domain.DemandKey{m1.Key, m2.Key, s1.Key, s2.Key}) {
		t.Fatalf("mac cohort + linux fill = %#v", got)
	}
}

// TestMixedFillBesideLiveLinux covers both remainder-fill leaves next to a
// live Linux VM: a macOS head spawning beside it with Linux filling the rest
// (mode HostLinux), and a Linux head establishing a macOS cohort within
// MaxActive (fillMacRemainder's active=false branch).
func TestMixedFillBesideLiveLinux(t *testing.T) {
	tests := []struct {
		name string
		head domain.Demand
		next domain.Demand
	}{
		{name: "mac head fills linux", head: demand("mac-a", 1, 10*time.Minute, "maestro"), next: demand("a/repo", 2, time.Minute, "small")},
		{name: "linux head establishes mac cohort", head: demand("a/repo", 1, 10*time.Minute, "small"), next: demand("mac-a", 2, time.Minute, "maestro")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			liveLinux := domain.Instance{ID: "linux-1", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "small",
				Route: "tiered", Resources: mixedConfig().Profiles["small"].Resources, State: domain.InstanceRunning}
			plan := PlanTick(mixedInput(t, []domain.Demand{test.head, test.next}, []domain.Instance{liveLinux}, State{}))
			if containsDrain(plan.Operations) {
				t.Fatalf("remainder fill must not drain: %#v", plan.Operations)
			}
			if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{test.head.Key, test.next.Key}) {
				t.Fatalf("head + remainder fill = %#v", got)
			}
		})
	}
}

// TestMixedLinuxHeadGrowsActiveMacProfile covers fillMacRemainder's active=true,
// canGrow branch (mode HostMixed): a second maestro joins the live one beside a
// Linux head.
func TestMixedLinuxHeadGrowsActiveMacProfile(t *testing.T) {
	smallHead := demand("a/repo", 1, 10*time.Minute, "small")
	maestro := demand("mac-a", 2, time.Minute, "maestro")
	liveMaestro := liveMaestro("maestro-1", "mac-b")
	liveLinux := domain.Instance{ID: "linux-1", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "small",
		Route: "tiered", Resources: mixedConfig().Profiles["small"].Resources, State: domain.InstanceRunning}
	plan := PlanTick(mixedInput(t, []domain.Demand{smallHead, maestro}, []domain.Instance{liveMaestro, liveLinux}, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{smallHead.Key, maestro.Key}) {
		t.Fatalf("linux head + grown maestro cohort = %#v", got)
	}
}

// TestMixedLinuxHeadRespectsMacMaxActive covers fillMacRemainder's !canGrow
// return: with the maestro cohort already at MaxActive, no third maestro is
// admitted beside the Linux head.
func TestMixedLinuxHeadRespectsMacMaxActive(t *testing.T) {
	smallHead := demand("a/repo", 1, 10*time.Minute, "small")
	maestro := demand("mac-a", 2, time.Minute, "maestro")
	live := []domain.Instance{liveMaestro("maestro-1", "mac-b"), liveMaestro("maestro-2", "mac-b"),
		{ID: "linux-1", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
			Resources: mixedConfig().Profiles["small"].Resources, State: domain.InstanceRunning}}
	plan := PlanTick(mixedInput(t, []domain.Demand{smallHead, maestro}, live, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{smallHead.Key}) {
		t.Fatalf("maestro MaxActive breached beside linux head: %#v", got)
	}
}

// TestMixedLinuxHeadSkipsIncompatibleMacProfile covers fillMacRemainder's
// empty-profileDemands return: the growable live cohort is maestro but the only
// queued macOS work is a builder, so the single-cohort rule admits no macOS.
func TestMixedLinuxHeadSkipsIncompatibleMacProfile(t *testing.T) {
	smallHead := demand("a/repo", 1, 10*time.Minute, "small")
	builder := demand("mac-a", 2, time.Minute, "builder")
	live := []domain.Instance{liveMaestro("maestro-1", "mac-b"),
		{ID: "linux-1", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered",
			Resources: mixedConfig().Profiles["small"].Resources, State: domain.InstanceRunning}}
	plan := PlanTick(mixedInput(t, []domain.Demand{smallHead, builder}, live, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{smallHead.Key}) {
		t.Fatalf("incompatible mac profile admitted beside cohort: %#v", got)
	}
}

// TestMixedFillMacSkippedWhileReservationHeld covers the reservation guard: when
// the Linux planner reserves a vector for an aged repo-capped head, no macOS is
// admitted into the reserved remainder.
func TestMixedFillMacSkippedWhileReservationHeld(t *testing.T) {
	// The head is a `medium` (2 CPU / 4096 MB) its own repository's cap holds
	// out, so ADR 0038 lends its vector rather than withholding it. What keeps
	// the maestro (4 CPU / 7168 MB) out is therefore ADR 0017's no-jump rule and
	// nothing else: a maestro could take the head's whole vector, so admitting it
	// would let a younger demand jump the oldest one.
	reservedHead := demand("blocked", 1, 12*time.Minute, "medium")
	backfill := demand("a/repo", 2, time.Minute, "small")
	maestro := demand("mac-a", 3, time.Minute, "maestro")
	liveLarge := domain.Instance{ID: "large-1", Repo: "blocked", Platform: domain.PlatformLinux, Profile: "large",
		Route: "legacy", Resources: mixedConfig().Profiles["large"].Resources, State: domain.InstanceRunning}
	in := mixedInput(t, []domain.Demand{reservedHead, backfill, maestro}, []domain.Instance{liveLarge}, State{})
	in.Config.RepoCaps["blocked"] = 1 // the aged head is blocked by cap, so it reserves its vector
	plan := PlanTick(in)
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != reservedHead.Key {
		t.Fatalf("expected a held reservation for the aged head: %#v", plan.Next.Reservation)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationSpawn && op.Profile == "maestro" {
			t.Fatalf("macOS that could take the reserved head's whole vector admitted into the "+
				"remainder: %#v", plan.Operations)
		}
	}
	// The refusal above has to be the no-jump rule rather than an exhausted
	// envelope, or this test proves nothing: the Linux `small` the head outranks
	// is admitted on the very same tick, out of the very same lent vector.
	if !containsDemandKey(spawnedKeys(plan), backfill.Key) {
		t.Fatalf("a cap-held head lends its vector to work it outranks: %#v", plan.Operations)
	}
}

// TestMixedFillSkippedWhileDrainInFlight covers the drain guard: a mac->linux
// profile switch (idle mac drained, linux spawned in the same dependent plan)
// must not have a mac admission bolted on by the remainder pass.
func TestMixedFillSkippedWhileDrainInFlight(t *testing.T) {
	linuxHead := demand("a/repo", 1, 10*time.Minute, "small")
	maestro := demand("mac-a", 2, time.Minute, "maestro")
	idleMac := domain.Instance{ID: "mac-idle", Repo: "mac-b", Platform: domain.PlatformMacOS, Profile: "builder",
		Route: "macos-builder", Resources: testConfig().Profiles["builder"].Resources, State: domain.InstanceOnlineIdle}
	// Default 8-CPU envelope: the idle builder consumes it all, so the linux head
	// cannot fit beside it and a real mac->linux profile switch (drain) is forced.
	in := input([]domain.Demand{linuxHead, maestro}, []domain.Instance{idleMac}, State{})
	in.Config.MixedPlatformAdmission = true
	in.Config.RepoCaps["mac-a"] = 2
	plan := PlanTick(in)
	// The switch drains the idle builder and spawns the linux head; the remainder
	// pass must be a no-op while that drain is in flight.
	if !containsDrain(plan.Operations) {
		t.Fatalf("expected a profile-switch drain: %#v", plan.Operations)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationSpawn && op.Profile == "maestro" {
			t.Fatalf("mixed admission refilled during an in-flight switch: %#v", plan.Operations)
		}
	}
}

// TestAppendPlannedSpawnsSkipsNonSpawnOps exercises appendPlannedSpawns directly
// with a mixed spawn+drain operation slice, and pins containsDrain both ways.
func TestAppendPlannedSpawnsSkipsNonSpawnOps(t *testing.T) {
	cfg := mixedConfig()
	spawn := spawnOperation(demand("a/repo", 1, 0, "small"), nil)
	drain := drainOperation(domain.Instance{ID: "d", Profile: "small", Route: "tiered"})
	got := appendPlannedSpawns(nil, cfg, []Operation{spawn, drain})
	if len(got) != 1 || got[0].Profile != "small" || got[0].State != domain.InstancePlanned {
		t.Fatalf("appendPlannedSpawns should model only the spawn: %#v", got)
	}
	if !containsDrain([]Operation{spawn, drain}) || containsDrain([]Operation{spawn}) {
		t.Fatalf("containsDrain misclassified operations")
	}
}
