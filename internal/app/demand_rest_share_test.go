package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// shardedDemandStore is a REST store whose statistics and durable demand are
// keyed per scale set, which is exactly the fact the single-statistics fake in
// demand_test.go cannot express and the whole subject of issue #153.
type shardedDemandStore struct {
	statistics map[int64]operations.DemandStatistics
	records    map[int64][]operations.DemandRecord
	jobs       map[int64][]operations.GitHubJobObservation
	statsErr   error
	recordErr  error
}

func newShardedDemandStore() *shardedDemandStore {
	return &shardedDemandStore{statistics: map[int64]operations.DemandStatistics{},
		records: map[int64][]operations.DemandRecord{}, jobs: map[int64][]operations.GitHubJobObservation{}}
}

func (s *shardedDemandStore) ApplyDemandBatch(context.Context, int64, int64, []operations.DemandEvent) (bool, error) {
	return false, nil
}

func (s *shardedDemandStore) ActiveDemands(_ context.Context, scaleSetID int64) ([]operations.DemandRecord, error) {
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	return append([]operations.DemandRecord(nil), s.records[scaleSetID]...), nil
}

func (s *shardedDemandStore) PutDemandStatistics(_ context.Context, scaleSetID int64, statistics operations.DemandStatistics) (bool, error) {
	s.statistics[scaleSetID] = statistics
	return true, nil
}

func (s *shardedDemandStore) DemandStatistics(_ context.Context, scaleSetID int64) (operations.DemandStatistics, error) {
	if s.statsErr != nil {
		return operations.DemandStatistics{}, s.statsErr
	}
	statistics, ok := s.statistics[scaleSetID]
	if !ok {
		return operations.DemandStatistics{}, operations.ErrNotFound
	}
	return statistics, nil
}

func (s *shardedDemandStore) ReconcileGitHubJobSnapshot(_ context.Context, _ time.Time,
	snapshot map[int64][]operations.GitHubJobObservation,
) (bool, error) {
	changed := false
	for scaleSetID, jobs := range snapshot {
		s.jobs[scaleSetID] = append([]operations.GitHubJobObservation(nil), jobs...)
		changed = changed || len(jobs) > 0
	}
	return changed, nil
}

func (s *shardedDemandStore) QueuedGitHubJobs(_ context.Context, scaleSetID int64) ([]operations.GitHubJobObservation, error) {
	return append([]operations.GitHubJobObservation(nil), s.jobs[scaleSetID]...), nil
}

// attributed reduces one binding's stored snapshot to the workflow job IDs it
// claimed, which is the number issue #153 bounds.
func (s *shardedDemandStore) attributed(scaleSetID int64) []int64 {
	ids := make([]int64, 0, len(s.jobs[scaleSetID]))
	for _, job := range s.jobs[scaleSetID] {
		ids = append(ids, job.WorkflowJobID)
	}
	return ids
}

var restShareEpoch = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// sharedLabelBinding is one node's scale set in a federated scope: same scope,
// same profile, byte-identical advertised labels, distinct durable key, and the
// declaration that says a peer node advertises them too.
func sharedLabelBinding(key int64) Binding {
	binding := soleBinding(key)
	binding.SharedLabels = true
	return binding
}

// soleBinding is the ordinary undeclared scale set every scope in the fleet has
// today: nothing else advertises its labels, so nothing bounds its claim.
func soleBinding(key int64) Binding {
	return Binding{StoreKey: key, ScaleSetID: key, Scope: "sudoku-repo", Targets: []string{"owner/repo"},
		ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
		Profile:        domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}}
}

// restJob is one repository-wide queued job carrying the shared label. age
// orders the queue, so a truncation that is not oldest-first is visible.
func restJob(id int64, age time.Duration) githubscaleset.WorkflowJob {
	return githubscaleset.WorkflowJob{ID: id, RunID: id * 10, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"},
		Name:       "Maestro", Status: "queued", Labels: []string{"self-hosted", "macos-maestro"},
		CreatedAt: restShareEpoch.Add(-age), QueueTimeExact: true}
}

func freshStatistics(available, assigned, running int) operations.DemandStatistics {
	return operations.DemandStatistics{MessageID: 1, Available: available, Assigned: assigned,
		Acquired: assigned, Running: running, Registered: assigned, Busy: running,
		ObservedAt: restShareEpoch.Add(-30 * time.Second)}
}

