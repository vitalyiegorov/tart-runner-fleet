package app

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type requeueMemory struct{ records []operations.OrphanRequeue }

func (s *requeueMemory) RecentOrphanRequeues(context.Context, string) ([]operations.OrphanRequeue, error) {
	return s.records, nil
}
func (s *requeueMemory) ClaimOrphanRequeue(_ context.Context, r operations.OrphanRequeue, cooldown time.Duration, limit int) (bool, error) {
	count := 0
	now := r.At
	if !r.RetryAt.IsZero() {
		now = r.RetryAt
	}
	for _, old := range s.records {
		if old.Repository == r.Repository && old.RunID == r.RunID && now.Sub(old.At) < cooldown {
			return false, nil
		}
		if now.Sub(old.At) < time.Hour {
			count++
		}
	}
	if count >= limit {
		return false, nil
	}
	if !r.RetryAt.IsZero() {
		for i, old := range s.records {
			if old.RunID == r.RunID && old.Repository == r.Repository && old.At.Equal(r.At) {
				r.At = r.RetryAt
				s.records[i].Outcome = "noop"
				s.records = append(s.records, r)
				return true, nil
			}
		}
		return false, nil
	}
	s.records = append(s.records, r)
	return true, nil
}
func (s *requeueMemory) FinishOrphanRequeue(_ context.Context, r operations.OrphanRequeue) error {
	s.records[len(s.records)-1] = r
	return nil
}

type requeueAPI struct {
	calls    int
	attempts int
	running  bool
	workflow string
	denied   bool
}

func (a *requeueAPI) RequeueOrphan(_ context.Context, _ githubscaleset.WorkflowJob, excluded func(string, []string) bool, act bool) (string, error) {
	a.attempts++
	if a.denied {
		return "permission_denied", nil
	}
	if a.running || excluded(a.workflow, []string{"Maestro"}) {
		return "held", nil
	}
	if !act {
		return "would_requeue", nil
	}
	a.calls++
	return "rerun", nil
}
func TestIssue520AutomaticOrphanRequeue(t *testing.T) {
	for _, scenario := range []string{"act", "running", "excluded", "observe", "off", "sibling owner", "cooldown restart", "rate", "permission", "threshold", "queue slo", "dedupe jobs"} {
		t.Run(scenario, func(t *testing.T) {
			store := newShardedDemandStore()
			c := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }, AuditedAssignments: []ScaleSetAssignment{{Scope: "sudoku-repo", ID: 1, Fresh: true}}}
			job := restJob(42, 78*time.Minute)
			snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{job}}
			ledger := &requeueMemory{}
			api := &requeueAPI{}
			cfg := config.OrphanRequeue{Mode: "act"}
			slo := 10 * time.Minute
			switch scenario {
			case "threshold":
				snapshot.jobs[0].CreatedAt = restShareEpoch.Add(-12 * time.Minute)
			case "queue slo":
				snapshot.jobs[0].CreatedAt = restShareEpoch.Add(-18 * time.Minute)
				slo = 20 * time.Minute
			case "dedupe jobs":
				cfg.Mode = "observe"
				snapshot.jobs = append(snapshot.jobs, snapshot.jobs[0])
			case "running":
				api.running = true
			case "excluded":
				cfg.Exclusions = []config.OrphanExclusion{{Workflow: "farm"}}
				api.workflow = "farm"
			case "observe":
				cfg.Mode = "observe"
			case "off":
				cfg.Mode = "off"
			case "sibling owner":
				snapshot.jobs[0].CreatedAt = restShareEpoch.Add(-20 * time.Minute)
				c.AuditedAssignments = append(c.AuditedAssignments, ScaleSetAssignment{Scope: "sudoku-repo", ID: 0, Labels: job.Labels, Fresh: true})
			case "rate":
				for i := range 3 {
					ledger.records = append(ledger.records, operations.OrphanRequeue{Repository: "other/repo", RunID: int64(i), At: restShareEpoch})
				}
			case "permission":
				api.denied = true
			}
			r := OrphanRecovery{Config: cfg, Store: ledger, API: api}
			findings, err := r.Reconcile(context.Background(), c, []Binding{sharedLabelBinding(1)}, snapshot, slo)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cooldown restart" || scenario == "permission" {
				r = OrphanRecovery{Config: cfg, Store: ledger, API: api}
				_, err = r.Reconcile(context.Background(), c, []Binding{sharedLabelBinding(1)}, snapshot, slo)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "dedupe jobs" && api.attempts != 1 {
				t.Fatal("duplicate run observed more than once")
			}
			if scenario == "permission" && api.attempts != 1 {
				t.Fatalf("permission retries=%d", api.attempts)
			}
			want := 0
			if scenario == "act" || scenario == "cooldown restart" {
				want = 1
			}
			if api.calls != want {
				t.Fatalf("calls=%d want %d", api.calls, want)
			}
			if scenario != "off" && len(findings) == 0 {
				t.Fatal("missing doctor mode/outcome")
			}
		})
	}
}

