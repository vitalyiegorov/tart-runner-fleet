package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// runningLinux is a live Linux instance that is executing a job: it consumes
// the host envelope and is NOT OnlineIdle, so no handoff may drain it. That is
// the shape every duplicate-admission scenario below needs — a Linux occupant
// the macOS head must wait for.
func runningLinux(id, repo string, profile domain.ProfileID) domain.Instance {
	configured := handoffConfig().Profiles[profile]
	return domain.Instance{ID: id, Repo: repo, Platform: domain.PlatformLinux, Profile: profile,
		Route: configured.Route, Resources: configured.Resources, State: domain.InstanceRunning,
		Power: domain.InstancePowerRunning, RunningSince: testNow.Add(-time.Minute)}
}

// handoffConfig reproduces the live envelope: a Linux capacity that fits one
// large runner beside small work but cannot fit the macOS builder head beside
// it, so the builder waits for a drain that a busy runner will never allow.
func handoffConfig() Config {
	cfg := testConfig()
	cfg.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 20_480, Slots: 4}
	cfg.RepoCaps["mac-a"] = 2
	cfg.RepoCaps["mac-b"] = 2
	return cfg
}

// handoffHost is the observed host for handoffConfig: it never relaxes the
// configured envelope, so admission is decided by policy rather than by an
// unobserved physical bound.
func handoffHost() domain.Observation[domain.Host] {
	return domain.Fresh(domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 20_480, Slots: 4}}, testNow)
}

// duplicateSpawns reports every demand this plan spawns more than once.
func duplicateSpawns(plan Plan) []domain.DemandKey {
	seen := make(map[domain.DemandKey]int, len(plan.Operations))
	var repeated []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind != OperationSpawn {
			continue
		}
		seen[operation.Demand]++
		if seen[operation.Demand] == 2 {
			repeated = append(repeated, operation.Demand)
		}
	}
	return repeated
}

// duplicateOperationIDs reports every operation identity this plan carries more
// than once, whatever its kind.
func duplicateOperationIDs(plan Plan) []string {
	seen := make(map[string]int, len(plan.Operations))
	var repeated []string
	for _, operation := range plan.Operations {
		seen[operation.ID]++
		if seen[operation.ID] == 2 {
			repeated = append(repeated, operation.ID)
		}
	}
	return repeated
}

// TestComplementaryPassNeverReadmitsADemandThisPlanAlreadySpawns replays the
// 2026-08-02 wedge on host vitalii-mac-mini.
//
// A macOS builder head aged behind a live Linux runner that could not be
// drained (it was executing a job, so never OnlineIdle). planMacHandoff could
// not spawn the head, so it admitted one aged smallest-tier Linux job as the
// bounded drain backfill and latched BackfillAdmitted. PlanTick then ran
// fillLinuxRemainder over the SAME Linux queue: appendPlannedSpawns teaches the
// complementary pass what the first pass COST, never which demands it already
// claimed, so the identical demand was admitted a second time.
//
// Both spawns are content-addressed from the same demand, so the plan carried
// two operations with one identity and two instance intents with one instance
// name. reconcile.Controller.Commit translated both, and the durable layer
// refused the second INSERT with a bare UNIQUE violation that classified as
// plan_commit_failed. The plan is a pure function of its inputs, so every
// subsequent tick rebuilt it: scheduler_state stayed pinned at version 2421
// from 09:59:51Z onward and no queue was scheduled again until an operator
// intervened.
func TestComplementaryPassNeverReadmitsADemandThisPlanAlreadySpawns(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		demands    []domain.Demand
		instances  []domain.Instance
		wantSpawns []domain.DemandKey
	}{
		{
			name: "macOS head backfills Linux and the Linux remainder pass repeats it",
			// The builder head is the aged priority head and cannot fit beside the
			// live large runner. The single aged small is eligible for the bounded
			// drain backfill AND for the ordinary Linux allocator.
			demands: []domain.Demand{
				demand("mac-a", 1, 20*time.Minute, "builder"),
				demand("a/repo", 2, 10*time.Minute, "small"),
			},
			instances:  []domain.Instance{runningLinux("linux-1", "b/repo", "large")},
			wantSpawns: []domain.DemandKey{{Repo: "a/repo", RunID: 100, Attempt: 1, JobID: 2}},
		},
		{
			name: "a second aged small does not hide the repeat",
			demands: []domain.Demand{
				demand("mac-a", 1, 20*time.Minute, "builder"),
				demand("a/repo", 2, 10*time.Minute, "small"),
				demand("b/repo", 3, 9*time.Minute, "small"),
			},
			// The remainder pass still admits the SECOND aged small: filtering
			// removes only what this plan already claimed, never the rest of the queue.
			instances: []domain.Instance{runningLinux("linux-1", "c/repo", "large")},
			wantSpawns: []domain.DemandKey{{Repo: "a/repo", RunID: 100, Attempt: 1, JobID: 2},
				{Repo: "b/repo", RunID: 100, Attempt: 1, JobID: 3}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			in := input(testCase.demands, testCase.instances, State{})
			in.Config = handoffConfig()
			in.Config.MixedPlatformAdmission = true
			in.Host = handoffHost()

			plan := PlanTick(in)

			if got := duplicateSpawns(plan); len(got) > 0 {
				t.Fatalf("plan spawns %#v twice; a repeated content address collides in the durable layer and wedges every later tick: %#v", got, plan.Operations)
			}
			if got := duplicateOperationIDs(plan); len(got) > 0 {
				t.Fatalf("plan carries duplicate operation identities %#v: %#v", got, plan.Operations)
			}
			if !reflect.DeepEqual(spawnedKeys(plan), testCase.wantSpawns) {
				t.Fatalf("spawned = %#v, want exactly %#v", spawnedKeys(plan), testCase.wantSpawns)
			}
		})
	}
}

