package domain

// OrphanRunJob is fresh latest-attempt evidence, never inferred from an absent
// REST page. Unknown states and active jobs veto cancelling the whole run.
type OrphanRunJob struct {
	ID     int64
	Name   string
	Status string
}

// CanRequeueOrphan is the single whole-run safety rule. Completed jobs are
// preserved by rerun-failed-jobs; at least the candidate must still be queued.
func CanRequeueOrphan(status string, jobs []OrphanRunJob, candidate int64) bool {
	if status != "queued" && status != "in_progress" {
		return false
	}
	target := false
	for _, job := range jobs {
		if job.Status != "queued" && job.Status != "waiting" && job.Status != "completed" {
			return false
		}
		target = target || (job.ID == candidate && job.Status == "queued")
	}
	return target
}
