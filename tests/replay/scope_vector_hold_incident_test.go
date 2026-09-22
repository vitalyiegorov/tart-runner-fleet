package replay_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// The 2026-09-22 afternoon on the mac mini, replayed from the rows the operator
// captured while ADR 0057 was live (v0.1.609, #345 and #347 both deployed).
// Issue #350.
//
//	13:45Z  pony's running job is cancelled by its owner; pony holds 0
//	        instances for the rest of the window.
//	13:46Z  budgie starts "iOS Maestro E2E / Maestro shard 0" on runner
//	        trf-maestro-0ef249257cb3ca65. It runs until 15:00Z.
//	`fleet queues --output json` through the window:
//	        pony    maestro  set 7   2 jobs  created 13:07:34Z, 13:07:58Z
//	        budgie  builder  6x12    1 job   created 12:46Z
//	        budgie  maestro  4x7     1 job   created 11:37Z
//	14:16Z  the OTHER slot frees.  IT GOES TO BUDGIE'S 11:37Z MAESTRO.
//	15:00Z  shard 0 ends.          The next slot goes to budgie's 12:46Z builder.
//
// The mini's `hostBudget` is 10 vCPU / 23 GiB, so it runs TWO 4x7 maestro guests
// side by side and budgie was demonstrably holding one of them at 14:16Z. That
// is exactly the arrangement ADR 0057's occupancy term exists to decide: one
// profile, both scopes hours past FairnessAge, one scope on a core and one on
// none. It decided it the wrong way.
//
// The 15:00Z decision is the rule working and is pinned below unchanged:
// `builder` and `maestro` are different profiles, ADR 0057 only ever exchanges
// demands of one profile, and pony queued no builder.
//
// The cause is that the rank asked `activeRepoCounts`, which answers a DIFFERENT
// question -- how many of a repository's concurrent cap slots are spent -- and
// ADR 0043 releases that slot early, at deregistration, so a cap cannot block a
// replacement the fleet has already committed to. The host vector is released
// later. On the four observations where the two disagree (`online-idle`,
// `deregistering`, `stopping` before the guest is proven idle, `failed`) a scope
// occupying a core reads as holding nothing, both scopes rank zero, and the tie
// falls through to ADR 0004's aged FIFO -- which budgie wins by having queued
// first, forever.

const (
	vectorHoldBudgie = "budgie-at/budgie"
	vectorHoldPony   = "pony-labs/pony"
)

func vectorHoldConfig() scheduler.Config {
	return scheduler.Config{
		// The mini's macOS envelope: hostBudget 10 vCPU / 23552 MiB, two slots.
		LinuxCapacity:       domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2},
		FairnessAge:         5 * time.Minute,
		AssignedTimeout:     15 * time.Minute,
		MixedProfileCohorts: true,
		RepoCaps:            map[string]int{vectorHoldBudgie: 4, vectorHoldPony: 4},
		Profiles: map[domain.ProfileID]domain.Profile{
			"macos-4x7": {ID: "macos-4x7", Platform: domain.PlatformMacOS, Route: "macos-maestro",
				Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2},
			"macos-6x12": {ID: "macos-6x12", Platform: domain.PlatformMacOS, Route: "macos-builder",
				Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1},
		},
	}
}

func vectorHoldDemand(repo string, jobID int64, created string, profile domain.ProfileID) domain.Demand {
	at, err := time.Parse(time.RFC3339, created)
	if err != nil {
		panic(err)
	}
	route := domain.Route("macos-maestro")
	if profile == "macos-6x12" {
		route = "macos-builder"
	}
	return domain.Demand{
		Key:       domain.DemandKey{Repo: repo, RunID: 9_200, Attempt: 1, JobID: jobID},
		CreatedAt: at, Profile: profile, Route: route,
		Platform: domain.PlatformMacOS, Event: domain.EventPush,
	}
}

// vectorHoldQueue is the mini's queue as `fleet queues` read it through the
// window, in the order the rows were captured.
func vectorHoldQueue() []domain.Demand {
	return []domain.Demand{
		vectorHoldDemand(vectorHoldBudgie, 1, "2026-09-22T11:37:00Z", "macos-4x7"),
		vectorHoldDemand(vectorHoldBudgie, 2, "2026-09-22T12:46:00Z", "macos-6x12"),
		vectorHoldDemand(vectorHoldPony, 3, "2026-09-22T13:07:34Z", "macos-4x7"),
		vectorHoldDemand(vectorHoldPony, 4, "2026-09-22T13:07:58Z", "macos-4x7"),
	}
}

// budgieShard0 is "iOS Maestro E2E / Maestro shard 0", holding one of the mini's
// two slots from 13:46Z to 15:00Z.
func budgieShard0(state domain.InstanceState) domain.Instance {
	started, _ := time.Parse(time.RFC3339, "2026-09-22T13:46:00Z")
	return domain.Instance{
		ID: "trf-maestro-0ef249257cb3ca65", Repo: vectorHoldBudgie, Platform: domain.PlatformMacOS,
		Demand:  domain.DemandKey{Repo: vectorHoldBudgie, RunID: 9_199, Attempt: 1, JobID: 99},
		Profile: "macos-4x7", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1},
		State:     state, Power: domain.InstancePowerRunning, RunningSince: started,
	}
}

