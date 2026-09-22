package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// The 2026-09-21/22 incident on the mac mini and the mac studio, replayed from
// the queue rows the operator captured (ADR 0057).
//
//	mac-mini `fleet queues --output json`, 2026-09-22T03:32Z
//	  scope budgie  set 3 (maestro)  1 job  queued 36m ago
//	  scope pony    set 7 (maestro)  3 jobs queued 3h01m ago
//	  instances: 2 x macos-4x7, both repo budgie-at/budgie, both running
//
// A Mac has two slots (ADR 0055), both profiles here are the one `macos-4x7`
// maestro, and `budgie` refills its batch as fast as it drains. The snapshot
// above is the tail of one such batch: every shard of it was queued before
// `pony`'s three jobs, so on every tick that freed a slot the aged band's pure
// FIFO handed it back to `budgie`. Three hours of a twenty-minute job waiting.
//
// The decisive tick is reconstructed below: 01:12Z, the moment the first
// `budgie` instance reached `stopping` and released its slot (ADR 0043), with
// the next shard of `budgie`'s batch one minute OLDER than `pony`'s jobs.

const (
	fairShareScopeBudgie = "budgie-at/budgie"
	fairShareScopePony   = "pony-labs/pony"
)

func fairShareIncidentConfig() scheduler.Config {
	return scheduler.Config{
		// The mini's macOS envelope: two maestro guests, 4 vCPU / 7 GiB each.
		LinuxCapacity:   domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2},
		FairnessAge:     5 * time.Minute,
		AssignedTimeout: 15 * time.Minute,
		RepoCaps:        map[string]int{fairShareScopeBudgie: 4, fairShareScopePony: 4},
		Profiles: map[domain.ProfileID]domain.Profile{
			"macos-4x7": {ID: "macos-4x7", Platform: domain.PlatformMacOS, Route: "macos-maestro",
				Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
		},
	}
}

func fairShareIncidentDemand(now time.Time, repo string, jobID int64, age time.Duration) domain.Demand {
	return domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 9_100, Attempt: 1, JobID: jobID},
		CreatedAt: now.Add(-age), Profile: "macos-4x7", Route: "macos-maestro",
		Platform: domain.PlatformMacOS, Event: domain.EventPush,
	}
}

// fairShareIncidentInstance builds one maestro guest. A `stopping` guest whose
// VM is off has released its slot (ADR 0043) and is the tick this replay turns
// on; a `running` one holds it.
func fairShareIncidentInstance(now time.Time, id, repo string, state domain.InstanceState) domain.Instance {
	power := domain.InstancePowerRunning
	if state == domain.InstanceStopping {
		power = domain.InstancePowerStopped
	}
	return domain.Instance{
		ID: id, Repo: repo, Platform: domain.PlatformMacOS, Profile: "macos-4x7", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State:     state, Power: power, RunningSince: now.Add(-40 * time.Minute),
	}
}

func fairShareIncidentInput(now time.Time, demands []domain.Demand, instances []domain.Instance) scheduler.Input {
	return scheduler.Input{
		Now: now, Config: fairShareIncidentConfig(),
		Demands:   domain.Fresh(demands, now),
		Instances: domain.Fresh(instances, now),
		Host: domain.Fresh(domain.Host{
			Available: domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2},
			Capacity:  domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 2}}, now),
	}
}

func fairShareSpawned(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

// TestScopeStarvationIncidentFreedSlotGoesToTheWaitingScope is the decisive
// tick. Before ADR 0057 the older `budgie` shard took the freed slot and the
// three-hour wait continued; after it, the scope holding nothing goes first.
func TestScopeStarvationIncidentFreedSlotGoesToTheWaitingScope(t *testing.T) {
	now := time.Date(2026, 9, 22, 1, 12, 0, 0, time.UTC)
	demands := []domain.Demand{
		fairShareIncidentDemand(now, fairShareScopeBudgie, 41, 42*time.Minute),
		fairShareIncidentDemand(now, fairShareScopePony, 71, 41*time.Minute),
		fairShareIncidentDemand(now, fairShareScopePony, 72, 41*time.Minute),
		fairShareIncidentDemand(now, fairShareScopePony, 73, 41*time.Minute),
	}
	instances := []domain.Instance{
		// Released its slot at deregistration, per ADR 0043.
		fairShareIncidentInstance(now, "gha-macos-1", fairShareScopeBudgie, domain.InstanceStopping),
		fairShareIncidentInstance(now, "gha-macos-2", fairShareScopeBudgie, domain.InstanceRunning),
	}

	spawned := fairShareSpawned(scheduler.PlanTick(fairShareIncidentInput(now, demands, instances)))
	if len(spawned) != 1 || spawned[0].Repo != fairShareScopePony {
		t.Fatalf("the slot freed at 01:12Z went to %v, want the scope that holds none of the node", spawned)
	}
}

// TestScopeStarvationIncidentYieldsBackOnTheNextSlot is the other half of the
// rule, and the reason it cannot invert the incident. Once the waiting scope
// holds a slot of its own, the next one goes back to the scope with the older
// queue: fair share is a rotation, not a preference.
func TestScopeStarvationIncidentYieldsBackOnTheNextSlot(t *testing.T) {
	now := time.Date(2026, 9, 22, 1, 52, 0, 0, time.UTC)
	demands := []domain.Demand{
		fairShareIncidentDemand(now, fairShareScopeBudgie, 41, 82*time.Minute),
		fairShareIncidentDemand(now, fairShareScopePony, 72, 81*time.Minute),
		fairShareIncidentDemand(now, fairShareScopePony, 73, 81*time.Minute),
	}
	instances := []domain.Instance{
		fairShareIncidentInstance(now, "gha-macos-3", fairShareScopePony, domain.InstanceRunning),
		fairShareIncidentInstance(now, "gha-macos-2", fairShareScopeBudgie, domain.InstanceStopping),
	}

	spawned := fairShareSpawned(scheduler.PlanTick(fairShareIncidentInput(now, demands, instances)))
	if len(spawned) != 1 || spawned[0].Repo != fairShareScopeBudgie {
		t.Fatalf("the next slot went to %v, want the scope whose queue is older now that both hold one", spawned)
	}
}
