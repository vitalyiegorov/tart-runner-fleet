package scalesetaudit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

var strandedAt = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// budgieConfig is node-b on 2026-09-21: it BINDS `trf-budgie-linux-amd64-4x8`
// (id 17), the set GitHub stopped delivering for (#336).
func budgieConfig() config.Config {
	cfg := sudokuConfig()
	scope := &cfg.GitHub.Scopes[0]
	scope.Name = "budgie"
	scope.ConfigURL = "https://github.com/vitalyiegorov/budgie"
	scope.Targets = []string{"vitalyiegorov/budgie"}
	scope.ScaleSets[0] = config.ScaleSet{Profile: "linux-4x8", Name: "trf-budgie-linux-amd64-4x8",
		ID: 17, MaxCapacity: 2}
	return cfg
}

// strandedListing is what GitHub answered for set 17 all day: three jobs
// assigned, three runners busy, not one runner registered and nothing acquired.
func strandedListing() []githubscaleset.ScaleSetSummary {
	return []githubscaleset.ScaleSetSummary{{ID: 17, Name: "trf-budgie-linux-amd64-4x8",
		Statistics: &githubscaleset.ScaleSetStatistics{AssignedJobs: 3, BusyRunners: 3}}}
}

func strandedRequest(client Client, local func(string, int) (Observation, bool)) Request {
	return Request{Config: budgieConfig(), Key: githubscaleset.NewPrivateKeySecret("pem"),
		Open:  func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil },
		Now:   func() time.Time { return strandedAt },
		Local: local}
}

func observed(instances, queued int, since time.Time) func(string, int) (Observation, bool) {
	return func(string, int) (Observation, bool) {
		return Observation{Instances: instances, Queued: queued, HoldingSince: since}, true
	}
}

// TestABoundSetGitHubHoldsWorkForWithNoRunnerAndNoInstanceIsStranded is issue
// #336, three times in one day: GitHub's own counters for a set this node BINDS
// read assigned=3 busy=3 registered=0 while the node's queue for it was empty,
// no instance existed for it, and no job had been delivered for hours. Every
// fleet check passed, including the doctor's ingest-delivery row.
//
// The reading alone is not the fault -- ADR 0054's amendment showed the same
// signature on an ordinary backlog and on a stale counter -- so the finding
// needs the two facts one node CAN establish about a set it serves: it holds no
// instance for the set, and the reading has stood longer than a boot takes.
func TestABoundSetGitHubHoldsWorkForWithNoRunnerAndNoInstanceIsStranded(t *testing.T) {
	client := &fakeClient{listed: strandedListing()}
	since := strandedAt.Add(-4 * time.Hour)

	result, err := Run(context.Background(), strandedRequest(client, observed(0, 0, since)))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ScaleSets) != 1 || result.ScaleSets[0].State != Bound {
		t.Fatalf("the configured set is bound: %#v", result.ScaleSets)
	}
	set := result.ScaleSets[0]
	if set.Assigned != 3 || set.Busy != 3 || set.Registered != 0 {
		t.Fatalf("the counters GitHub answered must be carried: %#v", set)
	}
	if set.Queued == nil || *set.Queued != 0 {
		t.Fatalf("the local queue reading must be carried: %#v", set)
	}
	if set.Instances == nil || *set.Instances != 0 || set.HoldingSince == nil || !set.HoldingSince.Equal(since) {
		t.Fatalf("the local observation must be carried: %#v", set)
	}
	if !set.Wedged {
		t.Fatalf("a bound set holding work with no runner, no instance and no delivery is a finding: %#v", set)
	}
	if set.Stranding {
		t.Fatal("a bound set is never a parked-set finding: the parked row stays informational (ADR 0054)")
	}
	wedged := result.Wedged()
	if len(wedged) != 1 || wedged[0].ID != 17 {
		t.Fatalf("the finding must be set 17: %#v", wedged)
	}
	reason := wedged[0].WedgedReason()
	for _, want := range []string{"budgie", "scale set 17", "trf-budgie-linux-amd64-4x8", "3 assigned job(s)",
		"no runner registered", "the node's queue for it is empty", "recreate the set"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the finding must say %q: %q", want, reason)
		}
	}
}

// A runner that has not finished booting reads exactly like a stranding. The
// boot timeout the node already declares is the discriminator, and it is read
// from the configuration rather than introduced as a second knob.
func TestABootInProgressIsNotAStranding(t *testing.T) {
	client := &fakeClient{listed: strandedListing()}
	fresh := strandedAt.Add(-30 * time.Second)

	result, err := Run(context.Background(), strandedRequest(client, observed(0, 0, fresh)))
	if err != nil {
		t.Fatal(err)
	}
	if result.ScaleSets[0].Wedged || len(result.Wedged()) != 0 {
		t.Fatalf("a reading younger than the boot timeout is not a finding: %#v", result.ScaleSets[0])
	}
}