func vectorHoldInput(now time.Time, demands []domain.Demand, instances []domain.Instance) scheduler.Input {
	return scheduler.Input{
		Now: now, Config: vectorHoldConfig(),
		Demands:   domain.Fresh(demands, now),
		Instances: domain.Fresh(instances, now),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 10, MemoryMB: 23_552, Slots: 2}}, now),
	}
}

func vectorHoldSpawns(plan scheduler.Plan) []domain.DemandKey {
	var keys []domain.DemandKey
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			keys = append(keys, operation.Demand)
		}
	}
	return keys
}

func vectorHoldAt(t *testing.T, stamp string) time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("parse %s: %v", stamp, err)
	}
	return now
}

// TestTheMini1416TickGoesToTheScopeOnNoCore is the 14:16Z decision, walked over
// every lifecycle state budgie's shard could have been observed in while it was
// physically on a core. Every one of them must give the freed slot to pony.
func TestTheMini1416TickGoesToTheScopeOnNoCore(t *testing.T) {
	now := vectorHoldAt(t, "2026-09-22T14:16:00Z")
	for _, state := range []domain.InstanceState{
		domain.InstanceRunning, domain.InstanceAssigned, domain.InstanceOnlineIdle,
		domain.InstanceDraining, domain.InstanceDeregistering, domain.InstanceStopping,
		domain.InstanceFailed,
	} {
		shard := budgieShard0(state)
		if !shard.ConsumesHostResources() {
			t.Fatalf("%s does not hold the mini's vector: this is not the observed arrangement", state)
		}
		plan := scheduler.PlanTick(vectorHoldInput(now, vectorHoldQueue(), []domain.Instance{shard}))
		keys := vectorHoldSpawns(plan)
		if len(keys) != 1 {
			t.Fatalf("shard %s: the 14:16Z tick admitted %#v, want exactly one maestro", state, keys)
		}
		if keys[0].Repo != vectorHoldPony {
			t.Fatalf("shard %s: the 14:16Z slot went to %q, want %q -- budgie was on the other core and pony on none",
				state, keys[0].Repo, vectorHoldPony)
		}
	}
}

// TestTheMini1416TickIsNotAboutTheInfeasibleBuilderHead refutes the reading that
// blamed the aged infeasible `builder` head and a backfill with its own
// ordering. There was no reservation to back-fill behind: reservations are
// authored over LINUX demands and this queue is entirely macOS. Strike the
// builder row out and the decision is identical.
func TestTheMini1416TickIsNotAboutTheInfeasibleBuilderHead(t *testing.T) {
	now := vectorHoldAt(t, "2026-09-22T14:16:00Z")
	var withoutBuilder []domain.Demand
	for _, queued := range vectorHoldQueue() {
		if queued.Profile != "macos-6x12" {
			withoutBuilder = append(withoutBuilder, queued)
		}
	}
	instances := []domain.Instance{budgieShard0(domain.InstanceDeregistering)}
	plan := scheduler.PlanTick(vectorHoldInput(now, withoutBuilder, instances))
	if plan.Next.Reservation != nil {
		t.Fatalf("a macOS-only queue authored a reservation: %#v", plan.Next.Reservation)
	}
	keys := vectorHoldSpawns(plan)
	if len(keys) != 1 || keys[0].Repo != vectorHoldPony {
		t.Fatalf("without the builder row the 14:16Z slot went to %#v, want the scope on no core", keys)
	}
}

// TestTheMini1500BuilderTickIsUnchanged pins the decision the trace made
// correctly, so the fix cannot be read as having changed it. At 15:00Z shard 0
// ends, budgie's 12:46Z `builder` is the oldest demand of its own profile, and
// pony has no builder queued: ADR 0057 exchanges demands of ONE profile and has
// nothing to say here.
func TestTheMini1500BuilderTickIsUnchanged(t *testing.T) {
	now := vectorHoldAt(t, "2026-09-22T15:00:00Z")
	var remaining []domain.Demand
	for _, queued := range vectorHoldQueue() {
		// budgie's 11:37Z maestro was admitted at 14:16Z and is no longer queued.
		if queued.Key.Repo == vectorHoldBudgie && queued.Profile == "macos-4x7" {
			continue
		}
		remaining = append(remaining, queued)
	}
	plan := scheduler.PlanTick(vectorHoldInput(now, remaining, nil))
	keys := vectorHoldSpawns(plan)
	if len(keys) != 1 || keys[0].Repo != vectorHoldBudgie {
		t.Fatalf("the 15:00Z builder tick admitted %#v, want budgie's builder: no other scope asked for that profile", keys)
	}
}