// TestRESTAttributionIsBoundedByTheScaleSetsOwnShare is the seam of issue #153.
//
// The REST inventory lane attributes repository-wide queued jobs to a binding by
// label match alone. Under ADR 0034's shared-label federation two nodes own two
// scale sets in ONE scope with identical labels, so that match hands BOTH nodes
// the whole scope backlog and each would derive demand for work GitHub gave the
// other. The bound is the scale set's own statistics, already ingested from
// every broker message.
func TestRESTAttributionIsBoundedByTheScaleSetsOwnShare(t *testing.T) {
	t.Parallel()
	nodeA, nodeC := sharedLabelBinding(1), sharedLabelBinding(2)
	four := []githubscaleset.WorkflowJob{restJob(101, 4*time.Minute), restJob(102, 3*time.Minute),
		restJob(103, 2*time.Minute), restJob(104, time.Minute)}

	for _, testCase := range []struct {
		name       string
		bindings   []Binding
		jobs       []githubscaleset.WorkflowJob
		statistics map[int64]operations.DemandStatistics
		records    map[int64][]operations.DemandRecord
		want       map[int64][]int64
	}{
		{
			// GitHub filled node A to its advertised 1 and node C to its
			// advertised 3, which is the exact split the #144 spike measured.
			// Neither node can name WHICH job is its own from REST alone, and it
			// does not have to: each claims a count it can serve, oldest first,
			// and the broker is what later binds a specific job to a specific
			// set. The four queued jobs produce a claim of one and a claim of
			// three, not two claims of four.
			name:     "shared label splits by each set's own assigned count",
			bindings: []Binding{nodeA, nodeC}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(0, 1, 0), 2: freshStatistics(0, 3, 0)},
			want:       map[int64][]int64{1: {101}, 2: {101, 102, 103}},
		},
		{
			// A set holding no assignment claims nothing, however loudly the
			// scope's queue matches its labels. Fail closed: the peer serves it.
			name:     "a set GitHub assigned nothing claims nothing",
			bindings: []Binding{nodeA, nodeC}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(0, 0, 0), 2: freshStatistics(0, 4, 0)},
			want:       map[int64][]int64{1: {}, 2: {101, 102, 103, 104}},
		},
		{
			// Running work is not queued work: a set whose whole assignment is
			// already executing has no claim on the scope's remaining backlog.
			name:     "assignments already running do not buy a claim on the queue",
			bindings: []Binding{nodeA, nodeC}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(0, 2, 2), 2: freshStatistics(0, 4, 0)},
			want:       map[int64][]int64{1: {}, 2: {101, 102, 103, 104}},
		},
		{
			// The classic JobAvailable flow: the queue is Available, not
			// Assigned, and the lookahead ADR 0015 exists for must survive.
			name:     "available work is the set's own share in the JobAvailable flow",
			bindings: []Binding{nodeA}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(4, 0, 0)},
			want:       map[int64][]int64{1: {101, 102, 103, 104}},
		},
		{
			// A single-set scope is bounded by the same rule and never notices
			// it: nothing else in the scope can be competing for the backlog.
			name:     "a single set claims its whole backlog",
			bindings: []Binding{nodeA}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(2, 2, 0)},
			want:       map[int64][]int64{1: {101, 102, 103, 104}},
		},
		{
			// An undeclared scale set alone on its labels is never bounded at
			// all: no peer can be holding this backlog, and ADR 0015's queue
			// lookahead past this set's own advertised capacity is the whole
			// reason the REST lane exists.
			name:     "an undeclared scale set keeps the ADR 0015 lookahead",
			bindings: []Binding{soleBinding(1)}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(0, 1, 0)},
			want:       map[int64][]int64{1: {101, 102, 103, 104}},
		},
		{
			// Statistics are the bound, so an absent bound is not a bound of
			// zero. ADR 0026 expires demand from what a COMPLETE snapshot did
			// not contain; silently shrinking one on a statistics outage would
			// expire genuinely queued work.
			name:     "an unobserved scale set is not a scale set with no share",
			bindings: []Binding{nodeA}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{},
			want:       map[int64][]int64{1: {101, 102, 103, 104}},
		},
		{
			name:     "stale statistics do not bound a fresh observation",
			bindings: []Binding{nodeA}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: {MessageID: 1, Available: 1,
				ObservedAt: restShareEpoch.Add(-time.Hour)}},
			want: map[int64][]int64{1: {101, 102, 103, 104}},
		},
		{
			// The broker vouching for a job is stronger evidence of ownership
			// than any counter, so a job this binding's own durable demand names
			// is never surrendered to the cap.
			name:     "a job the binding's own demand names is never dropped",
			bindings: []Binding{nodeA, nodeC}, jobs: four,
			statistics: map[int64]operations.DemandStatistics{1: freshStatistics(0, 1, 0), 2: freshStatistics(0, 3, 0)},
			records: map[int64][]operations.DemandRecord{1: {{ScaleSetID: 1, RunnerRequestID: 9, Owner: "owner",
				Repository: "repo", WorkflowRunID: 1_040, DisplayName: "Maestro", Status: operations.DemandJobAvailable}}},
			want: map[int64][]int64{1: {104}, 2: {101, 102, 103}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			store := newShardedDemandStore()
			for key, statistics := range testCase.statistics {
				store.statistics[key] = statistics
			}
			for key, records := range testCase.records {
				store.records[key] = records
			}
			coordinator := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
			if _, err := coordinator.ReconcileQueuedJobs(context.Background(), testCase.bindings,
				fakeQueueSnapshot{at: restShareEpoch, jobs: testCase.jobs}); err != nil {
				t.Fatalf("reconcile REST snapshot: %v", err)
			}
			for key, want := range testCase.want {
				if got := store.attributed(key); !reflect.DeepEqual(got, want) {
					t.Fatalf("scale set %d attributed %v, want %v", key, got, want)
				}
			}
		})
	}
}