type failingRequeueStore struct {
	*requeueMemory
	fail string
}

func (s failingRequeueStore) RecentOrphanRequeues(ctx context.Context, scope string) ([]operations.OrphanRequeue, error) {
	if s.fail == "history" {
		return nil, context.Canceled
	}
	return s.requeueMemory.RecentOrphanRequeues(ctx, scope)
}
func (s failingRequeueStore) ClaimOrphanRequeue(ctx context.Context, r operations.OrphanRequeue, d time.Duration, n int) (bool, error) {
	if s.fail == "claim" {
		return false, context.Canceled
	}
	return s.requeueMemory.ClaimOrphanRequeue(ctx, r, d, n)
}
func (s failingRequeueStore) FinishOrphanRequeue(ctx context.Context, r operations.OrphanRequeue) error {
	if s.fail == "finish" {
		return context.Canceled
	}
	return s.requeueMemory.FinishOrphanRequeue(ctx, r)
}

type failingRequeueAPI struct{}

func (failingRequeueAPI) RequeueOrphan(context.Context, githubscaleset.WorkflowJob, func(string, []string) bool, bool) (string, error) {
	return "", context.Canceled
}
func TestOrphanRecoveryErrorsFailClosed(t *testing.T) {
	for _, failure := range []string{"history", "claim", "finish", "action", "observe", "candidate"} {
		t.Run(failure, func(t *testing.T) {
			store := newShardedDemandStore()
			c := DemandCoordinator{Store: store, Now: func() time.Time { return restShareEpoch }, AuditedAssignments: []ScaleSetAssignment{{Scope: "sudoku-repo", ID: 1, Fresh: true}}}
			snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{restJob(42, time.Hour)}}
			ledger := failingRequeueStore{requeueMemory: &requeueMemory{}, fail: failure}
			r := OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: ledger, API: &requeueAPI{}}
			switch failure {
			case "action":
				r.API = failingRequeueAPI{}
			case "observe":
				r.Config.Mode = "observe"
				r.API = failingRequeueAPI{}
			case "candidate":
				store.recordErr = context.Canceled
			}
			if _, err := r.Reconcile(context.Background(), c, []Binding{sharedLabelBinding(1)}, snapshot, time.Minute); err == nil {
				t.Fatal("error ignored")
			}
		})
	}
	r := OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}}
	if _, err := r.Reconcile(context.Background(), DemandCoordinator{}, nil, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
}
func TestOrphanOwnerElectsAcrossDifferentJobLabels(t *testing.T) {
	low := sharedLabelBinding(1)
	high := sharedLabelBinding(2)
	high.Profile.Route = "other"
	high.ScaleSetLabels = []string{"self-hosted", "other"}
	first := restJob(42, time.Hour)
	second := restJob(43, time.Hour)
	second.RunID = first.RunID
	second.Labels = []string{"self-hosted", "other"}
	snapshot := fakeQueueSnapshot{jobs: []githubscaleset.WorkflowJob{first, second}}
	rows := []ScaleSetAssignment{{Scope: low.Scope, ID: 1, Labels: first.Labels, Fresh: true}, {Scope: high.Scope, ID: 2, Labels: second.Labels, Fresh: true}}
	if !orphanOwner(rows, []Binding{low}, snapshot, first, 20*time.Minute, 15*time.Minute, 30*time.Minute) || orphanOwner(rows, []Binding{high}, snapshot, second, 20*time.Minute, 15*time.Minute, 30*time.Minute) {
		t.Fatal("nodes disagreed on actor")
	}
	rows[0].Fresh = false
	if orphanOwner(rows, []Binding{high}, snapshot, second, 20*time.Minute, 15*time.Minute, 30*time.Minute) {
		t.Fatal("stale sibling elected actor")
	}
	snapshot.jobs = append(snapshot.jobs, first)
	snapshot.jobs[2].RunID = 421
	rows[0].Fresh = true
	rows = append(rows, ScaleSetAssignment{Scope: "foreign", ID: 0, Fresh: true})
	if !orphanOwner(rows, []Binding{low}, snapshot, first, 20*time.Minute, 15*time.Minute, 30*time.Minute) {
		t.Fatal("unrelated run/scope interfered")
	}
}

