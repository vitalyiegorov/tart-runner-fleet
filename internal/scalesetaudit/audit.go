// Package scalesetaudit reads the runner scale sets that EXIST on GitHub for
// each configured scope and says which of them this node serves.
//
// A scale set GitHub holds and no daemon polls is a black hole: GitHub routes a
// queued job to exactly one matching set, marks it assigned, and then offers it
// to nobody else — not even to an identically-labelled, healthy set in the same
// repository. Nothing in the fleet could see that, because every signal the node
// owns is about the sets it DOES serve (issue #164, ADR 0054).
package scalesetaudit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Client is the scope-scoped half of the runner scale-set admin API this audit
// uses, and nothing more: one listing per scope, one read per parked set. The
// per-repository job inventory is deliberately not on this path — it is the REST
// lane `github.canonicalJobInventory` gates, and this audit must be runnable on
// a node where that flag is off, which is every node issue #164 was filed from.
type Client interface {
	List(context.Context, string) ([]githubscaleset.ScaleSetSummary, error)
	Statistics(context.Context, int) (githubscaleset.ScaleSetStatistics, error)
}

// State is what this node knows about a scale set GitHub reports.
type State string

const (
	// Bound: this node's configuration names the set, so something here is
	// polling it.
	Bound State = "bound"
	// Parked: the set exists on GitHub and this node's configuration does not
	// name it. It may well be bound on a SIBLING node — shared-label federation
	// is the ordinary case (ADR 0034) — which is why a parked set holding nothing
	// is informational and never a finding.
	Parked State = "parked"
)

// ScaleSet is one set as the audit saw it.
type ScaleSet struct {
	Scope   string `json:"scope"`
	ID      int    `json:"id"`
	Name    string `json:"name"`
	State   State  `json:"state"`
	Profile string `json:"profile,omitempty"`

	Assigned   int `json:"assigned"`
	Busy       int `json:"busy"`
	Registered int `json:"registered"`
	Idle       int `json:"idle"`
	Available  int `json:"available"`
	Acquired   int `json:"acquired"`
	Running    int `json:"running"`

	// Stranding is the finding: a parked set that is holding work. GitHub has
	// given this set jobs and will give them to nobody else, and from here
	// nothing is known to be listening to it.
	Stranding  bool      `json:"stranding"`
	ObservedAt time.Time `json:"observedAt"`
}

// Holding reports whether GitHub says this set has work. Assigned jobs and busy
// runners are read together because either alone is enough: a set with an
// assigned job no runner has taken, and a set whose runner is mid-job, are both
// sets that must have a listener.
func (s ScaleSet) Holding() bool { return s.Assigned > 0 || s.Busy > 0 }

// Stranded is a Holding set nothing can be serving: work is assigned and not a
// single runner is registered against the set. A registered runner is itself
// proof of a listener — GitHub only registers one after a listener acquired
// the job — so a parked-but-registered set is a sibling's (ADR 0034 shared
// labels) and a finding here would fail this node's doctor forever on its
// sibling's normal traffic; the first live cadence run did exactly that.
func (s ScaleSet) Stranded() bool { return s.Holding() && s.Registered == 0 }

// Reason is the run-facing sentence for one stranding, written from the only
// thing the audit can honestly claim: this node does not serve the set. It
// cannot read a sibling's configuration, so it says what is known rather than
// accusing a node of being absent.
func (s ScaleSet) Reason() string {
	return fmt.Sprintf("%s scale set %d (%s) is parked here and holds %d assigned job(s) and %d busy runner(s) "+
		"with no runner registered: nothing can be listening to this set",
		s.Scope, s.ID, s.Name, s.Assigned, s.Busy)
}

type Result struct {
	ScaleSets []ScaleSet `json:"scaleSets"`
}

// Strandings is the alertable subset, in the order the sets were reported.
func (r Result) Strandings() []ScaleSet {
	findings := make([]ScaleSet, 0, len(r.ScaleSets))
	for _, set := range r.ScaleSets {
		if set.Stranding {
			findings = append(findings, set)
		}
	}
	return findings
}

type Request struct {
	Config config.Config
	// Key is the GitHub App private key when the caller already holds one — the
	// daemon does, for the whole of its run — and nil when the caller wants this
	// package to load and destroy one. Ownership follows: a key passed in is
	// never destroyed here.
	Key     *githubscaleset.PrivateKeySecret
	LoadKey func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error)
	Open    func(githubscaleset.GitHubAppAdminConfig) (Client, error)
	Version string
	Now     func() time.Time
}

