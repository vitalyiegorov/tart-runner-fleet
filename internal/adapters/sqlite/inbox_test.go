package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func demandEvent(kind operations.DemandEventKind, requestID int64) operations.DemandEvent {
	return operations.DemandEvent{
		Kind: kind, RunnerRequestID: requestID, Owner: "owner", Repository: "repo", WorkflowRunID: 77,
		JobID: "job-uuid", EventName: "push", Labels: []string{"self-hosted", "linux"}, QueueTime: time.Unix(100, 0).UTC(),
	}
}

func runnerDemandInstance(id string, requestID int64) operations.Instance {
	return operations.Instance{
		ID: id, Repo: "owner/repo", Platform: domain.PlatformMacOS, Profile: "maestro", Route: "macos-maestro",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7168, Slots: 1},
		Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 77, Attempt: 1, JobID: requestID},
		State:     operations.StateRunning,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: fmt.Sprintf("demand-%d", requestID), OperationID: fmt.Sprintf("spawn-%d", requestID)},
	}
}

// Regression for Budgie PR #597: GitHub rotated the scale-set request and
// protocol job UUID while the same REST workflow job remained queued. Queue
// age is a property of the logical job, not the latest broker delivery.
func TestDemandInboxPreservesOriginalAgeAcrossRotatedProtocolIdentities(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	original := time.Date(2026, 7, 17, 9, 47, 52, 0, time.UTC)
	for i, delivered := range []time.Time{
		time.Date(2026, 7, 17, 10, 41, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 10, 46, 0, 0, time.UTC),
		time.Date(2026, 7, 17, 10, 51, 0, 0, time.UTC),
	} {
		event := demandEvent(operations.DemandJobAvailable, int64(700+i))
		event.JobID = fmt.Sprintf("rotated-%d", i)
		event.DisplayName = "Build iOS E2E app"
		event.WorkflowRef = "budgie-at/budgie/.github/workflows/e2e.yml@refs/pull/597/merge"
		event.QueueTime = delivered
		if _, err := store.ApplyDemandBatch(ctx, 7155, int64(i+1), []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
	}
	observation := operations.GitHubJobObservation{WorkflowJobID: 16146281234, Owner: "owner", Repository: "repo",
		WorkflowRunID: 77, RunAttempt: 3, DisplayName: "Build iOS E2E app",
		WorkflowRef: "budgie-at/budgie/.github/workflows/e2e.yml@refs/pull/597/merge",
		Labels:      []string{"self-hosted", "linux"}, Status: "queued", CreatedAt: original, QueueTimeExact: true}
	if _, err := store.ReconcileGitHubJobs(ctx, 7155, time.Date(2026, 7, 17, 10, 52, 0, 0, time.UTC), []operations.GitHubJobObservation{observation}); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveDemands(ctx, 7155)
	if err != nil || len(active) != 3 {
		t.Fatalf("active = %#v, %v", active, err)
	}
	for _, record := range active {
		if record.FirstQueueTime != original || record.RunAttempt != 3 || record.WorkflowJobID != 16146281234 {
			t.Fatalf("rotated demand lost canonical identity: %#v", record)
		}
	}
}

func TestDemandInboxDoesNotGuessAmongSameNameMatrixJobs(t *testing.T) {
	for _, test := range []struct {
		name        string
		attempts    [2]int
		wantAttempt int
	}{
		{name: "common attempt is shared", attempts: [2]int{3, 3}, wantAttempt: 3},
		{name: "conflicting attempts remain unknown", attempts: [2]int{3, 4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			event := demandEvent(operations.DemandJobAvailable, 10)
			event.DisplayName = "E2E (${{ matrix.shard }})"
			if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
				t.Fatal(err)
			}
			jobs := []operations.GitHubJobObservation{
				{WorkflowJobID: 100, Owner: "owner", Repository: "repo", WorkflowRunID: 77, RunAttempt: test.attempts[0], DisplayName: event.DisplayName, Labels: event.Labels, Status: "queued", CreatedAt: time.Unix(50, 0).UTC(), QueueTimeExact: true},
				{WorkflowJobID: 101, Owner: "owner", Repository: "repo", WorkflowRunID: 77, RunAttempt: test.attempts[1], DisplayName: event.DisplayName, Labels: event.Labels, Status: "queued", CreatedAt: time.Unix(51, 0).UTC(), QueueTimeExact: true},
			}
			if _, err := store.ReconcileGitHubJobs(ctx, 1, time.Unix(60, 0).UTC(), jobs); err != nil {
				t.Fatal(err)
			}
			record, err := store.DemandRecord(ctx, 1, 10)
			if err != nil || record.WorkflowJobID != 0 || record.RunAttempt != test.wantAttempt || record.FirstQueueTime != jobs[0].CreatedAt {
				t.Fatalf("ambiguous correlation invented identity: %#v, %v", record, err)
			}
		})
	}
}

