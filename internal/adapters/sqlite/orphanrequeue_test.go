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