func TestOrphanCandidateAmbiguityCannotAuthorizeRecovery(t *testing.T) {
	low := sharedLabelBinding(1)
	other := sharedLabelBinding(2)
	other.Profile.ID = "different"
	job := restJob(42, time.Hour)
	snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{job}}
	c := DemandCoordinator{Store: newShardedDemandStore(), Now: func() time.Time { return restShareEpoch }, AuditedAssignments: []ScaleSetAssignment{{Scope: low.Scope, ID: 1, Fresh: true}}}
	if _, err := c.orphanCandidates(context.Background(), []Binding{low, other}, snapshot, time.Minute); err == nil {
		t.Fatal("ambiguous routing authorized recovery")
	}
	if localAuditMatch(c.AuditedAssignments[0], []Binding{low, other}, job) {
		t.Fatal("ambiguous binding counted as local")
	}
	if localAuditMatch(ScaleSetAssignment{ID: 99}, []Binding{low}, job) {
		t.Fatal("unrelated binding counted as local")
	}
}

func TestOrphanObserveCooldownAndStagger(t *testing.T) {
	for _, scenario := range []string{"observe cooldown", "offline owner", "stale audit"} {
		t.Run(scenario, func(t *testing.T) {
			job := restJob(42, 78*time.Minute)
			rows := []ScaleSetAssignment{{Scope: "sudoku-repo", ID: 1, Fresh: true}, {Scope: "sudoku-repo", ID: 0, Labels: job.Labels, Fresh: true}}
			if scenario == "stale audit" {
				rows[0].Fresh = false
			}
			c := DemandCoordinator{Store: newShardedDemandStore(), Now: func() time.Time { return restShareEpoch }, AuditedAssignments: rows}
			api := &requeueAPI{}
			mode := "act"
			if scenario == "observe cooldown" {
				mode = "observe"
				c.AuditedAssignments = rows[:1]
			}
			r := OrphanRecovery{Config: config.OrphanRequeue{Mode: mode}, Store: &requeueMemory{}, API: api}
			snapshot := fakeQueueSnapshot{at: restShareEpoch, jobs: []githubscaleset.WorkflowJob{job}}
			for range 2 {
				if _, err := r.Reconcile(context.Background(), c, []Binding{sharedLabelBinding(1)}, snapshot, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			want := 1
			if scenario == "stale audit" {
				want = 0
			}
			if api.attempts != want {
				t.Fatalf("attempts=%d want %d", api.attempts, want)
			}
		})
	}
}

func TestOrphanRecoveryAuditAge(t *testing.T) {
	now := restShareEpoch
	threshold := now.Add(-time.Minute)
	for _, test := range []struct {
		at   time.Time
		want bool
	}{
		{now.Add(-30 * time.Minute), false},
		{now.Add(-30 * time.Second), true},
		{now.Add(-2 * time.Minute), false},
		{time.Time{}, false},
		{now.Add(time.Second), false},
	} {
		if got := recoveryAuditFresh(test.at, now, threshold, time.Minute); got != test.want {
			t.Fatalf("at %s: %v", test.at, got)
		}
	}
	if !recoveryAuditFresh(now.Add(-2*time.Minute), now, now.Add(-3*time.Minute), time.Minute) {
		t.Fatal("audit after threshold rejected")
	}
}

func TestOrphanRecoveryResumesWithoutQueueAndReportsRemedy(t *testing.T) {
	ledger := &requeueMemory{records: []operations.OrphanRequeue{{Scope: "sudoku-repo", Repository: "o/r", RunID: 420, RunAttempt: 1, At: restShareEpoch.Add(-time.Hour), Outcome: "cancelled_not_rerun"}}}
	api := &requeueAPI{}
	r := OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: ledger, API: api}
	findings, err := r.Reconcile(context.Background(), DemandCoordinator{Now: func() time.Time { return restShareEpoch }}, []Binding{sharedLabelBinding(1)}, nil, time.Minute)
	if err != nil || api.calls != 1 || ledger.records[len(ledger.records)-1].Outcome != "rerun" {
		t.Fatalf("calls=%d records=%v err=%v", api.calls, ledger.records, err)
	}
	found := false
	for _, f := range findings {
		if f == "gh run rerun 420 --failed -R o/r" {
			found = true
		}
	}
	if !found {
		t.Fatal(findings)
	}
}

type resumeResultAPI struct {
	outcome string
	err     error
}

func (a resumeResultAPI) RequeueOrphan(context.Context, githubscaleset.WorkflowJob, func(string, []string) bool, bool) (string, error) {
	return a.outcome, a.err
}

func TestOrphanResumeKeepsUnfinishedOnFailure(t *testing.T) {
	for _, scenario := range []string{"api error", "permission", "finish error", "legacy attempt", "invalid repository"} {
		t.Run(scenario, func(t *testing.T) {
			record := operations.OrphanRequeue{Scope: "sudoku-repo", Repository: "o/r", RunID: 420, RunAttempt: 1, At: restShareEpoch.Add(-time.Hour), Outcome: "cancelled_not_rerun"}
			if scenario == "legacy attempt" {
				record.RunAttempt = 0
			}
			if scenario == "invalid repository" {
				record.Repository = "invalid"
			}
			memory := &requeueMemory{records: []operations.OrphanRequeue{record}}
			store := failingRequeueStore{requeueMemory: memory}
			api := resumeResultAPI{outcome: "permission_denied"}
			if scenario == "api error" {
				api = resumeResultAPI{err: context.Canceled}
			}
			if scenario == "finish error" {
				store.fail = "finish"
			}
			r := OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: store, API: api}
			_, err := r.Reconcile(context.Background(), DemandCoordinator{Now: func() time.Time { return restShareEpoch }}, []Binding{sharedLabelBinding(1)}, nil, time.Minute)
			if (err != nil) != (scenario == "api error" || scenario == "finish error") {
				t.Fatal(err)
			}
			if (scenario == "api error" || scenario == "permission") && memory.records[len(memory.records)-1].Outcome != "cancelled_not_rerun" {
				t.Fatal(memory.records)
			}
		})
	}
}