func TestDemandInboxStartsFreshQueueAgeForNewRunAttempt(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	event := demandEvent(operations.DemandJobAvailable, 10)
	event.DisplayName = "build"
	if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	first := operations.GitHubJobObservation{WorkflowJobID: 100, Owner: "owner", Repository: "repo", WorkflowRunID: 77,
		RunAttempt: 1, DisplayName: "build", Labels: event.Labels, Status: "queued", CreatedAt: time.Unix(50, 0).UTC(), QueueTimeExact: true}
	if _, err := store.ReconcileGitHubJobs(ctx, 1, time.Unix(60, 0).UTC(), []operations.GitHubJobObservation{first}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.WorkflowJobID = 101
	second.RunAttempt = 2
	second.CreatedAt = time.Unix(200, 0).UTC()
	if _, err := store.ReconcileGitHubJobs(ctx, 1, time.Unix(210, 0).UTC(), []operations.GitHubJobObservation{second}); err != nil {
		t.Fatal(err)
	}
	record, err := store.DemandRecord(ctx, 1, 10)
	if err != nil || record.RunAttempt != 2 || record.WorkflowJobID != 101 || record.FirstQueueTime != second.CreatedAt {
		t.Fatalf("rerun inherited prior attempt queue age: %#v, %v", record, err)
	}
}

func TestGitHubQueueSnapshotReplacementAndStatisticsMonotonicity(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	job := operations.GitHubJobObservation{WorkflowJobID: 100, Owner: "owner", Repository: "repo", WorkflowRunID: 1,
		RunAttempt: 1, DisplayName: "build", Labels: []string{"self-hosted", "linux-small"}, Status: "queued", CreatedAt: now.Add(-time.Minute), QueueTimeExact: true}
	if changed, err := store.ReconcileGitHubJobs(ctx, 1, now, []operations.GitHubJobObservation{job}); err != nil || !changed {
		t.Fatalf("initial snapshot = %v, %v", changed, err)
	}
	if jobs, err := store.QueuedGitHubJobs(ctx, 1); err != nil || len(jobs) != 1 || jobs[0].WorkflowJobID != 100 {
		t.Fatalf("queued snapshot = %#v, %v", jobs, err)
	}
	if changed, err := store.ReconcileGitHubJobs(ctx, 1, now.Add(time.Second), nil); err != nil || !changed {
		t.Fatalf("empty replacement = %v, %v", changed, err)
	}
	if jobs, err := store.QueuedGitHubJobs(ctx, 1); err != nil || len(jobs) != 0 {
		t.Fatalf("completed queue was not cleared = %#v, %v", jobs, err)
	}

	newer := operations.DemandStatistics{MessageID: 9, Available: 2, Assigned: 1, Registered: 2, Idle: 1, ObservedAt: now}
	if changed, err := store.PutDemandStatistics(ctx, 1, newer); err != nil || !changed {
		t.Fatalf("new statistics = %v, %v", changed, err)
	}
	older := newer
	older.MessageID = 8
	older.Available = 99
	if changed, err := store.PutDemandStatistics(ctx, 1, older); err != nil || changed {
		t.Fatalf("older statistics = %v, %v", changed, err)
	}
	got, err := store.DemandStatistics(ctx, 1)
	if err != nil || got.MessageID != 9 || got.Available != 2 || got.ObservedAt != now {
		t.Fatalf("statistics regressed = %#v, %v", got, err)
	}
}

func TestGitHubQueueScopeSnapshotRollsBackEveryProfile(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	job := func(id int64, name string) operations.GitHubJobObservation {
		return operations.GitHubJobObservation{WorkflowJobID: id, Owner: "owner", Repository: "repo", WorkflowRunID: id,
			RunAttempt: 1, DisplayName: name, Labels: []string{"self-hosted", name}, Status: "queued",
			CreatedAt: now.Add(-time.Minute), QueueTimeExact: true}
	}
	initial := map[int64][]operations.GitHubJobObservation{1: {job(101, "linux-small")}, 2: {job(202, "linux-large")}}
	if changed, err := store.ReconcileGitHubJobSnapshot(ctx, now, initial); err != nil || !changed {
		t.Fatalf("initial scope snapshot = %v, %v", changed, err)
	}
	replaces := 0
	store.injectFault = func(point string) error {
		if point == "githubjobs.replace" {
			replaces++
			if replaces == 2 {
				return errors.New("second profile failed")
			}
		}
		return nil
	}
	replacement := map[int64][]operations.GitHubJobObservation{1: nil, 2: {job(203, "linux-large")}}
	if changed, err := store.ReconcileGitHubJobSnapshot(ctx, now.Add(time.Minute), replacement); err == nil || changed {
		t.Fatalf("partial scope failure = %v, %v", changed, err)
	}
	store.injectFault = nil
	for scaleSetID, wantID := range map[int64]int64{1: 101, 2: 202} {
		jobs, err := store.QueuedGitHubJobs(ctx, scaleSetID)
		if err != nil || len(jobs) != 1 || jobs[0].WorkflowJobID != wantID {
			t.Fatalf("profile %d lost prior snapshot after rollback: %#v, %v", scaleSetID, jobs, err)
		}
	}
}

func TestCanonicalQueuePersistenceFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	job := operations.GitHubJobObservation{WorkflowJobID: 100, Owner: "owner", Repository: "repo", WorkflowRunID: 1,
		RunAttempt: 1, DisplayName: "build", Labels: []string{"self-hosted", "linux-small"}, Status: "queued", CreatedAt: now, QueueTimeExact: true}
	for _, point := range []string{"githubjobs.begin", "githubjobs.count", "githubjobs.mark.load", "githubjobs.replace",
		"githubjobs.upsert", "githubjobs.group", "githubjobs.project", "githubjobs.absent", "githubjobs.mark", "githubjobs.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if changed, err := store.ReconcileGitHubJobs(ctx, 1, now, []operations.GitHubJobObservation{job}); err == nil || changed {
				t.Fatalf("fault %s ignored: %v, %v", point, changed, err)
			}
		})
	}
	store := testStore(t)
	statistics := operations.DemandStatistics{MessageID: 1, Available: 1}
	store.injectFault = func(point string) error { return errors.New(point) }
	if _, err := store.PutDemandStatistics(ctx, 1, statistics); err == nil {
		t.Fatal("statistics write failure ignored")
	}
	if _, err := store.DemandStatistics(ctx, 1); err == nil {
		t.Fatal("statistics read failure ignored")
	}
	if _, err := store.QueuedGitHubJobs(ctx, 1); err == nil {
		t.Fatal("queued jobs read failure ignored")
	}
	store.injectFault = nil
	store.injectRows = func(point string) rowsScanner {
		if point == "githubjobs.queued" {
			return &injectedRows{rowsErr: errors.New("iterate")}
		}
		return nil
	}
	if _, err := store.QueuedGitHubJobs(ctx, 1); err == nil {
		t.Fatal("queued jobs iteration failure ignored")
	}
	store.injectRows = nil
	if _, err := store.PutDemandStatistics(ctx, 0, statistics); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid statistics scale set = %v", err)
	}
	if _, err := store.PutDemandStatistics(ctx, 1, operations.DemandStatistics{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid statistics = %v", err)
	}
	if _, err := store.DemandStatistics(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid statistics read = %v", err)
	}
	if _, err := store.DemandStatistics(ctx, 1); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing statistics = %v", err)
	}
	if _, err := store.ReconcileGitHubJobs(ctx, 0, now, nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid GitHub scale set = %v", err)
	}
	if _, err := store.ReconcileGitHubJobs(ctx, 1, time.Time{}, nil); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid observation time = %v", err)
	}
	invalid := job
	invalid.WorkflowJobID = 0
	if _, err := store.ReconcileGitHubJobs(ctx, 1, now, []operations.GitHubJobObservation{invalid}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid GitHub job = %v", err)
	}
	if _, err := store.QueuedGitHubJobs(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid queued read = %v", err)
	}
	var nanos nanosTime
	if err := nanos.Scan("bad"); err == nil {
		t.Fatal("invalid timestamp scan accepted")
	}
	if key := demandLogicalKey("", "repo", 1, "job", "", nil, ""); key != "" {
		t.Fatalf("invalid logical key = %q", key)
	}
}

