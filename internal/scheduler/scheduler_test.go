package scheduler

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var testNow = time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
		FairnessAge:   5 * time.Minute,
		RepoCaps:      map[string]int{"a/repo": 4, "b/repo": 4, "c/repo": 4},
		Profiles: map[domain.ProfileID]domain.Profile{
			"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
			"medium":  {ID: "medium", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}},
			"large":   {ID: "large", Platform: domain.PlatformLinux, Route: "legacy", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}},
			"builder": {ID: "builder", Platform: domain.PlatformMacOS, Route: "macos-builder", Resources: domain.Resources{CPU: 8, MemoryMB: 12_288, Slots: 1}, MaxActive: 1},
			"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
		},
	}
}

func input(demands []domain.Demand, instances []domain.Instance, prior State) Input {
	return Input{
		Now:       testNow,
		Config:    testConfig(),
		Demands:   domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, testNow),
		Prior:     prior,
	}
}

func demand(repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	return domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: jobID},
		CreatedAt: testNow.Add(-age),
		Profile:   profile,
		Route:     testConfig().Profiles[profile].Route,
		Platform:  testConfig().Profiles[profile].Platform,
		Event:     domain.EventPullRequest,
	}
}

func spawnedKeys(plan Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

func TestFailClosedOnStaleOrUnavailableObservation(t *testing.T) {
	cases := []Input{
		input([]domain.Demand{demand("a/repo", 1, time.Minute, "small")}, nil, State{}),
		input([]domain.Demand{demand("a/repo", 1, time.Minute, "small")}, nil, State{}),
	}
	cases[0].Demands = domain.Stale(cases[0].Demands.Value, testNow.Add(-time.Hour), "stale")
	cases[1].Instances = domain.Unavailable[[]domain.Instance]("unavailable")
	for _, candidate := range cases {
		plan := PlanTick(candidate)
		if len(plan.Operations) != 0 || plan.Status != PlanBlockedObservation {
			t.Fatalf("fail-closed plan = %#v", plan)
		}
	}
}

func TestStoppedAssignedRunnerIsRecoveredBeforeNewAdmission(t *testing.T) {
	stopped := domain.Instance{ID: "stopped", Repo: "a/repo", Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, State: domain.InstanceAssigned, Power: domain.InstancePowerStopped}
	plan := PlanTick(input([]domain.Demand{demand("b/repo", 9, time.Minute, "small")}, []domain.Instance{stopped}, State{}))
	if len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationDrain || plan.Operations[0].Instance != stopped.ID || !plan.Operations[0].Recovery {
		t.Fatalf("stopped-assignment recovery plan = %#v", plan)
	}
	running := stopped
	running.Power = domain.InstancePowerRunning
	if got := PlanTick(input(nil, []domain.Instance{running}, State{})); len(got.Operations) != 0 {
		t.Fatalf("running assignment was recovered: %#v", got)
	}
}

func TestRunningCompletedEphemeralRunnerRecoversOnlyWithFreshInactiveEvidence(t *testing.T) {
	instance := domain.Instance{ID: "completed", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}, State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
	queued := demand("b/repo", 9, time.Minute, "small")
	withoutEvidence := PlanTick(input([]domain.Demand{queued}, []domain.Instance{instance}, State{}))
	if len(withoutEvidence.Operations) != 1 || withoutEvidence.Operations[0].Kind != OperationSpawn {
		t.Fatalf("unconfirmed running instance recovery = %#v", withoutEvidence)
	}
	instance.RecoveryReady = true
	plan := PlanTick(input([]domain.Demand{queued}, []domain.Instance{instance}, State{}))
	if len(plan.Operations) != 1 || plan.Operations[0].Kind != OperationDrain || plan.Operations[0].Instance != instance.ID ||
		!plan.Operations[0].Recovery || !plan.Operations[0].ConfirmedInactive {
		t.Fatalf("confirmed inactive running recovery = %#v", plan)
	}
}

func TestStoppedDrainingOrphanCannotStarveUnrelatedCapacity(t *testing.T) {
	orphan := domain.Instance{ID: "orphan", Repo: "a/repo", Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, State: domain.InstanceDraining, Power: domain.InstancePowerStopped}
	queued := demand("b/repo", 9, time.Minute, "small")

	plan := PlanTick(input([]domain.Demand{queued}, []domain.Instance{orphan}, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{queued.Key}) {
		t.Fatalf("stopped draining orphan starved unrelated work: plan=%#v", plan)
	}
}

func TestQueuedSiblingsRemainDistinctWhenRunIsInProgress(t *testing.T) {
	first := demand("a/repo", 11, time.Minute, "small")
	second := demand("a/repo", 12, time.Minute, "small")
	first.RunStatus, second.RunStatus = domain.RunInProgress, domain.RunInProgress

	plan := PlanTick(input([]domain.Demand{first, second}, nil, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{first.Key, second.Key}) {
		t.Fatalf("spawned siblings = %#v", got)
	}
}

func TestAgedGlobalFIFOAllowsNoYoungerBypass(t *testing.T) {
	old := demand("a/repo", 1, 10*time.Minute, "large")
	young := demand("b/repo", 2, time.Minute, "small")
	live := []domain.Instance{{ID: "live", Repo: "c/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 2}, State: domain.InstanceRunning}}

	plan := PlanTick(input([]domain.Demand{young, old}, live, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{old.Key}) {
		t.Fatalf("aged FIFO spawned = %#v", got)
	}
}

func TestYoungWorkUsesDeterministicPerRepoFairBackfill(t *testing.T) {
	a1 := demand("a/repo", 1, time.Minute, "small")
	a2 := demand("a/repo", 2, 50*time.Second, "small")
	b1 := demand("b/repo", 3, 40*time.Second, "small")
	b2 := demand("b/repo", 4, 30*time.Second, "small")
	plan := PlanTick(input([]domain.Demand{a1, a2, b1, b2}, nil, State{}))
	want := []domain.DemandKey{a1.Key, b1.Key, a2.Key, b2.Key}
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("fair order = %#v, want %#v", got, want)
	}
	if again := PlanTick(input([]domain.Demand{b2, a2, b1, a1}, nil, State{})); !reflect.DeepEqual(plan, again) {
		t.Fatal("plan must be deterministic across input ordering")
	}
}

func TestAdmissionCountsOnlyFeasibleSelections(t *testing.T) {
	configInput := input([]domain.Demand{
		demand("a/repo", 1, 2*time.Minute, "large"),
		demand("a/repo", 2, time.Minute, "small"),
	}, nil, State{})
	configInput.Config.LinuxCapacity = domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}
	configInput.Config.RepoCaps["a/repo"] = 1

	plan := PlanTick(configInput)
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{{Repo: "a/repo", RunID: 100, Attempt: 1, JobID: 2}}) {
		t.Fatalf("feasible selection = %#v", got)
	}
}

func TestWrongRouteIdleDoesNotConsumeRepoCap(t *testing.T) {
	wanted := demand("a/repo", 1, time.Minute, "small")
	idleWrongRoute := domain.Instance{ID: "legacy", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "large", Route: "legacy", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}, State: domain.InstanceOnlineIdle}
	in := input([]domain.Demand{wanted}, []domain.Instance{idleWrongRoute}, State{})
	in.Config.RepoCaps["a/repo"] = 1
	plan := PlanTick(in)
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{wanted.Key}) {
		t.Fatalf("wrong-route idle blocked demand: %#v", plan)
	}
}

func TestIneligibleHeadCannotHideEligibleWork(t *testing.T) {
	blocked := demand("a/repo", 1, 2*time.Minute, "small")
	blocked.NotBefore = testNow.Add(time.Minute)
	ready := demand("b/repo", 2, time.Minute, "small")
	plan := PlanTick(input([]domain.Demand{blocked, ready}, nil, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{ready.Key}) {
		t.Fatalf("eligible tail hidden: %#v", plan)
	}
}

func TestPersistentExactReservationDrainsWithoutLosingIdentity(t *testing.T) {
	old := demand("a/repo", 1, 10*time.Minute, "large")
	live := []domain.Instance{{ID: "busy", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}, State: domain.InstanceRunning}}
	first := PlanTick(input([]domain.Demand{old}, live, State{}))
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != old.Key || first.Next.Reservation.Resources != testConfig().Profiles["large"].Resources {
		t.Fatalf("reservation = %#v", first.Next.Reservation)
	}
	if len(first.Operations) != 0 {
		t.Fatalf("reservation must drain: %#v", first.Operations)
	}
	second := PlanTick(input([]domain.Demand{old}, nil, first.Next))
	if got := spawnedKeys(second); !reflect.DeepEqual(got, []domain.DemandKey{old.Key}) || second.Next.Reservation != nil {
		t.Fatalf("reservation admission = %#v", second)
	}
}

func TestCurrentLiveResourcesAreAccounted(t *testing.T) {
	job := demand("a/repo", 1, time.Minute, "large")
	live := []domain.Instance{{ID: "busy", Repo: "b/repo", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}, State: domain.InstanceRunning}}
	if plan := PlanTick(input([]domain.Demand{job}, live, State{})); len(spawnedKeys(plan)) != 0 {
		t.Fatalf("oversubscribed live resources: %#v", plan)
	}
}

