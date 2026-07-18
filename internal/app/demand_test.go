package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeDemandStore struct {
	batches     []operations.DemandEvent
	records     []operations.DemandRecord
	scaleSetID  int64
	cursor      int64
	applied     bool
	err         error
	projected   []operations.DemandEvent
	projectErr  error
	statistics  operations.DemandStatistics
	statsWrites int
	githubJobs  map[int64][]operations.GitHubJobObservation
}

type noStatisticsStore struct{ inner *fakeDemandStore }

func (s noStatisticsStore) ApplyDemandBatch(ctx context.Context, scaleSetID, messageID int64, events []operations.DemandEvent) (bool, error) {
	return s.inner.ApplyDemandBatch(ctx, scaleSetID, messageID, events)
}
func (s noStatisticsStore) ActiveDemands(ctx context.Context, scaleSetID int64) ([]operations.DemandRecord, error) {
	return s.inner.ActiveDemands(ctx, scaleSetID)
}

type statisticsReadErrorStore struct{ *fakeDemandStore }

func (s statisticsReadErrorStore) DemandStatistics(context.Context, int64) (operations.DemandStatistics, error) {
	return operations.DemandStatistics{}, errors.New("statistics down")
}

type statisticsNotFoundStore struct{ *fakeDemandStore }

func (s statisticsNotFoundStore) DemandStatistics(context.Context, int64) (operations.DemandStatistics, error) {
	return operations.DemandStatistics{}, operations.ErrNotFound
}

func (f *fakeDemandStore) ReconcileGitHubJobSnapshot(_ context.Context, _ time.Time, snapshot map[int64][]operations.GitHubJobObservation) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.githubJobs == nil {
		f.githubJobs = map[int64][]operations.GitHubJobObservation{}
	}
	changed := false
	for scaleSetID, jobs := range snapshot {
		f.githubJobs[scaleSetID] = append([]operations.GitHubJobObservation(nil), jobs...)
		changed = changed || len(jobs) > 0
	}
	return changed, nil
}

func (f *fakeDemandStore) QueuedGitHubJobs(_ context.Context, scaleSetID int64) ([]operations.GitHubJobObservation, error) {
	return append([]operations.GitHubJobObservation(nil), f.githubJobs[scaleSetID]...), f.err
}

func (f *fakeDemandStore) PutDemandStatistics(_ context.Context, _ int64, statistics operations.DemandStatistics) (bool, error) {
	f.statistics = statistics
	f.statsWrites++
	return true, f.err
}

type fakeQueueSnapshot struct {
	at   time.Time
	jobs []githubscaleset.WorkflowJob
}

func (f fakeQueueSnapshot) ObservedAt() time.Time { return f.at }
func (f fakeQueueSnapshot) QueuedJobs() []githubscaleset.WorkflowJob {
	return append([]githubscaleset.WorkflowJob(nil), f.jobs...)
}

