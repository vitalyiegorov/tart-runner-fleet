package scalesetaudit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeClient struct {
	listed     []githubscaleset.ScaleSetSummary
	statistics map[int]githubscaleset.ScaleSetStatistics
	listErr    error
	readErr    error
	lists      int
	reads      []int
	group      string
}

func (f *fakeClient) List(_ context.Context, runnerGroup string) ([]githubscaleset.ScaleSetSummary, error) {
	f.lists++
	f.group = runnerGroup
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

func (f *fakeClient) Statistics(_ context.Context, id int) (githubscaleset.ScaleSetStatistics, error) {
	f.reads = append(f.reads, id)
	if f.readErr != nil {
		return githubscaleset.ScaleSetStatistics{}, f.readErr
	}
	return f.statistics[id], nil
}

var auditedAt = time.Date(2026, 8, 4, 18, 30, 0, 0, time.UTC)

// sudokuConfig is the mac studio's configuration on 2026-08-04: it binds the
// builder scale set 1 and names nothing else.
func sudokuConfig() config.Config {
	cfg := config.Default()
	cfg.GitHub = config.GitHub{SessionOwner: "host",
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes: []config.GitHubScope{{Name: "suuudokuuu", Kind: config.ScopeRepository,
			ConfigURL: "https://github.com/vitalyiegorov/suuudokuuu", Installation: "personal",
			Targets:   []string{"vitalyiegorov/suuudokuuu"},
			ScaleSets: []config.ScaleSet{{Profile: "builder", Name: "trf-sudoku-builder", ID: 1, MaxCapacity: 2}}}}}
	return cfg
}

func auditRequest(cfg config.Config, client Client) Request {
	return Request{Config: cfg, Key: githubscaleset.NewPrivateKeySecret("pem"),
		Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil },
		Now:  func() time.Time { return auditedAt }}
}

// TestAParkedScaleSetHoldingAssignedJobsIsAStranding is issue #164's Case A,
// exactly as it happened.
//
// GitHub held two scale sets for `vitalyiegorov/suuudokuuu`. The mac mini's
// `trf-sudoku-builder` (id 1) was polled, healthy and idle. The mac studio's
// `trf-sudoku-builder-studio` (id 7) was parked — its binding had been removed
// from the node's configuration while the object stayed on GitHub — and it held
// assigned 2 / busy 2. Two `mobile-e2e` jobs queued at 14:36:00Z were still
// queued 4.5 hours later, and GitHub never offered them to the identically
// labelled healthy set, because it had already routed them.
//
// `fleet queues` read 0, `fleet doctor` read PASS and every observation read
// fresh. This test is the answer to that: the audit names set 7.
func TestAParkedScaleSetHoldingAssignedJobsIsAStranding(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{}},
		{ID: 7, Name: "trf-sudoku-builder-studio", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 2}},
	}}

	result, err := Run(context.Background(), auditRequest(sudokuConfig(), client))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.ScaleSets) != 2 {
		t.Fatalf("both sets GitHub holds must be reported: %#v", result.ScaleSets)
	}
	if result.ScaleSets[0].State != Bound || result.ScaleSets[0].Stranding {
		t.Fatalf("the configured set is bound and is never a finding: %#v", result.ScaleSets[0])
	}
	strandings := result.Strandings()
	if len(strandings) != 1 || strandings[0].ID != 7 || strandings[0].Assigned != 2 || strandings[0].Busy != 2 {
		t.Fatalf("the stranding must be set 7 holding 2 jobs: %#v", strandings)
	}
	if strandings[0].State != Parked || !strandings[0].ObservedAt.Equal(auditedAt) {
		t.Fatalf("a stranding carries its state and when it was seen: %#v", strandings[0])
	}
	reason := strandings[0].Reason()
	for _, want := range []string{"suuudokuuu", "scale set 7", "trf-sudoku-builder-studio", "2 assigned job(s)",
		"nothing can be listening to this set"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the finding must say %q: %q", want, reason)
		}
	}
}

// A parked set holding NOTHING is informational and never a finding. Under ADR
// 0034 shared-label federation is the ordinary case: a set parked here is very
// often bound on a sibling node, and this audit cannot read a sibling's
// configuration. Reporting every such set as a fault would make the check
// useless on the very topology it was written for.
func TestAnIdleParkedSetIsInformationalRatherThanAFinding(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 9, Name: "trf-sudoku-builder-mini", Statistics: &githubscaleset.ScaleSetStatistics{IdleRunners: 1}},
	}}

	result, err := Run(context.Background(), auditRequest(sudokuConfig(), client))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ScaleSets) != 1 || result.ScaleSets[0].State != Parked {
		t.Fatalf("the set is reported, and reported as parked: %#v", result.ScaleSets)
	}
	if result.ScaleSets[0].Stranding || len(result.Strandings()) != 0 {
		t.Fatalf("a sibling's set holding no work is not this node's fault: %#v", result.ScaleSets)
	}
}

