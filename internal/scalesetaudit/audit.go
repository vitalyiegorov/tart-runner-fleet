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

	// Stranding is the evidence: a parked set that is holding work with no
	// registered runner. GitHub has given this set jobs and will give them to
	// nobody else, and from here nothing is known to be listening to it — which
	// is not the same as nothing listening anywhere (see Stranded).
	Stranding  bool      `json:"stranding"`
	ObservedAt time.Time `json:"observedAt"`

	// Instances is how many instances this node holds for a set it serves, and
	// HoldingSince is the first instant the Starving reading below was seen for
	// it. Both are nil when the node could not observe itself -- a CLI run with
	// no daemon to ask -- because an unobserved instance count is not an absent
	// instance (contributor rule 4), and a set nothing is known about cannot be
	// a finding.
	Instances    *int       `json:"instances,omitempty"`
	HoldingSince *time.Time `json:"holdingSince,omitempty"`
	// Wedged is the bound-set finding of issue #336: see Wedging.
	Wedged bool `json:"wedged,omitempty"`
}

// Observation is what this node knows about a set it SERVES: the instances it
// holds for the set, and since when the Starving reading has stood. It is
// answered by whoever can see the node (the daemon's own telemetry, or the
// daemon's published document for a CLI run), and its absence is reported as
// absence.
type Observation struct {
	Instances    int
	HoldingSince time.Time
}

// Key identifies one scale set across audits, which is what a reading has to
// persist against.
type Key struct {
	Scope string
	ID    int
}

// Holding reports whether GitHub says this set has work. Assigned jobs and busy
// runners are read together because either alone is enough: a set with an
// assigned job no runner has taken, and a set whose runner is mid-job, are both
// sets that must have a listener.
func (s ScaleSet) Holding() bool { return s.Assigned > 0 || s.Busy > 0 }

// Stranded is the strongest signal ONE node can read: a Holding set with not a
// single runner registered against it. A registered runner is proof of a
// listener — GitHub only registers one after a listener acquired the job — so a
// parked-but-registered set is plainly a sibling's (ADR 0034 shared labels).
//
// The converse does not hold, and 2026-09-21 proved it from a live fleet: the
// mac mini's bound sets (`trf-fleet-large`, `trf-sudoku-builder`,
// `trf-budgie-builder-2`) each read `assigned>0 busy>0 registered=0` from the
// Linux node while they were simply queued behind the mini's own capacity, and
// node-b's OWN bound set 16 read `assigned=4 busy=4 registered=0` with an empty
// local queue. GitHub's per-set statistics are stale and node-local; a genuine
// stranding and an ordinary backlog on a sibling are the SAME reading from here.
// This predicate is therefore evidence, never a verdict: only a fleet-wide view
// (the hub, issues #175/#218) can say that no node listens to a set.
func (s ScaleSet) Stranded() bool { return s.Holding() && s.Registered == 0 }

// Starving is the bound half of the same reading, and the only one a node has
// standing to judge: this node SERVES the set, GitHub says the set holds work
// (jobs assigned AND runners busy), not one runner is registered against it,
// and this node holds no instance for it. Every term is a fact about this node
// or about the object this node polls -- nothing here is a claim about a
// sibling, which is what made the parked reading evidence-only (ADR 0054).
//
// It is still not a fault on its own: a runner that has not finished booting
// reads exactly this way, which is what Wedging adds.
func (s ScaleSet) Starving() bool {
	return s.State == Bound && s.Assigned > 0 && s.Busy > 0 && s.Registered == 0 &&
		s.Instances != nil && *s.Instances == 0
}

// Wedging is issue #336: a Starving reading that has stood longer than a boot
// takes. Three times on 2026-09-21 a bound set read assigned=3 busy=3
// registered=0 for hours while the node's queue for it was empty and nothing
// was ever delivered; GitHub's counters for the set were stale, and the only
// remedy that worked was deleting the set and provisioning a replacement.
//
// The boot timeout is the node's own declared bound on how long a runner may
// take to register, so no second knob is introduced: below it the reading is
// an ordinary boot, above it nothing is coming.
func (s ScaleSet) Wedging(now time.Time, bootTimeout time.Duration) bool {
	if !s.Starving() || bootTimeout <= 0 || s.HoldingSince == nil || s.HoldingSince.IsZero() {
		return false
	}
	return now.Sub(*s.HoldingSince) > bootTimeout
}

