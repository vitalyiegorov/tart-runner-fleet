package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestIssue520RequeueLedgerSurvivesRestartAndLimitsNode(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	r := operations.OrphanRequeue{Scope: "scope", Repository: "o/r", RunID: 42, At: at, Outcome: "rerun"}
	claim := func(want bool) {
		t.Helper()
		ok, err := s.ClaimOrphanRequeue(ctx, r, 30*time.Minute, 3)
		if err != nil || ok != want {
			t.Fatalf("claim=%v err=%v want=%v", ok, err, want)
		}
	}
	claim(true)
	if err := s.FinishOrphanRequeue(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	claim(false)
	rows, err := s.RecentOrphanRequeues(ctx, "scope")
	if err != nil || len(rows) != 1 || rows[0].Outcome != "rerun" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	r.RunID++
	claim(true)
	r.RunID++
	claim(true)
	r.RunID++
	claim(false)
	r.At = at.Add(time.Hour)
	r.Outcome = "permission_denied"
	claim(true)
	if err := s.FinishOrphanRequeue(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.RunID++
	r.At = r.At.Add(time.Minute)
	claim(false)
	r.Scope = "another"
	claim(true)
}

func TestOrphanRequeueStoreFailsClosedOnUnavailableState(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecentOrphanRequeues(ctx, "scope"); err == nil {
		t.Fatal("read unavailable treated as empty")
	}
	if _, err := s.ClaimOrphanRequeue(ctx, operations.OrphanRequeue{}, time.Minute, 3); err == nil {
		t.Fatal("unavailable claim accepted")
	}
	if err := s.FinishOrphanRequeue(ctx, operations.OrphanRequeue{}); err == nil {
		t.Fatal("unavailable update accepted")
	}
}

func TestOrphanHeldBudgetAndLifetimeCap(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for i := range 4 {
		r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: int64(i), At: at, Outcome: "held"}
		ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3)
		if err != nil || !ok {
			t.Fatalf("held starved claim %d: %v", i, err)
		}
		if err := s.FinishOrphanRequeue(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: 99, At: at.Add(time.Duration(i) * time.Hour), Outcome: "rerun"}
		ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3)
		if err != nil || ok != (i < 2) {
			t.Fatalf("lifetime claim %d = %v: %v", i, ok, err)
		}
		if ok {
			if err := s.FinishOrphanRequeue(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := s.RecentOrphanRequeues(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	gaveUp := false
	for _, r := range rows {
		if r.RunID == 99 && r.Outcome == "gave_up" {
			gaveUp = true
		}
	}
	if !gaveUp {
		t.Fatal("lifetime cap missing from doctor history", rows)
	}
}

func TestOrphanUnfinishedBeyondRecentHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for i := range 12 {
		r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: int64(i + 1), RunAttempt: 7, At: at.Add(time.Duration(i) * time.Hour), Outcome: "held"}
		ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if i > 0 {
			if err := s.FinishOrphanRequeue(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := s.RecentOrphanRequeues(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.RunID == 1 && r.RunAttempt == 7 && r.Outcome == "reserved" {
			found = true
		}
	}
	if !found {
		t.Fatal(rows)
	}
}

func TestOrphanUncertainMutationsConsumeLifetimeBudget(t *testing.T) {
	for _, outcome := range []string{"reserved", "cancelled_not_rerun"} {
		t.Run(outcome, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: 42, At: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Outcome: outcome}
			for i := range 3 {
				ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3)
				if err != nil || ok != (i < 2) {
					t.Fatalf("claim %d=%v err=%v", i, ok, err)
				}
				if ok {
					if err := s.FinishOrphanRequeue(ctx, r); err != nil {
						t.Fatal(err)
					}
				}
				r.At = r.At.Add(2 * time.Hour)
			}
		})
	}
}
func TestOrphanResumeDurableHourlyAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: 42, At: at, Outcome: "cancelled_not_rerun", RunAttempt: 1}
	if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.FinishOrphanRequeue(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.RetryAt = at.Add(2 * time.Hour)
	if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	rows, err := s.RecentOrphanRequeues(ctx, "s")
	if err != nil || len(rows) != 2 || !rows[0].At.Equal(r.RetryAt) || rows[0].Outcome != "cancelled_not_rerun" {
		t.Fatalf("%v %v", rows, err)
	}
	// A restart sees the admitted mutation even if no HTTP outcome was persisted.
	r.At = r.RetryAt
	r.RetryAt = r.At.Add(30 * time.Minute)
	if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 1); err != nil || ok {
		t.Fatalf("hourly bypass: %v %v", ok, err)
	}
}

func TestOrphanResumeRetainsEveryHourlyCharge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: 42, At: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Outcome: "cancelled_not_rerun"}
	if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.FinishOrphanRequeue(ctx, r); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		r.RetryAt = r.At.Add(10 * time.Minute)
		ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3)
		if err != nil || ok != (i < 2) {
			t.Fatalf("retry %d=%v err=%v", i, ok, err)
		}
		r.At = r.RetryAt
	}
}

func TestOrphanResumeAdmissionRollsBackOnStorageFailure(t *testing.T) {
	for _, point := range []string{"orphan.resume.begin", "orphan.resume.claim", "orphan.resume.transfer", "orphan.resume.commit"} {
		t.Run(point, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			r := operations.OrphanRequeue{Scope: "s", Repository: "o/r", RunID: 42, At: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), Outcome: "cancelled_not_rerun"}
			if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.FinishOrphanRequeue(ctx, r); err != nil {
				t.Fatal(err)
			}
			r.RetryAt = r.At.Add(2 * time.Hour)
			s.injectFault = func(p string) error {
				if p == point {
					return context.Canceled
				}
				return nil
			}
			if ok, err := s.ClaimOrphanRequeue(ctx, r, time.Minute, 3); err == nil || ok {
				t.Fatal(ok, err)
			}
			s.injectFault = nil
			rows, err := s.RecentOrphanRequeues(ctx, "s")
			if err != nil || len(rows) != 1 || rows[0].Outcome != "cancelled_not_rerun" || !rows[0].At.Equal(r.At) {
				t.Fatalf("%v %v", rows, err)
			}
		})
	}
}
