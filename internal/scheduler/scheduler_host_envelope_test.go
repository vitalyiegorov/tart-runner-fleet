package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// incidentConfig mirrors the live Mac mini (Mac16,10: 10 cores, 24 GiB) config
// at the 2026-07-25 queue incident: an 8-vCPU/16-GiB shared envelope, a 7-vCPU
// macOS builder capped at one active VM, and mixed-platform admission enabled.
func incidentConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}
	cfg.FairnessAge = 5 * time.Minute
	cfg.MixedPlatformAdmission = true
	cfg.RepoCaps["mac-a"] = 2
	cfg.Profiles["builder"] = domain.Profile{ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder",
		Resources: domain.Resources{CPU: 7, MemoryMB: 12_288, Slots: 1}, MaxActive: 1}
	return cfg
}

func liveBuilder() domain.Instance {
	return domain.Instance{ID: "trf-builder-35917ac43a789b33", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "builder", Route: "macos-builder", Resources: domain.Resources{CPU: 7, MemoryMB: 12_288, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

func liveLinux(id, repo string, profile domain.ProfileID) domain.Instance {
	return domain.Instance{ID: id, Repo: repo, Platform: domain.PlatformLinux, Profile: profile,
		Route: incidentConfig().Profiles[profile].Route, Resources: incidentConfig().Profiles[profile].Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

// incidentInput replays the observed backlog: one live 7-vCPU builder, an aged
// macOS builder head that cannot grow its own cohort (MaxActive=1), aged
// maestro and large demands, and younger Linux work. The host observation is
// the one fleetd actually reported: 71% CPU idle, 11550 MiB available, and
// admission explicitly allowed.
func incidentInput(extra ...domain.Demand) Input {
	demands := []domain.Demand{
		demand("mac-a", 1, 37*time.Minute, "builder"),
		demand("mac-a", 2, 32*time.Minute, "maestro"),
		demand("a/repo", 3, 27*time.Minute, "large"),
		demand("a/repo", 4, 27*time.Minute, "large"),
	}
	in := input(append(demands, extra...), []domain.Instance{liveBuilder()}, State{})
	in.Config = incidentConfig()
	in.Host = domain.Fresh(domain.Host{
		Available: domain.Resources{CPU: 8, MemoryMB: 11_550, Slots: 4},
		Pressure: domain.HostPressure{AvailableMemoryMB: 11_550, FreeDiskGB: 152, SwapUsedMB: 574,
			CPUIdlePercent: 71.44, LoadAverage: 3.37, AdmissionAllowed: true, AdmissionReason: "capacity available"},
	}, testNow)
	return in
}

// TestExternallyBlockedReservationAdmitsFittingLinuxWork is the RED-first
// replay of the 2026-07-25 incident. A live 7-vCPU macOS builder leaves a
// residual of 1 vCPU / 4096 MiB inside the shared 8-vCPU/16-GiB envelope. The
// aged Linux head is a `large` (4 vCPU / 8192 MiB) which cannot fit that
// residual, so it reserves. A queued `small` (1 vCPU / 2048 MiB) fits the
// residual exactly.
//
// Today safeBackfill computes its remainder as free-minus-reservation, which
// underflows whenever the reservation itself does not fit, so it refuses ALL
// backfill and the planner emits zero operations for as long as the builder
// runs. The reserved large is blocked by the macOS builder, not by Linux
// capacity: it must wait for that VM to finish regardless, so admitting work
// that fits the residual now cannot delay it. The host is left 71% idle with a
// breached queue SLO while work that fits sits queued.
func TestExternallyBlockedReservationAdmitsFittingLinuxWork(t *testing.T) {
	fitting := demand("b/repo", 7, 3*time.Minute, "small")
	in := incidentInput(fitting)

	if free := linuxFree(in); !free.CanFit(in.Config.Profiles["small"].Resources) {
		t.Fatalf("precondition: the small must fit the residual envelope; free=%+v", free)
	}

	plan := PlanTick(in)
	if plan.Status != PlanReady {
		t.Fatalf("plan status = %s, want ready (host admission was allowed)", plan.Status)
	}
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{fitting.Key}) {
		t.Fatalf("spawns = %#v, want the fitting small %#v admitted into the stranded residual", got, fitting.Key)
	}
	for _, op := range plan.Operations {
		if op.Kind == OperationDrain {
			t.Fatalf("backfill beside a live macOS builder must never drain: %#v", plan.Operations)
		}
	}
	// The aged large keeps its reservation: backfill borrows only the residual.
	if plan.Next.Reservation == nil || plan.Next.Reservation.Profile != "large" {
		t.Fatalf("reservation = %#v, want the aged large to retain its reservation", plan.Next.Reservation)
	}
}

// TestExternallyBlockedBackfillStaysInsideResidual proves the relaxed backfill
// is still bounded by the exact residual envelope: it may use what the macOS
// builder left, never more.
func TestExternallyBlockedBackfillStaysInsideResidual(t *testing.T) {
	// Three smalls are queued but only one fits the 1-vCPU residual.
	in := incidentInput(
		demand("b/repo", 7, 3*time.Minute, "small"),
		demand("c/repo", 8, 3*time.Minute, "small"),
		demand("a/repo", 9, 3*time.Minute, "small"),
	)
	free := linuxFree(in)
	plan := PlanTick(in)

	used := domain.Resources{}
	for _, op := range plan.Operations {
		if op.Kind != OperationSpawn {
			continue
		}
		resources := in.Config.Profiles[op.Profile].Resources
		used = domain.Resources{CPU: used.CPU + resources.CPU, MemoryMB: used.MemoryMB + resources.MemoryMB,
			Slots: used.Slots + resources.Slots}
	}
	if !free.CanFit(used) {
		t.Fatalf("backfill used %+v, which exceeds the residual envelope %+v", used, free)
	}
	if used.CPU == 0 {
		t.Fatalf("backfill admitted nothing; want the residual put to work: %#v", plan.Operations)
	}
}

// TestLinuxBlockedReservationStillRefusesBackfill guards ADR 0005. When the
// reservation is blocked by live LINUX occupancy it would fit again as soon as
// that Linux work drains, so backfill must stay strictly inside
// free-minus-reservation and must not extend the reserved job's wait.
func TestLinuxBlockedReservationStillRefusesBackfill(t *testing.T) {
	// No macOS instance: the envelope is consumed by Linux mediums instead.
	in := input([]domain.Demand{
		demand("a/repo", 3, 27*time.Minute, "large"),
		demand("b/repo", 7, 3*time.Minute, "small"),
	}, []domain.Instance{
		liveLinux("trf-linux-1", "a/repo", "medium"),
		liveLinux("trf-linux-2", "b/repo", "medium"),
		liveLinux("trf-linux-3", "c/repo", "medium"),
	}, State{})
	in.Config = incidentConfig()
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 11_550, Slots: 4}}, testNow)

	plan := PlanTick(in)
	// free = 8-6 CPU / 16384-12288 MiB = (2, 4096); the large (4, 8192) is
	// blocked purely by Linux occupancy, so the strict remainder applies.
	if got := spawnedKeys(plan); len(got) != 0 {
		t.Fatalf("spawns = %#v, want none: a Linux-blocked reservation keeps the strict remainder", got)
	}
	if plan.Next.Reservation == nil || plan.Next.Reservation.Profile != "large" {
		t.Fatalf("reservation = %#v, want the aged large reserved", plan.Next.Reservation)
	}
}

// TestOversizedReservationKeepsStrictBackfill covers the misconfiguration
// guard: a reserved vector that does not fit even the bare configured envelope
// is contention-free by definition, so it must keep the strict remainder rather
// than be mistaken for foreign blocking and unlock an endless backfill stream.
func TestOversizedReservationKeepsStrictBackfill(t *testing.T) {
	in := input([]domain.Demand{
		demand("a/repo", 3, 27*time.Minute, "large"),
		demand("b/repo", 7, 3*time.Minute, "small"),
	}, nil, State{})
	in.Config = incidentConfig()
	// The large (4 vCPU / 8192 MiB) cannot fit this envelope at all.
	in.Config.LinuxCapacity = domain.Resources{CPU: 8, MemoryMB: 4_096, Slots: 4}
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 4_096, Slots: 4}}, testNow)

	if externallyBlocked(in, in.Config.Profiles["large"].Resources) {
		t.Fatal("a vector larger than the configured envelope must not count as externally blocked")
	}
	if got := spawnedKeys(PlanTick(in)); len(got) != 0 {
		t.Fatalf("spawns = %#v, want none: an oversized reservation keeps the strict remainder", got)
	}
}