func TestRESTQueueReconciliationUsesScaleSetLabelCompatibility(t *testing.T) {
	store := &fakeDemandStore{}
	at := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	snapshot := fakeQueueSnapshot{at: at, jobs: []githubscaleset.WorkflowJob{
		{ID: 101, RunID: 11, RunAttempt: 2, Repository: githubscaleset.Repository{Owner: "budgie-at", Name: "budgie"},
			Name: "Build iOS E2E app", Status: "queued", Labels: []string{"self-hosted", "macOS", "ARM64", "macos-tartelet"}, CreatedAt: at.Add(-time.Hour), QueueTimeExact: true},
		{ID: 102, RunID: 12, RunAttempt: 1, Repository: githubscaleset.Repository{Owner: "budgie-at", Name: "budgie"},
			Name: "Maestro", Status: "queued", Labels: []string{"self-hosted", "macos-maestro"}, CreatedAt: at.Add(-time.Minute), QueueTimeExact: true},
		{ID: 103, RunID: 13, RunAttempt: 1, Repository: githubscaleset.Repository{Owner: "budgie-at", Name: "budgie"},
			Name: "Code quality", Status: "queued", Labels: []string{"self-hosted", "linux-ci"}, CreatedAt: at.Add(-time.Minute), QueueTimeExact: true},
		{ID: 104, RunID: 14, RunAttempt: 1, Repository: githubscaleset.Repository{Owner: "budgie-at", Name: "budgie"},
			Name: "Named scale set", Status: "queued", Labels: []string{"self-hosted", "trf-budgie-large"}, CreatedAt: at.Add(-time.Minute), QueueTimeExact: true},
	}}
	bindings := []Binding{
		{StoreKey: 201, ScaleSetID: 1, Scope: "budgie", Targets: []string{"budgie-at/budgie"},
			ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-builder", "macos-tartelet"},
			Profile:        domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}},
		{StoreKey: 202, ScaleSetID: 2, Scope: "budgie", Targets: []string{"budgie-at/budgie"},
			ScaleSetLabels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"},
			Profile:        domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{StoreKey: 203, ScaleSetID: 3, Scope: "budgie", Targets: []string{"budgie-at/budgie"},
			ScaleSetLabels: []string{"self-hosted", "linux-tiered", "linux-large", "linux-ci", "linux-burst", "trf-budgie-large"},
			Profile:        domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}},
		{StoreKey: 204, ScaleSetID: 4, Scope: "budgie", Targets: []string{"budgie-at/budgie"},
			ScaleSetLabels: []string{"self-hosted", "linux-tiered", "linux-small"},
			Profile:        domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}},
	}
	changed, err := (DemandCoordinator{Store: store}).ReconcileQueuedJobs(context.Background(), bindings, snapshot)
	if err != nil || !changed || len(store.githubJobs[201]) != 1 || store.githubJobs[201][0].WorkflowJobID != 101 ||
		len(store.githubJobs[202]) != 1 || store.githubJobs[202][0].WorkflowJobID != 102 ||
		len(store.githubJobs[203]) != 2 || store.githubJobs[203][0].WorkflowJobID != 103 || store.githubJobs[203][1].WorkflowJobID != 104 ||
		len(store.githubJobs[204]) != 0 {
		t.Fatalf("REST routing = %#v, changed=%v err=%v", store.githubJobs, changed, err)
	}
	guarded := bindings[2]
	guarded.RequiredLabels = []string{"tart-fleet-canary"}
	if guarded.matchesRESTLabels([]string{"self-hosted", "linux-ci"}) {
		t.Fatal("REST routing bypassed the exact-scope canary label guard")
	}
}

func TestRESTQueueReconciliationValidationAndStoreFailure(t *testing.T) {
	now := time.Now().UTC()
	valid := Binding{ScaleSetID: 1, Profile: domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}}
	snapshot := fakeQueueSnapshot{at: now}
	if _, err := (DemandCoordinator{}).ReconcileQueuedJobs(context.Background(), []Binding{valid}, snapshot); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("missing REST store = %v", err)
	}
	store := &fakeDemandStore{}
	if _, err := (DemandCoordinator{Store: store}).ReconcileQueuedJobs(context.Background(), []Binding{{}}, snapshot); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid REST binding = %v", err)
	}
	if _, err := (DemandCoordinator{Store: store}).ReconcileQueuedJobs(context.Background(), []Binding{valid}, fakeQueueSnapshot{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid REST snapshot = %v", err)
	}
	store.err = errors.New("store")
	if changed, err := (DemandCoordinator{Store: store}).ReconcileQueuedJobs(context.Background(), []Binding{valid}, snapshot); err == nil || changed {
		t.Fatalf("REST store failure = %v, %v", changed, err)
	}
	if containsFold([]string{"one"}, "two") {
		t.Fatal("label mismatch accepted")
	}
}