// TestRESTAttributionSurvivesTheSharedLabelTopology states the relaxation the
// cap makes safe. ADR 0015 fails a whole scope observation closed when one job
// matches two scale sets, which under a shared label is EVERY job and would make
// the lane unusable on a federated scope. Two bindings that serve one scope with
// one profile are interchangeable servers of that job, so both may record it,
// each bounded by its own share. Anything else remains ambiguous.
func TestRESTAttributionSurvivesTheSharedLabelTopology(t *testing.T) {
	t.Parallel()
	job := restJob(701, time.Minute)
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{job}}

	federated := []Binding{sharedLabelBinding(1), sharedLabelBinding(2)}
	store := newShardedDemandStore()
	store.statistics[1] = freshStatistics(0, 1, 0)
	store.statistics[2] = freshStatistics(0, 1, 0)
	coordinator := DemandCoordinator{Store: store, StrictJobRouting: true, Now: func() time.Time { return restShareEpoch }}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), federated, snapshot); err != nil {
		t.Fatalf("shared-label scope must not fail the scope observation closed: %v", err)
	}
	if got := store.attributed(1); len(got) != 1 {
		t.Fatalf("federated peer A attributed %v, want the one job it was assigned", got)
	}
	if got := store.attributed(2); len(got) != 1 {
		t.Fatalf("federated peer C attributed %v, want the one job it was assigned", got)
	}

	// Two profiles are not two peers: a job matching a builder set and a maestro
	// set names two different VM shapes and is still genuine ambiguity.
	shapes := []Binding{sharedLabelBinding(1), sharedLabelBinding(2)}
	shapes[1].Profile = domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), shapes, snapshot); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("two profiles claiming one job = %v, want a conflict", err)
	}

	// Two scopes are not two peers either: that is a repository routed to two
	// different GitHub scopes, which no capacity number can disambiguate.
	scopes := []Binding{sharedLabelBinding(1), sharedLabelBinding(2)}
	scopes[1].Scope = "knee-doctor"
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), scopes, snapshot); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("two scopes claiming one job = %v, want a conflict", err)
	}
}

// TestRESTAttributionFailsTheSnapshotWhenItsOwnBoundIsUnreadable keeps the bound
// evidential. A store that cannot answer what this scale set holds has not
// proven a share of zero, and the snapshot may not be committed on a guess.
func TestRESTAttributionFailsTheSnapshotWhenItsOwnBoundIsUnreadable(t *testing.T) {
	t.Parallel()
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{
		restJob(801, 2*time.Minute), restJob(802, time.Minute)}}
	bindings := []Binding{sharedLabelBinding(1)}

	store := newShardedDemandStore()
	store.statsErr = errors.New("statistics unreadable")
	coordinator := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, snapshot); err == nil {
		t.Fatal("an unreadable statistics store must fail the scope observation")
	}

	store = newShardedDemandStore()
	store.statistics[1] = freshStatistics(0, 1, 0)
	store.recordErr = errors.New("demand unreadable")
	coordinator = DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, snapshot); err == nil {
		t.Fatal("unreadable durable demand must fail the scope observation")
	}
}

// TestQueueSummaryReportsOnlyTheSetsOwnShareOfAFederatedScope is the composition
// point: QueueSummary lets REST LEAD the broker (the ADR 0015 lookahead), so an
// uncapped attribution would double-count one scope's queue across two nodes and
// report a backlog neither node can serve.
func TestQueueSummaryReportsOnlyTheSetsOwnShareOfAFederatedScope(t *testing.T) {
	t.Parallel()
	store := newShardedDemandStore()
	store.statistics[1] = freshStatistics(0, 1, 0)
	store.statistics[2] = freshStatistics(0, 3, 0)
	bindings := []Binding{sharedLabelBinding(1), sharedLabelBinding(2)}
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{
		restJob(101, 4*time.Minute), restJob(102, 3*time.Minute), restJob(103, 2*time.Minute), restJob(104, time.Minute)}}
	coordinator := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, snapshot); err != nil {
		t.Fatalf("reconcile REST snapshot: %v", err)
	}
	total := 0
	for _, binding := range bindings {
		summary, err := coordinator.QueueSummary(context.Background(), binding, nil)
		if err != nil {
			t.Fatalf("queue summary: %v", err)
		}
		total += summary.Count
	}
	if total != 4 {
		t.Fatalf("federated scope reported a queue of %d for four queued jobs", total)
	}
}