func TestLinuxAndMacOSNeverOverlap(t *testing.T) {
	linuxJob := demand("a/repo", 1, time.Minute, "small")
	mac := domain.Instance{ID: "mac", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "builder", Route: "macos-builder", Resources: testConfig().Profiles["builder"].Resources, State: domain.InstanceRunning}
	plan := PlanTick(input([]domain.Demand{linuxJob}, []domain.Instance{mac}, State{}))
	if len(spawnedKeys(plan)) != 0 {
		t.Fatalf("spawned Linux alongside macOS: %#v", plan)
	}
}

func TestBlockedLinuxHandoffFillsSecondMaestroSlotOnce(t *testing.T) {
	linux := demand("a/linux", 1, 10*time.Minute, "small")
	queuedMaestro := demand("b/mobile", 2, 5*time.Minute, "maestro")
	runningMaestro := domain.Instance{
		ID: "maestro-1", Repo: "c/mobile", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro", Resources: testConfig().Profiles["maestro"].Resources,
		State: domain.InstanceRunning,
	}
	in := input([]domain.Demand{linux, queuedMaestro}, []domain.Instance{runningMaestro}, State{})

	first := PlanTick(in)
	if got := spawnedKeys(first); !reflect.DeepEqual(got, []domain.DemandKey{queuedMaestro.Key}) {
		t.Fatalf("blocked Linux handoff left the second Maestro slot idle: %#v", first)
	}
	if first.Next.LinuxHandoff == nil || first.Next.LinuxHandoff.Demand != linux.Key || !first.Next.LinuxHandoff.BackfillAdmitted {
		t.Fatalf("bounded Linux handoff state = %#v", first.Next.LinuxHandoff)
	}

	in.Prior = first.Next
	second := PlanTick(in)
	if got := spawnedKeys(second); len(got) != 0 {
		t.Fatalf("Linux handoff admitted an unbounded Maestro stream: %#v", second)
	}
}

func TestLinuxHandoffBackfillWaitsForCapacityAndMatchesActiveProfile(t *testing.T) {
	linux := demand("a/repo", 1, 10*time.Minute, "small")
	maestro := demand("b/repo", 2, 5*time.Minute, "maestro")
	builder := demand("c/repo", 3, 4*time.Minute, "builder")
	running := domain.Instance{
		ID: "maestro-1", Repo: "c/repo", Platform: domain.PlatformMacOS,
		Profile: "maestro", Route: "macos-maestro", Resources: testConfig().Profiles["maestro"].Resources,
		State: domain.InstanceRunning,
	}

	in := input([]domain.Demand{linux, maestro, builder}, []domain.Instance{running}, State{})
	in.Host = domain.Fresh(domain.Host{Available: running.Resources}, testNow)
	blocked := PlanTick(in)
	if got := spawnedKeys(blocked); len(got) != 0 {
		t.Fatalf("host-exhausted backfill = %#v", blocked)
	}
	if blocked.Next.LinuxHandoff == nil || blocked.Next.LinuxHandoff.BackfillAdmitted {
		t.Fatalf("host-exhausted backfill consumed its one-shot budget: %#v", blocked.Next.LinuxHandoff)
	}

	in.Prior = blocked.Next
	in.Host = domain.Fresh(domain.Host{Available: testConfig().LinuxCapacity}, testNow)
	admitted := PlanTick(in)
	if got := spawnedKeys(admitted); !reflect.DeepEqual(got, []domain.DemandKey{maestro.Key}) {
		t.Fatalf("capacity recovery selected a cross-profile demand: %#v", admitted)
	}
}