func TestRESTQueueReconciliationFailsClosedOnUnroutableOrAmbiguousSelfHostedJob(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	job := githubscaleset.WorkflowJob{ID: 501, RunID: 50, RunAttempt: 1,
		Repository: githubscaleset.Repository{Owner: "owner", Name: "repo"}, Name: "quality", Status: "queued",
		Labels: []string{"self-hosted", "linux-burst"}, CreatedAt: now.Add(-time.Minute), QueueTimeExact: true}
	snapshot := fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{job}}
	bindings := []Binding{
		{ScaleSetID: 1, Targets: []string{"owner/repo"}, ScaleSetLabels: []string{"self-hosted", "linux-small"},
			Profile: domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}},
		{ScaleSetID: 2, Targets: []string{"owner/repo"}, ScaleSetLabels: []string{"self-hosted", "linux-large"},
			Profile: domain.Profile{ID: "large", Route: "linux-large", Platform: domain.PlatformLinux}},
	}
	coordinator := DemandCoordinator{Store: &fakeDemandStore{}, StrictJobRouting: true}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, snapshot); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("unroutable self-hosted job = %v", err)
	}
	bindings[0].ScaleSetLabels = append(bindings[0].ScaleSetLabels, "linux-burst")
	bindings[1].ScaleSetLabels = append(bindings[1].ScaleSetLabels, "linux-burst")
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, snapshot); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("ambiguous self-hosted job = %v", err)
	}
	job.Labels = []string{"ubuntu-latest"}
	if _, err := coordinator.ReconcileQueuedJobs(context.Background(), bindings, fakeQueueSnapshot{at: now, jobs: []githubscaleset.WorkflowJob{job}}); err != nil {
		t.Fatalf("GitHub-hosted job affected fleet routing: %v", err)
	}
}

func TestQueueSummaryIncludesRESTOnlyBacklogWithoutMakingItExecutable(t *testing.T) {
	oldest := time.Date(2026, 7, 17, 9, 47, 52, 0, time.UTC)
	store := &fakeDemandStore{githubJobs: map[int64][]operations.GitHubJobObservation{3: {
		{WorkflowJobID: 100, CreatedAt: oldest, QueueTimeExact: true},
		{WorkflowJobID: 101, CreatedAt: oldest.Add(time.Minute), QueueTimeExact: true},
	}}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	executable := []domain.Demand{{CreatedAt: oldest.Add(10 * time.Minute)}}
	summary, err := (DemandCoordinator{Store: store}).QueueSummary(context.Background(), binding, executable)
	if err != nil || summary.Count != 2 || summary.Oldest != oldest || len(executable) != 1 {
		t.Fatalf("queue summary = %#v, %v", summary, err)
	}
}

func TestQueueSummaryDoesNotTurnInferredRunCreationIntoQueueSLOAge(t *testing.T) {
	protocolAge := time.Date(2026, 7, 17, 11, 59, 0, 0, time.UTC)
	store := &fakeDemandStore{githubJobs: map[int64][]operations.GitHubJobObservation{3: {
		{WorkflowJobID: 100, CreatedAt: protocolAge.Add(-time.Hour), QueueTimeExact: false},
	}}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	summary, err := (DemandCoordinator{Store: store}).QueueSummary(context.Background(), binding, []domain.Demand{{CreatedAt: protocolAge}})
	if err != nil || summary.Count != 1 || summary.Oldest != protocolAge {
		t.Fatalf("queue summary trusted inferred REST time = %#v, %v", summary, err)
	}
}

func TestQueueSummaryPropagatesObservationFailure(t *testing.T) {
	store := &fakeDemandStore{err: errors.New("down")}
	binding := Binding{ScaleSetID: 1, Profile: domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}}
	if _, err := (DemandCoordinator{Store: store}).QueueSummary(context.Background(), binding, nil); err == nil {
		t.Fatal("queue observation failure ignored")
	}
}

func TestQueueSummaryWorksWithoutRESTInventory(t *testing.T) {
	oldest := time.Now().UTC().Add(-time.Minute)
	summary, err := (DemandCoordinator{Store: noStatisticsStore{inner: &fakeDemandStore{}}}).QueueSummary(context.Background(), Binding{}, []domain.Demand{{CreatedAt: oldest}})
	if err != nil || summary.Count != 1 || summary.Oldest != oldest {
		t.Fatalf("protocol-only summary = %#v, %v", summary, err)
	}
}

func (f *fakeDemandStore) DemandStatistics(context.Context, int64) (operations.DemandStatistics, error) {
	return f.statistics, f.err
}

func (f *fakeDemandStore) ProjectDemandEvent(_ context.Context, _ int64, event operations.DemandEvent) error {
	f.projected = append(f.projected, event)
	return f.projectErr
}

func (f *fakeDemandStore) ApplyDemandBatch(_ context.Context, scaleSetID, messageID int64, events []operations.DemandEvent) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.cursor = messageID
	f.scaleSetID = scaleSetID
	f.batches = append([]operations.DemandEvent(nil), events...)
	return f.applied, nil
}
func (f *fakeDemandStore) ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error) {
	return append([]operations.DemandRecord(nil), f.records...), f.err
}

