// Package provision performs the explicit bootstrap-time creation of GitHub
// Actions scale sets. It is deliberately separate from fleetd reconciliation.
package provision

import (
	"context"
	"fmt"
	"slices"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type Client interface {
	Inspect(context.Context, githubscaleset.ScaleSetSpec) (githubscaleset.ScaleSetPlan, error)
	Ensure(context.Context, githubscaleset.ScaleSetSpec) (scaleset.RunnerScaleSet, error)
}

type Request struct {
	Config config.Config
	Apply  bool
	// ReconcileDrift permits repairing an existing scale set whose GitHub object no
	// longer matches configuration. Default false keeps drift failing closed, so a
	// run intended to create missing scale sets can never silently mutate existing
	// ones.
	ReconcileDrift bool
	LoadKey        func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error)
	Open           func(githubscaleset.GitHubAppAdminConfig) (Client, error)
	Version        string
}

type Change struct {
	Scope   string                        `json:"scope"`
	Profile string                        `json:"profile"`
	Name    string                        `json:"name"`
	ID      int                           `json:"id"`
	Action  githubscaleset.ScaleSetAction `json:"action"`
}

type Result struct {
	Config  config.Config
	Changes []Change `json:"changes"`
}

type planned struct {
	scopeIndex int
	setIndex   int
	client     Client
	spec       githubscaleset.ScaleSetSpec
	plan       githubscaleset.ScaleSetPlan
}

func Run(ctx context.Context, request Request) (Result, error) {
	if request.LoadKey == nil || request.Open == nil {
		return Result{}, operations.ErrInvalid
	}
	result := Result{Config: request.Config.Clone()}
	if len(result.Config.GitHub.Scopes) == 0 || result.Config.ValidateAuthority() != nil {
		return Result{}, operations.ErrInvalid
	}
	key, err := request.LoadKey(ctx, result.Config.GitHub.App.KeychainService, result.Config.GitHub.App.KeychainAccount,
		result.Config.GitHub.App.PrivateKeyFile)
	if err != nil {
		return Result{}, fmt.Errorf("load GitHub App key: %w", err)
	}
	if key == nil {
		return Result{}, operations.ErrInvalid
	}
	defer key.Destroy()
	installations := make(map[string]int64, len(result.Config.GitHub.Installations))
	for _, installation := range result.Config.GitHub.Installations {
		installations[installation.Name] = installation.InstallationID
	}
	// Every scale set advertises its profile's canonical resource label and each
	// alias beside whatever the configuration lists, so adopting the canonical
	// vocabulary is a provisioning run rather than a rewrite of every consumer
	// workflow (ADR 0032).
	labelSets := result.Config.ProfileLabelSets()
	scopeIndexes := make([]int, len(result.Config.GitHub.Scopes))
	for index := range scopeIndexes {
		scopeIndexes[index] = index
	}
	slices.SortFunc(scopeIndexes, func(a, b int) int {
		return compare(result.Config.GitHub.Scopes[a].Name, result.Config.GitHub.Scopes[b].Name)
	})
	var plans []planned
	for _, scopeIndex := range scopeIndexes {
		scope := result.Config.GitHub.Scopes[scopeIndex]
		client, err := request.Open(githubscaleset.GitHubAppAdminConfig{GitHubConfigURL: scope.ConfigURL,
			ClientID: result.Config.GitHub.App.ClientID, InstallationID: installations[scope.Installation], PrivateKey: key,
			System: "tart-runner-fleet", Version: request.Version, Subsystem: "provisioner"})
		if err != nil {
			return Result{}, fmt.Errorf("open GitHub scope %q: %w", scope.Name, err)
		}
		setIndexes := make([]int, len(scope.ScaleSets))
		for index := range setIndexes {
			setIndexes[index] = index
		}
		slices.SortFunc(setIndexes, func(a, b int) int { return compare(scope.ScaleSets[a].Name, scope.ScaleSets[b].Name) })
		for _, setIndex := range setIndexes {
			set := scope.ScaleSets[setIndex]
			spec := githubscaleset.ScaleSetSpec{Name: set.Name, RunnerGroup: scope.RunnerGroup,
				Labels: labelSets[set.Profile].Advertise(set.Labels)}
			plan, err := client.Inspect(ctx, spec)
			if err != nil {
				return Result{}, fmt.Errorf("inspect %s/%s: %w", scope.Name, set.Profile, err)
			}
			if plan.Action != githubscaleset.ScaleSetCreate && plan.Action != githubscaleset.ScaleSetReuse &&
				plan.Action != githubscaleset.ScaleSetUpdate {
				return Result{}, operations.ErrUncertain
			}
			if set.ID > 0 && plan.ID > 0 && set.ID != plan.ID {
				return Result{}, fmt.Errorf("configured scale-set ID differs for %s/%s: %w", scope.Name, set.Profile, operations.ErrConflict)
			}
			plans = append(plans, planned{scopeIndex: scopeIndex, setIndex: setIndex, client: client, spec: spec, plan: plan})
			result.Changes = append(result.Changes, Change{Scope: scope.Name, Profile: set.Profile, Name: set.Name, ID: plan.ID, Action: plan.Action})
		}
	}
	if !request.Apply {
		return result, nil
	}
	for index, item := range plans {
		id := item.plan.ID
		if item.plan.Action == githubscaleset.ScaleSetCreate || item.plan.Action == githubscaleset.ScaleSetUpdate {
			created, err := item.client.Ensure(ctx, item.spec)
			if err != nil {
				return result, fmt.Errorf("provision %s/%s: %w", result.Changes[index].Scope, result.Changes[index].Profile, err)
			}
			id = created.ID
		}
		if id <= 0 {
			return result, operations.ErrUncertain
		}
		result.Config.GitHub.Scopes[item.scopeIndex].ScaleSets[item.setIndex].ID = id
		result.Changes[index].ID = id
	}
	return result, nil
}

func compare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
