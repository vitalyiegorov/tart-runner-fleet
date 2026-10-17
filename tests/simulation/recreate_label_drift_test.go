package simulation_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
)

// The broker world starts with already-provisioned bindings. This administration
// world extends that boundary: GitHub accepts label updates without applying
// them, and a repair can stop before inspection, delete, create, or config write.
// The oracle runs the real constructor, adapter and Recreate, not a lifecycle
// mirror. Issue #135 / PR #364 is a fleet defect, not an oracle defect.
type recreateWorld struct {
	current *scaleset.RunnerScaleSet
	failAt  string
	effects []string
}

func (*recreateWorld) GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error) {
	panic("the default runner group needs no lookup")
}

func (w *recreateWorld) GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error) {
	if w.failAt == "inspect" {
		return nil, operations.ErrUncertain
	}
	return w.current, nil
}

func (w *recreateWorld) UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	w.effects = append(w.effects, "update")
	return w.current, nil // The triggering GitHub behavior: labels stay unchanged.
}

func (w *recreateWorld) DeleteRunnerScaleSet(_ context.Context, id int) error {
	if w.current == nil || id != w.current.ID || id != 17 {
		panic("only the configured old object may be deleted")
	}
	w.effects = append(w.effects, "delete")
	if w.failAt == "delete" {
		return operations.ErrUncertain
	}
	w.current = nil
	return nil
}

func (w *recreateWorld) CreateRunnerScaleSet(_ context.Context, desired *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	if w.current != nil {
		panic("creation must follow deletion")
	}
	w.effects = append(w.effects, "create")
	if w.failAt == "create" {
		return nil, operations.ErrUncertain
	}
	created := *desired
	created.ID = 19
	w.current = &created
	return w.current, nil
}

func TestRecreateLabelDriftRecoversAcrossEveryInterruption(t *testing.T) {
	for _, interruption := range []string{"none", "inspect", "delete", "create", "write"} {
		t.Run(interruption, func(t *testing.T) {
			cfg := config.Default()
			cfg.Targets = []config.Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4}}
			cfg.Linux.Profiles[0].Aliases = []string{"linux-portable-small"}
			cfg.GitHub = config.GitHub{SessionOwner: "host", CanonicalJobInventory: true,
				App:           config.GitHubApp{ClientID: "client", KeychainService: "service", KeychainAccount: "account"},
				Installations: []config.GitHubInstallation{{Name: "personal", InstallationID: 7}},
				Scopes: []config.GitHubScope{{Name: "repo", Kind: config.ScopeRepository,
					ConfigURL: "https://github.com/owner/repo", Installation: "personal", Targets: []string{"owner/repo"},
					ScaleSets: []config.ScaleSet{{ID: 17, Profile: "small", Name: "repo-small", MaxCapacity: 4,
						Labels: []string{"self-hosted", "linux-small"}}}}}}
			w := &recreateWorld{current: &scaleset.RunnerScaleSet{ID: 17, Name: "repo-small", RunnerGroupID: 1,
				Labels:        []scaleset.Label{{Name: "self-hosted"}, {Name: "linux-small"}, {Name: "repo-small"}},
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}}
			ctx := context.Background()
			request := provision.RecreateRequest{Config: cfg, Name: "repo-small",
				LoadKey: func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
					return githubscaleset.NewPrivateKeySecret("simulation-key"), nil
				},
				Open: func(c githubscaleset.GitHubAppAdminConfig) (provision.Recreater, error) {
					client, err := githubscaleset.NewRecreater(c)
					client.Client = w
					return client, err
				}}
			// Ordinary provisioning still refuses the very same observed drift.
			spec := githubscaleset.ScaleSetSpec{Name: request.Name,
				Labels: cfg.ProfileLabelSets()["small"].Advertise([]string{"self-hosted", "linux-small"})}
			if _, err := (githubscaleset.Provisioner{Client: w}).Inspect(ctx, spec); !errors.Is(err, operations.ErrConflict) {
				t.Fatalf("ordinary provision must refuse drift: %v", err)
			}
			// An explicitly requested in-place update cannot repair these labels.
			if _, err := (githubscaleset.Provisioner{Client: w, ReconcileDrift: true}).Ensure(ctx, spec); !errors.Is(err, operations.ErrUncertain) || !strings.Contains(err.Error(), "fleet scale-sets recreate") {
				t.Fatalf("ignored label update must name the guarded remedy: %v", err)
			}
			w.effects = nil
			w.failAt = interruption
			result, err := provision.Recreate(ctx, request)
			wantEffects := []string{"delete", "create"}
			switch interruption {
			case "inspect", "delete", "create":
				if !errors.Is(err, operations.ErrUncertain) {
					t.Fatalf("interrupted repair must surface uncertainty: %v", err)
				}
				if interruption == "inspect" && len(w.effects) != 0 || interruption == "delete" && !slices.Equal(w.effects, []string{"delete"}) {
					t.Fatalf("a refusal must stop later effects: %v", w.effects)
				}
				w.failAt = ""
				w.effects = nil
				result, err = provision.Recreate(ctx, request)
				if interruption == "create" {
					wantEffects = []string{"create"}
				}
			case "write":
				if err != nil {
					t.Fatal(err)
				}
				// Lose the returned configuration: the retry still names old id 17.
				w.effects = nil
				result, err = provision.Recreate(ctx, request)
				wantEffects = nil
			}
			if err != nil || result.OldID != 17 || result.NewID != 19 || result.Config.GitHub.Scopes[0].ScaleSets[0].ID != 19 {
				t.Fatalf("repair must converge to a new persisted binding: %+v, %v", result, err)
			}
			if !slices.Equal(w.effects, wantEffects) || cfg.GitHub.Scopes[0].ScaleSets[0].ID != 17 {
				t.Fatalf("effects=%v want=%v; input configuration must stay immutable", w.effects, wantEffects)
			}
			var labels []string
			for _, label := range w.current.Labels {
				labels = append(labels, label.Name)
			}
			for _, required := range []string{"self-hosted", "linux-small", "linux-portable-small", "trf-linux-arm64-1x2", "repo-small"} {
				if !slices.Contains(labels, required) {
					t.Fatalf("replacement is missing %q: %v", required, labels)
				}
			}
		})
	}
}