type fakeMessages struct {
	demand         githubscaleset.Demand
	err            error
	afterCommitErr error
	committed      bool
}

func (f *fakeMessages) Handle(ctx context.Context, commit func(context.Context, githubscaleset.Demand) error) error {
	if f.err != nil {
		return f.err
	}
	if err := commit(ctx, f.demand); err != nil {
		return err
	}
	f.committed = true
	return f.afterCommitErr
}

func TestIngestCommitsSanitizedConcreteEvents(t *testing.T) {
	queue := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 7,
		Statistics: githubscaleset.DemandStatistics{MessageID: 7, Available: 1, Assigned: 1, Running: 0}, Events: []githubscaleset.JobEvent{{
			Kind: githubscaleset.JobAvailable, RunnerRequestID: 11, Owner: "owner", Repository: "repo", WorkflowRunID: 22,
			JobID: "uuid", DisplayName: "build", WorkflowRef: "owner/repo/.github/workflows/ci.yml@refs/pull/1/merge",
			EventName: "pull_request", Labels: []string{"self-hosted", "linux-small"}, QueueTime: queue,
		}}}}
	store := &fakeDemandStore{applied: true}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	if err := (DemandCoordinator{Store: store}).IngestOnce(context.Background(), binding, source); err != nil {
		t.Fatal(err)
	}
	if !source.committed || store.cursor != 7 || len(store.batches) != 1 || store.statsWrites != 1 || store.statistics.Available != 1 {
		t.Fatalf("commit = %v cursor=%d events=%#v", source.committed, store.cursor, store.batches)
	}
	got := store.batches[0]
	if got.Kind != operations.DemandJobAvailable || got.RunnerRequestID != 11 || got.DisplayName != "build" || got.WorkflowRef == "" || !reflect.DeepEqual(got.Labels, []string{"self-hosted", "linux-small"}) {
		t.Fatalf("event = %#v", got)
	}
	source.demand.Events[0].Labels[0] = "mutated"
	if store.batches[0].Labels[0] != "self-hosted" {
		t.Fatal("stored conversion aliases source")
	}
}

func TestQueuedDemandsUsesOfficialStatisticsAsAdmissionBound(t *testing.T) {
	queue := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 1, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "repo", WorkflowRunID: 9, QueueTime: queue.Add(time.Second), FirstQueueTime: queue.Add(time.Second)},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: queue, FirstQueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 1 || got[0].Key.JobID != 1 {
		t.Fatalf("statistics admission bound = %#v, %v", got, err)
	}
}

func TestAssignedStatisticsDoNotAuthorizeStaleAvailableRequests(t *testing.T) {
	queue := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Assigned: 2, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 0 {
		t.Fatalf("assigned count admitted stale available request: %#v, %v", got, err)
	}
}

func TestQueuedDemandsPropagatesStatisticsReadFailure(t *testing.T) {
	queue := time.Now().UTC()
	store := statisticsReadErrorStore{fakeDemandStore: &fakeDemandStore{records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: queue},
	}}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	if _, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding); err == nil {
		t.Fatal("statistics read failure ignored")
	}
}