func TestLinuxHandoffStateRoundTripsAndResetsForANewHead(t *testing.T) {
	first := demand("a/repo", 1, 10*time.Minute, "small")
	second := demand("b/repo", 2, 9*time.Minute, "small")
	want := State{LinuxHandoff: &LinuxHandoff{Demand: first.Key, Since: testNow, BackfillAdmitted: true}}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var decoded State
	if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(decoded, want) {
		t.Fatalf("durable Linux handoff = %#v, %v", decoded, err)
	}
	if got := linuxHandoffFor(decoded.LinuxHandoff, first, testNow.Add(time.Minute)); !reflect.DeepEqual(got, *want.LinuxHandoff) {
		t.Fatalf("same-head handoff changed = %#v", got)
	}
	if got := linuxHandoffFor(decoded.LinuxHandoff, second, testNow.Add(time.Minute)); got.Demand != second.Key || got.BackfillAdmitted || got.Since != testNow.Add(time.Minute) {
		t.Fatalf("new-head handoff did not reset = %#v", got)
	}
	if _, ok := activeMacProfile([]domain.Instance{
		{ID: "one", Platform: domain.PlatformMacOS, Profile: "maestro", State: domain.InstanceRunning},
		{ID: "two", Platform: domain.PlatformMacOS, Profile: "builder", State: domain.InstanceRunning},
	}); ok {
		t.Fatal("mixed active macOS profiles accepted")
	}
}

func TestOldestAgedMacProfileWinsWithoutPermanentBuilderPriority(t *testing.T) {
	maestro := demand("a/repo", 1, 10*time.Minute, "maestro")
	builder := demand("b/repo", 2, time.Minute, "builder")
	plan := PlanTick(input([]domain.Demand{builder, maestro}, nil, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{maestro.Key}) {
		t.Fatalf("mac profile arbitration = %#v", got)
	}
}

func TestMaestroIsCappedAtTwoAndFairAcrossRepos(t *testing.T) {
	a1 := demand("a/repo", 1, time.Minute, "maestro")
	a2 := demand("a/repo", 2, 50*time.Second, "maestro")
	b1 := demand("b/repo", 3, 40*time.Second, "maestro")
	plan := PlanTick(input([]domain.Demand{a1, a2, b1}, nil, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{a1.Key, b1.Key}) {
		t.Fatalf("maestro allocation = %#v", got)
	}
}

func TestProfileSwitchPlansDrainAndSpawnInSameTick(t *testing.T) {
	maestro := demand("a/repo", 1, 10*time.Minute, "maestro")
	idleBuilder := domain.Instance{ID: "builder-1", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "builder", Route: "macos-builder", Resources: testConfig().Profiles["builder"].Resources, State: domain.InstanceOnlineIdle}
	plan := PlanTick(input([]domain.Demand{maestro}, []domain.Instance{idleBuilder}, State{}))
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != OperationDrain || plan.Operations[1].Kind != OperationSpawn {
		t.Fatalf("switch operations = %#v", plan.Operations)
	}
	if len(plan.Operations[1].DependsOn) != 1 || plan.Operations[1].DependsOn[0] != plan.Operations[0].ID {
		t.Fatalf("spawn must depend on drain: %#v", plan.Operations)
	}
}

func TestPlanAndOperationIDsAreDeterministicAndDistinct(t *testing.T) {
	a := demand("a/repo", 1, time.Minute, "small")
	b := demand("b/repo", 2, time.Minute, "small")
	first := PlanTick(input([]domain.Demand{a, b}, nil, State{}))
	second := PlanTick(input([]domain.Demand{b, a}, nil, State{}))
	if first.ID == "" || first.ID != second.ID || !reflect.DeepEqual(first.Operations, second.Operations) {
		t.Fatalf("non-deterministic plans: %#v / %#v", first, second)
	}
	if first.Operations[0].ID == first.Operations[1].ID {
		t.Fatal("operation IDs must be distinct")
	}
}

func TestPlannerEdgePathsAndNormalization(t *testing.T) {
	valid := demand("a/repo", 1, time.Minute, "small")
	invalidKey := valid
	invalidKey.Key.JobID = 0
	unknownProfile := demand("a/repo", 2, time.Minute, "small")
	unknownProfile.Profile = "missing"
	wrongPlatform := demand("a/repo", 3, time.Minute, "small")
	wrongPlatform.Platform = domain.PlatformMacOS
	wrongRoute := demand("a/repo", 4, time.Minute, "small")
	wrongRoute.Route = "legacy"
	notReady := demand("a/repo", 5, time.Minute, "small")
	notReady.NotBefore = testNow.Add(time.Minute)
	in := input([]domain.Demand{valid, valid, invalidKey, unknownProfile, wrongPlatform, wrongRoute, notReady}, nil, State{})
	if got := normalizedDemands(in); !reflect.DeepEqual(got, []domain.Demand{valid}) {
		t.Fatalf("normalized = %#v", got)
	}

	empty := PlanTick(input(nil, nil, State{Reservation: &domain.Reservation{Demand: valid.Key}}))
	if empty.Next.Reservation != nil || len(empty.Operations) != 0 {
		t.Fatalf("empty plan = %#v", empty)
	}

	overlap := input([]domain.Demand{valid}, []domain.Instance{
		{ID: "linux", Platform: domain.PlatformLinux, State: domain.InstanceRunning},
		{ID: "mac", Platform: domain.PlatformMacOS, State: domain.InstanceRunning},
	}, State{})
	if got := PlanTick(overlap); got.Status != PlanInvalidObservation {
		t.Fatalf("overlap plan = %#v", got)
	}

	mac := domain.Instance{ID: "mac", Platform: domain.PlatformMacOS, State: domain.InstanceRunning}
	if got := PlanTick(input([]domain.Demand{valid}, []domain.Instance{mac}, State{})); len(got.Operations) != 0 {
		t.Fatalf("Linux should wait for macOS: %#v", got)
	}
}

func TestCompatibleIdleConsumesDemandAndClearsReservation(t *testing.T) {
	job := demand("a/repo", 1, time.Minute, "small")
	idle := domain.Instance{ID: "idle", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered", Resources: testConfig().Profiles["small"].Resources, State: domain.InstanceOnlineIdle}
	prior := State{Reservation: &domain.Reservation{Demand: job.Key, Profile: job.Profile, Resources: testConfig().Profiles["small"].Resources}}
	plan := PlanTick(input([]domain.Demand{job}, []domain.Instance{idle}, prior))
	if len(plan.Operations) != 0 || plan.Next.Reservation != nil {
		t.Fatalf("idle matching plan = %#v", plan)
	}
}

func TestExistingReservationIsCopiedWhileBlockedAndClearedWhenDemandDisappears(t *testing.T) {
	job := demand("a/repo", 1, 10*time.Minute, "large")
	reservation := &domain.Reservation{Demand: job.Key, Profile: job.Profile, Resources: testConfig().Profiles["large"].Resources, Since: testNow.Add(-time.Minute)}
	live := []domain.Instance{{ID: "busy", Repo: "b/repo", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}, State: domain.InstanceRunning}}
	blocked := PlanTick(input([]domain.Demand{job}, live, State{Reservation: reservation}))
	if blocked.Next.Reservation == reservation || !reflect.DeepEqual(blocked.Next.Reservation, reservation) {
		t.Fatalf("reservation was not value-copied: %#v", blocked.Next.Reservation)
	}
	other := demand("b/repo", 2, time.Minute, "small")
	cleared := PlanTick(input([]domain.Demand{other}, nil, State{Reservation: reservation}))
	if cleared.Next.Reservation != nil || len(spawnedKeys(cleared)) != 1 {
		t.Fatalf("stale reservation plan = %#v", cleared)
	}
	if copyReservation(nil) != nil {
		t.Fatal("nil reservation copy must remain nil")
	}
}

func TestLinuxResourceUnderflowFailsClosed(t *testing.T) {
	in := input(nil, []domain.Instance{{ID: "oversized", Platform: domain.PlatformLinux, Resources: domain.Resources{CPU: 9, MemoryMB: 20_000, Slots: 5}, State: domain.InstanceRunning}}, State{})
	if got := linuxFree(in); got != (domain.Resources{}) {
		t.Fatalf("underflow free resources = %#v", got)
	}
}

func TestFairOrderCursorEventPriorityAndHelpers(t *testing.T) {
	push := demand("a/repo", 1, 2*time.Minute, "small")
	push.Event = domain.EventPush
	pr := demand("a/repo", 2, time.Minute, "small")
	b := demand("b/repo", 3, time.Minute, "small")
	ordered := fairOrder([]domain.Demand{push, pr, b}, "a/repo")
	want := []domain.Demand{b, pr, push}
	if !reflect.DeepEqual(ordered, want) {
		t.Fatalf("cursor/event order = %#v", ordered)
	}
	if got := fairOrder(nil, ""); got != nil {
		t.Fatalf("empty fair order = %#v", got)
	}
	if eventRank(domain.EventSchedule) != 1 {
		t.Fatal("non-PR event rank changed")
	}
}

func TestExactSelectionHonorsCapsResourcesAndLexicographicPreference(t *testing.T) {
	config := testConfig()
	config.RepoCaps["a/repo"] = 1
	candidates := []domain.Demand{
		demand("a/repo", 1, time.Minute, "small"),
		demand("a/repo", 2, time.Minute, "small"),
		demand("b/repo", 3, time.Minute, "large"),
		demand("c/repo", 4, time.Minute, "small"),
	}
	selected := exactSelect(candidates, domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 2}, map[string]int{}, config)
	if got := []int64{selected[0].Key.JobID, selected[1].Key.JobID}; !reflect.DeepEqual(got, []int64{1, 4}) {
		t.Fatalf("exact selected = %#v", got)
	}
	if betterSelection([]int{1}, []int{0}) || !betterSelection([]int{0}, []int{1}) || betterSelection([]int{0}, []int{0}) {
		t.Fatal("lexicographic selection preference changed")
	}
	delete(config.RepoCaps, "c/repo")
	if got := exactSelect([]domain.Demand{candidates[3]}, domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}, nil, config); len(got) != 1 {
		t.Fatalf("default exact-selection cap rejected work: %#v", got)
	}
}

