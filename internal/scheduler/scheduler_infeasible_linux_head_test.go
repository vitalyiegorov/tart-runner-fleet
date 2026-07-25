package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// 2026-07-25 incident, second half. A macOS builder wedged in `draining` (its
// deregister refused 60+ times because GitHub will not deregister a busy runner)
// kept holding 7 CPU / 12288 MB. The operator raised maxLinuxCpu 8 -> 10, which
// left 3 CPU / 4096 MB free — room for a `medium` (2 CPU / 4096 MB). Five medium
// and five large jobs were queued, the daemon was ready and ticking, and still
// NOTHING was admitted for ~45 minutes.
//
// The suppressor is head-of-line blocking inside planLinux, not PR #91's
// mixed-admission drain guard (the wedged instance is in `draining`, so the
// scheduler plans no drain operation at all and containsDrain is false). The
// aged FIFO head is a `large` (4 CPU / 8192 MB) that does not fit the residual
// envelope, so planLinux reserves its full vector and safeBackfill computes the
// remainder as free - reservation, which underflows and yields NO backfill. The
// whole residual envelope is then held idle behind a head that cannot start
// until live instances release resources it needs.
//
// This is the same distinction PR #78/#83 drew for a resource-infeasible macOS
// head ("nothing is live to drain and make room for it, so it is NOT waiting on
// drainable work"), left unhandled for the Linux head.

// incidentLinuxHeadConfig is the live topology: LinuxCapacity 10 CPU / 16384 MB
// (maxLinuxCpu raised to the M4's ten physical cores), the cpu:7 builder
// profile, and a host reporting 12288 MB available.
func incidentLinuxHeadConfig() (Config, domain.Host) {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 16_384, Slots: 4}
	cfg.MixedPlatformAdmission = true
	cfg.RepoCaps = map[string]int{"a/repo": 4, "b/repo": 4, "c/repo": 4, "mac-a": 2}
	builder := cfg.Profiles["builder"]
	builder.Resources = domain.Resources{CPU: 7, MemoryMB: 12_288, Slots: 1}
	cfg.Profiles["builder"] = builder
	return cfg, domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 12_288, Slots: 4}}
}

// wedgedBuilderInstance is the builder whose drain cannot complete: GitHub
// refuses to deregister it while it runs a brokered job, so it sits in
// `draining` while still consuming the host.
func wedgedBuilderInstance(cfg Config) domain.Instance {
	return domain.Instance{ID: "trf-builder-35917ac43a789b33", Repo: "mac-a", Platform: domain.PlatformMacOS,
		Profile: "builder", Route: cfg.Profiles["builder"].Route, Resources: cfg.Profiles["builder"].Resources,
		State: domain.InstanceDraining, Power: domain.InstancePowerRunning}
}

// incidentQueue builds the queued backlog: five large and five medium jobs,
// with the head profile's demands made the oldest so the caller controls which
// profile is the aged global-FIFO head.
func incidentQueue(cfg Config, headProfile domain.ProfileID) []domain.Demand {
	var demands []domain.Demand
	id := int64(0)
	add := func(repo string, age time.Duration, profile domain.ProfileID) {
		id++
		demands = append(demands, domain.Demand{
			Key: domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: id}, CreatedAt: testNow.Add(-age),
			Profile: profile, Route: cfg.Profiles[profile].Route, Platform: cfg.Profiles[profile].Platform,
			Event: domain.EventPullRequest})
	}
	for i := 0; i < 5; i++ {
		offset := time.Duration(i) * time.Minute
		largeAge, mediumAge := 30*time.Minute-offset, 45*time.Minute-offset
		if headProfile == "large" {
			largeAge, mediumAge = mediumAge, largeAge
		}
		add("a/repo", largeAge, "large")
		add("b/repo", mediumAge, "medium")
	}
	return demands
}

