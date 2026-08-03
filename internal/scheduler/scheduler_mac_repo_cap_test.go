package scheduler

import (
	"reflect"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// FINDING 2 of ADR 0031, found by the simulation harness (#128).
//
// `appendMacSpawns` bounded macOS admission by the resource envelope and by the
// profile's `MaxActive`, and by nothing else: it never read `Config.RepoCaps`.
// `activeRepoCounts` charges a macOS instance to its repository exactly like a
// Linux one, so the cap was a Linux-only bound in practice — one repository
// could hold its whole Linux cap plus an unbounded macOS cohort.
//
// It is not a cosmetic inconsistency. ADR 0030's `reservedRepoSlack` computes
// how many demands a complementary pass may admit in the reserved head's own
// repository as `cap - occupied - 1`, which is only a slot the head can rely on
// if the cap actually bounds every platform that consumes it. ADR 0012 says the
// same thing from the other side: caps and vectors are shared across platforms.
//
// Every macOS spawn in the planner is emitted by `appendMacSpawns` — the head
// branch, the mixed remainder, both handoff waves, and the exclusive path all
// funnel through it — so the guard belongs there and nowhere else, exactly as
// #129 gave the queue one plannable seam instead of one rule per pass.

// macCapConfig is the default test topology with per-repository caps that bite:
// `a/repo` may hold one instance, `b/repo` four.
func macCapConfig(mutate func(*Config)) Config {
	cfg := testConfig()
	cfg.RepoCaps = map[string]int{"a/repo": 1, "b/repo": 4}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func macCapInput(cfg Config, demands []domain.Demand, instances []domain.Instance) Input {
	in := input(demands, instances, State{})
	in.Config = cfg
	return in
}

func spawnedJobIDs(plan Plan) []int64 {
	ids := make([]int64, 0, len(plan.Operations))
	for _, key := range spawnedKeys(plan) {
		ids = append(ids, key.JobID)
	}
	return ids
}

// runningInstance is one live, job-carrying instance: it holds its repository's
// cap slot, which an idle or draining instance does not.
func runningInstance(cfg Config, id, repo string, profile domain.ProfileID) domain.Instance {
	definition := cfg.Profiles[profile]
	return domain.Instance{ID: id, Repo: repo, Platform: definition.Platform, Profile: profile,
		Route: definition.Route, Resources: definition.Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning}
}

// TestMacOSAdmissionHonorsRepositoryCaps walks every seam that reaches
// `appendMacSpawns`. Each case leaves room in the envelope and under `MaxActive`
// for more macOS work than the repository cap allows, so the only thing that can
// refuse the extra spawn is the cap.
//
// The refusal is per demand, not per pass: a capped repository is SKIPPED and
// the next repository's work takes the vector, which is what the Linux allocator
// already does in `exactSelect` and what keeps the queue moving.
func TestMacOSAdmissionHonorsRepositoryCaps(t *testing.T) {
	for _, test := range []struct {
		name      string
		seam      string
		mutate    func(*Config)
		instances []domain.Instance
		demands   []domain.Demand
		want      []int64
	}{
		{
			name: "head admission on an idle host",
			seam: "planMacOS",
			demands: []domain.Demand{
				demand("a/repo", 1, 30*time.Minute, "maestro"),
				demand("a/repo", 2, 29*time.Minute, "maestro"),
				demand("b/repo", 3, 28*time.Minute, "maestro"),
			},
			want: []int64{1, 3},
		},
		{
			name: "a live instance already holds the only slot",
			seam: "planMacOS beside live Linux",
			demands: []domain.Demand{
				demand("a/repo", 1, 30*time.Minute, "maestro"),
				demand("b/repo", 2, 29*time.Minute, "maestro"),
			},
			instances: []domain.Instance{runningInstance(macCapConfig(nil), "linux-live", "a/repo", "small")},
			want:      []int64{2},
		},
		{
			name:   "the mixed remainder behind a Linux head",
			seam:   "fillMacRemainder",
			mutate: func(cfg *Config) { cfg.MixedPlatformAdmission = true },
			demands: []domain.Demand{
				demand("a/repo", 1, time.Minute, "small"),
				demand("a/repo", 2, 2*time.Minute, "maestro"),
			},
			want: []int64{1},
		},
		{
			name: "the macOS backfill wave of a Linux handoff",
			seam: "planLinuxHandoff",
			demands: []domain.Demand{
				demand("a/repo", 1, time.Minute, "small"),
				demand("a/repo", 2, 2*time.Minute, "maestro"),
				demand("b/repo", 3, 3*time.Minute, "maestro"),
			},
			instances: []domain.Instance{runningInstance(macCapConfig(nil), "mac-live", "a/repo", "maestro")},
			want:      []int64{3},
		},
		{
			name:   "the exclusive macOS handoff",
			seam:   "planExclusiveMacHandoff",
			mutate: func(cfg *Config) { cfg.MacOSExclusive = true },
			demands: []domain.Demand{
				demand("a/repo", 1, 30*time.Minute, "maestro"),
				demand("a/repo", 2, 29*time.Minute, "maestro"),
				demand("b/repo", 3, 28*time.Minute, "maestro"),
			},
			want: []int64{1, 3},
		},
		{
			name:   "an unconfigured cap normalizes to one",
			seam:   "planMacOS",
			mutate: func(cfg *Config) { cfg.RepoCaps = map[string]int{} },
			demands: []domain.Demand{
				demand("a/repo", 1, 30*time.Minute, "maestro"),
				demand("a/repo", 2, 29*time.Minute, "maestro"),
				demand("b/repo", 3, 28*time.Minute, "maestro"),
			},
			want: []int64{1, 3},
		},
		{
			name:   "work inside the cap is still admitted",
			seam:   "planMacOS",
			mutate: func(cfg *Config) { cfg.RepoCaps["a/repo"] = 4 },
			demands: []domain.Demand{
				demand("a/repo", 1, 30*time.Minute, "maestro"),
				demand("a/repo", 2, 29*time.Minute, "maestro"),
			},
			want: []int64{1, 2},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := macCapConfig(test.mutate)
			plan := PlanTick(macCapInput(cfg, test.demands, test.instances))
			if got := spawnedJobIDs(plan); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("%s admitted jobs %v, want %v\n%#v", test.seam, got, test.want, plan.Operations)
			}
		})
	}
}