func TestQueuedDemandsFailsClosedWithoutFreshOfficialStatistics(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	record := operations.DemandRecord{Status: operations.DemandJobAvailable, RunnerRequestID: 1,
		Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: now.Add(-time.Minute)}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	for _, test := range []struct {
		name       string
		store      DemandStore
		wantReason string
	}{
		{name: "missing store", store: noStatisticsStore{inner: &fakeDemandStore{records: []operations.DemandRecord{record}}}, wantReason: "unavailable"},
		{name: "missing snapshot", store: statisticsNotFoundStore{fakeDemandStore: &fakeDemandStore{records: []operations.DemandRecord{record}}}, wantReason: "not observed"},
		{name: "stale snapshot", store: &fakeDemandStore{records: []operations.DemandRecord{record}, statistics: operations.DemandStatistics{
			MessageID: 9, Available: 1, ObservedAt: now.Add(-3 * time.Minute)}}, wantReason: "stale or invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (DemandCoordinator{Store: test.store, Now: func() time.Time { return now }, StatisticsMaxAge: time.Minute}).QueuedDemands(context.Background(), binding)
			if !errors.Is(err, operations.ErrUncertain) || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("QueuedDemands() error = %v", err)
			}
		})
	}
	if got, err := (DemandCoordinator{Store: noStatisticsStore{inner: &fakeDemandStore{}}, Now: func() time.Time { return now }}).
		QueuedDemands(context.Background(), binding); err != nil || len(got) != 0 {
		t.Fatalf("idle queue required statistics: %#v, %v", got, err)
	}
}

func TestPreassignedStatisticsAuthorizeOnlySyntheticRequests(t *testing.T) {
	queue := time.Now().UTC()
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Assigned: 1, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1 << 62, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: queue},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1<<62 + 1, Owner: "owner", Repository: "repo", WorkflowRunID: 9, QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 1 || got[0].Key.JobID != 1<<62 {
		t.Fatalf("preassigned bound = %#v, %v", got, err)
	}
}

func TestRunningStatisticsCannotMakePreassignedCapacityNegative(t *testing.T) {
	queue := time.Now().UTC()
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Assigned: 1, Running: 2, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1 << 62, Owner: "owner", Repository: "repo", WorkflowRunID: 8, QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 0 {
		t.Fatalf("negative preassigned capacity admitted demand: %#v, %v", got, err)
	}
}

func TestQueuedDemandsCollapsesRotatedAliasesToNewestRequestAndOriginalAge(t *testing.T) {
	original := time.Date(2026, 7, 17, 9, 47, 52, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 1, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 700, Owner: "budgie-at", Repository: "budgie", WorkflowRunID: 77,
			LogicalKey: "canonical", QueueTime: original.Add(time.Hour), FirstQueueTime: original, UpdatedAt: original.Add(time.Hour)},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 701, Owner: "budgie-at", Repository: "budgie", WorkflowRunID: 77,
			LogicalKey: "canonical", QueueTime: original.Add(2 * time.Hour), FirstQueueTime: original, RunAttempt: 3, UpdatedAt: original.Add(2 * time.Hour)},
	}}
	binding := Binding{ScaleSetID: 3, Targets: []string{"budgie-at/budgie"}, Profile: domain.Profile{ID: "builder", Route: "macos-builder", Platform: domain.PlatformMacOS}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 1 || got[0].Key.JobID != 701 || got[0].Key.Attempt != 3 || got[0].CreatedAt != original {
		t.Fatalf("canonical demand = %#v, %v", got, err)
	}
}

func TestIngestResultPreservesDurableChangeAcrossAcknowledgementFailure(t *testing.T) {
	want := errors.New("ack failed")
	store := &fakeDemandStore{applied: true}
	source := &fakeMessages{afterCommitErr: want, demand: githubscaleset.Demand{MessageID: 7}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	changed, err := (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source)
	if !changed || !errors.Is(err, want) {
		t.Fatalf("IngestOnceResult() = %v, %v; want durable change plus acknowledgement error", changed, err)
	}
	store.applied = false
	source.afterCommitErr = nil
	changed, err = (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source)
	if changed || err != nil {
		t.Fatalf("duplicate IngestOnceResult() = %v, %v", changed, err)
	}
}

func TestIngestRequiresAndPersistsOfficialStatistics(t *testing.T) {
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 7, Statistics: githubscaleset.DemandStatistics{MessageID: 7}}}
	if _, err := (DemandCoordinator{Store: noStatisticsStore{inner: &fakeDemandStore{}}}).IngestOnceResult(context.Background(), binding, source); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("missing statistics persistence = %v", err)
	}
	store := &fakeDemandStore{err: errors.New("write")}
	if _, err := (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source); err == nil {
		t.Fatal("statistics persistence failure ignored")
	}
}