// TestOversubscribedForeignCohortAdmitsNothing covers the envelope-underflow
// branch: if live macOS instances already exceed the configured envelope (a
// transient a config change can produce), the residual is empty and admission
// must stay closed rather than borrow capacity that does not exist.
func TestOversubscribedForeignCohortAdmitsNothing(t *testing.T) {
	maestro := domain.Instance{ID: "trf-macos-1", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	in := incidentInput(demand("b/repo", 7, 3*time.Minute, "small"))
	// builder (7 vCPU / 12288 MiB) + maestro (4 / 7168) exceeds 8 vCPU / 16 GiB.
	in.Instances = domain.Fresh([]domain.Instance{liveBuilder(), maestro}, testNow)

	if !externallyBlocked(in, in.Config.Profiles["large"].Resources) {
		t.Fatal("an over-subscribed foreign cohort must count as externally blocked")
	}
	if free := linuxFree(in); free != (domain.Resources{}) {
		t.Fatalf("residual = %+v, want empty when the foreign cohort exceeds the envelope", free)
	}
	if got := spawnedKeys(PlanTick(in)); len(got) != 0 {
		t.Fatalf("spawns = %#v, want none: an empty residual must admit nothing", got)
	}
}

// TestExternalBlockClearsRestoresStrictBackfill proves the relaxation is
// self-limiting rather than an unbounded stream: once the macOS builder is
// gone the reservation is no longer externally blocked, so the strict
// free-minus-reservation remainder governs again and no new work jumps ahead
// of the aged large.
func TestExternalBlockClearsRestoresStrictBackfill(t *testing.T) {
	reserved := demand("a/repo", 3, 27*time.Minute, "large")
	in := incidentInput(demand("b/repo", 7, 3*time.Minute, "small"))
	// The builder finished; one backfilled small is still in flight.
	in.Instances = domain.Fresh([]domain.Instance{liveLinux("trf-linux-1", "b/repo", "small")}, testNow)
	in.Prior = State{Reservation: &domain.Reservation{Demand: reserved.Key, Profile: "large",
		Resources: in.Config.Profiles["large"].Resources, Since: testNow.Add(-time.Minute)}}

	plan := PlanTick(in)
	// free = 8-1 CPU / 16384-2048 MiB = (7, 14336): the large now fits and must
	// take the capacity itself.
	spawns := spawnedKeys(plan)
	if len(spawns) == 0 || spawns[0] != reserved.Key {
		t.Fatalf("spawns = %#v, want the reserved large admitted first once its external blocker cleared", spawns)
	}
}