// A listing without statistics is not evidence of an empty set. The one
// question that matters is answered by a read per PARKED set, and by no read at
// all for a set this node serves — that is the whole of the audit's rate-limit
// budget: one list per scope, one get per parked set.
func TestAListingWithoutStatisticsIsReadPerParkedSetOnly(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder"},
		{ID: 7, Name: "trf-sudoku-builder-studio"},
	}, statistics: map[int]githubscaleset.ScaleSetStatistics{7: {AssignedJobs: 2, BusyRunners: 2}}}

	result, err := Run(context.Background(), auditRequest(sudokuConfig(), client))
	if err != nil {
		t.Fatal(err)
	}
	if client.lists != 1 || len(client.reads) != 1 || client.reads[0] != 7 {
		t.Fatalf("one list per scope and one read per parked set: lists=%d reads=%v", client.lists, client.reads)
	}
	if len(result.Strandings()) != 1 {
		t.Fatalf("the read must reach the finding: %#v", result.ScaleSets)
	}
}

// A set provisioned but not yet persisted into the configuration is matched by
// name, because it IS served here. Calling it parked would be a false finding
// against the node that created it.
func TestASetBoundByNameWithoutAPersistedIDIsNotParked(t *testing.T) {
	cfg := sudokuConfig()
	cfg.GitHub.Scopes[0].ScaleSets[0].ID = 0
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 1, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{AssignedJobs: 3}},
	}}

	result, err := Run(context.Background(), auditRequest(cfg, client))
	if err != nil {
		t.Fatal(err)
	}
	if result.ScaleSets[0].State != Bound || result.ScaleSets[0].Stranding {
		t.Fatalf("a set this node serves is bound however its id was recorded: %#v", result.ScaleSets[0])
	}
}

// A scale set GitHub RECREATED under a name this node configures is parked, not
// bound. The node's session polls the id it was configured with; a new object
// carries a new id, so nothing here is listening to it — and matching on the
// name would report the set as served while its assigned work sat unreachable,
// which is the exact blindness this audit exists to remove.
func TestASetRecreatedUnderAConfiguredNameIsParked(t *testing.T) {
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 42, Name: "trf-sudoku-builder", Statistics: &githubscaleset.ScaleSetStatistics{
			AssignedJobs: 2, BusyRunners: 1}},
	}}

	result, err := Run(context.Background(), auditRequest(sudokuConfig(), client))
	if err != nil {
		t.Fatal(err)
	}
	if result.ScaleSets[0].State != Parked {
		t.Fatalf("the configured id is 1, so set 42 is not served here: %#v", result.ScaleSets[0])
	}
	strandings := result.Strandings()
	if len(strandings) != 1 || strandings[0].ID != 42 || strandings[0].Assigned != 2 {
		t.Fatalf("its assigned work must be a finding: %#v", strandings)
	}
}

// The runner group is the one the scope declares, so a fleet that does not use
// the default group audits the group it actually provisions into.
func TestTheScopesRunnerGroupIsAudited(t *testing.T) {
	cfg := sudokuConfig()
	cfg.GitHub.Scopes[0].RunnerGroup = "macos"
	client := &fakeClient{}

	if _, err := Run(context.Background(), auditRequest(cfg, client)); err != nil {
		t.Fatal(err)
	}
	if client.group != "macos" {
		t.Fatalf("runner group = %q, want the scope's own", client.group)
	}
}

// An audit GitHub would not answer fails. It must never return an empty result,
// because an empty result reads as "no set is parked" — the exact conflation
// that let issue #164 run for 4.5 hours under a PASS.
func TestAnUnreachableGitHubFailsRatherThanReportingNothingParked(t *testing.T) {
	for name, client := range map[string]*fakeClient{
		"list refused": {listErr: errors.New("GitHub is unreachable")},
		"read refused": {listed: []githubscaleset.ScaleSetSummary{{ID: 7, Name: "studio"}},
			readErr: errors.New("GitHub is unreachable")},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := Run(context.Background(), auditRequest(sudokuConfig(), client))
			if err == nil {
				t.Fatalf("an unanswered audit must not pass: %#v", result)
			}
			if len(result.ScaleSets) != 0 {
				t.Fatalf("a failed audit reports no sets: %#v", result.ScaleSets)
			}
		})
	}
}