// TestRemainderPassStillAdmitsWorkTheFirstPassDidNotClaim is the guard against
// over-correcting: filtering already-admitted demands must not close the
// complementary pass. A live macOS cohort blocks nothing here, and the second
// Linux demand the bounded backfill declined still has to reach the residual
// envelope on this same tick.
func TestRemainderPassStillAdmitsWorkTheFirstPassDidNotClaim(t *testing.T) {
	builderHead := demand("mac-a", 1, 20*time.Minute, "builder")
	agedSmall := demand("a/repo", 2, 10*time.Minute, "small")
	youngSmall := demand("b/repo", 3, time.Minute, "small")

	in := input([]domain.Demand{builderHead, agedSmall, youngSmall}, []domain.Instance{runningLinux("linux-1", "c/repo", "large")}, State{})
	in.Config = handoffConfig()
	in.Config.MixedPlatformAdmission = true
	in.Host = handoffHost()

	plan := PlanTick(in)

	if got := duplicateSpawns(plan); len(got) > 0 {
		t.Fatalf("plan spawns %#v twice: %#v", got, plan.Operations)
	}
	spawned := spawnedKeys(plan)
	if len(spawned) != 2 {
		t.Fatalf("spawned = %#v, want the aged backfill AND the young remainder admission", spawned)
	}
	for _, want := range []domain.DemandKey{agedSmall.Key, youngSmall.Key} {
		found := false
		for _, got := range spawned {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("spawned = %#v, want it to contain %#v", spawned, want)
		}
	}
}

// TestRemainderPassesShareTheSameGuard pins the mirrored call site directly.
// fillMacRemainder runs after planners that can themselves admit macOS work —
// planLinuxHandoff appends a bounded macOS backfill wave — and it receives the
// same macOS queue those planners saw. Whether or not today's reachability
// analysis finds a live path there, the guard is the invariant: a complementary
// pass never re-admits a demand this plan already spawns, because a repeated
// content address is refused by the durable layer and rebuilt by every later
// tick.
func TestRemainderPassesShareTheSameGuard(t *testing.T) {
	cfg := mixedConfig()
	cfg.MixedPlatformAdmission = true
	macJob := demand("mac-b", 2, 10*time.Minute, "maestro")
	in := input([]domain.Demand{macJob}, nil, State{})
	in.Config = cfg
	in.Host = domain.Fresh(domain.Host{Available: domain.Resources{CPU: 12, MemoryMB: 24_576, Slots: 4}}, testNow)

	claimed := Plan{Status: PlanReady, Operations: []Operation{spawnOperation(macJob, nil)}}

	filled := fillMacRemainder(in, claimed, []domain.Demand{macJob})

	if got := duplicateSpawns(filled); len(got) > 0 {
		t.Fatalf("fillMacRemainder re-admitted %#v the plan already spawns: %#v", got, filled.Operations)
	}
	if len(filled.Operations) != 1 {
		t.Fatalf("operations = %#v, want the single already-claimed spawn untouched", filled.Operations)
	}
}

// TestDemandsAwaitingAdmissionIsExact pins the filter itself: it removes exactly
// the demands the plan already spawns, preserves order, ignores drains, and
// returns the input untouched when nothing was spawned.
func TestDemandsAwaitingAdmissionIsExact(t *testing.T) {
	first := demand("a/repo", 1, time.Minute, "small")
	second := demand("b/repo", 2, time.Minute, "small")
	third := demand("c/repo", 3, time.Minute, "small")
	drain := Operation{Kind: OperationDrain, Instance: "linux-1"}

	for _, testCase := range []struct {
		name       string
		demands    []domain.Demand
		operations []Operation
		want       []domain.DemandKey
	}{
		{name: "no operations returns every demand", demands: []domain.Demand{first, second},
			want: []domain.DemandKey{first.Key, second.Key}},
		{name: "drains claim nothing", demands: []domain.Demand{first, second}, operations: []Operation{drain},
			want: []domain.DemandKey{first.Key, second.Key}},
		{name: "a spawned demand is removed and order is preserved",
			demands: []domain.Demand{first, second, third}, operations: []Operation{spawnOperation(second, nil)},
			want: []domain.DemandKey{first.Key, third.Key}},
		{name: "every demand spawned yields none",
			demands:    []domain.Demand{first, second},
			operations: []Operation{spawnOperation(first, nil), drain, spawnOperation(second, nil)}},
		{name: "no demands yields none", operations: []Operation{spawnOperation(first, nil)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := demandsAwaitingAdmission(testCase.demands, testCase.operations)
			if len(got) != len(testCase.want) {
				t.Fatalf("demandsAwaitingAdmission length = %d (%#v), want %d", len(got), got, len(testCase.want))
			}
			for i, want := range testCase.want {
				if got[i].Key != want {
					t.Fatalf("demandsAwaitingAdmission[%d] = %#v, want %#v", i, got[i].Key, want)
				}
			}
		})
	}
}