func TestQueuedDemandsMapsOnlyAvailableRecords(t *testing.T) {
	queue := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 3, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAssigned, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 9},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "schedule", QueueTime: queue},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 3, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "push", QueueTime: queue.Add(time.Second)},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 4, Owner: "owner", Repository: "repo", WorkflowRunID: 9, EventName: "pull_request_target", QueueTime: queue.Add(2 * time.Second)},
	}}
	binding := Binding{ScaleSetID: 3, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 3 {
		t.Fatalf("QueuedDemands() = %#v, %v", got, err)
	}
	if got[0].Key.JobID != 2 || got[0].Key.Attempt != 1 || got[0].Event != domain.EventSchedule || got[1].Event != domain.EventPush || got[2].Event != domain.EventPullRequest {
		t.Fatalf("mapped demands = %#v", got)
	}
	if got[0].Profile != "small" || got[0].Route != "tiered" || got[0].Platform != domain.PlatformLinux || got[0].CreatedAt != queue {
		t.Fatalf("profile mapping = %#v", got[0])
	}
}

func TestQueuedDemandsFiltersRecordsOutsideTheBindingScope(t *testing.T) {
	queue := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := &fakeDemandStore{statistics: operations.DemandStatistics{MessageID: 9, Available: 1, ObservedAt: time.Now().UTC()}, records: []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "other", WorkflowRunID: 9, QueueTime: queue},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "allowed", WorkflowRunID: 9, QueueTime: queue},
	}}
	binding := Binding{ScaleSetID: 3, Targets: []string{"owner/allowed"}, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	got, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(got) != 1 || got[0].Key.JobID != 2 {
		t.Fatalf("scoped QueuedDemands() = %#v, %v", got, err)
	}
}

