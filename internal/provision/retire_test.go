package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
)

const (
	retireCurrentURL = "https://github.com/owner"
	retireFromURL    = "https://github.com/owner/repo"
)

// fakeRetireAdmin is one GitHub tenant's scale-set admin API: what it lists,
// what it counts, and what it was asked to delete.
type fakeRetireAdmin struct {
	sets      []githubscaleset.ScaleSetSummary
	listErr   error
	stats     map[int]githubscaleset.ScaleSetStatistics
	statsErr  error
	deleted   []int
	deleteErr error
	groups    []string
}

func (f *fakeRetireAdmin) List(_ context.Context, group string) ([]githubscaleset.ScaleSetSummary, error) {
	f.groups = append(f.groups, group)
	return f.sets, f.listErr
}

func (f *fakeRetireAdmin) Statistics(_ context.Context, id int) (githubscaleset.ScaleSetStatistics, error) {
	if f.statsErr != nil {
		return githubscaleset.ScaleSetStatistics{}, f.statsErr
	}
	stats, ok := f.stats[id]
	if !ok {
		return githubscaleset.ScaleSetStatistics{}, operations.ErrUncertain
	}
	return stats, nil
}

func (f *fakeRetireAdmin) Delete(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

// retireConfig is the configuration AFTER the kind change has been provisioned
// and written: the scope is an organization scope and its set carries the new
// org-level id, 31. The old repo-level set, id 23, lives at retireFromURL.
func retireConfig() config.Config {
	cfg := provisionConfig()
	scope := &cfg.GitHub.Scopes[0]
	scope.Kind = config.ScopeOrganization
	scope.ConfigURL = retireCurrentURL
	for index, set := range scope.ScaleSets {
		if set.Name == "repo-large" {
			scope.ScaleSets[index].ID = 31
		}
	}
	return cfg
}

type retireWorld struct {
	current *fakeRetireAdmin
	from    *fakeRetireAdmin
	opened  []string
}

func newRetireWorld() *retireWorld {
	return &retireWorld{
		current: &fakeRetireAdmin{sets: []githubscaleset.ScaleSetSummary{{ID: 31, Name: "repo-large"}},
			stats: map[int]githubscaleset.ScaleSetStatistics{31: {AssignedJobs: 2, BusyRunners: 2}}},
		from: &fakeRetireAdmin{sets: []githubscaleset.ScaleSetSummary{{ID: 22, Name: "repo-small"}, {ID: 23, Name: "repo-large"}},
			stats: map[int]githubscaleset.ScaleSetStatistics{23: {}}},
	}
}

func (w *retireWorld) request() RetireRequest {
	return RetireRequest{Config: retireConfig(), Scope: "repo", Name: "repo-large", From: retireFromURL,
		LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
			return githubscaleset.NewPrivateKeySecret("pem"), nil
		},
		OpenAudit: func(cfg githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) {
			w.opened = append(w.opened, "audit "+cfg.GitHubConfigURL)
			if cfg.GitHubConfigURL != retireCurrentURL {
				return nil, errors.New("audit port opened at the wrong scope")
			}
			return w.current, nil
		},
		Open: func(cfg githubscaleset.GitHubAppAdminConfig) (Retirer, error) {
			w.opened = append(w.opened, "retire "+cfg.GitHubConfigURL)
			if cfg.GitHubConfigURL != retireFromURL || cfg.InstallationID != 7 || cfg.ClientID != "client" {
				return nil, errors.New("retire port opened with the wrong identity")
			}
			return w.from, nil
		}}
}

// TestRetireDeletesTheIdleOldSetAtTheFormerScope is issue #475's happy path: a
// scope moved from repository to organization, the org-level replacement is
// provisioned and bound, and the repo-level set it replaced holds nothing. Only
// that one object, at the OLD configUrl, is deleted.
func TestRetireDeletesTheIdleOldSetAtTheFormerScope(t *testing.T) {
	world := newRetireWorld()
	request := world.request()
	before := request.Config.Clone()

	result, err := Retire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(world.from.deleted) != 1 || world.from.deleted[0] != 23 {
		t.Fatalf("exactly the old set must be deleted: %v", world.from.deleted)
	}
	if len(world.current.deleted) != 0 {
		t.Fatalf("nothing at the current scope may be deleted: %v", world.current.deleted)
	}
	if result.Scope != "repo" || result.Name != "repo-large" || result.From != retireFromURL || result.ID != 23 {
		t.Fatalf("result = %#v", result)
	}
	if request.Config.GitHub.Scopes[0].ScaleSets[2].ID != before.GitHub.Scopes[0].ScaleSets[2].ID {
		t.Fatal("retire must not change the configuration")
	}
}

