package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestIssue456ContainerSeed143 pins the reduced nightly incident, rather than
// relying on a generator seed that a future event can change.
func TestIssue456ContainerSeed143(t *testing.T) {
	t.Parallel()
	cfg := containerNodeWorld()
	trace := simTrace{Seed: 143, Config: cfg.Name, Ticks: 100, Events: []simEvent{
		{Tick: 1, Kind: eventArrive, Repo: "ops/fleet", Profile: "xl", Event: "push"},
		{Tick: 2, Kind: eventSiblingSubstitute, Count: 6},
		{Tick: 9, Kind: eventOverrunJob, Count: 3},
		{Tick: 10, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "push"},
		{Tick: 10, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "push"},
		{Tick: 13, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "push"},
		{Tick: 13, Kind: eventArrive, Repo: "a/repo", Profile: "medium", Event: "push"},
		{Tick: 13, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "push"},
		{Tick: 13, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "schedule"},
		{Tick: 14, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "push"},
		{Tick: 14, Kind: eventArrive, Repo: "b/repo", Profile: "xl", Event: "schedule"},
		{Tick: 14, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "pull_request"},
		{Tick: 14, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "push"},
		{Tick: 15, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "schedule"},
		{Tick: 19, Kind: eventArrive, Repo: "ops/fleet", Profile: "xl", Event: "push"},
		{Tick: 21, Kind: eventOverrunJob, Count: 4},
		{Tick: 23, Kind: eventArrive, Repo: "c/repo", Profile: "xl", Event: "schedule"},
		{Tick: 24, Kind: eventArrive, Repo: "ops/fleet", Profile: "medium", Event: "push"},
		{Tick: 24, Kind: eventArrive, Repo: "c/repo", Profile: "large", Event: "pull_request"},
		{Tick: 27, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "pull_request"},
		{Tick: 28, Kind: eventSlowBoot, Count: 3},
		{Tick: 30, Kind: eventArrive, Repo: "ops/fleet", Profile: "large", Event: "push"},
		{Tick: 39, Kind: eventArrive, Repo: "ops/fleet", Profile: "large", Event: "schedule"},
		{Tick: 39, Kind: eventArrive, Repo: "b/repo", Profile: "large", Event: "schedule"},
		{Tick: 41, Kind: eventArrive, Repo: "a/repo", Profile: "large", Event: "pull_request"},
		{Tick: 43, Kind: eventArrive, Repo: "c/repo", Profile: "small", Event: "push"},
		{Tick: 50, Kind: eventArrive, Repo: "b/repo", Profile: "small", Event: "schedule"},
		{Tick: 52, Kind: eventArrive, Repo: "ops/fleet", Profile: "small", Event: "push"},
	}}
	if findings := runTrace(t, cfg, trace); len(findings) > 0 {
		t.Fatalf("seed 143 reduced replay: %s", findings[0])
	}
}

// A profile's fair-share winner inherits that profile's earlier queue position.
// The intervening medium did not compete for that large slot; comparing only
// its rank with the promoted large mistakes the lawful permutation for FIFO.
func TestIssue456FairSharePromotionAcrossAnotherProfile(t *testing.T) {
	cfg := containerNodeWorld()
	waiting := oracleDemand(cfg, simControlPlaneRepo, 15, 34*time.Minute, "medium")
	overtaker := oracleDemand(cfg, "b/repo", 20, 26*time.Minute+30*time.Second, "large")
	peer := oracleDemand(cfg, "c/repo", 16, 34*time.Minute, "large")
	live := []domain.Instance{oracleInstance(cfg, "trf-large-a", "a/repo", "large"), oracleInstance(cfg, "trf-medium-b", "b/repo", "medium"), oracleInstance(cfg, "trf-small-ops", simControlPlaneRepo, "small")}
	prefix := []domain.Demand{
		oracleDemand(cfg, "c/repo", 7, 40*time.Minute, "xl"),
		oracleDemand(cfg, "c/repo", 10, 39*time.Minute, "xl"),
		oracleDemand(cfg, "c/repo", 14, 35*time.Minute, "xl"),
		oracleDemand(cfg, "b/repo", 9, 39*time.Minute, "xl"),
		oracleDemand(cfg, simControlPlaneRepo, 13, 36*time.Minute, "xl"),
	}
	for _, tc := range []struct {
		name      string
		peer      domain.Demand
		include   bool
		exempt    bool
		barrier   bool
		lowerTier bool
		equalRank bool
	}{
		{"lower-rank large inherits an earlier large position", peer, true, true, false, false, false},
		{"without that earlier position the inversion is real", peer, false, false, false, false, false},
		{"a later peer cannot justify crossing the medium", oracleDemand(cfg, "c/repo", 16, 30*time.Minute, "large"), true, false, false, false, false},
		{"another profile cannot donate a position", oracleDemand(cfg, "c/repo", 16, 34*time.Minute, "small"), true, false, false, false, false},
		{"a platform barrier forbids the promotion", peer, true, false, true, false, false},
		{"a lower tier cannot borrow that position", peer, true, false, false, true, false},
		{"equal-ranked peers keep their age order", peer, true, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			demands := append([]domain.Demand{}, prefix...)
			if tc.equalRank {
				demands = demands[1:]
			}
			waiting := waiting
			if tc.lowerTier {
				waiting.Priority.Rank = 1
			}
			if tc.barrier {
				foreign := oracleDemand(cfg, "foreign/repo", 99, 32*time.Minute, "small")
				foreign.Platform = domain.PlatformMacOS
				demands = append(demands, foreign)
			}
			demands = append(demands, waiting, overtaker)
			if tc.include {
				demands = append(demands, tc.peer)
			}
			observation := admitting(oracleObservation(92, demands, live, domain.Resources{CPU: 5, MemoryMB: 16384, Slots: 4}, nil), overtaker)
			if got := yieldsOnScopeShare(cfg, observation, overtaker, waiting); got != tc.exempt {
				t.Fatalf("exempt=%v want %v", got, tc.exempt)
			}
		})
	}
}

// Neither a lower rank in the same profile nor a scope's own FIFO is donated
// away by a different scope's peer. Keep these real inversions reportable.
func TestIssue456PromotionCannotExcuseOwnQueueInversion(t *testing.T) {
	cfg := containerNodeWorld()
	peer := oracleDemand(cfg, "c/repo", 10, 40*time.Minute, "large")
	for _, tc := range []struct {
		name    string
		repo    string
		profile domain.ProfileID
	}{
		{"same-profile lower rank", "a/repo", "large"},
		{"same-scope older demand", "b/repo", "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waiting := oracleDemand(cfg, tc.repo, 15, 34*time.Minute, tc.profile)
			candidate := oracleDemand(cfg, "b/repo", 20, 26*time.Minute, "large")
			demands := []domain.Demand{waiting, candidate, peer}
			for i := int64(0); i < 4; i++ {
				demands = append(demands, oracleDemand(cfg, "c/repo", 30+i, 45*time.Minute, "xl"))
			}
			live := []domain.Instance{oracleInstance(cfg, "trf-medium-b", "b/repo", "medium")}
			observation := oracleObservation(92, demands, live, domain.Resources{CPU: 8, MemoryMB: 24576, Slots: 3}, nil)
			if yieldsOnScopeShare(cfg, observation, candidate, waiting) {
				t.Fatal("unrelated peer excused own queue inversion")
			}
		})
	}
}
