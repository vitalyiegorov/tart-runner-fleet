package replay_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
)

type replayAuditor struct {
	listed []githubscaleset.ScaleSetSummary
}

func (r replayAuditor) List(context.Context, string) ([]githubscaleset.ScaleSetSummary, error) {
	return r.listed, nil
}

func (r replayAuditor) Statistics(context.Context, int) (githubscaleset.ScaleSetStatistics, error) {
	return githubscaleset.ScaleSetStatistics{}, nil
}

// strandedFleet is the fleet as it actually stands, and the shape the first cut
// of this fixture got wrong: profiles are SHARED across scopes. node-b binds
// `linux-4x8` from the fleet scope and from budgie; the mini binds `maestro`
// from budgie (set 3) and from pony (set 2), and `maestro-studio` on the studio
// (set 6). A fixture binding one set per profile cannot see the defect that
// made this detector a no-op, because a per-profile instance count is only
// wrong when a profile has more than one set.
func strandedFleet() (config.Config, []app.Binding) {
	cfg := config.Default()
	cfg.Targets = []config.Target{
		{Type: "repo", Slug: "vitalyiegorov/tart-runner-fleet", MaxActive: 4},
		{Type: "repo", Slug: "vitalyiegorov/budgie", MaxActive: 4},
		{Type: "repo", Slug: "vitalyiegorov/pony", MaxActive: 4},
	}
	cfg.GitHub = config.GitHub{SessionOwner: "node-b",
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{
			{Name: "fleet", Kind: config.ScopeRepository, ConfigURL: "https://github.com/vitalyiegorov/tart-runner-fleet",
				Installation: "personal", Targets: []string{"vitalyiegorov/tart-runner-fleet"},
				ScaleSets: []config.ScaleSet{
					{Profile: "linux-4x8", Name: "trf-fleet-linux-amd64-4x8", ID: 12, MaxCapacity: 2},
					{Profile: "maestro", Name: "trf-fleet-maestro", ID: 16, MaxCapacity: 2}}},
			{Name: "budgie", Kind: config.ScopeRepository, ConfigURL: "https://github.com/vitalyiegorov/budgie",
				Installation: "personal", Targets: []string{"vitalyiegorov/budgie"},
				ScaleSets: []config.ScaleSet{
					{Profile: "linux-4x8", Name: "trf-budgie-linux-amd64-4x8", ID: 17, MaxCapacity: 2},
					{Profile: "maestro", Name: "trf-pony-maestro", ID: 2, MaxCapacity: 2},
					{Profile: "builder-2", Name: "trf-budgie-builder-2", ID: 9, MaxCapacity: 1},
					{Profile: "builder-2-studio", Name: "trf-budgie-builder-2-studio", ID: 10, MaxCapacity: 1}}},
			{Name: "pony", Kind: config.ScopeRepository, ConfigURL: "https://github.com/vitalyiegorov/pony",
				Installation: "personal", Targets: []string{"vitalyiegorov/pony"},
				ScaleSets: []config.ScaleSet{
					{Profile: "maestro", Name: "trf-pony-maestro-budgie", ID: 3, MaxCapacity: 2},
					{Profile: "maestro-studio", Name: "trf-pony-maestro-studio", ID: 6, MaxCapacity: 2}}}}}
	bindings := []app.Binding{
		{ScaleSetID: 12, Scope: "fleet", Targets: []string{"vitalyiegorov/tart-runner-fleet"},
			Profile: domain.Profile{ID: "linux-4x8", Route: "linux-4x8", Platform: domain.PlatformLinux}},
		{ScaleSetID: 16, Scope: "fleet", Targets: []string{"vitalyiegorov/tart-runner-fleet"},
			Profile: domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{ScaleSetID: 17, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"},
			Profile: domain.Profile{ID: "linux-4x8", Route: "linux-4x8", Platform: domain.PlatformLinux}},
		{ScaleSetID: 2, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"},
			Profile: domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{ScaleSetID: 3, Scope: "pony", Targets: []string{"vitalyiegorov/pony"},
			Profile: domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{ScaleSetID: 6, Scope: "pony", Targets: []string{"vitalyiegorov/pony"},
			Profile: domain.Profile{ID: "maestro-studio", Route: "macos-maestro", Platform: domain.PlatformMacOS}},
		{ScaleSetID: 9, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"},
			Profile: domain.Profile{ID: "builder-2", Route: "macos-builder", Platform: domain.PlatformMacOS}},
		{ScaleSetID: 10, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"},
			Profile: domain.Profile{ID: "builder-2-studio", Route: "macos-builder", Platform: domain.PlatformMacOS}},
	}
	return cfg, bindings
}

// TestIssue336StrandedBoundSetsAreFoundBesideTheirBusySiblings replays
// 2026-09-21 as both nodes read it, on a fleet whose profiles are shared.
//
// Three sets each node BINDS read `assigned>0 busy>0 registered=0` with an
// empty local queue, no instance of their own and no delivery for hours:
// budgie's 17 `trf-budgie-linux-amd64-4x8`, budgie's 2 `trf-pony-maestro`, and
// the studio's 6 `trf-pony-maestro-studio`. Every check passed, including the
// doctor's ingest-delivery row, and each was only cleared by deleting the set
// and provisioning a replacement.
//
// Beside them, on the SAME profiles, sit the sets that must stay silent: 12 and
// 3 are running instances (a per-profile instance count would have let those
// refute 17 and 2, which is the defect this fixture exists to catch), and 16
// has a registered runner -- the ADR 0054 false positive.
func TestIssue336StrandedBoundSetsAreFoundBesideTheirBusySiblings(t *testing.T) {
	cfg, bindings := strandedFleet()
	client := replayAuditor{listed: []githubscaleset.ScaleSetSummary{
		{ID: 12, Name: "trf-fleet-linux-amd64-4x8", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
		{ID: 16, Name: "trf-fleet-maestro", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 4, BusyRunners: 4, RegisteredRunners: 2}},
		{ID: 17, Name: "trf-budgie-linux-amd64-4x8", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
		{ID: 2, Name: "trf-pony-maestro", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
		{ID: 3, Name: "trf-pony-maestro-budgie", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
		{ID: 6, Name: "trf-pony-maestro-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
		// The two sets of 2026-09-21 ~17:00 UTC, one per Mac: identical counters,
		// no instance, no registered runner -- and a job of their own queued here.
		{ID: 9, Name: "trf-budgie-builder-2", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
		{ID: 10, Name: "trf-budgie-builder-2-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
	}}
	// The live VMs, as the node's own inventory saw them: one runner for the
	// fleet scope's linux-4x8 set and two for pony's maestro set. Nothing is
	// running for 17, 2 or 6 -- which is the whole finding, and which a count
	// taken per profile would have hidden behind these three.
	observedAt := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	instances := domain.Fresh([]domain.Instance{
		{ID: "trf-a", Repo: "vitalyiegorov/tart-runner-fleet", Profile: "linux-4x8", State: domain.InstanceRunning},
		{ID: "trf-b", Repo: "vitalyiegorov/pony", Profile: "maestro", State: domain.InstanceRunning},
		{ID: "trf-c", Repo: "vitalyiegorov/pony", Profile: "maestro", State: domain.InstanceOnlineIdle},
	}, observedAt)
	counts := app.ScaleSetInstances(bindings, instances)
	// The node's OWN queue per set, as `fleet queues` printed it. Sets 9 and 10
	// each hold one job the broker delivered at 15:14Z: GitHub is delivering for
	// them and both Mac slots are busy with Maestro shards, so they are waiting
	// on capacity. Every other bound set's queue is empty, which is the
	// signature of a set nothing is being offered for.
	queued := map[int]int{9: 1, 10: 1}

	holding := map[scalesetaudit.Key]time.Time{}
	now := observedAt
	run := func() scalesetaudit.Result {
		at := now
		result, err := scalesetaudit.Run(context.Background(), scalesetaudit.Request{Config: cfg,
			Key:  githubscaleset.NewPrivateKeySecret("pem"),
			Open: func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
			Now:  func() time.Time { return at },
			Local: func(scope string, id int) (scalesetaudit.Observation, bool) {
				for _, row := range counts {
					if row.Scope != scope || int(row.ScaleSetID) != id {
						continue
					}
					return scalesetaudit.Observation{Instances: row.Count, Queued: queued[id],
						HoldingSince: holding[scalesetaudit.Key{Scope: scope, ID: id}]}, true
				}
				return scalesetaudit.Observation{}, false
			}})
		if err != nil {
			t.Fatal(err)
		}
		holding = scalesetaudit.Track(holding, result, at)
		return result
	}

	// 09:00 — the first cadence. Nothing is a finding yet: at this instant every
	// one of these readings is indistinguishable from a runner mid-boot.
	if findings := run().Wedged(); len(findings) != 0 {
		t.Fatalf("a first reading is never a verdict: %#v", findings)
	}

	// 09:15 — one cadence later, past the node's three-minute boot timeout.
	now = now.Add(15 * time.Minute)
	ids := []int{}
	for _, set := range run().Wedged() {
		ids = append(ids, set.ID)
		if !strings.Contains(set.WedgedReason(), "recreate the set") {
			t.Fatalf("each finding must name the remedy: %q", set.WedgedReason())
		}
	}
	// Scope order, then id within a scope: budgie's 2 and 17, then pony's 6.
	// Sets 9 and 10 read the same on GitHub and are NOT findings: the node holds
	// a delivered job for each, which is proof of a listener.
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 17 || ids[2] != 6 {
		t.Fatalf("the three sets recreated on 2026-09-21 are the findings, and only they: %v", ids)
	}

	// 09:30 — GitHub resumes delivering for 17: a runner registers against it.
	// The finding is a live reading, not a latch.
	client.listed[2].Statistics.RegisteredRunners = 2
	now = now.Add(15 * time.Minute)
	ids = ids[:0]
	for _, set := range run().Wedged() {
		ids = append(ids, set.ID)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 6 {
		t.Fatalf("a set GitHub delivers for again must clear: %v", ids)
	}

	// 09:45 — the mini finishes a Maestro shard and set 9's job is dispatched:
	// its queue drains to empty while GitHub's counters stand unchanged. The set
	// must STILL not be a finding, because the clock a verdict needs was never
	// started while the node held work for it.
	queued[9] = 0
	now = now.Add(15 * time.Minute)
	for _, set := range run().Wedged() {
		if set.ID == 9 {
			t.Fatalf("a set that was waiting on capacity starts its clock fresh, never retroactively: %#v", set)
		}
	}
}