// WedgedReason is the operator-facing sentence, and unlike the parked one it is
// a verdict: every fact in it is this node's own.
func (s ScaleSet) WedgedReason() string {
	held := "an unknown time"
	if s.HoldingSince != nil {
		held = s.ObservedAt.Sub(*s.HoldingSince).Round(time.Second).String()
	}
	return fmt.Sprintf("%s scale set %d (%s) is bound here and has held %d assigned job(s) and %d busy runner(s) "+
		"for %s with no runner registered and no instance on this node: GitHub is delivering nothing for this set "+
		"-- recreate the set (`fleet scale-sets recreate %s --config <path> --confirm recreate-scale-set "+
		"--reason <text>`) and restart the daemon",
		s.Scope, s.ID, s.Name, s.Assigned, s.Busy, held, s.Name)
}

// Track is where the persistence Wedging needs comes from: a pure fold of one
// audit over the previous one. A qualifying reading keeps the instant it was
// first seen; a reading that stops qualifying -- a runner registered, an
// instance booted, the work drained -- drops its clock entirely, so the timer
// can only ever measure one unbroken stretch of the same fault.
func Track(previous map[Key]time.Time, result Result, now time.Time) map[Key]time.Time {
	tracked := make(map[Key]time.Time, len(result.ScaleSets))
	for _, set := range result.ScaleSets {
		if !set.Starving() {
			continue
		}
		key := Key{Scope: set.Scope, ID: set.ID}
		since := now.UTC()
		if earlier, ok := previous[key]; ok && !earlier.IsZero() {
			since = earlier
		}
		tracked[key] = since
	}
	return tracked
}

// Reason is the run-facing sentence for one finding, written from the only thing
// the audit can honestly claim: this node does not serve the set and saw no
// registered runner. It cannot read a sibling's configuration or a sibling's
// queue, so it says the set MAY be stranded and names the confirmation step.
func (s ScaleSet) Reason() string {
	return fmt.Sprintf("%s scale set %d (%s) is parked here and holds %d assigned job(s) and %d busy runner(s) "+
		"with no runner registered: it may be stranded, or a sibling node may be serving it — "+
		"confirm with `fleet scale-sets audit` on every node before acting",
		s.Scope, s.ID, s.Name, s.Assigned, s.Busy)
}

type Result struct {
	ScaleSets []ScaleSet `json:"scaleSets"`
}

// Strandings is the evidence subset, in the order the sets were reported. It is
// what an operator must carry to the other nodes, not a list of faults.
func (r Result) Strandings() []ScaleSet {
	findings := make([]ScaleSet, 0, len(r.ScaleSets))
	for _, set := range r.ScaleSets {
		if set.Stranding {
			findings = append(findings, set)
		}
	}
	return findings
}

// Wedged is the finding subset: the bound sets this node serves and GitHub has
// stopped delivering for. Unlike Strandings these are faults, not evidence.
func (r Result) Wedged() []ScaleSet {
	findings := make([]ScaleSet, 0, len(r.ScaleSets))
	for _, set := range r.ScaleSets {
		if set.Wedged {
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
	// Local answers, for a set this node's configuration binds, what the node
	// holds for it (see Observation). It is nil when nothing can see the node,
	// and a false second return says this particular set was not observed;
	// neither is reported as zero instances.
	Local func(scope string, id int) (Observation, bool)
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
		sets, err := auditScope(ctx, client, scope, observedAt, request)
		if err != nil {
			return Result{}, fmt.Errorf("audit GitHub scope %q: %w", scope.Name, err)
		}
		result.ScaleSets = append(result.ScaleSets, sets...)
	}
	return result, nil
}

func auditScope(ctx context.Context, client Client, scope config.GitHubScope, observedAt time.Time,
	request Request) ([]ScaleSet, error) {
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
		if statistics == nil {
			// The listing carried no counts, so the one question that matters —
			// is this set holding work? — is unanswered, and one read per set
			// answers it. A bound set is read too since issue #336: its counters
			// are the whole evidence for Wedging, and the other signals the node
			// publishes about a set it serves are exactly the ones that read
			// healthy for hours while nothing was delivered.
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
		if row.State == Bound && request.Local != nil {
			if observation, observed := request.Local(scope.Name, summary.ID); observed {
				instances := observation.Instances
				row.Instances = &instances
				if !observation.HoldingSince.IsZero() {
					since := observation.HoldingSince.UTC()
					row.HoldingSince = &since
				}
			}
		}
		row.Wedged = row.Wedging(observedAt, request.Config.Timeouts.Boot)
		sets = append(sets, row)
	}
	slices.SortFunc(sets, func(a, b ScaleSet) int { return a.ID - b.ID })
	return sets, nil
}
