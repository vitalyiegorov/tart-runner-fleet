package provision

import (
	"context"
	"fmt"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scalesetaudit"
)

// Retirer is the port `fleet scale-sets retire` holds against a scope's FORMER
// configUrl: the audit listing and live counters, plus one delete by id. It
// cannot create or repair a scale set, so nothing at the old scope can be
// brought back to life by the command meant to remove it.
type Retirer interface {
	scalesetaudit.Client
	Delete(context.Context, int) error
}

// RetireRequest names ONE scale set left at a scope's former configUrl after
// the scope moved (issue #475: a repository scope became an organization one).
// Scope is required, because the scope's current configUrl is what the request
// is checked against; From is the old configUrl the set is still registered at.
type RetireRequest struct {
	Config  config.Config
	Scope   string
	Name    string
	From    string
	LoadKey func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error)
	// OpenAudit reads the scope's CURRENT configUrl, to prove the replacement
	// is provisioned. It is the read-only audit port: the bound scope is never
	// opened with a client that can delete.
	OpenAudit func(githubscaleset.GitHubAppAdminConfig) (scalesetaudit.Client, error)
	// Open reaches From, and only From.
	Open    func(githubscaleset.GitHubAppAdminConfig) (Retirer, error)
	Version string
}

// RetireResult is the object that was deleted.
type RetireResult struct {
	Scope string
	Name  string
	From  string
	ID    int
}

