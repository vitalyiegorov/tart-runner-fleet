package domain

import "testing"

func TestCanRequeueOrphan(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "completed", "unknown"} {
		for _, jobStatus := range []string{"queued", "waiting", "completed", "in_progress", "pending", ""} {
			jobs := []OrphanRunJob{{ID: 1, Status: "completed"}, {ID: 2, Status: jobStatus}}
			want := (status == "queued" || status == "in_progress") && jobStatus == "queued"
			if got := CanRequeueOrphan(status, jobs, 2); got != want {
				t.Fatalf("run=%s job=%s got=%v", status, jobStatus, got)
			}
		}
	}
	if CanRequeueOrphan("queued", nil, 1) {
		t.Fatal("missing jobs authorized cancellation")
	}
	if !CanRequeueOrphan("in_progress", []OrphanRunJob{{ID: 1, Status: "waiting"}, {ID: 2, Status: "queued"}}, 2) {
		t.Fatal("waiting sibling vetoed")
	}
}