// The caller's key is the caller's: the daemon holds one for its whole run and
// an audit that destroyed it would take the node's GitHub authority with it.
func TestAKeyPassedInIsNotDestroyed(t *testing.T) {
	key := githubscaleset.NewPrivateKeySecret("pem")
	request := auditRequest(sudokuConfig(), &fakeClient{})
	request.Key = key

	if _, err := Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	opened := false
	request.Open = func(admin githubscaleset.GitHubAppAdminConfig) (Client, error) {
		opened = admin.PrivateKey == key
		return &fakeClient{}, nil
	}
	if _, err := Run(context.Background(), request); err != nil || !opened {
		t.Fatalf("the same key must still be usable: opened=%v err=%v", opened, err)
	}
}

// A key this package loads is this package's to destroy, and a loader that
// cannot produce one is a refusal rather than an empty audit.
func TestALoadedKeyIsDestroyedAndAFailedLoadRefuses(t *testing.T) {
	loaded := githubscaleset.NewPrivateKeySecret("pem")
	request := auditRequest(sudokuConfig(), &fakeClient{})
	request.Key = nil
	request.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return loaded, nil
	}
	if _, err := Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	request.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return nil, errors.New("keychain refused")
	}
	if _, err := Run(context.Background(), request); err == nil {
		t.Fatal("a key that cannot be loaded is not an empty audit")
	}
	request.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return nil, nil
	}
	if _, err := Run(context.Background(), request); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a nil key is invalid, not a pass: %v", err)
	}
}

// A request that cannot audit says so rather than reporting an audited fleet.
func TestAnUnaudiableRequestIsRefused(t *testing.T) {
	valid := auditRequest(sudokuConfig(), &fakeClient{})
	for name, edit := range map[string]func(*Request){
		"no opener": func(r *Request) { r.Open = nil },
		"no clock":  func(r *Request) { r.Now = nil },
		"no scopes": func(r *Request) { r.Config = config.Default() },
		"no key at all": func(r *Request) {
			r.Key, r.LoadKey = nil, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			edit(&request)
			if _, err := Run(context.Background(), request); !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("err = %v, want invalid", err)
			}
		})
	}
	request := valid
	request.Open = func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return nil, errors.New("no credential") }
	if _, err := Run(context.Background(), request); err == nil {
		t.Fatal("a scope that cannot be opened is a refusal")
	}
	request.Open = func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return nil, nil }
	if _, err := Run(context.Background(), request); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a nil client is invalid: %v", err)
	}
}

// Scopes and sets are reported in a stable order, so two runs against one state
// read identically and a diff between them means the fleet changed.
func TestTheAuditIsDeterministicallyOrdered(t *testing.T) {
	cfg := sudokuConfig()
	cfg.GitHub.Scopes = append(cfg.GitHub.Scopes, config.GitHubScope{Name: "aaa-first", Kind: config.ScopeRepository,
		ConfigURL: "https://github.com/vitalyiegorov/knee-doctor", Installation: "personal",
		Targets: []string{"vitalyiegorov/knee-doctor"}})
	client := &fakeClient{listed: []githubscaleset.ScaleSetSummary{
		{ID: 7, Name: "seven", Statistics: &githubscaleset.ScaleSetStatistics{}},
		{ID: 2, Name: "two", Statistics: &githubscaleset.ScaleSetStatistics{}},
	}}

	result, err := Run(context.Background(), auditRequest(cfg, client))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ScaleSets) != 4 || result.ScaleSets[0].Scope != "aaa-first" || result.ScaleSets[0].ID != 2 {
		t.Fatalf("scopes sort by name and sets by id: %#v", result.ScaleSets)
	}
}

// A parked set whose work is accompanied by a registered runner is a
// sibling's normal traffic under ADR 0034's shared labels, not a stranding:
// GitHub registers a runner only after a listener acquired the job. Without
// this rule the first live cadence run failed node-b's doctor forever on the
// mac mini's own builder.
func TestAParkedSetWithARegisteredRunnerIsASiblingsNotAStranding(t *testing.T) {
	set := ScaleSet{Scope: "sudoku-repo", ID: 1, Name: "trf-sudoku-builder", State: Parked, Assigned: 1, Busy: 1, Registered: 1}
	if set.Stranded() {
		t.Fatal("a registered runner is proof of a listener")
	}
	set.Registered = 0
	if !set.Stranded() {
		t.Fatal("assigned work with no registered runner is a stranding")
	}
}