func TestOrphanResumeAdmissionAndReservedRemedy(t *testing.T) {
	for _, outcome := range []string{"reserved", "cancelled_not_rerun"} {
		t.Run(outcome, func(t *testing.T) {
			ledger := &requeueMemory{records: []operations.OrphanRequeue{{Scope: "sudoku-repo", Repository: "o/r", RunID: 420, RunAttempt: 1, At: restShareEpoch.Add(-2 * time.Hour), Outcome: outcome}}}
			for i := range 3 {
				if outcome == "reserved" {
					break
				}
				ledger.records = append(ledger.records, operations.OrphanRequeue{Repository: "other/r", RunID: int64(i), At: restShareEpoch, Outcome: "rerun"})
			}
			api := &requeueAPI{}
			r := OrphanRecovery{Config: config.OrphanRequeue{Mode: "act"}, Store: ledger, API: api}
			c := DemandCoordinator{Store: newShardedDemandStore(), Now: func() time.Time { return restShareEpoch }}
			findings, err := r.Reconcile(context.Background(), c, []Binding{sharedLabelBinding(1)}, fakeQueueSnapshot{at: restShareEpoch}, time.Minute)
			if err != nil || api.attempts != 0 {
				t.Fatalf("attempts=%d err=%v", api.attempts, err)
			}
			if outcome == "reserved" {
				found := false
				for _, f := range findings {
					if f == "gh run rerun 420 --failed -R o/r" {
						found = true
					}
				}
				if !found {
					t.Fatal(findings)
				}
			}
		})
	}
}