func TestFeasibilityDefaultsCapAndCountsOnlySelectedRepo(t *testing.T) {
	resource := domain.Resources{CPU: 1, MemoryMB: 1, Slots: 1}
	free := domain.Resources{CPU: 2, MemoryMB: 2, Slots: 2}
	selected := []domain.Demand{demand("b/repo", 1, time.Minute, "small")}
	if !feasible(resource, free, "a/repo", nil, selected, nil) {
		t.Fatal("default cap should admit first repo allocation")
	}
	selected = append(selected, demand("a/repo", 2, time.Minute, "small"))
	if feasible(resource, free, "a/repo", nil, selected, nil) {
		t.Fatal("default cap should reject second repo allocation")
	}
	if feasible(domain.Resources{CPU: 3, MemoryMB: 1, Slots: 1}, free, "a/repo", nil, nil, nil) {
		t.Fatal("oversized request admitted")
	}
	drain := Operation{Kind: OperationDrain}
	spawn := Operation{Kind: OperationSpawn, Demand: demand("a/repo", 3, time.Minute, "small").Key}
	if got := spawnedDemands([]Operation{drain, spawn}); len(got) != 1 || got[0].Key != spawn.Demand {
		t.Fatalf("spawn filtering = %#v", got)
	}
}

func TestMacHandoffDrainsIdleLinuxAndSpawnsSameTick(t *testing.T) {
	macJob := demand("a/repo", 1, time.Minute, "builder")
	idleLinux := domain.Instance{ID: "z-idle", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered", State: domain.InstanceOnlineIdle}
	terminal := domain.Instance{ID: "done", Platform: domain.PlatformLinux, State: domain.InstanceDeleted}
	plan := PlanTick(input([]domain.Demand{macJob}, []domain.Instance{idleLinux, terminal}, State{}))
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != OperationDrain || plan.Operations[1].Kind != OperationSpawn {
		t.Fatalf("idle handoff = %#v", plan)
	}
	if !reflect.DeepEqual(plan.Operations[1].DependsOn, []string{plan.Operations[0].ID}) {
		t.Fatalf("handoff dependency = %#v", plan.Operations)
	}

	busyLinux := idleLinux
	busyLinux.State = domain.InstanceRunning
	blocked := PlanTick(input([]domain.Demand{macJob}, []domain.Instance{busyLinux}, State{}))
	if len(spawnedKeys(blocked)) != 0 {
		t.Fatalf("busy Linux overlap = %#v", blocked)
	}
}

func TestBlockedMacHandoffBackfillsOnlyOneAgedSmallestTier(t *testing.T) {
	macJob := demand("a/repo", 1, 20*time.Minute, "builder")
	agedLarge := demand("b/repo", 2, 15*time.Minute, "large")
	agedSmall := demand("b/repo", 3, 10*time.Minute, "small")
	secondSmall := demand("c/repo", 4, 9*time.Minute, "small")
	youngSmall := demand("c/repo", 5, time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}

	plan := PlanTick(input([]domain.Demand{youngSmall, secondSmall, agedSmall, agedLarge, macJob}, []domain.Instance{holder}, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{agedSmall.Key}) {
		t.Fatalf("bounded drain backfill = %#v, want %#v", got, agedSmall.Key)
	}
	if plan.Next.MacHandoff == nil || plan.Next.MacHandoff.Demand != macJob.Key || !plan.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("durable mac handoff = %#v", plan.Next.MacHandoff)
	}
	for _, operation := range plan.Operations {
		if operation.Kind == OperationSpawn && operation.Profile == "builder" {
			t.Fatalf("macOS overlapped Linux drain: %#v", plan)
		}
	}
}

func TestBlockedMacHandoffBackfillIsMonotonicAcrossTicks(t *testing.T) {
	macJob := demand("a/repo", 1, 20*time.Minute, "builder")
	firstSmall := demand("b/repo", 2, 10*time.Minute, "small")
	laterSmall := demand("c/repo", 3, 9*time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}

	first := PlanTick(input([]domain.Demand{macJob, firstSmall}, []domain.Instance{holder}, State{}))
	secondInput := input([]domain.Demand{macJob, firstSmall, laterSmall}, []domain.Instance{holder}, first.Next)
	second := PlanTick(secondInput)
	if got := spawnedKeys(second); len(got) != 0 {
		t.Fatalf("repeated handoff backfill = %#v", got)
	}
	if second.Next.MacHandoff == nil || !second.Next.MacHandoff.BackfillAdmitted || second.Next.MacHandoff.Since != first.Next.MacHandoff.Since {
		t.Fatalf("handoff budget reset: first=%#v second=%#v", first.Next.MacHandoff, second.Next.MacHandoff)
	}

	cleared := PlanTick(input([]domain.Demand{macJob, laterSmall}, nil, second.Next))
	if got := spawnedKeys(cleared); !reflect.DeepEqual(got, []domain.DemandKey{macJob.Key}) || cleared.Next.MacHandoff != nil {
		t.Fatalf("completed handoff = %#v", cleared)
	}
}

func TestMacHandoffSurvivesTemporaryOlderLinuxSelection(t *testing.T) {
	macJob := demand("mac/repo", 1, 20*time.Minute, "builder")
	olderLinux := demand("linux/repo", 2, 30*time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "holder/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}
	prior := State{MacHandoff: &MacHandoff{Demand: macJob.Key, Profile: macJob.Profile, Since: testNow.Add(-time.Minute), BackfillAdmitted: true}}

	linuxTick := PlanTick(input([]domain.Demand{macJob, olderLinux}, []domain.Instance{holder}, prior))
	if linuxTick.Next.MacHandoff == nil || !reflect.DeepEqual(linuxTick.Next.MacHandoff, prior.MacHandoff) {
		t.Fatalf("temporary Linux selection lost handoff: %#v", linuxTick.Next.MacHandoff)
	}
	macTick := PlanTick(input([]domain.Demand{macJob}, []domain.Instance{holder}, linuxTick.Next))
	if got := spawnedKeys(macTick); len(got) != 0 || macTick.Next.MacHandoff == nil || !macTick.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("preserved handoff admitted a second backfill: %#v", macTick)
	}
}

func TestBlockedMacHandoffBackfillsYoungControlPlaneNotYoungStandard(t *testing.T) {
	macJob := demand("mac/repo", 1, 10*time.Minute, "builder")
	standard := demand("standard/repo", 2, time.Minute, "small")
	control := demand("control/repo", 3, time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "holder/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}

	standardInput := input([]domain.Demand{macJob, standard}, []domain.Instance{holder}, State{})
	if got := spawnedKeys(PlanTick(standardInput)); len(got) != 0 {
		t.Fatalf("young standard drain work admitted: %#v", got)
	}
	controlInput := input([]domain.Demand{macJob, control}, []domain.Instance{holder}, State{})
	controlInput.Config.RepoSchedulingClasses = map[string]domain.SchedulingClass{control.Key.Repo: domain.SchedulingControlPlane}
	if got := spawnedKeys(PlanTick(controlInput)); !reflect.DeepEqual(got, []domain.DemandKey{control.Key}) {
		t.Fatalf("young control-plane drain work = %#v", got)
	}
}

func TestMacHandoffHeadUsesAuthoritativePriorityAcrossPermutations(t *testing.T) {
	standard := demand("standard/repo", 1, 2*time.Minute, "builder")
	control := demand("control/repo", 2, time.Minute, "builder")
	holder := domain.Instance{ID: "holder", Repo: "holder/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}

	var want *MacHandoff
	for _, demands := range [][]domain.Demand{{standard, control}, {control, standard}} {
		in := input(demands, []domain.Instance{holder}, State{})
		in.Config.RepoSchedulingClasses = map[string]domain.SchedulingClass{control.Key.Repo: domain.SchedulingControlPlane}
		got := PlanTick(in).Next.MacHandoff
		if got == nil || got.Demand != control.Key || got.Profile != control.Profile {
			t.Fatalf("priority head = %#v, want %s/%s", got, control.Key.String(), control.Profile)
		}
		if want != nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation changed handoff: first=%#v next=%#v", want, got)
		}
		want = got
	}
}

func TestMacHandoffStateHelpersRejectMissingAndMismatchedHeads(t *testing.T) {
	first := demand("a/repo", 1, time.Minute, "builder")
	second := demand("b/repo", 2, time.Minute, "builder")
	prior := &MacHandoff{Demand: first.Key, Profile: first.Profile, Since: testNow, BackfillAdmitted: true}
	if got := retainedMacHandoff(nil, []domain.Demand{first}); got != nil {
		t.Fatalf("nil handoff retained: %#v", got)
	}
	if got := retainedMacHandoff(prior, []domain.Demand{second}); got != nil {
		t.Fatalf("missing handoff retained: %#v", got)
	}
	if got := stableMacHandoffOrder(nil, []domain.Demand{first, second}); !reflect.DeepEqual(got, []domain.Demand{first, second}) {
		t.Fatalf("nil handoff reordered demands: %#v", got)
	}
	if got := stableMacHandoffOrder(prior, []domain.Demand{first, second}); !reflect.DeepEqual(got, []domain.Demand{first, second}) {
		t.Fatalf("head handoff reordered demands: %#v", got)
	}
	if got := stableMacHandoffOrder(prior, []domain.Demand{second, first}); !reflect.DeepEqual(got, []domain.Demand{first, second}) {
		t.Fatalf("tracked handoff was not promoted: %#v", got)
	}
	missing := &MacHandoff{Demand: demand("c/repo", 3, time.Minute, "builder").Key, Profile: "builder"}
	if got := stableMacHandoffOrder(missing, []domain.Demand{first, second}); !reflect.DeepEqual(got, []domain.Demand{first, second}) {
		t.Fatalf("missing handoff reordered demands: %#v", got)
	}
}

func TestBlockedMacHandoffBackfillRequiresObservedHostCapacity(t *testing.T) {
	macJob := demand("a/repo", 1, 20*time.Minute, "builder")
	small := demand("b/repo", 2, 10*time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}
	in := input([]domain.Demand{macJob, small}, []domain.Instance{holder}, State{})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 0, MemoryMB: 0, Slots: 0}}, testNow)
	if plan := PlanTick(in); len(spawnedKeys(plan)) != 0 || plan.Next.MacHandoff == nil || plan.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("host-exhausted drain backfill = %#v", plan)
	}
}

