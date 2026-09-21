package replay_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
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

// TestIssue336StrandedBoundSetsAreFoundAndOrdinaryBacklogIsNot replays
// 2026-09-21 as both nodes read it.
//
// Three sets each node BINDS read `assigned>0 busy>0 registered=0` with an
// empty local queue, no instance and no delivery for hours: node-b's 17
// `trf-budgie-linux-amd64-4x8`, the mini's 2 `trf-pony-maestro`, and the
// studio's 6 `trf-pony-maestro-studio`. Every check passed, including the
// doctor's ingest-delivery row, and each was only cleared by deleting the set
// and provisioning a replacement.
//
// The fourth set in this fixture is the case ADR 0054's amendment was written
// about and which must stay silent: the same counters with a runner REGISTERED
// against the set, which is an ordinary backlog rather than a wedge. The fifth
// is a set whose runner is mid-boot, which reads identically and is separated
// only by the node's boot timeout.
func TestIssue336StrandedBoundSetsAreFoundAndOrdinaryBacklogIsNot(t *testing.T) {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{SessionOwner: "node-b",
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{{Name: "budgie", Kind: config.ScopeRepository,
			ConfigURL: "https://github.com/vitalyiegorov/budgie", Installation: "personal",
			Targets: []string{"vitalyiegorov/budgie"},
			ScaleSets: []config.ScaleSet{
				{Profile: "linux-4x8", Name: "trf-budgie-linux-amd64-4x8", ID: 17, MaxCapacity: 2},
				{Profile: "maestro", Name: "trf-pony-maestro", ID: 2, MaxCapacity: 2},
				{Profile: "maestro-studio", Name: "trf-pony-maestro-studio", ID: 6, MaxCapacity: 2},
				{Profile: "builder", Name: "trf-budgie-builder-2", ID: 16, MaxCapacity: 2},
				{Profile: "small", Name: "trf-fleet-small", ID: 8, MaxCapacity: 2}}}}}
	client := replayAuditor{listed: []githubscaleset.ScaleSetSummary{
		{ID: 17, Name: "trf-budgie-linux-amd64-4x8", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
		{ID: 2, Name: "trf-pony-maestro", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 3, BusyRunners: 3}},
		{ID: 6, Name: "trf-pony-maestro-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
		{ID: 16, Name: "trf-budgie-builder-2", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 4, BusyRunners: 4, RegisteredRunners: 2}},
		{ID: 8, Name: "trf-fleet-small", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 1, BusyRunners: 1}},
	}}

	// The node holds no instance for any of them, which is the state its own
	// telemetry published all day.
	observed := map[scalesetaudit.Key]time.Time{}
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	run := func() scalesetaudit.Result {
		at := now
		result, err := scalesetaudit.Run(context.Background(), scalesetaudit.Request{Config: cfg,
			Key:  githubscaleset.NewPrivateKeySecret("pem"),
			Open: func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return client, nil },
			Now:  func() time.Time { return at },
			Local: func(scope string, id int, _ string) (scalesetaudit.Observation, bool) {
				return scalesetaudit.Observation{Instances: 0,
					HoldingSince: observed[scalesetaudit.Key{Scope: scope, ID: id}]}, true
			}})
		if err != nil {
			t.Fatal(err)
		}
		observed = scalesetaudit.Track(observed, result, at)
		return result
	}

	// 09:00 — the first cadence. Nothing is a finding yet: at this instant every
	// one of these readings is indistinguishable from a runner mid-boot.
	if findings := run().Wedged(); len(findings) != 0 {
		t.Fatalf("a first reading is never a verdict: %#v", findings)
	}

	// 09:15 — one cadence later, past the node's three-minute boot timeout. The
	// set whose runner has since registered (16) drops out on its own.
	now = now.Add(15 * time.Minute)
	findings := run().Wedged()
	if len(findings) != 4 {
		t.Fatalf("every bound set holding work with no runner and no instance is a finding: %#v", findings)
	}

	// 09:30 — set 8's runner finished booting and took the job, which is what
	// the boot timeout exists to allow; it must stop being a finding, and the
	// three sets of issue #336 must remain.
	client.listed[4].Statistics.RegisteredRunners = 1
	now = now.Add(15 * time.Minute)
	ids := []int{}
	for _, set := range run().Wedged() {
		ids = append(ids, set.ID)
		if !strings.Contains(set.WedgedReason(), "recreate the set") {
			t.Fatalf("each finding must name the remedy: %q", set.WedgedReason())
		}
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 6 || ids[2] != 17 {
		t.Fatalf("the three sets recreated on 2026-09-21 are the findings: %v", ids)
	}
}