func TestQueuedGitHubJobsRejectsCorruptLabels(t *testing.T) {
	store := testStore(t)
	now := time.Now().UTC().UnixNano()
	if _, err := store.db.Exec(`INSERT INTO github_job_observations(scale_set_id,workflow_job_id,owner,repository,workflow_run_id,run_attempt,display_name,workflow_ref,labels,status,created_at,observed_at)
		VALUES(1,1,'o','r',1,1,'job','','{','queued',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueuedGitHubJobs(context.Background(), 1); err == nil {
		t.Fatal("corrupt queued labels accepted")
	}
	if _, err := store.db.Exec(`INSERT INTO github_job_observations(scale_set_id,workflow_job_id,owner,repository,workflow_run_id,run_attempt,display_name,workflow_ref,labels,status,created_at,observed_at)
		VALUES(2,'not-an-integer','o','r',1,1,'job','','[]','queued',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueuedGitHubJobs(context.Background(), 2); err == nil {
		t.Fatal("corrupt queued row accepted")
	}
}

func TestDemandInboxAtomicDuplicateConflictRestartAndSanitization(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	events := []operations.DemandEvent{demandEvent(operations.DemandJobAvailable, 10), demandEvent(operations.DemandJobAssigned, 11)}
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, events); err != nil || !applied {
		t.Fatalf("first batch: %v %v", applied, err)
	}
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, events); err != nil || applied {
		t.Fatalf("duplicate batch: %v %v", applied, err)
	}
	conflict := append([]operations.DemandEvent(nil), events...)
	conflict[0].Repository = "different"
	if applied, err := store.ApplyDemandBatch(ctx, 9, 42, conflict); applied || !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("conflicting redelivery: %v %v", applied, err)
	}
	var encoded string
	if err := store.db.QueryRow(`SELECT events FROM scale_set_inbox WHERE scale_set_id=9 AND message_id=42`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(encoded), "acquirejob") || strings.Contains(strings.ToLower(encoded), "jit") || strings.Contains(encoded, "secret") {
		t.Fatalf("secret-bearing protocol data persisted: %s", encoded)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	active, err := store.ActiveDemands(ctx, 9)
	if err != nil || len(active) != 2 || active[0].RunnerRequestID != 10 || active[1].Status != operations.DemandJobAssigned {
		t.Fatalf("restart snapshot: %#v %v", active, err)
	}
	if cursor, err := store.DemandCursor(ctx, 9); err != nil || cursor != 42 {
		t.Fatalf("restart cursor: %d %v", cursor, err)
	}
}