func incidentInput(cfg Config, host domain.Host, demands []domain.Demand, instances []domain.Instance, prior State) Input {
	return Input{Now: testNow, Config: cfg, Demands: domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow), Host: domain.Fresh(host, testNow), Prior: prior}
}

func spawnedProfiles(plan Plan) []domain.ProfileID {
	var profiles []domain.ProfileID
	for _, operation := range plan.Operations {
		if operation.Kind == OperationSpawn {
			profiles = append(profiles, operation.Profile)
		}
	}
	return profiles
}

// TestAgedInfeasibleLinuxHeadAdmitsFeasibleResidual is the incident replay plus
// its isolating control. Both cases share the identical wedged instance, host,
// capacity and queue; only which profile is the oldest demand differs. The
// medium-head case already admits work on current main, which proves the
// suppressor is the infeasible head's reservation and not the wedged drain.
func TestAgedInfeasibleLinuxHeadAdmitsFeasibleResidual(t *testing.T) {
	for _, test := range []struct {
		name string
		head domain.ProfileID
	}{
		{name: "aged large head does not fit the residual envelope", head: "large"},
		{name: "control: aged medium head fits and is admitted", head: "medium"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, host := incidentLinuxHeadConfig()
			instances := []domain.Instance{wedgedBuilderInstance(cfg)}
			in := incidentInput(cfg, host, incidentQueue(cfg, test.head), instances, State{})

			plan := PlanTick(in)

			free := linuxFree(in)
			if free != (domain.Resources{CPU: 3, MemoryMB: 4_096, Slots: 3}) {
				t.Fatalf("residual envelope drifted from the incident measurement: %#v", free)
			}
			if containsDrain(plan.Operations) {
				t.Fatal("the wedged instance is already draining, so no drain may be planned: " +
					"PR #91's mixed-admission drain guard cannot be the suppressor here")
			}
			profiles := spawnedProfiles(plan)
			if len(profiles) == 0 {
				t.Fatalf("starvation: a %s head froze admission while %#v was free — room for a medium; reservation=%#v",
					test.head, free, plan.Next.Reservation)
			}
			for _, profile := range profiles {
				if profile == "large" {
					t.Fatalf("a large (4 CPU / 8192 MB) cannot fit %#v; admitted %v", free, profiles)
				}
			}
		})
	}
}

// TestAgedInfeasibleLinuxHeadKeepsReservationAndPriority pins that residual
// admission behind an infeasible head does not weaken the reservation contract:
// the head stays reserved, and on the very next tick — once the wedged builder
// is gone and its vector fits — the head is admitted BEFORE any further
// backfill. Backfill may use stranded capacity; it may never take the head's
// turn.
func TestAgedInfeasibleLinuxHeadKeepsReservationAndPriority(t *testing.T) {
	cfg, host := incidentLinuxHeadConfig()
	demands := incidentQueue(cfg, "large")
	blocked := PlanTick(incidentInput(cfg, host, demands, []domain.Instance{wedgedBuilderInstance(cfg)}, State{}))

	reservation := blocked.Next.Reservation
	if reservation == nil || reservation.Profile != "large" {
		t.Fatalf("the infeasible head must keep its reservation, got %#v", reservation)
	}
	if !containsSpawn(blocked.Operations) {
		t.Fatalf("feasible residual work must be admitted behind the infeasible head, got %#v", blocked.Operations)
	}

	// The wedged drain finally completes: the builder is gone and the head fits.
	released := PlanTick(incidentInput(cfg, host, demands, nil, blocked.Next))
	admittedHead := false
	for _, operation := range released.Operations {
		if operation.Kind == OperationSpawn && operation.Demand == reservation.Demand {
			admittedHead = true
		}
	}
	if !admittedHead {
		t.Fatalf("the reserved head must win the first vector large enough for it, got %#v", released.Operations)
	}
	if released.Next.Reservation != nil {
		t.Fatalf("an admitted head must release its reservation, got %#v", released.Next.Reservation)
	}
}