func TestDemandCoordinatorFailsClosed(t *testing.T) {
	want := errors.New("down")
	validBinding := Binding{ScaleSetID: 1, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	tests := []struct {
		name        string
		coordinator DemandCoordinator
		binding     Binding
		source      MessageSource
	}{
		{name: "nil store", binding: validBinding, source: &fakeMessages{}},
		{name: "bad binding", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, source: &fakeMessages{}},
		{name: "nil source", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, binding: validBinding},
		{name: "source error", coordinator: DemandCoordinator{Store: &fakeDemandStore{}}, binding: validBinding, source: &fakeMessages{err: want}},
		{name: "store error", coordinator: DemandCoordinator{Store: &fakeDemandStore{err: want}}, binding: validBinding, source: &fakeMessages{demand: githubscaleset.Demand{MessageID: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.coordinator.IngestOnce(context.Background(), tt.binding, tt.source); err == nil {
				t.Fatal("IngestOnce() unexpectedly succeeded")
			}
		})
	}
	if _, err := (DemandCoordinator{}).QueuedDemands(context.Background(), validBinding); err == nil {
		t.Fatal("nil-store QueuedDemands succeeded")
	}
	if _, err := (DemandCoordinator{Store: &fakeDemandStore{err: want}}).QueuedDemands(context.Background(), validBinding); !errors.Is(err, want) {
		t.Fatalf("QueuedDemands error = %v", err)
	}
	badRecords := &fakeDemandStore{records: []operations.DemandRecord{{Status: operations.DemandJobAvailable, RunnerRequestID: 1}}}
	if _, err := (DemandCoordinator{Store: badRecords}).QueuedDemands(context.Background(), validBinding); err == nil {
		t.Fatal("incomplete durable demand accepted")
	}
}

func TestDemandCoordinatorFiltersScopeTargetsAndUsesDurableStoreKey(t *testing.T) {
	store := &fakeDemandStore{applied: true}
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 8, Events: []githubscaleset.JobEvent{
		{Kind: githubscaleset.JobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "allowed", WorkflowRunID: 9, QueueTime: time.Now()},
		{Kind: githubscaleset.JobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "other", WorkflowRunID: 10, QueueTime: time.Now()},
	}}}
	binding := Binding{StoreKey: 99, ScaleSetID: 3, Scope: "scope", Targets: []string{"owner/allowed"}, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	if _, err := (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source); err != nil {
		t.Fatal(err)
	}
	if store.scaleSetID != 99 || len(store.batches) != 1 || store.batches[0].Repository != "allowed" || len(store.projected) != 1 {
		t.Fatalf("stored = id %d events %#v", store.scaleSetID, store.batches)
	}
}

func TestCanaryDemandIsolationRequiresDedicatedLabelAtIngestionAndRead(t *testing.T) {
	queue := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	binding := Binding{
		StoreKey: 99, ScaleSetID: 3, Scope: "fleet-repo", Targets: []string{"owner/repo"},
		RequiredLabels: []string{"tart-fleet-canary"},
		Profile:        domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux},
	}
	ordinary := githubscaleset.JobEvent{Kind: githubscaleset.JobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "repo",
		WorkflowRunID: 9, QueueTime: queue, Labels: []string{"self-hosted", "linux-tiered", "linux-small"}}
	canary := ordinary
	canary.RunnerRequestID = 2
	canary.WorkflowRunID = 10
	canary.Labels = append(append([]string(nil), ordinary.Labels...), "tart-fleet-canary")

	store := &fakeDemandStore{applied: true}
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 8,
		Statistics: githubscaleset.DemandStatistics{MessageID: 8, Available: 1},
		Events:     []githubscaleset.JobEvent{ordinary, canary}}}
	if _, err := (DemandCoordinator{Store: store}).IngestOnceResult(context.Background(), binding, source); err != nil {
		t.Fatal(err)
	}
	if len(store.batches) != 1 || store.batches[0].RunnerRequestID != canary.RunnerRequestID {
		t.Fatalf("canary ingest admitted ordinary same-profile demand: %#v", store.batches)
	}

	store.records = []operations.DemandRecord{
		{Status: operations.DemandJobAvailable, RunnerRequestID: 1, Owner: "owner", Repository: "repo", WorkflowRunID: 9,
			QueueTime: queue, Labels: append([]string(nil), ordinary.Labels...)},
		{Status: operations.DemandJobAvailable, RunnerRequestID: 2, Owner: "owner", Repository: "repo", WorkflowRunID: 10,
			QueueTime: queue, Labels: append([]string(nil), canary.Labels...)},
	}
	queued, err := (DemandCoordinator{Store: store}).QueuedDemands(context.Background(), binding)
	if err != nil || len(queued) != 1 || queued[0].Key.JobID != canary.RunnerRequestID {
		t.Fatalf("canary queue isolation = %#v, %v", queued, err)
	}
}

func TestDemandProjectionFailurePreventsAcknowledgement(t *testing.T) {
	want := errors.New("projection")
	store := &fakeDemandStore{applied: true, projectErr: want}
	source := &fakeMessages{demand: githubscaleset.Demand{MessageID: 3, Events: []githubscaleset.JobEvent{{Kind: githubscaleset.JobAssigned,
		RunnerRequestID: 4, Owner: "o", Repository: "r"}}}}
	binding := Binding{ScaleSetID: 1, Profile: domain.Profile{ID: "small", Route: "tiered", Platform: domain.PlatformLinux}}
	changed, err := (DemandCoordinator{Store: store, Projector: store}).IngestOnceResult(context.Background(), binding, source)
	if !changed || !errors.Is(err, want) || source.committed {
		t.Fatalf("projection result = changed %v err %v committed %v", changed, err, source.committed)
	}
}
