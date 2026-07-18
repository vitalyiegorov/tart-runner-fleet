package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

func TestMacHandoffStateSurvivesStoreRestartAndLegacyJSON(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	now := time.Unix(2_000, 0).UTC()
	want := scheduler.State{MacHandoff: &scheduler.MacHandoff{
		Demand:  domain.DemandKey{Repo: "owner/repo", RunID: 1, Attempt: 1, JobID: 2},
		Profile: "builder", Since: now.Add(-time.Minute), BackfillAdmitted: true,
	}}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	plan := operations.Plan{ID: "handoff", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1, Data: encoded}}
	if applied, err := store.ApplyPlan(ctx, plan); err != nil || !applied {
		t.Fatalf("apply handoff state: %v %v", applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.SchedulerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got scheduler.State
	if err := json.Unmarshal(record.Data, &got); err != nil || got.MacHandoff == nil || *got.MacHandoff != *want.MacHandoff {
		t.Fatalf("restarted handoff = %#v, %v", got.MacHandoff, err)
	}
	var legacy scheduler.State
	if err := json.Unmarshal([]byte(`{"DRRCursor":"legacy/repo"}`), &legacy); err != nil || legacy.DRRCursor != "legacy/repo" || legacy.MacHandoff != nil {
		t.Fatalf("legacy state = %#v, %v", legacy, err)
	}
}

func TestExclusiveMacHandoffSurvivesRestartWithoutLinuxBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	now := time.Unix(3_000, 0).UTC()
	macKey := domain.DemandKey{Repo: "mobile/repo", RunID: 7, Attempt: 1, JobID: 8}
	want := scheduler.State{MacHandoff: &scheduler.MacHandoff{Demand: macKey, Profile: "maestro", Since: now.Add(-time.Minute)}}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := store.ApplyPlan(ctx, operations.Plan{ID: "exclusive-handoff", CreatedAt: now, Scheduler: operations.SchedulerState{Version: 1, Data: encoded}}); err != nil || !applied {
		t.Fatalf("apply exclusive handoff state: %v %v", applied, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record, err := store.SchedulerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var restarted scheduler.State
	if err := json.Unmarshal(record.Data, &restarted); err != nil {
		t.Fatal(err)
	}
	profiles := map[domain.ProfileID]domain.Profile{
		"small":   {ID: "small", Platform: domain.PlatformLinux, Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2_048, Slots: 1}},
		"medium":  {ID: "medium", Platform: domain.PlatformLinux, Route: "linux-medium", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}},
		"maestro": {ID: "maestro", Platform: domain.PlatformMacOS, Route: "macos-maestro", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}, MaxActive: 4},
	}
	config := scheduler.Config{LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}, FairnessAge: 5 * time.Minute,
		RepoCaps: map[string]int{"mobile/repo": 4, "linux/repo": 4, "holder/repo": 4}, Profiles: profiles, MacOSExclusive: true}
	mac := domain.Demand{Key: macKey, CreatedAt: now.Add(-20 * time.Minute), Profile: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS, Event: domain.EventPullRequest}
	linux := domain.Demand{Key: domain.DemandKey{Repo: "linux/repo", RunID: 9, Attempt: 1, JobID: 10}, CreatedAt: now.Add(-10 * time.Minute),
		Profile: "small", Route: "linux-small", Platform: domain.PlatformLinux, Event: domain.EventPullRequest}
	holder := domain.Instance{ID: "holder", Repo: "holder/repo", Platform: domain.PlatformLinux, Profile: "medium", Route: "linux-medium", Resources: profiles["medium"].Resources, State: domain.InstanceRunning}
	plan := scheduler.PlanTick(scheduler.Input{Now: now, Config: config, Demands: domain.Fresh([]domain.Demand{linux, mac}, now),
		Instances: domain.Fresh([]domain.Instance{holder}, now), Host: domain.Fresh(domain.Host{Available: config.LinuxCapacity}, now), Prior: restarted})
	if len(plan.Operations) != 0 || plan.Next.MacHandoff == nil || plan.Next.MacHandoff.Since != want.MacHandoff.Since || plan.Next.MacHandoff.BackfillAdmitted {
		t.Fatalf("restarted exclusive handoff admitted Linux or lost fairness state: %#v", plan)
	}
}