func TestDemandInboxMixedLifecycleOutOfOrderAndCursorMonotonic(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	completed := demandEvent(operations.DemandJobCompleted, 20)
	completed.RunnerID, completed.RunnerName, completed.Result = 5, "runner", "success"
	mixed := []operations.DemandEvent{
		demandEvent(operations.DemandJobAvailable, 21),
		demandEvent(operations.DemandJobAssigned, 21),
		demandEvent(operations.DemandJobStarted, 21),
		completed,
	}
	if _, err := store.ApplyDemandBatch(ctx, 1, 20, mixed); err != nil {
		t.Fatal(err)
	}
	// A late lower-lifecycle event enriches metadata without resurrecting a
	// completed demand.
	late := demandEvent(operations.DemandJobAvailable, 20)
	late.Repository = "enriched"
	if _, err := store.ApplyDemandBatch(ctx, 1, 19, []operations.DemandEvent{late}); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveDemands(ctx, 1)
	if err != nil || len(active) != 1 || active[0].RunnerRequestID != 21 || active[0].Status != operations.DemandJobStarted {
		t.Fatalf("active lifecycle projection: %#v %v", active, err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.demandRecord(ctx, tx, 1, 20)
	_ = tx.Rollback()
	if err != nil || record.Status != operations.DemandJobCompleted || record.Repository != "enriched" || record.Result != "success" {
		t.Fatalf("out-of-order projection: %#v %v", record, err)
	}
	if cursor, err := store.DemandCursor(ctx, 1); err != nil || cursor != 20 {
		t.Fatalf("cursor regressed: %d %v", cursor, err)
	}
	finish := demandEvent(operations.DemandJobCompleted, 21)
	finish.Result = "cancelled"
	if _, err := store.ApplyDemandBatch(ctx, 1, 21, []operations.DemandEvent{finish}); err != nil {
		t.Fatal(err)
	}
	if active, err := store.ActiveDemands(ctx, 1); err != nil || len(active) != 0 {
		t.Fatalf("completed demand remains active: %#v %v", active, err)
	}
}

func TestDemandInboxRejectsSyntheticIdentityCollision(t *testing.T) {
	store := testStore(t)
	first := demandEvent(operations.DemandJobAvailable, 1<<62|9)
	if _, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{first}); err != nil {
		t.Fatal(err)
	}
	second := first
	second.JobID = "different-job-uuid"
	if applied, err := store.ApplyDemandBatch(context.Background(), 1, 2, []operations.DemandEvent{second}); applied || !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("collision = applied %v err %v", applied, err)
	}
}

func TestDemandInboxValidationAndEmptySnapshot(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, test := range []struct {
		scaleSet int64
		message  int64
		events   []operations.DemandEvent
	}{
		{0, 1, nil}, {1, 0, nil}, {1, 1, []operations.DemandEvent{{Kind: "bad", RunnerRequestID: 1}}},
	} {
		if _, err := store.ApplyDemandBatch(ctx, test.scaleSet, test.message, test.events); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
	}
	if active, err := store.ActiveDemands(ctx, 1); err != nil || len(active) != 0 {
		t.Fatalf("empty active snapshot: %#v %v", active, err)
	}
	if cursor, err := store.DemandCursor(ctx, 1); err != nil || cursor != 0 {
		t.Fatalf("empty cursor: %d %v", cursor, err)
	}
	if _, err := store.ActiveDemands(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid active request: %v", err)
	}
	if _, err := store.DemandCursor(ctx, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid cursor request: %v", err)
	}
	invalidTime := demandEvent(operations.DemandJobAvailable, 1)
	invalidTime.QueueTime = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := store.ApplyDemandBatch(ctx, 1, 2, []operations.DemandEvent{invalidTime}); err == nil {
		t.Fatal("unencodable demand event accepted")
	}
}