// Retire deletes one scale set registered at a scope's former configUrl, once
// that is provably safe.
//
// A scope whose kind changes from repository to organization provisions a new
// set at its new configUrl and leaves the old one registered at the old URL with
// the same labels and no listener. GitHub can assign jobs to it and never
// reassigns a job, so those jobs strand (issue #475). Every guard fails closed,
// and each runs before the next client is opened:
//
//   - From must be a valid configUrl that is NOT the scope's current one, so
//     the set the node binds can never be named;
//   - the scope must still carry the name with an id, and its current
//     configUrl must hold that exact id: the replacement exists and is bound;
//   - GitHub's live counters for the old set must read no assigned or running
//     job, no busy runner and no registered runner. Counters that cannot be read
//     are uncertain, never zero.
//
// It does not touch the configuration and does not restart anything.
func Retire(ctx context.Context, request RetireRequest) (RetireResult, error) {
	if request.LoadKey == nil || request.Open == nil || request.OpenAudit == nil || request.Name == "" ||
		request.Scope == "" {
		return RetireResult{}, operations.ErrInvalid
	}
	cfg := request.Config.Clone()
	if len(cfg.GitHub.Scopes) == 0 || cfg.ValidateAuthority() != nil {
		return RetireResult{}, operations.ErrInvalid
	}
	fromKey, err := config.ScopeURLKey(request.From)
	if err != nil {
		return RetireResult{}, fmt.Errorf("--from %q: %w: %w", request.From, err, operations.ErrInvalid)
	}
	scope, set, err := retiredScope(cfg, request.Scope, request.Name)
	if err != nil {
		return RetireResult{}, err
	}
	// The configuration validated, so its current configUrl has a key.
	if currentKey, _ := config.ScopeURLKey(scope.ConfigURL); currentKey == fromKey {
		return RetireResult{}, fmt.Errorf("--from is scope %q's current configUrl, whose set this node binds: %w",
			scope.Name, operations.ErrConflict)
	}
	key, err := request.LoadKey(ctx, cfg.GitHub.App.KeychainService, cfg.GitHub.App.KeychainAccount,
		cfg.GitHub.App.PrivateKeyFile)
	if err != nil {
		return RetireResult{}, fmt.Errorf("load GitHub App key: %w", err)
	}
	if key == nil {
		return RetireResult{}, operations.ErrInvalid
	}
	defer key.Destroy()
	admin := func(url string) githubscaleset.GitHubAppAdminConfig {
		installation := int64(0)
		for _, candidate := range cfg.GitHub.Installations {
			if candidate.Name == scope.Installation {
				installation = candidate.InstallationID
			}
		}
		return githubscaleset.GitHubAppAdminConfig{GitHubConfigURL: url, ClientID: cfg.GitHub.App.ClientID,
			InstallationID: installation, PrivateKey: key, System: "tart-runner-fleet", Version: request.Version,
			Subsystem: "provisioner"}
	}
	current, err := request.OpenAudit(admin(scope.ConfigURL))
	if err != nil {
		return RetireResult{}, fmt.Errorf("open GitHub scope %q: %w", scope.Name, err)
	}
	replacement, found, err := findScaleSet(ctx, current, scope.RunnerGroup, set.Name)
	if err != nil {
		return RetireResult{}, fmt.Errorf("list %s at %s: %w", set.Name, scope.ConfigURL, err)
	}
	if !found || replacement.ID != set.ID {
		return RetireResult{}, fmt.Errorf("scope %q does not hold %s as id %d at %s; provision the replacement "+
			"first: %w", scope.Name, set.Name, set.ID, scope.ConfigURL, operations.ErrConflict)
	}
	client, err := request.Open(admin(request.From))
	if err != nil {
		return RetireResult{}, fmt.Errorf("open %s: %w", request.From, err)
	}
	old, found, err := findScaleSet(ctx, client, scope.RunnerGroup, set.Name)
	if err != nil {
		return RetireResult{}, fmt.Errorf("list %s at %s: %w", set.Name, request.From, err)
	}
	if !found {
		return RetireResult{}, fmt.Errorf("no scale set %q at %s: %w", set.Name, request.From, operations.ErrNotFound)
	}
	if old.ID <= 0 {
		return RetireResult{}, operations.ErrUncertain
	}
	stats, err := client.Statistics(ctx, old.ID)
	if err != nil {
		return RetireResult{}, fmt.Errorf("read scale set %d at %s: %w: %w", old.ID, request.From, err,
			operations.ErrUncertain)
	}
	if stats.AssignedJobs > 0 || stats.RunningJobs > 0 || stats.BusyRunners > 0 || stats.RegisteredRunners > 0 {
		return RetireResult{}, fmt.Errorf("scale set %d (%s) at %s still holds %d assigned job(s), %d running job(s), "+
			"%d busy and %d registered runner(s); wait until it drains: %w", old.ID, set.Name, request.From,
			stats.AssignedJobs, stats.RunningJobs, stats.BusyRunners, stats.RegisteredRunners, operations.ErrConflict)
	}
	if err := client.Delete(ctx, old.ID); err != nil {
		return RetireResult{}, fmt.Errorf("delete scale set %d at %s: %w", old.ID, request.From, err)
	}
	return RetireResult{Scope: scope.Name, Name: set.Name, From: request.From, ID: old.ID}, nil
}

// retiredScope is the scope the operator named and the set it still carries. A
// set without an id has no bound replacement yet, which is the precondition
// for removing the one it replaces.
func retiredScope(cfg config.Config, scopeName, setName string) (config.GitHubScope, config.ScaleSet, error) {
	for _, scope := range cfg.GitHub.Scopes {
		if scope.Name != scopeName {
			continue
		}
		for _, set := range scope.ScaleSets {
			if set.Name == setName && set.ID > 0 {
				return scope, set, nil
			}
		}
		return config.GitHubScope{}, config.ScaleSet{}, fmt.Errorf("scope %q carries no provisioned scale set %q; "+
			"provision and write the replacement first: %w", scopeName, setName, operations.ErrConflict)
	}
	return config.GitHubScope{}, config.ScaleSet{}, fmt.Errorf("scope %q is not in this configuration: %w",
		scopeName, operations.ErrNotFound)
}

func findScaleSet(ctx context.Context, client scalesetaudit.Client, group, name string) (githubscaleset.ScaleSetSummary, bool, error) {
	sets, err := client.List(ctx, group)
	if err != nil {
		return githubscaleset.ScaleSetSummary{}, false, err
	}
	for _, set := range sets {
		if set.Name == name {
			return set, true, nil
		}
	}
	return githubscaleset.ScaleSetSummary{}, false, nil
}
