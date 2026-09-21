package provision

import (
	"context"
	"fmt"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Recreater is the provisioning client plus the one destructive call the fleet
// makes against GitHub's scale-set admin API.
//
// It is a separate interface rather than a method on Client so the ordinary
// provisioning path cannot reach Delete at all: `scale-sets provision` holds a
// Client and could not delete a scale set if it tried, whatever the adapter
// behind it is able to do.
type Recreater interface {
	Client
	Delete(context.Context, int) error
}

// RecreateRequest names ONE scale set, by the name the configuration gives it.
// There is no "all" form and no id form: the operator states the set they mean,
// and the configuration is what says which object that is.
type RecreateRequest struct {
	Config config.Config
	// Scope narrows an ambiguous name to one scope. It is optional, and only a
	// name carried by more than one scope needs it.
	Scope   string
	Name    string
	LoadKey func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error)
	Open    func(githubscaleset.GitHubAppAdminConfig) (Recreater, error)
	Version string
}

// RecreateResult reports the substitution the operator has to act on: the
// object that was deleted, the object that replaced it, and the configuration
// carrying the new id, which the caller persists.
type RecreateResult struct {
	Config  config.Config
	Scope   string
	Profile string
	Name    string
	OldID   int
	NewID   int
}

// Recreate deletes one runner scale set on GitHub and provisions a replacement
// with the same name, labels and runner group.
//
// It exists because a stranded bound set cannot be repaired (issue #336, ADR
// 0056): GitHub's counters for the set say it holds assigned jobs and busy
// runners while it delivers nothing, and no update to the object clears them.
// Only a new object with a new id starts receiving work again, because GitHub
// routes a queued job to a scale-set ID.
//
// The order is delete, then create, then write, and it stops at the first
// failure: a configuration naming an id that was never created would leave the
// node polling nothing at all, which is a worse state than the one being
// repaired.
func Recreate(ctx context.Context, request RecreateRequest) (RecreateResult, error) {
	if request.LoadKey == nil || request.Open == nil || request.Name == "" {
		return RecreateResult{}, operations.ErrInvalid
	}
	cfg := request.Config.Clone()
	if len(cfg.GitHub.Scopes) == 0 || cfg.ValidateAuthority() != nil {
		return RecreateResult{}, operations.ErrInvalid
	}
	scopeIndex, setIndex, err := locate(cfg, request.Scope, request.Name)
	if err != nil {
		return RecreateResult{}, err
	}
	scope := cfg.GitHub.Scopes[scopeIndex]
	set := scope.ScaleSets[setIndex]
	key, err := request.LoadKey(ctx, cfg.GitHub.App.KeychainService, cfg.GitHub.App.KeychainAccount,
		cfg.GitHub.App.PrivateKeyFile)
	if err != nil {
		return RecreateResult{}, fmt.Errorf("load GitHub App key: %w", err)
	}
	if key == nil {
		return RecreateResult{}, operations.ErrInvalid
	}
	defer key.Destroy()
	installation := int64(0)
	for _, candidate := range cfg.GitHub.Installations {
		if candidate.Name == scope.Installation {
			installation = candidate.InstallationID
		}
	}
	client, err := request.Open(githubscaleset.GitHubAppAdminConfig{GitHubConfigURL: scope.ConfigURL,
		ClientID: cfg.GitHub.App.ClientID, InstallationID: installation, PrivateKey: key,
		System: "tart-runner-fleet", Version: request.Version, Subsystem: "provisioner"})
	if err != nil {
		return RecreateResult{}, fmt.Errorf("open GitHub scope %q: %w", scope.Name, err)
	}
	if set.ID > 0 {
		if err := client.Delete(ctx, set.ID); err != nil {
			return RecreateResult{}, fmt.Errorf("delete scale set %d (%s/%s): %w", set.ID, scope.Name, set.Name, err)
		}
	}
	spec := githubscaleset.ScaleSetSpec{Name: set.Name, RunnerGroup: scope.RunnerGroup,
		Labels: cfg.ProfileLabelSets()[set.Profile].Advertise(set.Labels)}
	created, err := client.Ensure(ctx, spec)
	if err != nil {
		return RecreateResult{}, fmt.Errorf("provision %s/%s: %w", scope.Name, set.Profile, err)
	}
	// The same id back is not a replacement: it means GitHub still holds the
	// object the delete was supposed to remove, and the set is still stranded.
	if created.ID <= 0 || (set.ID > 0 && created.ID == set.ID) {
		return RecreateResult{}, operations.ErrUncertain
	}
	cfg.GitHub.Scopes[scopeIndex].ScaleSets[setIndex].ID = created.ID
	return RecreateResult{Config: cfg, Scope: scope.Name, Profile: set.Profile, Name: set.Name,
		OldID: set.ID, NewID: created.ID}, nil
}

// locate answers which configured set the operator named, and refuses to guess.
// A name no scope carries is not found; a name two scopes carry is a conflict
// until the operator says which scope they mean, because the call that follows
// deletes an object and cannot be taken back.
func locate(cfg config.Config, scopeName, setName string) (int, int, error) {
	scopeIndex, setIndex, found := -1, -1, 0
	for i, scope := range cfg.GitHub.Scopes {
		if scopeName != "" && scope.Name != scopeName {
			continue
		}
		for j, set := range scope.ScaleSets {
			if set.Name != setName {
				continue
			}
			scopeIndex, setIndex, found = i, j, found+1
		}
	}
	switch {
	case found == 0:
		return 0, 0, fmt.Errorf("scale set %q is not in this configuration: %w", setName, operations.ErrNotFound)
	case found > 1:
		return 0, 0, fmt.Errorf("scale set %q is named by %d scopes; name one with --scope: %w",
			setName, found, operations.ErrConflict)
	}
	return scopeIndex, setIndex, nil
}