func TestMacHandoffBudgetResetsOnlyForDifferentSelectedDemand(t *testing.T) {
	oldMac := demand("a/repo", 1, 20*time.Minute, "builder")
	newMac := demand("a/repo", 2, 19*time.Minute, "builder")
	small := demand("b/repo", 3, 10*time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}
	prior := State{MacHandoff: &MacHandoff{Demand: oldMac.Key, Profile: oldMac.Profile, Since: testNow.Add(-time.Minute), BackfillAdmitted: true}}

	plan := PlanTick(input([]domain.Demand{newMac, small}, []domain.Instance{holder}, prior))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{small.Key}) {
		t.Fatalf("replacement handoff backfill = %#v", got)
	}
	if plan.Next.MacHandoff == nil || plan.Next.MacHandoff.Demand != newMac.Key || plan.Next.MacHandoff.Since != testNow {
		t.Fatalf("replacement handoff state = %#v", plan.Next.MacHandoff)
	}
}

func TestBlockedMacHandoffDoesNotBackfillYoungOrRepoCappedWork(t *testing.T) {
	macJob := demand("a/repo", 1, 20*time.Minute, "builder")
	young := demand("b/repo", 2, time.Minute, "small")
	holder := domain.Instance{ID: "holder", Repo: "b/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "tiered",
		Resources: testConfig().Profiles["medium"].Resources, State: domain.InstanceRunning}

	if plan := PlanTick(input([]domain.Demand{macJob, young}, []domain.Instance{holder}, State{})); len(spawnedKeys(plan)) != 0 {
		t.Fatalf("young drain work admitted: %#v", plan)
	}
	aged := young
	aged.CreatedAt = testNow.Add(-10 * time.Minute)
	in := input([]domain.Demand{macJob, aged}, []domain.Instance{holder}, State{})
	in.Config.RepoCaps[aged.Key.Repo] = 1
	if plan := PlanTick(in); len(spawnedKeys(plan)) != 0 {
		t.Fatalf("repo-capped drain work admitted: %#v", plan)
	}
}

