package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func scopeQueueHealth(t *testing.T) *Health {
	t.Helper()
	health, err := NewHealth(&fakeClock{now: time.Unix(1000, 0).UTC()},
		HealthConfig{Profiles: []string{"builder"}, CriticalObservations: []string{"scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	return health
}

// TestSetScopeQueuesPublishesEveryBinding proves the per-scope breakdown reaches
// the snapshot, so two scopes sharing one profile stay distinguishable.
func TestSetScopeQueuesPublishesEveryBinding(t *testing.T) {
	health := scopeQueueHealth(t)
	oldest := time.Unix(940, 0).UTC()
	rows := []ScopeQueueMetrics{
		{Scope: "budgie-org", Profile: "builder", ScaleSetID: 1, Count: 4, OldestEnqueuedAt: oldest},
		{Scope: "sudoku-repo", Profile: "builder", ScaleSetID: 1, Count: 1},
	}
	if err := health.SetScopeQueues(rows); err != nil {
		t.Fatalf("SetScopeQueues() = %v", err)
	}
	got := health.Snapshot().ScopeQueues
	if len(got) != 2 {
		t.Fatalf("ScopeQueues = %#v, want 2 rows", got)
	}
	if got[0].Scope != "budgie-org" || got[0].Count != 4 || !got[0].OldestEnqueuedAt.Equal(oldest) {
		t.Fatalf("first row = %#v", got[0])
	}
	if got[1].Scope != "sudoku-repo" || got[1].Count != 1 {
		t.Fatalf("second row = %#v", got[1])
	}
}

// TestSetScopeQueuesReplacesWholeSet proves a binding removed from configuration
// stops being reported. A partial update could not express removal, so a stale
// scope would linger and read as live demand forever.
func TestSetScopeQueuesReplacesWholeSet(t *testing.T) {
	health := scopeQueueHealth(t)
	if err := health.SetScopeQueues([]ScopeQueueMetrics{
		{Scope: "gone-scope", Profile: "builder", Count: 3},
		{Scope: "kept-scope", Profile: "builder", Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := health.SetScopeQueues([]ScopeQueueMetrics{
		{Scope: "kept-scope", Profile: "builder", Count: 1},
	}); err != nil {
		t.Fatal(err)
	}
	got := health.Snapshot().ScopeQueues
	if len(got) != 1 || got[0].Scope != "kept-scope" {
		t.Fatalf("ScopeQueues = %#v, want only kept-scope", got)
	}
}

// TestSetScopeQueuesRejectsIncoherentRows proves an unusable row is refused
// rather than published: a scope or profile with no name cannot be attributed to
// anything, and a negative count is not a queue depth.
func TestSetScopeQueuesRejectsIncoherentRows(t *testing.T) {
	for _, row := range []ScopeQueueMetrics{
		{Scope: "", Profile: "builder", Count: 1},
		{Scope: "s", Profile: "", Count: 1},
		{Scope: "s", Profile: "builder", Count: -1},
	} {
		health := scopeQueueHealth(t)
		if err := health.SetScopeQueues([]ScopeQueueMetrics{row}); !errors.Is(err, errInvalidMetric) {
			t.Fatalf("SetScopeQueues(%#v) = %v, want errInvalidMetric", row, err)
		}
		if got := health.Snapshot().ScopeQueues; len(got) != 0 {
			t.Fatalf("a refused row was still published: %#v", got)
		}
	}
}

// TestScopeQueueSnapshotIsACopy proves callers cannot mutate published telemetry
// through the returned slice.
func TestScopeQueueSnapshotIsACopy(t *testing.T) {
	health := scopeQueueHealth(t)
	if err := health.SetScopeQueues([]ScopeQueueMetrics{{Scope: "s", Profile: "builder", Count: 2}}); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot().ScopeQueues
	snapshot[0].Count = 999
	if again := health.Snapshot().ScopeQueues; again[0].Count != 2 {
		t.Fatalf("snapshot mutation leaked into telemetry: %#v", again[0])
	}
}

// TestStatusEnvelopePublishesScopeQueues proves the per-scope breakdown reaches
// the versioned API, with age derived only from a real enqueue time: a row that
// has never queued anything must report zero rather than an age measured from the
// zero clock.
func TestStatusEnvelopePublishesScopeQueues(t *testing.T) {
	clock := &fakeClock{now: time.Unix(4000, 0).UTC()}
	health, err := NewHealth(clock, HealthConfig{Profiles: []string{"builder"},
		CriticalObservations: []string{"scheduler"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetScopeQueues([]ScopeQueueMetrics{
		{Scope: "budgie-org", Profile: "builder", ScaleSetID: 1, Count: 4,
			OldestEnqueuedAt: time.Unix(1000, 0).UTC()},
		{Scope: "idle-repo", Profile: "builder", ScaleSetID: 2, Count: 0},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(health, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, server, http.MethodGet, adminapi.StatusPath)
	defer response.Body.Close()
	var envelope adminapi.StatusEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	rows := envelope.Data.ScopeQueues
	if len(rows) != 2 {
		t.Fatalf("scopeQueues = %#v, want 2 rows", rows)
	}
	if rows[0].Scope != "budgie-org" || rows[0].Jobs != 4 || rows[0].OldestAgeSeconds != 3000 {
		t.Fatalf("busy row = %#v, want age 3000s", rows[0])
	}
	if rows[1].Scope != "idle-repo" || rows[1].OldestAgeSeconds != 0 {
		t.Fatalf("idle row = %#v, want zero age", rows[1])
	}
}
