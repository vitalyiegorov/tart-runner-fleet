package simulation_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// listRunners is GitHub's repository runner listing in this world: the
// repository's standalone runners, and nothing for the fleet's own scale-set
// runners, which the real listing shows without labels.
func (w *world) listRunners(_ context.Context, repo githubscaleset.Repository) ([]githubscaleset.Runner, error) {
	var listed []githubscaleset.Runner
	for _, runner := range w.standaloneRunners {
		if runner.Repository == repo {
			listed = append(listed, runner)
		}
	}
	return listed, nil
}

// TestIssue498StandaloneRunnerJobDoesNotPoisonAServedPlatform injects the
// Mac mini's knee-doctor state into a node that serves macOS: a queued
// `[self-hosted, macOS, webkit]` job that only the repository's standalone
// runner serves. While that runner is listed the job is foreign and the local
// job still lands; without it the same snapshot stays uncertain, as #434 asks.
func TestIssue498StandaloneRunnerJobDoesNotPoisonAServedPlatform(t *testing.T) {
	for _, cfg := range []worldConfig{macOSOnlyNodeWorld(), defaultWorld()} {
		for _, listed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/standalone-listed=%v", cfg.Name, listed), func(t *testing.T) {
				for seed := int64(1); seed <= 12; seed++ {
					w := newWorld(t, cfg, generateTrace(seed, 8, cfg))
					w.demand.StrictJobRouting = true
					repo := githubscaleset.Repository{Owner: "a", Name: "repo"}
					if listed {
						w.standaloneRunners = []githubscaleset.Runner{{ID: 1, Repository: repo, Name: "macmini-webkit",
							Status: "online", Busy: true, Labels: []string{"self-hosted", "ARM64", "macOS", "webkit"}}}
					}
					w.tick = 1
					var local *simJob
					for _, profile := range cfg.Profiles {
						if cfg.Scheduler.Profiles[profile].Platform == domain.PlatformMacOS {
							local = w.arrive("a/repo", profile, "pull_request")
							break
						}
					}
					if local == nil {
						t.Fatal("local macOS job was not generated")
					}
					w.captureSnapshot()
					snapshot := w.restQueue[len(w.restQueue)-1]
					snapshot.jobs = append(snapshot.jobs, githubscaleset.WorkflowJob{
						ID: 100000 + seed, RunID: 100000 + seed, RunAttempt: 1, Repository: repo,
						Status: "queued", Labels: []string{"self-hosted", "macOS", "webkit"}, CreatedAt: w.now})
					snapshot.outstanding++
					w.deliverSnapshots()
					if !listed {
						if len(w.findings) == 0 {
							t.Fatalf("seed %d: unproven job accepted without a standalone runner", seed)
						}
						w.close()
						continue
					}
					if len(w.findings) > 0 {
						t.Fatalf("seed %d: standalone runner's job poisoned observation: %+v", seed, w.findings)
					}
					count := 0
					for _, binding := range cfg.Bindings {
						jobs, err := w.store.QueuedGitHubJobs(w.ctx, binding.ScaleSetID)
						if err != nil {
							t.Fatal(err)
						}
						for _, job := range jobs {
							if job.WorkflowJobID != local.requestID {
								t.Fatalf("seed %d: standalone runner's job attributed locally: %+v", seed, job)
							}
							count++
						}
					}
					if count != 1 {
						t.Fatalf("seed %d: local job lost or multiplied: count=%d", seed, count)
					}
					w.close()
				}
			})
		}
	}
}