func TestSmallestLinuxResourcesIsDeterministic(t *testing.T) {
	profiles := testConfig().Profiles
	want := profiles["small"].Resources
	for index := 0; index < 20; index++ {
		if got, ok := smallestLinuxResources(profiles); !ok || got != want {
			t.Fatalf("smallest resources = %#v, %t; want %#v", got, ok, want)
		}
	}
	if _, ok := smallestLinuxResources(map[domain.ProfileID]domain.Profile{"builder": profiles["builder"]}); ok {
		t.Fatal("mac-only profiles produced a Linux backfill tier")
	}
	if got := boundedDrainBackfill(Input{Config: Config{}}, nil); got != nil {
		t.Fatalf("backfill without a Linux profile = %#v", got)
	}
}

func TestProfileLessUsesTheCompleteDeterministicTuple(t *testing.T) {
	profile := func(id domain.ProfileID, cpu, memory, slots int) domain.Profile {
		return domain.Profile{ID: id, Resources: domain.Resources{CPU: cpu, MemoryMB: memory, Slots: slots}}
	}
	tests := []struct {
		name string
		a    domain.Profile
		b    domain.Profile
		want bool
	}{
		{name: "cpu", a: profile("z", 1, 9, 9), b: profile("a", 2, 1, 1), want: true},
		{name: "memory", a: profile("z", 1, 1, 9), b: profile("a", 1, 2, 1), want: true},
		{name: "memory greater", a: profile("a", 1, 2, 1), b: profile("z", 1, 1, 9), want: false},
		{name: "slots", a: profile("z", 1, 1, 1), b: profile("a", 1, 1, 2), want: true},
		{name: "slots greater", a: profile("a", 1, 1, 2), b: profile("z", 1, 1, 1), want: false},
		{name: "id", a: profile("a", 1, 1, 1), b: profile("b", 1, 1, 1), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := profileLess(test.a, test.b); got != test.want {
				t.Fatalf("profileLess(%#v, %#v) = %t, want %t", test.a, test.b, got, test.want)
			}
		})
	}
}

func TestMacProfileBusySwitchWaitsAndActiveProfileCaps(t *testing.T) {
	maestro := demand("a/repo", 1, time.Minute, "maestro")
	busyBuilder := domain.Instance{ID: "builder", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "builder", State: domain.InstanceRunning}
	if plan := PlanTick(input([]domain.Demand{maestro}, []domain.Instance{busyBuilder}, State{})); len(plan.Operations) != 0 {
		t.Fatalf("busy profile switched: %#v", plan)
	}

	builder := demand("a/repo", 2, time.Minute, "builder")
	activeBuilder := domain.Instance{ID: "active", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "builder", State: domain.InstanceOnlineIdle}
	if plan := PlanTick(input([]domain.Demand{builder}, []domain.Instance{activeBuilder}, State{})); len(plan.Operations) != 0 {
		t.Fatalf("builder maxActive exceeded: %#v", plan)
	}

	if got := appendMacSpawns(input(nil, nil, State{}), Plan{}, nil, nil); len(got.Operations) != 0 {
		t.Fatalf("empty mac append = %#v", got)
	}

	maestroInput := input([]domain.Demand{maestro}, nil, State{})
	maestroProfile := maestroInput.Config.Profiles["maestro"]
	maestroProfile.MaxActive = 3
	maestroInput.Config.Profiles["maestro"] = maestroProfile
	if got := appendMacSpawns(maestroInput, Plan{}, []domain.Demand{maestro}, nil); len(got.Operations) != 1 {
		t.Fatalf("maestro hard cap path = %#v", got)
	}
	builderInput := input([]domain.Demand{builder}, nil, State{})
	builderProfile := builderInput.Config.Profiles["builder"]
	builderProfile.MaxActive = 0
	builderInput.Config.Profiles["builder"] = builderProfile
	if got := appendMacSpawns(builderInput, Plan{}, []domain.Demand{builder}, nil); len(got.Operations) != 1 {
		t.Fatalf("default mac cap path = %#v", got)
	}
}

func TestNoStarvationAfterAgePromotion(t *testing.T) {
	scheduled := demand("a/repo", 1, 6*time.Minute, "small")
	scheduled.Event = domain.EventSchedule
	var flood []domain.Demand
	for id := int64(2); id <= 8; id++ {
		flood = append(flood, demand("b/repo", id, time.Minute, "small"))
	}
	plan := PlanTick(input(append(flood, scheduled), nil, State{}))
	if got := spawnedKeys(plan); len(got) == 0 || got[0] != scheduled.Key {
		t.Fatalf("aged scheduled job starved: %#v", got)
	}
}

