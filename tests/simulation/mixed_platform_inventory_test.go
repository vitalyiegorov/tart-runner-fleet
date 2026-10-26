package simulation_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The generator previously observed only jobs served by its simulated node.
// Repository REST snapshots also contain sibling-platform jobs; inject that
// world-model state into the captured scope snapshot, with no local broker
// assignment for the foreign job.
func TestMixedPlatformScopeSnapshotPreservesLocalInventory(t *testing.T) {
	for _, cfg := range []worldConfig{macOSOnlyNodeWorld(), containerNodeWorld(), foreignArchitectureWorld("arm64"), foreignArchitectureWorld("amd64"), foreignSizeWorld()} {
		t.Run(cfg.Name, func(t *testing.T) {
			for seed := int64(1); seed <= 24; seed++ {
				t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
					w := newWorld(t, cfg, generateTrace(seed, 8, cfg))
					defer w.close()
					w.demand.StrictJobRouting = true
					w.tick = 1
					profile := cfg.Profiles[int(seed)%len(cfg.Profiles)]
					local := w.arrive("a/repo", profile, "pull_request")
					if local == nil {
						t.Fatal("local job was not generated")
					}
					w.captureSnapshot()
					snapshot := w.restQueue[len(w.restQueue)-1]
					foreign := domain.PlatformLinux
					arch := "arm64"
					if cfg.Bindings[0].Profile.Platform == domain.PlatformLinux {
						foreign = domain.PlatformMacOS
					}
					if strings.HasPrefix(cfg.Name, "mixed-architecture-") {
						foreign = domain.PlatformLinux
						if strings.HasSuffix(cfg.Name, "arm64") {
							arch = "amd64"
						}
					}
					route := "trf-" + string(foreign) + "-" + arch + "-4x8"
					if cfg.Name == "mixed-size" {
						// Same OS and architecture as every local set, at a size
						// none advertises (issue #434).
						route = "trf-linux-amd64-16x64"
					}
					snapshot.jobs = append(snapshot.jobs, githubscaleset.WorkflowJob{
						ID: 100000 + seed, RunID: 100000 + seed, RunAttempt: 1,
						Repository: githubscaleset.Repository{Owner: "a", Name: "repo"},
						Status:     "queued", Labels: []string{"self-hosted", route}, CreatedAt: w.now})
					snapshot.outstanding++
					w.deliverSnapshots()
					if len(w.findings) > 0 {
						t.Fatalf("foreign work poisoned observation: %+v", w.findings)
					}
					count := 0
					for _, binding := range cfg.Bindings {
						jobs, err := w.store.QueuedGitHubJobs(w.ctx, binding.ScaleSetID)
						if err != nil {
							t.Fatal(err)
						}
						for _, job := range jobs {
							if job.WorkflowJobID != local.requestID {
								t.Fatalf("foreign job attributed locally: %+v", job)
							}
							count++
						}
					}
					if count != 1 {
						t.Fatalf("local job lost or multiplied: count=%d", count)
					}
				})
			}
		})
	}
}

func foreignArchitectureWorld(arch string) worldConfig {
	cfg := containerNodeWorld()
	cfg.Name = "mixed-architecture-" + arch
	for i := range cfg.Bindings {
		profile := cfg.Bindings[i].Profile
		canonical := fmt.Sprintf("trf-linux-%s-%dx%d", arch, profile.Resources.CPU, profile.Resources.MemoryMB/1024)
		cfg.Bindings[i].ScaleSetLabels = append(cfg.Bindings[i].ScaleSetLabels, canonical)
	}
	return cfg
}

// foreignSizeWorld advertises each local set's canonical AMD64 route; the
// injected job names the same platform and architecture at an unserved size.
func foreignSizeWorld() worldConfig {
	cfg := foreignArchitectureWorld("amd64")
	cfg.Name = "mixed-size"
	return cfg
}
