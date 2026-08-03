package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeClient struct {
	plans   map[string]githubscaleset.ScaleSetPlan
	created []string
	err     error
	inspect func(githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error)
	ensure  func(githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error)
}

func (f *fakeClient) Inspect(_ context.Context, spec githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
	if f.inspect != nil {
		return f.inspect(spec)
	}
	if f.err != nil {
		return githubscaleset.ScaleSetPlan{}, f.err
	}
	plan, ok := f.plans[spec.Name]
	if !ok {
		plan.Action = githubscaleset.ScaleSetCreate
	}
	return plan, nil
}
func (f *fakeClient) Ensure(_ context.Context, spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
	f.created = append(f.created, spec.Name)
	if f.ensure != nil {
		return f.ensure(spec)
	}
	if f.err != nil {
		return scaleset.RunnerScaleSet{}, f.err
	}
	return scaleset.RunnerScaleSet{ID: len(f.created) + 100, Name: spec.Name}, nil
}

func provisionConfig() config.Config {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4}}
	sets := make([]config.ScaleSet, 0, 5)
	for _, value := range []struct {
		profile, route string
		capacity       int
	}{{"small", "linux-small", 4}, {"medium", "linux-medium", 4}, {"large", "linux-large", 2}, {"builder", "macos-builder", 1}, {"maestro", "macos-maestro", 2}} {
		sets = append(sets, config.ScaleSet{Profile: value.profile, Name: "repo-" + value.profile, MaxCapacity: value.capacity, Labels: []string{"self-hosted", value.route}})
	}
	cfg.GitHub = config.GitHub{SessionOwner: "host", CanonicalJobInventory: true,
		App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes:        []config.GitHubScope{{Name: "repo", Kind: config.ScopeRepository, ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"}, ScaleSets: sets}}}
	return cfg
}

func TestRunPlansBeforeApplyingAndPersistsReturnedIDs(t *testing.T) {
	client := &fakeClient{plans: map[string]githubscaleset.ScaleSetPlan{"repo-small": {Action: githubscaleset.ScaleSetReuse, ID: 55}}}
	request := Request{Config: provisionConfig(), Apply: true, LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}, Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil }}
	result, err := Run(context.Background(), request)
	if err != nil || len(result.Changes) != 5 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if result.Config.GitHub.Scopes[0].ScaleSets[0].ID != 55 || result.Config.GitHub.Scopes[0].ScaleSets[1].ID == 0 || len(client.created) != 4 {
		t.Fatalf("result IDs=%#v created=%v", result.Config.GitHub.Scopes[0].ScaleSets, client.created)
	}
}

// TestProvisionedScaleSetAdvertisesCanonicalAndAliasLabels proves the migration
// path of ADR 0032: a configuration that still names its profiles by role gets
// the derived resource label attached at provisioning time, so both names route
// to the one scale set and no consumer workflow has to change first.
func TestProvisionedScaleSetAdvertisesCanonicalAndAliasLabels(t *testing.T) {
	specs := map[string][]string{}
	client := &fakeClient{inspect: func(spec githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
		specs[spec.Name] = spec.Labels
		return githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetCreate}, nil
	}}
	cfg := provisionConfig()
	cfg.Linux.Profiles[0].Aliases = []string{"linux-burst"}
	request := Request{Config: cfg, LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}, Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil }}
	if _, err := Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"repo-small":   {"self-hosted", "linux-small", "trf-linux-arm64-1x2", "linux-burst"},
		"repo-builder": {"self-hosted", "macos-builder", "trf-macos-arm64-8x12"},
	}
	for name, labels := range want {
		got := specs[name]
		if len(got) != len(labels) {
			t.Fatalf("%s labels = %#v, want %#v", name, got, labels)
		}
		for index, label := range labels {
			if got[index] != label {
				t.Fatalf("%s labels = %#v, want %#v", name, got, labels)
			}
		}
	}
}

