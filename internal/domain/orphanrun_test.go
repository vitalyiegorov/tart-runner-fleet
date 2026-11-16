package domain

import (
	"testing"
	"time"
)

func TestCanRequeueOrphan(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "completed", "unknown"} {
		for _, jobStatus := range []string{"queued", "waiting", "completed", "in_progress", "pending", ""} {
			jobs := []OrphanRunJob{{ID: 1, Status: "completed"}, {ID: 2, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Status: jobStatus}}
			want := (status == "queued" || status == "in_progress") && jobStatus == "queued"
			if got := CanRequeueOrphan(status, jobs, 2, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); got != want {
				t.Fatalf("run=%s job=%s got=%v", status, jobStatus, got)
			}
		}
	}
	if CanRequeueOrphan("queued", nil, 1, time.Time{}) {
		t.Fatal("missing jobs authorized cancellation")
	}
	if !CanRequeueOrphan("in_progress", []OrphanRunJob{{ID: 1, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Status: "waiting"}, {ID: 2, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Status: "queued"}}, 2, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("waiting sibling vetoed")
	}
}
