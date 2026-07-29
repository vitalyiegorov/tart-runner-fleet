package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// Queues are reported per profile, aggregated across every scope bound to that
// profile. A host serving five scopes therefore cannot answer the one question an
// operator actually asks during an incident: is THIS scope's THIS profile
// receiving demand?
//
// 2026-07-29: four budgie-at/budgie iOS jobs sat queued on GitHub for ten hours
// while the daemon reported `builder: 1 queued` and ran builders continuously --
// for a different scope. Establishing that the fleet was never told about
// budgie's demand took hours of cross-referencing GitHub's per-repository runner
// lists against `tart list`, because the versioned API aggregates the scope away.
// Every binding is already observed separately, so the information exists; only
// its reporting collapses it.

// TestEngineTickReportsQueuesPerScope is the RED-first case: a tick must report
// each binding's queue depth against its own scope, so an idle scope is
// distinguishable from a busy one sharing the same profile.
func TestEngineTickReportsQueuesPerScope(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := &tickStore{}
	profile := tickConfig().Profiles["small"]
	// Two scopes bound to the SAME profile, exactly the topology that hid the
	// incident: one aggregated `small` row cannot separate them.
	bindings := []Binding{
		{StoreKey: 101, ScaleSetID: 1, Scope: "budgie-org", Profile: profile},
		{StoreKey: 202, ScaleSetID: 1, Scope: "sudoku-repo", Profile: profile},
	}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
			host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity,
				Pressure: domain.HostPressure{FreeDiskGB: 200, AdmissionAllowed: true}}, now)},
		Config: tickConfig(), Bindings: bindings, ControllerID: "controller",
		Mode: reconcile.Authority, Now: func() time.Time { return now }}

	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() = %v", err)
	}

	if len(result.ScopeQueues) != len(bindings) {
		t.Fatalf("ScopeQueues = %#v, want one row per binding (%d)", result.ScopeQueues, len(bindings))
	}
	seen := map[string]ScopeQueue{}
	for _, row := range result.ScopeQueues {
		seen[row.Scope] = row
	}
	for _, binding := range bindings {
		row, ok := seen[binding.Scope]
		if !ok {
			t.Fatalf("scope %q missing from ScopeQueues %#v", binding.Scope, result.ScopeQueues)
		}
		if row.Profile != profile.ID {
			t.Fatalf("scope %q profile = %q, want %q", binding.Scope, row.Profile, profile.ID)
		}
		if row.ScaleSetID != binding.ScaleSetID {
			t.Fatalf("scope %q scaleSetID = %d, want %d", binding.Scope, row.ScaleSetID, binding.ScaleSetID)
		}
	}
}

// TestEngineTickScopeQueuesAreDeterministic proves the rows are ordered, so
// operators, JSON consumers, and replay fixtures see a stable sequence rather
// than Go map iteration order.
func TestEngineTickScopeQueuesAreDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	profile := tickConfig().Profiles["small"]
	bindings := []Binding{
		{StoreKey: 303, ScaleSetID: 9, Scope: "zeta-repo", Profile: profile},
		{StoreKey: 101, ScaleSetID: 1, Scope: "alpha-repo", Profile: profile},
		{StoreKey: 202, ScaleSetID: 5, Scope: "middle-repo", Profile: profile},
	}
	var first []ScopeQueue
	for attempt := 0; attempt < 4; attempt++ {
		store := &tickStore{}
		engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
			Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
				host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)},
			Config: tickConfig(), Bindings: bindings, ControllerID: "c",
			Mode: reconcile.Observe, Now: func() time.Time { return now }}
		result, err := engine.Tick(context.Background())
		if err != nil {
			t.Fatalf("Tick() = %v", err)
		}
		if attempt == 0 {
			first = result.ScopeQueues
			continue
		}
		if len(result.ScopeQueues) != len(first) {
			t.Fatalf("row count drifted: %d vs %d", len(result.ScopeQueues), len(first))
		}
		for i := range first {
			if result.ScopeQueues[i].Scope != first[i].Scope {
				t.Fatalf("row %d scope = %q, want stable %q", i, result.ScopeQueues[i].Scope, first[i].Scope)
			}
		}
	}
	// Ascending by scope then profile keeps the sequence auditable.
	for i := 1; i < len(first); i++ {
		if first[i-1].Scope > first[i].Scope {
			t.Fatalf("ScopeQueues not sorted by scope: %q before %q", first[i-1].Scope, first[i].Scope)
		}
	}
}

// TestEngineTickScopeQueuesPreserveAggregate proves the addition is additive: the
// existing per-profile aggregate still sums every scope, so nothing that reads
// Queues today changes behavior.
func TestEngineTickScopeQueuesPreserveAggregate(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := &tickStore{}
	profile := tickConfig().Profiles["small"]
	bindings := []Binding{
		{StoreKey: 101, ScaleSetID: 1, Scope: "a-scope", Profile: profile},
		{StoreKey: 202, ScaleSetID: 2, Scope: "b-scope", Profile: profile},
	}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
			host: domain.Fresh(domain.Host{Available: tickConfig().LinuxCapacity}, now)},
		Config: tickConfig(), Bindings: bindings, ControllerID: "c",
		Mode: reconcile.Observe, Now: func() time.Time { return now }}

	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() = %v", err)
	}
	perScope := 0
	for _, row := range result.ScopeQueues {
		perScope += row.Count
	}
	if aggregate := result.Queues[profile.ID].Count; aggregate != perScope {
		t.Fatalf("aggregate %d != sum of per-scope %d", aggregate, perScope)
	}
}

// TestEngineTickScopeQueuesOrderWithinAScope proves the tiebreakers: one scope
// binds many profiles, and a profile can bind more than one scale set, so scope
// alone cannot order the rows. Without this the sequence would depend on binding
// iteration order for every real multi-profile scope.
func TestEngineTickScopeQueuesOrderWithinAScope(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	small := tickConfig().Profiles["small"]
	medium := domain.Profile{ID: "medium", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 1, MemoryMB: 1024, Slots: 1}}
	config := tickConfig()
	config.Profiles["medium"] = medium
	// One scope, two profiles; and one profile bound to two scale sets.
	bindings := []Binding{
		{StoreKey: 4, ScaleSetID: 9, Scope: "one-scope", Profile: small},
		{StoreKey: 3, ScaleSetID: 2, Scope: "one-scope", Profile: medium},
		{StoreKey: 2, ScaleSetID: 4, Scope: "one-scope", Profile: small},
	}
	store := &tickStore{}
	engine := Engine{Store: store, Demand: DemandCoordinator{Store: store},
		Inventory: fakeInventory{instances: domain.Fresh([]domain.Instance(nil), now),
			host: domain.Fresh(domain.Host{Available: config.LinuxCapacity}, now)},
		Config: config, Bindings: bindings, ControllerID: "c",
		Mode: reconcile.Observe, Now: func() time.Time { return now }}

	result, err := engine.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick() = %v", err)
	}
	if len(result.ScopeQueues) != 3 {
		t.Fatalf("ScopeQueues = %#v, want 3 rows", result.ScopeQueues)
	}
	// medium sorts before small within the scope; small's two scale sets ascend.
	want := []struct {
		profile domain.ProfileID
		scaleID int64
	}{{"medium", 2}, {"small", 4}, {"small", 9}}
	for i, expected := range want {
		got := result.ScopeQueues[i]
		if got.Profile != expected.profile || got.ScaleSetID != expected.scaleID {
			t.Fatalf("row %d = %s/%d, want %s/%d", i, got.Profile, got.ScaleSetID,
				expected.profile, expected.scaleID)
		}
	}
}
