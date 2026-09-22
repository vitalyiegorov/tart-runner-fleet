package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The 2026-09-22 trace on the mac mini, and issue #350.
//
// ADR 0057 orders the aged band by how much of this node each scope holds, and
// it asked `activeRepoCounts` -- which answers a different question. That
// function counts REPOSITORY CAP slots, and ADR 0043 releases a cap slot early,
// at deregistration, precisely so a cap cannot block a replacement the fleet has
// already committed to. The host vector is released later still. On the four
// observations where the two disagree, a scope occupying one of the mini's two
// cores read as holding nothing, both scopes ranked zero, and the tie fell
// through to ADR 0004's aged FIFO -- which a scope that has been streaming work
// for hours wins every time.

const (
	vectorHoldBudgie = "budgie-at/budgie"
	vectorHoldPony   = "pony-org/pony"
)

// miniConfig is the mac mini's real macOS envelope: `hostBudget` 10 vCPU /
// 23 GiB, so TWO 4x7 maestro guests run side by side, and a 6x12 builder fits
// beside one of them.
func miniConfig() Config {
	config := testConfig()
	config.LinuxCapacity = domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2}
	config.MixedProfileCohorts = true
	config.RepoCaps = map[string]int{vectorHoldBudgie: 4, vectorHoldPony: 4}
	config.Profiles["builder"] = domain.Profile{ID: "builder", Platform: domain.PlatformMacOS,
		Route: "macos-builder", Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1}
	config.Profiles["maestro"] = domain.Profile{ID: "maestro", Platform: domain.PlatformMacOS,
		Route: "macos-maestro", Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2}
	return config
}

func miniDemand(repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	return domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 100, Attempt: 1, JobID: jobID},
		CreatedAt: testNow.Add(-age),
		Profile:   profile,
		Route:     miniConfig().Profiles[profile].Route,
		Platform:  domain.PlatformMacOS,
		Event:     domain.EventPullRequest,
	}
}

// miniQueue is the mini's queue through the 2026-09-22 window: `budgie` holds a
// slot and still has an older `maestro` and an older `builder` queued; `pony`,
// whose own job its owner cancelled at 13:45Z, holds nothing and has two.
func miniQueue() []domain.Demand {
	return []domain.Demand{
		miniDemand(vectorHoldBudgie, 1, 159*time.Minute, "maestro"), // queued 11:37Z
		miniDemand(vectorHoldBudgie, 2, 90*time.Minute, "builder"),  // queued 12:46Z
		miniDemand(vectorHoldPony, 3, 68*time.Minute, "maestro"),    // queued 13:07:34Z
		miniDemand(vectorHoldPony, 4, 67*time.Minute, "maestro"),    // queued 13:07:58Z
	}
}

// budgieShard is budgie's live maestro shard, the one that held the mini's other
// slot from 13:46Z to 15:00Z.
func budgieShard(state domain.InstanceState) domain.Instance {
	return domain.Instance{
		ID: "trf-maestro-0ef249257cb3ca65", Repo: vectorHoldBudgie, Platform: domain.PlatformMacOS,
		Demand:  domain.DemandKey{Repo: vectorHoldBudgie, RunID: 99, Attempt: 1, JobID: 99},
		Profile: "maestro", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State:     state, Power: domain.InstancePowerRunning, RunningSince: testNow.Add(-30 * time.Minute),
	}
}

func miniInput(demands []domain.Demand, instances []domain.Instance) Input {
	return Input{
		Now:       testNow,
		Config:    miniConfig(),
		Demands:   domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2}}, testNow),
	}
}