// An instance for the set is proof this node IS serving the work, whatever
// GitHub's counters say.
func TestAnInstanceForTheSetRefutesTheFinding(t *testing.T) {
	client := &fakeClient{listed: strandedListing()}

	result, err := Run(context.Background(), strandedRequest(client, observed(1, 0, strandedAt.Add(-4*time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	if result.ScaleSets[0].Wedged {
		t.Fatalf("an instance for the set refutes the finding: %#v", result.ScaleSets[0])
	}
}

// TestASetTheNodeHoldsQueuedWorkForIsWaitingOnCapacity is the live false
// positive of 2026-09-21 ~17:00 UTC, on the release that first carried this
// detector: `budgie` set 9 (`trf-budgie-builder-2`) on the mac mini and set 10
// (`trf-budgie-builder-2-studio`) on the studio read assigned=2 busy=2
// registered=0 with no instance for an hour, and the doctor told the operator
// to RECREATE them -- while `fleet queues` on the same node at the same instant
// showed `jobs: 1, delivered: 1` for the set. GitHub was delivering; both Mac
// slots were simply busy with Maestro shards, and the sets were waiting on
// capacity.
//
// The stranding signature always carried this term -- ADR 0056 states that a
// stranded set's local queue is empty by construction -- and the predicate did
// not read it. A set this node is holding delivered work for has a listener by
// proof: recreating it would destroy a healthy set and its queued jobs.
func TestASetTheNodeHoldsQueuedWorkForIsWaitingOnCapacity(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{{ID: 17, Name: "trf-budgie-builder-2",
		Statistics: &githubscaleset.ScaleSetStatistics{AssignedJobs: 2, BusyRunners: 2}}}}

	result, err := Run(context.Background(), strandedRequest(client, observed(0, 1, strandedAt.Add(-time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	set := result.ScaleSets[0]
	if set.Queued == nil || *set.Queued != 1 {
		t.Fatalf("the node's own queue depth for the set must be carried: %#v", set)
	}
	if set.Starving() {
		t.Fatalf("a set this node holds queued work for is not starving: %#v", set)
	}
	if set.Wedged || len(result.Wedged()) != 0 {
		t.Fatalf("a set waiting on capacity is not a finding: %#v", set)
	}
	// The clock must not merely be judged -- it must never start, so an hour of
	// capacity waiting cannot become a verdict the moment the queue drains.
	if tracked := Track(nil, result, strandedAt); len(tracked) != 0 {
		t.Fatalf("a set waiting on capacity keeps no clock: %#v", tracked)
	}
}

// A node that has published no queue row for a set it serves has not observed
// that set's queue, and an unobserved queue is never an empty one (contributor
// rule 4). The daemon publishes the instance rows and the scope-queue rows from
// the same tick, so this is the shape of a partial publication.
func TestAnUnobservedQueueMakesNoBoundFinding(t *testing.T) {
	client := &fakeClient{listed: strandedListing()}

	result, err := Run(context.Background(), strandedRequest(client, func(string, int) (Observation, bool) {
		return Observation{}, false
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ScaleSets[0].Queued != nil || result.ScaleSets[0].Wedged {
		t.Fatalf("an unobserved queue makes no finding: %#v", result.ScaleSets[0])
	}
}

// A node that cannot observe itself reports NO finding and no zero: an
// unobserved instance count is not an absent instance (contributor rule 4).
func TestAnUnobservedNodeMakesNoBoundFinding(t *testing.T) {
	client := &fakeClient{listed: strandedListing()}

	result, err := Run(context.Background(), strandedRequest(client, nil))
	if err != nil {
		t.Fatal(err)
	}
	set := result.ScaleSets[0]
	if set.Instances != nil || set.HoldingSince != nil {
		t.Fatalf("an unobserved node must carry no local reading: %#v", set)
	}
	if set.Wedged {
		t.Fatal("an unobserved node cannot make a bound finding")
	}
}

// A bound set whose listing carried no statistics is now READ: its counters are
// the evidence this detector stands on, so leaving a bound set uncounted would
// leave #336 invisible on every scope whose listing omits them.
func TestABoundSetWithoutListedStatisticsIsRead(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{{ID: 17, Name: "trf-budgie-linux-amd64-4x8"}},
		statistics: map[int]githubscaleset.ScaleSetStatistics{17: {AssignedJobs: 3, BusyRunners: 3}}}

	result, err := Run(context.Background(), strandedRequest(client, observed(0, 0, strandedAt.Add(-4*time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.reads) != 1 || client.reads[0] != 17 {
		t.Fatalf("the bound set must be read once: %#v", client.reads)
	}
	if !result.ScaleSets[0].Wedged {
		t.Fatalf("the read counters are the finding: %#v", result.ScaleSets[0])
	}
}

// Track is where the persistence the predicate needs comes from, and it is a
// pure fold over one audit: a reading that qualifies starts its clock and keeps
// it, and a reading that stops qualifying drops it.
func TestTrackStartsKeepsAndDropsTheClock(t *testing.T) {
	holding := ScaleSet{Scope: "budgie", ID: 17, State: Bound, Assigned: 3, Busy: 3,
		Instances: new(int), Queued: new(int)}
	first := Track(nil, Result{ScaleSets: []ScaleSet{holding}}, strandedAt)
	key := Key{Scope: "budgie", ID: 17}
	if since, ok := first[key]; !ok || !since.Equal(strandedAt) {
		t.Fatalf("a qualifying reading starts the clock: %#v", first)
	}

	later := strandedAt.Add(time.Hour)
	kept := Track(first, Result{ScaleSets: []ScaleSet{holding}}, later)
	if since, ok := kept[key]; !ok || !since.Equal(strandedAt) {
		t.Fatalf("a reading that still qualifies keeps its first instant: %#v", kept)
	}

	delivered := holding
	delivered.Registered = 2
	if cleared := Track(kept, Result{ScaleSets: []ScaleSet{delivered}}, later); len(cleared) != 0 {
		t.Fatalf("a set with a registered runner drops its clock: %#v", cleared)
	}

	queued := holding
	one := 1
	queued.Queued = &one
	if cleared := Track(kept, Result{ScaleSets: []ScaleSet{queued}}, later); len(cleared) != 0 {
		t.Fatalf("a set the node holds queued work for drops its clock: %#v", cleared)
	}
}