func TestDemandInboxInjectedDatabaseFailuresAreAtomic(t *testing.T) {
	for _, point := range []string{"inbox.begin", "inbox.load", "inbox.demand.load", "inbox.group", "inbox.group.load", "inbox.project", "inbox.record", "inbox.cursor", "inbox.commit"} {
		t.Run(point, func(t *testing.T) {
			store := testStore(t)
			store.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if applied, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{demandEvent(operations.DemandJobAvailable, 1)}); applied || err == nil {
				t.Fatalf("fault %s was ignored: %v %v", point, applied, err)
			}
			store.injectFault = nil
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM scale_set_inbox`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("fault %s partially committed: %d %v", point, count, err)
			}
		})
	}
	t.Run("started registering advance failure", func(t *testing.T) {
		store := testStore(t)
		instance := operations.Instance{ID: "registering-failure", State: operations.StateRegistering,
			Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
		if err := store.CreateInstance(context.Background(), instance); err != nil {
			t.Fatal(err)
		}
		store.injectFault = func(point string) error {
			if point == "advance.begin" {
				return errors.New("injected")
			}
			return nil
		}
		if err := store.projectDemandRank(context.Background(), instance, operations.DemandJobStarted); err == nil {
			t.Fatal("registering storage failure was ignored")
		}
	})
	store := testStore(t)
	store.injectFault = func(point string) error {
		if point == "inbox.active.query" || point == "inbox.cursor.load" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := store.ActiveDemands(context.Background(), 1); err == nil {
		t.Fatal("active query failure ignored")
	}
	if _, err := store.DemandCursor(context.Background(), 1); err == nil {
		t.Fatal("cursor query failure ignored")
	}
}

func TestDemandInboxCorruptProjectionFailsClosed(t *testing.T) {
	store := testStore(t)
	event := demandEvent(operations.DemandJobAvailable, 1)
	if _, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runner_demands SET labels='{'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveDemands(context.Background(), 1); err == nil {
		t.Fatal("corrupt demand projection accepted")
	}
	if _, err := scanDemand(scanError{errors.New("scan")}); err == nil {
		t.Fatal("demand scan error ignored")
	}
	if _, err := scanDemand(scanError{sql.ErrNoRows}); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("demand not found mapping: %v", err)
	}
}

func TestDemandRecordFallsBackToProtocolQueueTimeForLegacyRows(t *testing.T) {
	store := testStore(t)
	event := demandEvent(operations.DemandJobAvailable, 1)
	if _, err := store.ApplyDemandBatch(context.Background(), 1, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runner_demands SET first_queue_time=0 WHERE scale_set_id=1 AND runner_request_id=1`); err != nil {
		t.Fatal(err)
	}
	record, err := store.DemandRecord(context.Background(), 1, 1)
	if err != nil || record.FirstQueueTime != event.QueueTime {
		t.Fatalf("legacy queue fallback = %#v, %v", record, err)
	}
}

func TestProjectDemandEventDrivesRunnerLifecycleAndQueuesDrain(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	events := []operations.DemandEvent{{Kind: operations.DemandJobAvailable, RunnerRequestID: 71, Owner: "owner", Repository: "repo", WorkflowRunID: 22, QueueTime: now}}
	if _, err := store.ApplyDemandBatch(ctx, 99, 1, events); err != nil {
		t.Fatal(err)
	}
	instance := operations.Instance{ID: "trf-small-project", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1}, Demand: domain.DemandKey{Repo: "owner/repo", RunID: 22, Attempt: 1, JobID: 71},
		State: operations.StateRegistering, Ownership: operations.Ownership{ControllerID: "c", ResourceID: "d", OperationID: "spawn"}, CreatedAt: now}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	for index, kind := range []operations.DemandEventKind{operations.DemandJobAssigned, operations.DemandJobStarted, operations.DemandJobCompleted} {
		event := events[0]
		event.Kind = kind
		if _, err := store.ApplyDemandBatch(ctx, 99, int64(index+2), []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
		if err := store.ProjectDemandEvent(ctx, 99, event); err != nil {
			t.Fatalf("project %s: %v", kind, err)
		}
	}
	got, err := store.Instance(ctx, instance.ID)
	if err != nil || got.State != operations.StateDraining {
		t.Fatalf("projected instance = %#v, %v", got, err)
	}
	claimed, err := store.Claim(ctx, "worker", 2, time.Now().UTC().Add(time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Kind != "deregister" || claimed[0].ResourceID != instance.ID {
		t.Fatalf("drain operation = %#v, %v", claimed, err)
	}
	if err := store.ProjectDemandEvent(ctx, 99, events[0]); err != nil {
		t.Fatalf("available projection = %v", err)
	}
}

// GitHub runner scale sets may assign two acquired requests to the opposite
// registered runners. RunnerName is the authoritative execution identity; the
// request used to provision a JIT runner is only a reservation.
func TestProjectDemandEventAtomicallySwapsCrossAssignedRunnerDemands(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const scaleSetID = 99

	first := demandEvent(operations.DemandJobAvailable, 101)
	second := demandEvent(operations.DemandJobAvailable, 102)
	if _, err := store.ApplyDemandBatch(ctx, scaleSetID, 1, []operations.DemandEvent{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, runnerDemandInstance("runner-alpha", 101)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, runnerDemandInstance("runner-beta", 102)); err != nil {
		t.Fatal(err)
	}

	first.Kind, first.RunnerName = operations.DemandJobStarted, "runner-beta"
	second.Kind, second.RunnerName = operations.DemandJobStarted, "runner-alpha"
	for index, event := range []operations.DemandEvent{first, second} {
		if _, err := store.ApplyDemandBatch(ctx, scaleSetID, int64(index+2), []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
		if err := store.ProjectDemandEvent(ctx, scaleSetID, event); err != nil {
			t.Fatalf("project cross-assigned start %d: %v", event.RunnerRequestID, err)
		}
	}

	first.Kind = operations.DemandJobCompleted
	if _, err := store.ApplyDemandBatch(ctx, scaleSetID, 4, []operations.DemandEvent{first}); err != nil {
		t.Fatal(err)
	}
	if err := store.ProjectDemandEvent(ctx, scaleSetID, first); err != nil {
		t.Fatalf("project cross-assigned completion: %v", err)
	}

	alpha, alphaErr := store.Instance(ctx, "runner-alpha")
	beta, betaErr := store.Instance(ctx, "runner-beta")
	if alphaErr != nil || betaErr != nil {
		t.Fatalf("load runners: alpha=%v beta=%v", alphaErr, betaErr)
	}
	if alpha.State != operations.StateRunning || alpha.Demand.JobID != 102 {
		t.Fatalf("active runner mapping = %#v", alpha)
	}
	if beta.State != operations.StateDraining || beta.Demand.JobID != 101 {
		t.Fatalf("completed runner mapping = %#v", beta)
	}
	if alpha.Ownership.ResourceID != "demand-101" || beta.Ownership.ResourceID != "demand-102" {
		t.Fatalf("immutable ownership changed: alpha=%#v beta=%#v", alpha.Ownership, beta.Ownership)
	}
	alphaDemand, alphaDemandErr := store.DemandRecord(ctx, scaleSetID, alpha.Demand.JobID)
	betaDemand, betaDemandErr := store.DemandRecord(ctx, scaleSetID, beta.Demand.JobID)
	if alphaDemandErr != nil || betaDemandErr != nil || alphaDemand.Status != operations.DemandJobStarted || betaDemand.Status != operations.DemandJobCompleted {
		t.Fatalf("runner safety records: alpha=%#v/%v beta=%#v/%v", alphaDemand, alphaDemandErr, betaDemand, betaDemandErr)
	}
	claimed, err := store.Claim(ctx, "worker", 2, time.Now().UTC().Add(time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ResourceID != beta.ID {
		t.Fatalf("cross-assigned drain operation = %#v, %v", claimed, err)
	}
}

func TestAlignRunnerDemandFailsClosedAndRollsBack(t *testing.T) {
	ctx := context.Background()
	source := runnerDemandInstance("runner-alpha", 101)
	target := runnerDemandInstance("runner-beta", 102)

	store := testStore(t)
	if _, err := store.alignRunnerDemand(ctx, operations.Instance{}, nil, "", 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid alignment error = %v", err)
	}
	if _, err := store.alignRunnerDemand(ctx, target, []operations.Instance{target}, "owner/repo", 101); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("missing reservation error = %v", err)
	}
	if got, err := store.alignRunnerDemand(ctx, source, []operations.Instance{source}, "owner/repo", 101); err != nil || got.ID != source.ID {
		t.Fatalf("already aligned runner = %#v, %v", got, err)
	}
	duplicate := source
	duplicate.ID = "runner-duplicate"
	if _, err := store.alignRunnerDemand(ctx, target, []operations.Instance{source, duplicate, target}, "owner/repo", 101); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("duplicate reservation error = %v", err)
	}
	incompatible := target
	incompatible.Profile = "builder"
	if _, err := store.alignRunnerDemand(ctx, incompatible, []operations.Instance{source, incompatible}, "owner/repo", 101); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("incompatible runner error = %v", err)
	}
	invalidSource, invalidTarget := source, target
	invalidSource.Resources.CPU, invalidTarget.Resources.CPU = 0, 0
	if _, err := store.alignRunnerDemand(ctx, invalidTarget, []operations.Instance{invalidSource, invalidTarget}, "owner/repo", 101); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("invalid scheduling metadata error = %v", err)
	}

	newPair := func(t *testing.T) (*Store, operations.Instance, operations.Instance, []operations.Instance) {
		t.Helper()
		pairStore := testStore(t)
		if err := pairStore.CreateInstance(ctx, source); err != nil {
			t.Fatal(err)
		}
		if err := pairStore.CreateInstance(ctx, target); err != nil {
			t.Fatal(err)
		}
		instances, err := pairStore.LiveInstances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return pairStore, instances[0], instances[1], instances
	}
	assertOriginal := func(t *testing.T, pairStore *Store) {
		t.Helper()
		alpha, alphaErr := pairStore.Instance(ctx, source.ID)
		beta, betaErr := pairStore.Instance(ctx, target.ID)
		if alphaErr != nil || betaErr != nil || alpha.Demand.JobID != 101 || beta.Demand.JobID != 102 {
			t.Fatalf("alignment partially committed: alpha=%#v/%v beta=%#v/%v", alpha, alphaErr, beta, betaErr)
		}
	}

	for _, point := range []string{"runner-demand.begin", "runner-demand.source", "runner-demand.target", "runner-demand.commit"} {
		t.Run(point, func(t *testing.T) {
			pairStore, alpha, beta, instances := newPair(t)
			pairStore.injectFault = func(candidate string) error {
				if candidate == point {
					return errors.New("injected")
				}
				return nil
			}
			if _, err := pairStore.alignRunnerDemand(ctx, beta, instances, "owner/repo", alpha.Demand.JobID); err == nil {
				t.Fatalf("fault %s was ignored", point)
			}
			pairStore.injectFault = nil
			assertOriginal(t, pairStore)
		})
	}

	for _, staleID := range []string{source.ID, target.ID} {
		t.Run("stale-"+staleID, func(t *testing.T) {
			pairStore, alpha, beta, instances := newPair(t)
			if _, err := pairStore.db.ExecContext(ctx, `UPDATE instances SET version=version+1 WHERE id=?`, staleID); err != nil {
				t.Fatal(err)
			}
			if _, err := pairStore.alignRunnerDemand(ctx, beta, instances, "owner/repo", alpha.Demand.JobID); !errors.Is(err, operations.ErrConflict) {
				t.Fatalf("stale runner error = %v", err)
			}
			alphaAfter, _ := pairStore.Instance(ctx, source.ID)
			betaAfter, _ := pairStore.Instance(ctx, target.ID)
			if alphaAfter.Demand.JobID != 101 || betaAfter.Demand.JobID != 102 {
				t.Fatalf("stale alignment partially committed: alpha=%#v beta=%#v", alphaAfter, betaAfter)
			}
		})
	}
}

func TestProjectDemandRankCoversMonotonicLifecycleAndIdempotency(t *testing.T) {
	tests := []struct {
		name      string
		status    operations.DemandEventKind
		state     operations.State
		wantState operations.State
	}{
		{name: "assigned catches up registering", status: operations.DemandJobAssigned, state: operations.StateRegistering, wantState: operations.StateAssigned},
		{name: "assigned advances idle", status: operations.DemandJobAssigned, state: operations.StateOnlineIdle, wantState: operations.StateAssigned},
		{name: "assigned is idempotent", status: operations.DemandJobAssigned, state: operations.StateRunning, wantState: operations.StateRunning},
		{name: "started catches up registering", status: operations.DemandJobStarted, state: operations.StateRegistering, wantState: operations.StateRunning},
		{name: "started catches up idle", status: operations.DemandJobStarted, state: operations.StateOnlineIdle, wantState: operations.StateRunning},
		{name: "started advances assigned", status: operations.DemandJobStarted, state: operations.StateAssigned, wantState: operations.StateRunning},
		{name: "started is idempotent", status: operations.DemandJobStarted, state: operations.StateDraining, wantState: operations.StateDraining},
		{name: "completed catches up registering", status: operations.DemandJobCompleted, state: operations.StateRegistering, wantState: operations.StateDraining},
		{name: "completed drains idle", status: operations.DemandJobCompleted, state: operations.StateOnlineIdle, wantState: operations.StateDraining},
		{name: "completed drains assigned", status: operations.DemandJobCompleted, state: operations.StateAssigned, wantState: operations.StateDraining},
		{name: "completed drains running", status: operations.DemandJobCompleted, state: operations.StateRunning, wantState: operations.StateDraining},
		{name: "completed is idempotent", status: operations.DemandJobCompleted, state: operations.StateStopping, wantState: operations.StateStopping},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			instance := operations.Instance{ID: "vm", State: test.state,
				Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
			if err := store.CreateInstance(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			if err := store.projectDemandRank(context.Background(), instance, test.status); err != nil {
				t.Fatalf("project demand rank: %v", err)
			}
			got, err := store.Instance(context.Background(), instance.ID)
			if err != nil || got.State != test.wantState {
				t.Fatalf("instance state = %s, %v; want %s", got.State, err, test.wantState)
			}
			if test.wantState == operations.StateDraining && test.state != operations.StateStopping && test.state != operations.StateDraining {
				claimed, claimErr := store.Claim(context.Background(), "worker", 1, time.Now().UTC().Add(time.Minute), time.Minute)
				if claimErr != nil || len(claimed) != 1 || claimed[0].Kind != "deregister" {
					t.Fatalf("drain operation = %#v, %v", claimed, claimErr)
				}
			}
		})
	}
}

func TestCompletedPreRegistrationDemandOrdersDrainBehindProvision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	instance := operations.Instance{
		ID: "reachable", Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
		Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
		Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 77, Attempt: 1, JobID: 91}, State: operations.StateReachable,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"},
	}
	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"spawn", "spawn", "clone:reachable", lifecycle.OperationProvision, instance.ID, `{}`, operations.OperationPending, 0,
		now.UnixNano(), "", 0, "", now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := store.projectDemandRank(ctx, instance, operations.DemandJobCompleted); err != nil {
		t.Fatalf("project completion: %v", err)
	}
	got, err := store.Instance(ctx, instance.ID)
	if err != nil || got.State != operations.StateDraining {
		t.Fatalf("projected instance = %#v, %v", got, err)
	}
	claimed, err := store.Claim(ctx, "worker", 2, now.Add(time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != "spawn" {
		t.Fatalf("provision must gate drain: %#v, %v", claimed, err)
	}
	if _, err := store.Complete(ctx, "spawn", "worker", "clone:reachable", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.Claim(ctx, "worker", 2, now.Add(2*time.Minute), time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].Kind != lifecycle.OperationDrain || claimed[0].ResourceID != instance.ID {
		t.Fatalf("drain was not released after provision yielded: %#v, %v", claimed, err)
	}
}

func TestProjectDemandRankFailsClosedForImpossibleStatesAndStorageErrors(t *testing.T) {
	for _, test := range []struct {
		status operations.DemandEventKind
		state  operations.State
	}{
		{status: operations.DemandJobAssigned, state: operations.StatePlanned},
		{status: operations.DemandJobStarted, state: operations.StatePlanned},
		{status: operations.DemandJobCompleted, state: operations.StateFailed},
	} {
		status := test.status
		t.Run(string(status)+" uncertain", func(t *testing.T) {
			store := testStore(t)
			instance := operations.Instance{ID: "impossible", State: test.state,
				Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
			if err := store.CreateInstance(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			if err := store.projectDemandRank(context.Background(), instance, status); !errors.Is(err, operations.ErrUncertain) {
				t.Fatalf("impossible state error = %v", err)
			}
		})
		t.Run(string(status)+" advance failure", func(t *testing.T) {
			store := testStore(t)
			state := operations.StateRegistering
			if status == operations.DemandJobStarted {
				state = operations.StateOnlineIdle
			}
			instance := operations.Instance{ID: "advance-failure", State: state,
				Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
			if err := store.CreateInstance(context.Background(), instance); err != nil {
				t.Fatal(err)
			}
			store.injectFault = func(point string) error {
				if point == "advance.begin" {
					return errors.New("injected")
				}
				return nil
			}
			if err := store.projectDemandRank(context.Background(), instance, status); err == nil {
				t.Fatal("storage failure was ignored")
			}
		})
	}
	store := testStore(t)
	instance := operations.Instance{ID: "invalid-status", State: operations.StateOnlineIdle,
		Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
	if err := store.projectDemandRank(context.Background(), instance, operations.DemandEventKind("unknown")); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid status error = %v", err)
	}
}

func TestProjectDemandEventUsesDurableRecordAndUniqueOwnedInstance(t *testing.T) {
	ctx := context.Background()
	newStoreWithDemand := func(t *testing.T, kind operations.DemandEventKind) (*Store, operations.DemandEvent) {
		t.Helper()
		store := testStore(t)
		event := demandEvent(kind, 91)
		if _, err := store.ApplyDemandBatch(ctx, 7, 1, []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
		return store, event
	}
	newInstance := func(id string, state operations.State) operations.Instance {
		return operations.Instance{ID: id, Repo: "owner/repo", Platform: domain.PlatformLinux, Profile: "small", Route: "linux-small",
			Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
			Demand:    domain.DemandKey{Repo: "owner/repo", RunID: 77, Attempt: 1, JobID: 91}, State: state,
			Ownership: operations.Ownership{ControllerID: "controller", ResourceID: "demand", OperationID: "spawn"}}
	}

	if err := testStore(t).ProjectDemandEvent(ctx, 0, operations.DemandEvent{}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid projection error = %v", err)
	}
	availableStore := testStore(t)
	if err := availableStore.ProjectDemandEvent(ctx, 1, demandEvent(operations.DemandJobAvailable, 1)); err != nil {
		t.Fatalf("available event should not mutate lifecycle: %v", err)
	}
	missingStore := testStore(t)
	if err := missingStore.ProjectDemandEvent(ctx, 1, demandEvent(operations.DemandJobStarted, 1)); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing durable demand error = %v", err)
	}

	completedStore, completed := newStoreWithDemand(t, operations.DemandJobCompleted)
	if err := completedStore.ProjectDemandEvent(ctx, 7, completed); err != nil {
		t.Fatalf("completion without a live VM should be idempotent: %v", err)
	}
	startedStore, started := newStoreWithDemand(t, operations.DemandJobStarted)
	if err := startedStore.ProjectDemandEvent(ctx, 7, started); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("active demand without VM error = %v", err)
	}
	for _, kind := range []operations.DemandEventKind{operations.DemandJobStarted, operations.DemandJobCompleted} {
		namedStore := testStore(t)
		named := demandEvent(kind, 92)
		named.RunnerName = "missing-runner"
		if _, err := namedStore.ApplyDemandBatch(ctx, 7, 1, []operations.DemandEvent{named}); err != nil {
			t.Fatal(err)
		}
		err := namedStore.ProjectDemandEvent(ctx, 7, named)
		if kind == operations.DemandJobStarted && !errors.Is(err, operations.ErrUncertain) {
			t.Fatalf("named active demand without VM error = %v", err)
		}
		if kind == operations.DemandJobCompleted && err != nil {
			t.Fatalf("named completion after VM deletion = %v", err)
		}
	}
	misalignedStore := testStore(t)
	misaligned := demandEvent(operations.DemandJobStarted, 93)
	misaligned.RunnerName = "named-target"
	if _, err := misalignedStore.ApplyDemandBatch(ctx, 7, 1, []operations.DemandEvent{misaligned}); err != nil {
		t.Fatal(err)
	}
	if err := misalignedStore.CreateInstance(ctx, runnerDemandInstance(misaligned.RunnerName, 94)); err != nil {
		t.Fatal(err)
	}
	if err := misalignedStore.ProjectDemandEvent(ctx, 7, misaligned); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("named runner without reservation error = %v", err)
	}

	duplicateStore, duplicate := newStoreWithDemand(t, operations.DemandJobAssigned)
	for _, id := range []string{"one", "two"} {
		if err := duplicateStore.CreateInstance(ctx, newInstance(id, operations.StateOnlineIdle)); err != nil {
			t.Fatal(err)
		}
	}
	if err := duplicateStore.ProjectDemandEvent(ctx, 7, duplicate); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("duplicate ownership error = %v", err)
	}

	queryStore, queryEvent := newStoreWithDemand(t, operations.DemandJobAssigned)
	queryStore.injectFault = func(point string) error {
		if point == "instances.live.query" {
			return errors.New("injected")
		}
		return nil
	}
	if err := queryStore.ProjectDemandEvent(ctx, 7, queryEvent); err == nil {
		t.Fatal("live inventory failure was ignored")
	}
}

func TestDemandRecordValidationLookupAndCorruption(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.DemandRecord(ctx, 0, 1); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid scale set error = %v", err)
	}
	if _, err := store.DemandRecord(ctx, 1, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid request error = %v", err)
	}
	if _, err := store.DemandRecord(ctx, 1, 1); !errors.Is(err, operations.ErrNotFound) {
		t.Fatalf("missing record error = %v", err)
	}
	event := demandEvent(operations.DemandJobAssigned, 1)
	if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
		t.Fatal(err)
	}
	record, err := store.DemandRecord(ctx, 1, 1)
	if err != nil || record.Status != operations.DemandJobAssigned {
		t.Fatalf("demand record = %#v, %v", record, err)
	}
}