func TestRunPlanOnlyAndDriftFailBeforeCreate(t *testing.T) {
	client := &fakeClient{plans: map[string]githubscaleset.ScaleSetPlan{}}
	base := Request{Config: provisionConfig(), LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}, Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil }}
	if result, err := Run(context.Background(), base); err != nil || len(result.Changes) != 5 || len(client.created) != 0 {
		t.Fatalf("plan = %#v, %v, creates=%v", result, err, client.created)
	}
	want := errors.New("drift")
	client.err = want
	base.Apply = true
	if _, err := Run(context.Background(), base); !errors.Is(err, want) || len(client.created) != 0 {
		t.Fatalf("drift = %v creates=%v", err, client.created)
	}
	base.LoadKey = nil
	if _, err := Run(context.Background(), base); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("invalid request = %v", err)
	}
}

func TestRunPassesConfiguredPrivateKeyFileToCredentialLoader(t *testing.T) {
	cfg := provisionConfig()
	cfg.GitHub.App.PrivateKeyFile = "/Users/runner/.config/tart-runner-fleet/app.pem"
	var gotService, gotAccount, gotPath string
	request := Request{Config: cfg, LoadKey: func(_ context.Context, service, account, path string) (*githubscaleset.PrivateKeySecret, error) {
		gotService, gotAccount, gotPath = service, account, path
		return githubscaleset.NewPrivateKeySecret("pem"), nil
	}, Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return &fakeClient{}, nil }}
	if _, err := Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if gotService != "service" || gotAccount != "account" || gotPath != cfg.GitHub.App.PrivateKeyFile {
		t.Fatalf("credential reference = %q/%q/%q", gotService, gotAccount, gotPath)
	}
}

func validRequest(client Client) Request {
	return Request{Config: provisionConfig(), LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
		return githubscaleset.NewPrivateKeySecret("PRIVATE-KEY-SENTINEL"), nil
	}, Open: func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return client, nil }}
}

func TestRunRejectsInvalidConfigurationAndKeyFailures(t *testing.T) {
	client := &fakeClient{}
	tests := []struct {
		name string
		edit func(*Request)
		want error
	}{
		{name: "missing open", edit: func(r *Request) { r.Open = nil }, want: operations.ErrInvalid},
		{name: "no scopes", edit: func(r *Request) { r.Config.GitHub.Scopes = nil }, want: operations.ErrInvalid},
		{name: "invalid authority", edit: func(r *Request) { r.Config.GitHub.App.ClientID = "" }, want: operations.ErrInvalid},
		{name: "load error", edit: func(r *Request) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, errors.New("keychain unavailable")
			}
		}, want: errors.New("keychain unavailable")},
		{name: "nil key", edit: func(r *Request) {
			r.LoadKey = func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
				return nil, nil
			}
		}, want: operations.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := validRequest(client)
			tt.edit(&request)
			_, err := Run(context.Background(), request)
			if tt.name == "load error" {
				if err == nil || !strings.Contains(err.Error(), tt.want.Error()) {
					t.Fatalf("Run() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRunSortsScopesAndSetsWithoutMutatingInput(t *testing.T) {
	cfg := provisionConfig()
	cfg.Targets = append(cfg.Targets, config.Target{Type: "repo", Slug: "owner/another", MaxActive: 2})
	second := cfg.GitHub.Scopes[0]
	second.Name = "aaa"
	second.ConfigURL = "https://github.com/owner/another"
	second.Targets = []string{"owner/another"}
	second.ScaleSets = append([]config.ScaleSet(nil), second.ScaleSets...)
	for i := range second.ScaleSets {
		second.ScaleSets[i].Name = "aaa-" + second.ScaleSets[i].Profile
		second.ScaleSets[i].Labels = append([]string(nil), second.ScaleSets[i].Labels...)
		switch second.ScaleSets[i].Profile {
		case "small", "medium", "large", "maestro":
			second.ScaleSets[i].MaxCapacity = 2
		case "builder":
			second.ScaleSets[i].MaxCapacity = 1
		}
	}
	cfg.GitHub.Scopes[0].Name = "zzz"
	cfg.GitHub.Scopes = append(cfg.GitHub.Scopes, second)

	var opened []string
	var inspected []string
	client := &fakeClient{inspect: func(spec githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
		inspected = append(inspected, spec.Name)
		return githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetCreate}, nil
	}}
	request := validRequest(client)
	request.Config = cfg
	request.Open = func(value githubscaleset.GitHubAppAdminConfig) (Client, error) {
		opened = append(opened, value.GitHubConfigURL)
		if value.InstallationID != 7 || value.PrivateKey == nil {
			t.Fatalf("open config = %#v", value)
		}
		return client, nil
	}
	result, err := Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opened, ","); got != "https://github.com/owner/another,https://github.com/owner/repo" {
		t.Fatalf("open order = %s", got)
	}
	if len(inspected) != 10 || inspected[0] != "aaa-builder" || inspected[4] != "aaa-small" || result.Changes[0].Scope != "aaa" {
		t.Fatalf("inspect order=%v changes=%#v", inspected, result.Changes)
	}
	if request.Config.GitHub.Scopes[1].ScaleSets[0].ID != 0 {
		t.Fatal("input configuration mutated")
	}
}