func TestLinuxExactEnumerationIsBoundedAtFourSlots(t *testing.T) {
	var jobs []domain.Demand
	for id := int64(1); id <= 8; id++ {
		jobs = append(jobs, demand("a/repo", id, time.Minute, "small"))
	}
	in := input(jobs, nil, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 8}
	in.Config.RepoCaps["a/repo"] = 8
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
	if got := len(spawnedKeys(PlanTick(in))); got != 4 {
		t.Fatalf("spawned %d Linux VMs, want hard bound 4", got)
	}
}

func TestPersistentReservationAllowsOnlyVectorSafeBackfill(t *testing.T) {
	reserved := demand("a/repo", 1, 10*time.Minute, "large")
	backfill := demand("b/repo", 2, time.Minute, "small")
	live := []domain.Instance{{
		ID: "a-running", Repo: "a/repo", Platform: domain.PlatformLinux,
		Profile: "small", Route: "tiered", Resources: testConfig().Profiles["small"].Resources,
		State: domain.InstanceRunning,
	}}
	in := input([]domain.Demand{reserved, backfill}, live, State{})
	in.Config.RepoCaps["a/repo"] = 1
	first := PlanTick(in)
	if first.Next.Reservation == nil || first.Next.Reservation.Demand != reserved.Key {
		t.Fatalf("missing exact reservation: %#v", first)
	}
	if got := spawnedKeys(first); !reflect.DeepEqual(got, []domain.DemandKey{backfill.Key}) {
		t.Fatalf("safe backfill = %#v", got)
	}
	remaining := domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 2}
	if !remaining.CanFit(first.Next.Reservation.Resources) {
		t.Fatalf("backfill consumed reserved vector: %#v", first)
	}
}

func TestReservationAfterAgedAdmissionBackfillsWithUpdatedRepoCounts(t *testing.T) {
	firstAged := demand("a/repo", 1, 12*time.Minute, "small")
	reserved := demand("a/repo", 2, 10*time.Minute, "large")
	backfill := demand("b/repo", 3, time.Minute, "small")
	in := input([]domain.Demand{firstAged, reserved, backfill}, nil, State{})
	in.Config.RepoCaps["a/repo"] = 1
	plan := PlanTick(in)
	if plan.Next.Reservation == nil || plan.Next.Reservation.Demand != reserved.Key {
		t.Fatalf("head reservation = %#v", plan)
	}
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{firstAged.Key, backfill.Key}) {
		t.Fatalf("admit/reserve/backfill order = %#v", got)
	}
}

func TestPersistentReservationBackfillRotatesAcrossRepos(t *testing.T) {
	reserved := demand("a/repo", 1, 10*time.Minute, "large")
	b1 := demand("b/repo", 2, time.Minute, "small")
	c1 := demand("c/repo", 3, time.Minute, "small")
	live := []domain.Instance{{ID: "a-running", Repo: "a/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "tiered", Resources: testConfig().Profiles["small"].Resources, State: domain.InstanceRunning}}
	in := input([]domain.Demand{reserved, b1, c1}, live, State{})
	in.Config.LinuxCapacity = domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}
	in.Config.RepoCaps["a/repo"] = 1
	in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
	first := PlanTick(in)
	if got := spawnedKeys(first); !reflect.DeepEqual(got, []domain.DemandKey{b1.Key}) {
		t.Fatalf("first backfill = %#v", got)
	}
	b2 := demand("b/repo", 4, 30*time.Second, "small")
	in.Demands = domain.Fresh([]domain.Demand{reserved, b2, c1}, testNow)
	in.Prior = first.Next
	second := PlanTick(in)
	if got := spawnedKeys(second); !reflect.DeepEqual(got, []domain.DemandKey{c1.Key}) {
		t.Fatalf("reservation backfill failed to rotate: %#v", got)
	}
}

func TestTeardownStatesDoNotConsumeRepoAdmissionCap(t *testing.T) {
	for _, state := range []domain.InstanceState{
		domain.InstanceDraining, domain.InstanceDeregistering, domain.InstanceStopping,
		domain.InstanceDeleted, domain.InstanceFailed,
	} {
		counts := activeRepoCounts([]domain.Instance{{ID: string(state), Repo: "a/repo", State: state}})
		if counts["a/repo"] != 0 {
			t.Fatalf("state %s consumed cap: %#v", state, counts)
		}
	}
}

func FuzzPlanTickDeterministicAndSafe(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6})
	f.Add([]byte{255, 0, 17, 9, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		config := testConfig()
		for index, repo := range []string{"a/repo", "b/repo", "c/repo"} {
			config.RepoCaps[repo] = int(data[index%len(data)]%4) + 1
		}
		profiles := []domain.ProfileID{"small", "medium", "large"}
		repos := []string{"a/repo", "b/repo", "c/repo"}
		count := int(data[0]%9) + 1
		demands := make([]domain.Demand, 0, count)
		for i := 0; i < count; i++ {
			value := data[i%len(data)]
			profile := profiles[int(value)%len(profiles)]
			job := demand(repos[int(value/3)%len(repos)], int64(i+1), time.Duration(value%12)*time.Minute, profile)
			if value%5 == 0 {
				job.Event = domain.EventSchedule
			}
			demands = append(demands, job)
		}
		in := input(demands, nil, State{DRRCursor: repos[int(data[0])%len(repos)]})
		in.Config = config
		first := PlanTick(in)
		reversed := append([]domain.Demand(nil), demands...)
		for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
			reversed[left], reversed[right] = reversed[right], reversed[left]
		}
		in.Demands = domain.Fresh(reversed, testNow)
		second := PlanTick(in)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("non-deterministic plan: %#v / %#v", first, second)
		}

		seen := make(map[domain.DemandKey]bool)
		used := domain.Resources{}
		repoCount := make(map[string]int)
		agedPresent := false
		for _, job := range demands {
			if testNow.Sub(job.CreatedAt) >= config.FairnessAge {
				agedPresent = true
			}
		}
		for _, operation := range first.Operations {
			if operation.Kind != OperationSpawn {
				continue
			}
			if seen[operation.Demand] {
				t.Fatalf("duplicate spawn for %s", operation.Demand)
			}
			seen[operation.Demand] = true
			profile := config.Profiles[operation.Profile]
			if profile.Platform != domain.PlatformLinux {
				t.Fatalf("unexpected platform %s", profile.Platform)
			}
			used = used.Add(profile.Resources)
			repoCount[operation.Demand.Repo]++
			if repoCount[operation.Demand.Repo] > config.RepoCaps[operation.Demand.Repo] {
				t.Fatalf("repo cap exceeded: %#v", first)
			}
			if agedPresent && first.Next.Reservation == nil {
				for _, job := range demands {
					if job.Key == operation.Demand && testNow.Sub(job.CreatedAt) < config.FairnessAge {
						t.Fatalf("younger job bypassed aged work: %#v", first)
					}
				}
			}
		}
		if !config.LinuxCapacity.CanFit(used) || used.Slots > 4 {
			t.Fatalf("resource overcommit: used=%#v plan=%#v", used, first)
		}
	})
}

