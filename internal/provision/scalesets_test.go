package provision

import (
	"context"
	"errors"
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
}

func (f *fakeClient) Inspect(_ context.Context, spec githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error) {
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
	if f.err != nil {
		return scaleset.RunnerScaleSet{}, f.err
	}
	return scaleset.RunnerScaleSet{ID: len(f.created) + 100, Name: spec.Name}, nil
}

func provisionConfig() config.Config {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4}}
	sets := make([]config.ScaleSet, 0, 5)
	for _, value := range []struct{ profile, route string }{{"small", "linux-small"}, {"medium", "linux-medium"}, {"large", "linux-large"}, {"builder", "macos-builder"}, {"maestro", "macos-maestro"}} {
		sets = append(sets, config.ScaleSet{Profile: value.profile, Name: "repo-" + value.profile, MaxCapacity: 1, Labels: []string{"self-hosted", value.route}})
	}
	cfg.GitHub = config.GitHub{SessionOwner: "host", App: config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
		Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
		Scopes:        []config.GitHubScope{{Name: "repo", Kind: config.ScopeRepository, ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"}, ScaleSets: sets}}}
	return cfg
}

func TestRunPlansBeforeApplyingAndPersistsReturnedIDs(t *testing.T) {
	client := &fakeClient{plans: map[string]githubscaleset.ScaleSetPlan{"repo-small": {Action: githubscaleset.ScaleSetReuse, ID: 55}}}
	request := Request{Config: provisionConfig(), Apply: true, LoadKey: func(context.Context, string, string) (*githubscaleset.PrivateKeySecret, error) {
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

func TestRunPlanOnlyAndDriftFailBeforeCreate(t *testing.T) {
	client := &fakeClient{plans: map[string]githubscaleset.ScaleSetPlan{}}
	base := Request{Config: provisionConfig(), LoadKey: func(context.Context, string, string) (*githubscaleset.PrivateKeySecret, error) {
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