func TestRunFailsClosedDuringPlanning(t *testing.T) {
	tests := []struct {
		name         string
		plan         githubscaleset.ScaleSetPlan
		configuredID int
		openErr      error
		want         error
	}{
		{name: "open", openErr: errors.New("client unavailable"), want: errors.New("client unavailable")},
		// "update" became a recognized action when drift repair landed, so an
		// unrecognized action needs a token the provisioner will never emit.
		{name: "inspect action", plan: githubscaleset.ScaleSetPlan{Action: "unrecognized-action", ID: 7}, want: operations.ErrUncertain},
		{name: "configured id conflict", plan: githubscaleset.ScaleSetPlan{Action: githubscaleset.ScaleSetReuse, ID: 8}, configuredID: 7, want: operations.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{inspect: func(githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) { return tt.plan, nil }}
			request := validRequest(client)
			if tt.configuredID > 0 {
				request.Config.GitHub.Scopes[0].ScaleSets[0].ID = tt.configuredID
			}
			if tt.openErr != nil {
				request.Open = func(githubscaleset.GitHubAppAdminConfig) (Client, error) { return nil, tt.openErr }
			}
			_, err := Run(context.Background(), request)
			if tt.openErr != nil {
				if err == nil || !strings.Contains(err.Error(), tt.want.Error()) {
					t.Fatalf("Run() error = %v", err)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
			if len(client.created) != 0 {
				t.Fatalf("planning failure created %v", client.created)
			}
		})
	}
}

func TestRunApplyReportsPartialFailureAndRejectsUncertainID(t *testing.T) {
	for _, tt := range []struct {
		name   string
		ensure func(githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error)
		want   error
	}{
		{name: "upstream", ensure: func(spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
			if spec.Name == "repo-large" {
				return scaleset.RunnerScaleSet{}, errors.New("create denied")
			}
			return scaleset.RunnerScaleSet{ID: 99, Name: spec.Name}, nil
		}, want: errors.New("create denied")},
		{name: "invalid returned id", ensure: func(spec githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
			return scaleset.RunnerScaleSet{Name: spec.Name}, nil
		}, want: operations.ErrUncertain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeClient{ensure: tt.ensure}
			request := validRequest(client)
			request.Apply = true
			result, err := Run(context.Background(), request)
			if tt.name == "upstream" {
				if err == nil || !strings.Contains(err.Error(), tt.want.Error()) {
					t.Fatalf("Run() = %#v, %v", result, err)
				}
				if len(client.created) != 2 || result.Changes[0].ID != 99 {
					t.Fatalf("partial result=%#v creates=%v", result.Changes, client.created)
				}
			} else if !errors.Is(err, tt.want) {
				t.Fatalf("Run() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCompareProvidesStableThreeWayOrdering(t *testing.T) {
	if compare("a", "b") >= 0 || compare("b", "a") <= 0 || compare("a", "a") != 0 {
		t.Fatal("compare does not implement total ordering")
	}
}