// TestRetireReadsTheScopesRunnerGroup: an organization scope can name a runner
// group, and its replacement is looked up there. A repository-level --from has
// only the default group, so the old set is always looked up there (PR #476
// review): looking it up in the org's named group would never find it.
func TestRetireReadsTheScopesRunnerGroup(t *testing.T) {
	world := newRetireWorld()
	request := world.request()
	request.Config.GitHub.Scopes[0].RunnerGroup = "fleet"
	if _, err := Retire(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(world.from.groups) != 1 || world.from.groups[0] != "" {
		t.Fatalf("old repo-level set looked up in %v, want the default group", world.from.groups)
	}
	if len(world.current.groups) != 1 || world.current.groups[0] != "fleet" {
		t.Fatalf("replacement looked up in %v, want [fleet]", world.current.groups)
	}
}

// TestRetireRefusesASetGitHubStillHoldsWorkFor is the guard the whole command
// exists to state: GitHub never reassigns a job, so deleting a set that still
// holds assigned or running work, or still has runners registered, loses it.
func TestRetireRefusesASetGitHubStillHoldsWorkFor(t *testing.T) {
	for name, stats := range map[string]githubscaleset.ScaleSetStatistics{
		"assigned":   {AssignedJobs: 1},
		"running":    {RunningJobs: 1},
		"busy":       {BusyRunners: 1},
		"registered": {RegisteredRunners: 1, IdleRunners: 1},
	} {
		t.Run(name, func(t *testing.T) {
			world := newRetireWorld()
			world.from.stats[23] = stats
			_, err := Retire(context.Background(), world.request())
			if !errors.Is(err, operations.ErrConflict) {
				t.Fatalf("err = %v, want conflict", err)
			}
			if len(world.from.deleted) != 0 {
				t.Fatalf("a busy set must not be deleted: %v", world.from.deleted)
			}
		})
	}
}

// TestRetireRefusesWithoutLiveStatistics: an unread counter is not a zero
// counter (contributor rule 4).
func TestRetireRefusesWithoutLiveStatistics(t *testing.T) {
	world := newRetireWorld()
	world.from.statsErr = errors.New("gateway timeout")
	_, err := Retire(context.Background(), world.request())
	if !errors.Is(err, operations.ErrUncertain) || len(world.from.deleted) != 0 {
		t.Fatalf("err = %v deleted = %v, want uncertain and nothing deleted", err, world.from.deleted)
	}
}

func TestRetireReportsAnAbsentOldSetAsNotFound(t *testing.T) {
	world := newRetireWorld()
	world.from.sets = []githubscaleset.ScaleSetSummary{{ID: 22, Name: "repo-small"}}
	_, err := Retire(context.Background(), world.request())
	if !errors.Is(err, operations.ErrNotFound) || len(world.from.deleted) != 0 {
		t.Fatalf("err = %v deleted = %v, want not found and nothing deleted", err, world.from.deleted)
	}
}

// TestRetireNeverTargetsTheBoundScope: --from equal to the scope's current
// configUrl, however it is spelled, names the set the node binds.
func TestRetireNeverTargetsTheBoundScope(t *testing.T) {
	for _, from := range []string{retireCurrentURL, "https://GitHub.com/Owner/", "https://github.com//owner"} {
		world := newRetireWorld()
		request := world.request()
		request.From = from
		_, err := Retire(context.Background(), request)
		if !errors.Is(err, operations.ErrConflict) || len(world.opened) != 0 {
			t.Fatalf("from %q: err = %v opened = %v, want conflict before any client", from, err, world.opened)
		}
	}
}

// TestRetireNeverTargetsAnotherConfiguredScope: --from equal to ANY scope this
// node is configured for, not just the named one, names sets the node binds
// (PR #476 review). It is refused before any client is opened.
func TestRetireNeverTargetsAnotherConfiguredScope(t *testing.T) {
	world := newRetireWorld()
	request := world.request()
	cfg := request.Config
	cfg.Targets = append(cfg.Targets, config.Target{Type: "repo", Slug: "owner/other", MaxActive: 4})
	other := cfg.GitHub.Scopes[0]
	other.Name, other.Kind, other.ConfigURL, other.Targets = "other", config.ScopeRepository,
		"https://github.com/owner/other", []string{"owner/other"}
	large := other.ScaleSets[2]
	large.Name, large.ID = "other-large", 0
	other.ScaleSets = []config.ScaleSet{large}
	cfg.GitHub.Scopes = append(cfg.GitHub.Scopes, other)
	request.Config = cfg
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("fixture must be a valid configuration: %v", err)
	}
	for _, from := range []string{"https://github.com/owner/other", "https://github.com:443/Owner/Other/"} {
		request.From = from
		_, err := Retire(context.Background(), request)
		if !errors.Is(err, operations.ErrConflict) || len(world.opened) != 0 {
			t.Fatalf("from %q: err = %v opened = %v, want conflict before any client", from, err, world.opened)
		}
	}
}

