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