func TestFairnessIsBoundedAcrossTicks(t *testing.T) {
	jobs := []domain.Demand{
		demand("a/repo", 1, time.Minute, "small"),
		demand("b/repo", 2, time.Minute, "small"),
		demand("c/repo", 3, time.Minute, "small"),
	}
	state := State{}
	seen := make(map[string]bool)
	for tick := 0; tick < 3; tick++ {
		in := input(jobs, nil, state)
		in.Config.LinuxCapacity = domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}
		in.Host = domain.Fresh(domain.Host{Available: in.Config.LinuxCapacity}, testNow)
		plan := PlanTick(in)
		spawned := spawnedKeys(plan)
		if len(spawned) != 1 {
			t.Fatalf("tick %d spawned %#v", tick, spawned)
		}
		seen[spawned[0].Repo] = true
		var remaining []domain.Demand
		for _, job := range jobs {
			if job.Key != spawned[0] {
				remaining = append(remaining, job)
			}
		}
		jobs, state = remaining, plan.Next
	}
	if len(seen) != 3 {
		t.Fatalf("repo starvation across ticks: %#v", seen)
	}
}

func TestPlatformOverlapInvariantAcrossLifecycleStates(t *testing.T) {
	states := []domain.InstanceState{
		domain.InstancePlanned, domain.InstanceCloning, domain.InstanceBooting,
		domain.InstanceReachable, domain.InstanceRegistering, domain.InstanceOnlineIdle,
		domain.InstanceAssigned, domain.InstanceRunning, domain.InstanceDraining,
		domain.InstanceDeregistering, domain.InstanceStopping, domain.InstanceDeleted,
		domain.InstanceFailed,
	}
	for _, platform := range []domain.Platform{domain.PlatformLinux, domain.PlatformMacOS} {
		for _, state := range states {
			instance := domain.Instance{ID: "existing", Repo: "b/repo", Platform: platform, State: state}
			profile := domain.ProfileID("small")
			if platform == domain.PlatformLinux {
				profile = "builder"
			}
			job := demand("a/repo", 1, time.Minute, profile)
			plan := PlanTick(input([]domain.Demand{job}, []domain.Instance{instance}, State{}))
			for _, operation := range plan.Operations {
				if operation.Kind != OperationSpawn || !instance.Live() || platform == job.Platform {
					continue
				}
				if state != domain.InstanceOnlineIdle {
					t.Fatalf("spawn overlaps %s instance in %s: %#v", platform, state, plan)
				}
				if len(operation.DependsOn) != 1 || plan.Operations[0].Kind != OperationDrain || operation.DependsOn[0] != plan.Operations[0].ID {
					t.Fatalf("cross-platform spawn lacks drain dependency: %#v", plan)
				}
			}
		}
	}
}

func TestMacAdmissionHonorsHostResourceVector(t *testing.T) {
	maestro := demand("a/repo", 1, time.Minute, "maestro")
	in := input([]domain.Demand{maestro}, nil, State{})
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 7_000, Slots: 2}}, testNow)
	if plan := PlanTick(in); len(spawnedKeys(plan)) != 0 {
		t.Fatalf("macOS memory overcommit: %#v", plan)
	}
	active := domain.Instance{ID: "oversized", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "maestro", Resources: domain.Resources{CPU: 5, MemoryMB: 8_000, Slots: 1}, State: domain.InstanceRunning}
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 2}}, testNow)
	in.Instances = domain.Fresh([]domain.Instance{active}, testNow)
	if plan := PlanTick(in); len(spawnedKeys(plan)) != 0 {
		t.Fatalf("inconsistent live mac resources admitted work: %#v", plan)
	}
}

func TestHostIdleArbitratesPlatformsByGlobalAge(t *testing.T) {
	olderLinux := demand("a/repo", 1, 4*time.Minute, "small")
	youngerBuilder := demand("b/repo", 2, time.Minute, "builder")
	plan := PlanTick(input([]domain.Demand{youngerBuilder, olderLinux}, nil, State{}))
	if got := spawnedKeys(plan); !reflect.DeepEqual(got, []domain.DemandKey{olderLinux.Key}) {
		t.Fatalf("young macOS demand outranked older Linux: %#v", plan)
	}
}

func TestMixedPlatformAndMacProfilesCannotStarveAcrossTicks(t *testing.T) {
	pending := []domain.Demand{
		demand("a/repo", 1, 4*time.Minute, "small"),
		demand("b/repo", 2, 3*time.Minute, "builder"),
		demand("c/repo", 3, 2*time.Minute, "maestro"),
	}
	seen := make(map[domain.DemandKey]bool)
	state := State{}
	for tick := 0; tick < 3; tick++ {
		plan := PlanTick(input(pending, nil, state))
		spawned := spawnedKeys(plan)
		if len(spawned) == 0 {
			t.Fatalf("tick %d made no progress: %#v", tick, plan)
		}
		for _, key := range spawned {
			seen[key] = true
		}
		var remaining []domain.Demand
		for _, job := range pending {
			if !seen[job.Key] {
				remaining = append(remaining, job)
			}
		}
		pending, state = remaining, plan.Next
	}
	if len(seen) != 3 {
		t.Fatalf("mixed-platform starvation: seen=%#v pending=%#v", seen, pending)
	}
}

func TestIdleMacToLinuxSwitchSpawnsInSameDependentPlan(t *testing.T) {
	linux := demand("a/repo", 1, 10*time.Minute, "small")
	idleMac := domain.Instance{ID: "mac-idle", Repo: "b/repo", Platform: domain.PlatformMacOS, Profile: "builder", Resources: testConfig().Profiles["builder"].Resources, State: domain.InstanceOnlineIdle}
	deleted := domain.Instance{ID: "deleted", Platform: domain.PlatformMacOS, State: domain.InstanceDeleted}
	plan := PlanTick(input([]domain.Demand{linux}, []domain.Instance{idleMac, deleted}, State{}))
	if len(plan.Operations) != 2 || plan.Operations[0].Kind != OperationDrain || plan.Operations[1].Kind != OperationSpawn {
		t.Fatalf("mac-to-Linux switch = %#v", plan)
	}
	if !reflect.DeepEqual(plan.Operations[1].DependsOn, []string{plan.Operations[0].ID}) {
		t.Fatalf("Linux spawn missing drain dependency: %#v", plan)
	}
	if plan.Next.LinuxHandoff != nil {
		t.Fatalf("completed Linux handoff retained stale state: %#v", plan.Next.LinuxHandoff)
	}
}