// TestAScopeOccupyingACoreNeverReadsAsHoldingNothing is the incident, walked
// over every lifecycle state in which the guest is on the machine.
//
// The four that used to fail are exactly the four where ADR 0043's cap edge and
// the host vector disagree: `online-idle`, `deregistering`, `stopping` before
// the guest is proven idle, and `failed`. They are not exotic -- three of them
// are ticks every job passes through on its way out, and the fourth is a warm
// runner between jobs.
func TestAScopeOccupyingACoreNeverReadsAsHoldingNothing(t *testing.T) {
	for _, state := range []domain.InstanceState{
		domain.InstancePlanned, domain.InstanceCloning, domain.InstanceBooting, domain.InstanceReachable,
		domain.InstanceRegistering, domain.InstanceAssigned, domain.InstanceRunning, domain.InstanceOnlineIdle,
		domain.InstanceDraining, domain.InstanceDeregistering, domain.InstanceStopping, domain.InstanceFailed,
	} {
		shard := budgieShard(state)
		if !shard.ConsumesHostResources() {
			t.Fatalf("%s does not hold the host vector: this is not the arrangement under test", state)
		}
		plan := PlanTick(miniInput(miniQueue(), []domain.Instance{shard}))
		keys := spawnedKeys(plan)
		if len(keys) != 1 || keys[0].Repo != vectorHoldPony {
			t.Fatalf("with budgie's guest %s on a core, the free slot went to %#v, want the scope holding none of the node",
				state, keys)
		}
	}
}

// TestAReleasedVectorIsNotStillHeld is the other side of the predicate, and the
// bound that keeps it from becoming a cap. Once the guest is proven idle or
// proven absent (ADR 0022, ADR 0043) the scope holds nothing, because it really
// does hold nothing -- and the slot it released is the one being given away.
func TestAReleasedVectorIsNotStillHeld(t *testing.T) {
	for _, released := range []domain.Instance{
		func() domain.Instance {
			shard := budgieShard(domain.InstanceStopping)
			shard.Power = domain.InstancePowerStopped
			return shard
		}(),
		func() domain.Instance {
			shard := budgieShard(domain.InstanceDraining)
			shard.Power = domain.InstancePowerAbsent
			return shard
		}(),
	} {
		if released.ConsumesHostResources() {
			t.Fatalf("%s/%s still holds the vector", released.State, released.Power)
		}
		if counts := activeScopeCounts([]domain.Instance{released}); len(counts) != 0 {
			t.Fatalf("a released vector was counted as a hold: %v", counts)
		}
	}
}

// TestFairShareStillLeavesOneScopeAlone keeps the guarantee ADR 0057 makes to a
// node with a single tenant: one scope is one rank sequence, which is age order.
func TestFairShareStillLeavesOneScopeAlone(t *testing.T) {
	demands := []domain.Demand{
		miniDemand(vectorHoldBudgie, 1, 159*time.Minute, "maestro"),
		miniDemand(vectorHoldBudgie, 2, 90*time.Minute, "maestro"),
	}
	plan := PlanTick(miniInput(demands, []domain.Instance{budgieShard(domain.InstanceRunning)}))
	keys := spawnedKeys(plan)
	if len(keys) != 1 || keys[0].JobID != 1 {
		t.Fatalf("a single scope's queue admitted %#v, want its oldest demand", keys)
	}
}

// TestActiveScopeCountsIsTheVectorNotTheCap pins the predicate itself, because
// the defect was a function answering a question it was not asked.
func TestActiveScopeCountsIsTheVectorNotTheCap(t *testing.T) {
	idle := budgieShard(domain.InstanceOnlineIdle)
	deregistering := budgieShard(domain.InstanceDeregistering)
	deregistering.ID = "trf-maestro-second"
	charge := budgieShard(domain.InstancePlanned)
	charge.ID = reservedChargePrefix + "budgie"
	deleted := budgieShard(domain.InstanceDeleted)

	for name, testCase := range map[string]struct {
		instances []domain.Instance
		want      int
	}{
		"a warm runner between jobs is on a core":       {instances: []domain.Instance{idle}, want: 1},
		"so is a guest past ADR 0043's cap edge":        {instances: []domain.Instance{deregistering}, want: 1},
		"both at once are two of this node's slots":     {instances: []domain.Instance{idle, deregistering}, want: 2},
		"the reserved head's charge is not a guest":     {instances: []domain.Instance{charge}, want: 0},
		"a deleted instance holds nothing (ADR 0022)":   {instances: []domain.Instance{deleted}, want: 0},
		"the cap and the vector agree while it is live": {instances: []domain.Instance{budgieShard(domain.InstanceRunning)}, want: 1},
	} {
		if got := activeScopeCounts(testCase.instances)["budgie-at"]; got != testCase.want {
			t.Fatalf("%s: counted %d, want %d", name, got, testCase.want)
		}
	}
}