// Run audits every configured scope. It never polls: one listing per scope and
// one read per parked set, then it returns.
func Run(ctx context.Context, request Request) (Result, error) {
	if request.Open == nil || request.Now == nil || len(request.Config.GitHub.Scopes) == 0 {
		return Result{}, operations.ErrInvalid
	}
	key := request.Key
	if key == nil {
		if request.LoadKey == nil {
			return Result{}, operations.ErrInvalid
		}
		loaded, err := request.LoadKey(ctx, request.Config.GitHub.App.KeychainService,
			request.Config.GitHub.App.KeychainAccount, request.Config.GitHub.App.PrivateKeyFile)
		if err != nil {
			return Result{}, fmt.Errorf("load GitHub App key: %w", err)
		}
		if loaded == nil {
			return Result{}, operations.ErrInvalid
		}
		defer loaded.Destroy()
		key = loaded
	}
	installations := make(map[string]int64, len(request.Config.GitHub.Installations))
	for _, installation := range request.Config.GitHub.Installations {
		installations[installation.Name] = installation.InstallationID
	}
	scopes := append([]config.GitHubScope(nil), request.Config.GitHub.Scopes...)
	slices.SortFunc(scopes, func(a, b config.GitHubScope) int { return strings.Compare(a.Name, b.Name) })
	observedAt := request.Now().UTC()
	result := Result{ScaleSets: []ScaleSet{}}
	for _, scope := range scopes {
		client, err := request.Open(githubscaleset.GitHubAppAdminConfig{GitHubConfigURL: scope.ConfigURL,
			ClientID: request.Config.GitHub.App.ClientID, InstallationID: installations[scope.Installation],
			PrivateKey: key, System: "tart-runner-fleet", Version: request.Version, Subsystem: "auditor"})
		if err != nil {
			return Result{}, fmt.Errorf("open GitHub scope %q: %w", scope.Name, err)
		}
		sets, err := auditScope(ctx, client, scope, observedAt)
		if err != nil {
			return Result{}, fmt.Errorf("audit GitHub scope %q: %w", scope.Name, err)
		}
		result.ScaleSets = append(result.ScaleSets, sets...)
	}
	return result, nil
}

func auditScope(ctx context.Context, client Client, scope config.GitHubScope, observedAt time.Time) ([]ScaleSet, error) {
	if client == nil {
		return nil, operations.ErrInvalid
	}
	listed, err := client.List(ctx, scope.RunnerGroup)
	if err != nil {
		return nil, err
	}
	boundIDs := make(map[int]string, len(scope.ScaleSets))
	boundNames := make(map[string]string, len(scope.ScaleSets))
	for _, set := range scope.ScaleSets {
		if set.ID > 0 {
			// A configured id is the node's whole claim about which object it
			// serves: the session polls THAT id. A set GitHub recreated under the
			// same name carries a different id, so the node is still polling the
			// old one and the new one is parked -- reading the name as a binding
			// would hide exactly the stranding this audit exists for.
			boundIDs[set.ID] = set.Profile
			continue
		}
		if set.Name != "" {
			boundNames[set.Name] = set.Profile
		}
	}
	sets := make([]ScaleSet, 0, len(listed))
	for _, summary := range listed {
		row := ScaleSet{Scope: scope.Name, ID: summary.ID, Name: summary.Name, State: Parked, ObservedAt: observedAt}
		// Identity is the configured id, and the name only for a set whose id was
		// never persisted: a node that has provisioned but not yet written the id
		// back still serves the set it created, and calling that parked would be a
		// false finding.
		if profile, bound := boundIDs[summary.ID]; bound {
			row.State, row.Profile = Bound, profile
		} else if profile, bound := boundNames[summary.Name]; bound {
			row.State, row.Profile = Bound, profile
		}
		statistics := summary.Statistics
		if statistics == nil && row.State == Parked {
			// The listing carried no counts, so the one question that matters —
			// is this set holding work? — is unanswered. One read per parked set
			// answers it; a bound set is left uncounted rather than paid for,
			// because a set this node serves is already reported by every other
			// signal the node publishes.
			read, err := client.Statistics(ctx, summary.ID)
			if err != nil {
				return nil, err
			}
			statistics = &read
		}
		if statistics != nil {
			row.Assigned, row.Busy = statistics.AssignedJobs, statistics.BusyRunners
			row.Registered, row.Idle = statistics.RegisteredRunners, statistics.IdleRunners
			row.Available, row.Acquired, row.Running = statistics.AvailableJobs, statistics.AcquiredJobs, statistics.RunningJobs
		}
		row.Stranding = row.State == Parked && row.Stranded()
		sets = append(sets, row)
	}
	slices.SortFunc(sets, func(a, b ScaleSet) int { return a.ID - b.ID })
	return sets, nil
}