// TestRetireNeverDeletesASetTheConfigurationBinds: whatever --from names, a set
// whose id any configured scope carries is bound by this node and is refused.
func TestRetireNeverDeletesASetTheConfigurationBinds(t *testing.T) {
	world := newRetireWorld()
	world.from.sets = []githubscaleset.ScaleSetSummary{{ID: 31, Name: "repo-large"}}
	world.from.stats = map[int]githubscaleset.ScaleSetStatistics{31: {}}
	_, err := Retire(context.Background(), world.request())
	if !errors.Is(err, operations.ErrConflict) || len(world.from.deleted) != 0 {
		t.Fatalf("err = %v deleted = %v, want conflict and nothing deleted", err, world.from.deleted)
	}
}

func TestRetireRejectsAnInvalidFromURL(t *testing.T) {
	for _, from := range []string{"", "http://github.com/owner/repo", "https://github.com", "https://github.com/a/b/c",
		"https://user@github.com/owner/repo", "https://github.com/owner/repo?x=1", "https://github.com/owner/repo#f", "::"} {
		world := newRetireWorld()
		request := world.request()
		request.From = from
		_, err := Retire(context.Background(), request)
		if !errors.Is(err, operations.ErrInvalid) || len(world.opened) != 0 {
			t.Fatalf("from %q: err = %v opened = %v, want invalid", from, err, world.opened)
		}
	}
}

// TestRetireRequiresTheReplacementToBeProvisioned: the old set may go only once
// the new one exists at the current scope with the id the configuration binds.
func TestRetireRequiresTheReplacementToBeProvisioned(t *testing.T) {
	cases := map[string]func(*retireWorld, *RetireRequest){
		"unknown scope": func(_ *retireWorld, r *RetireRequest) { r.Scope = "other" },
		"name not in scope": func(_ *retireWorld, r *RetireRequest) {
			r.Name = "repo-missing"
		},
		"no configured id": func(_ *retireWorld, r *RetireRequest) {
			r.Config.GitHub.Scopes[0].ScaleSets[2].ID = 0
		},
		"absent at current scope": func(w *retireWorld, _ *RetireRequest) {
			w.current.sets = nil
		},
		"different id at current scope": func(w *retireWorld, _ *RetireRequest) {
			w.current.sets = []githubscaleset.ScaleSetSummary{{ID: 32, Name: "repo-large"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			world := newRetireWorld()
			request := world.request()
			mutate(world, &request)
			_, err := Retire(context.Background(), request)
			notFound := name == "unknown scope"
			if errors.Is(err, operations.ErrNotFound) != notFound || !notFound && !errors.Is(err, operations.ErrConflict) {
				t.Fatalf("err = %v, want not found=%v, else conflict", err, notFound)
			}
			if len(world.from.deleted) != 0 {
				t.Fatalf("nothing may be deleted: %v", world.from.deleted)
			}
		})
	}
}

func TestRetireFailsClosedOnEveryUnreadableStep(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(*retireWorld, *RetireRequest){
		"nil loader": func(_ *retireWorld, r *RetireRequest) { r.LoadKey = nil },
		"nil open":   func(_ *retireWorld, r *RetireRequest) { r.Open = nil },
		"nil audit":  func(_ *retireWorld, r *RetireRequest) { r.OpenAudit = nil },
		"no name":    func(_ *retireWorld, r *RetireRequest) { r.Name = "" },
		"no scope":   func(_ *retireWorld, r *RetireRequest) { r.Scope = "" },
		"no scopes":  func(_ *retireWorld, r *RetireRequest) { r.Config.GitHub.Scopes = nil },
		"key error": func(_ *retireWorld, r *RetireRequest) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, boom
			}
		},
		"nil key": func(_ *retireWorld, r *RetireRequest) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, nil
			}
		},
		"audit open error": func(_ *retireWorld, r *RetireRequest) {
			r.OpenAudit = func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error) { return nil, boom }
		},
		"current list error": func(w *retireWorld, _ *RetireRequest) { w.current.listErr = boom },
		"retire open error": func(_ *retireWorld, r *RetireRequest) {
			r.Open = func(githubscaleset.GitHubAppAdminConfig) (Retirer, error) { return nil, boom }
		},
		"from list error": func(w *retireWorld, _ *RetireRequest) { w.from.listErr = boom },
		"old id unusable": func(w *retireWorld, _ *RetireRequest) {
			w.from.sets = []githubscaleset.ScaleSetSummary{{ID: 0, Name: "repo-large"}}
		},
		"delete error": func(w *retireWorld, _ *RetireRequest) { w.from.deleteErr = boom },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			world := newRetireWorld()
			request := world.request()
			mutate(world, &request)
			if _, err := Retire(context.Background(), request); err == nil {
				t.Fatal("Retire() succeeded, want a refusal")
			}
			if name != "delete error" && len(world.from.deleted) != 0 {
				t.Fatalf("nothing may be deleted: %v", world.from.deleted)
			}
		})
	}
}
