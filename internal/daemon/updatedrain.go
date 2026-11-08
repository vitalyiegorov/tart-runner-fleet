package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// updateDrainController turns the drain policy's decision into this node's own
// admission, and publishes what it did.
//
// It reads one fact from the filesystem — whether a generation newer than the
// running one is already materialised under `releases/` — because that is the
// signal a daemon can establish about itself without coordinating with the
// separate updater process that put it there.
type updateDrainController struct {
	state *autoupdate.DrainState
	// root is the installation root holding `releases/`; running is this
	// process's own version. Either being empty disables detection, which
	// disables draining: a node must never refuse admission over a fact it
	// could not establish.
	root, running string
	readDir       func(string) ([]string, error)
	// modTime dates a release directory, so the wait for a candidate survives
	// a daemon restart (issue #481).
	modTime      func(string) (time.Time, error)
	report       func(action, version string, instances int)
	pendingSince time.Time
	candidate    string
}

// updateDrainPolicy is the drain policy this node's configuration declares,
// with the node's own stagger added to PendingFor so siblings that take the
// same release do not all stop admitting work on the same minute (issue #500).
func updateDrainPolicy(cfg config.Config) autoupdate.DrainPolicy {
	stagger := autoupdate.StaggerOffset(updateDrainIdentity(cfg), cfg.UpdateDrain.MaxWait)
	return autoupdate.DrainPolicy{Enabled: cfg.UpdateDrain.Enabled, PendingFor: cfg.UpdateDrain.PendingFor + stagger,
		MaxWait: cfg.UpdateDrain.MaxWait, Cooldown: cfg.UpdateDrain.Cooldown}
}

// updateDrainIdentity names this node by the scale sets it owns. ADR 0034 §2
// gives each (scope, scale set) pair exactly one owning node, so the set of them
// tells siblings apart without any new configuration, and file order does not
// change it.
func updateDrainIdentity(cfg config.Config) string {
	sets := cfg.ScopedScaleSets()
	names := make([]string, 0, len(sets))
	for _, scoped := range sets {
		names = append(names, fmt.Sprintf("%s/%s/%d", scoped.ScopeName, scoped.Name(), scoped.ScaleSet.ID))
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

func newUpdateDrainController(policy autoupdate.DrainPolicy, root, running string,
	readDir func(string) ([]string, error), reporter *failureReporter) *updateDrainController {
	controller := &updateDrainController{state: autoupdate.NewDrainState(policy),
		root: root, running: running, readDir: readDir, modTime: autoupdate.ReleaseModTime}
	if reporter != nil {
		controller.report = func(action, version string, instances int) {
			reporter.reportUpdateDrain(action, version, instances)
		}
	}
	return controller
}

// Draining reports whether admission is currently being refused to reach an
// update. It is the gate the scheduler's host observation consults, and it is
// safe on a nil controller so every mode that has no updater simply never
// drains.
func (c *updateDrainController) Draining() bool {
	return c != nil && c.state.Draining()
}

// Candidate names the generation this node is draining toward, for publication.
func (c *updateDrainController) Candidate() string {
	if c == nil {
		return ""
	}
	return c.candidate
}

// Observe folds one tick into the policy. live is the number of instances that
// stand between this node and the quiescence its updater is waiting for.
func (c *updateDrainController) Observe(at time.Time, live int) {
	if c == nil {
		return
	}
	pending, candidate := autoupdate.PendingCandidate(c.root, c.running, c.readDir)
	c.candidate = candidate
	var materialised time.Time
	if !pending {
		c.pendingSince = time.Time{}
	} else if c.pendingSince.IsZero() {
		materialised = autoupdate.PendingSince(c.root, c.running, c.readDir, c.modTime)
		c.pendingSince = at
		if !materialised.IsZero() && !materialised.After(at) {
			c.pendingSince = materialised
		}
	}
	action := c.state.Observe(autoupdate.DrainFacts{At: at, CandidatePending: pending, LiveInstances: live,
		PendingSince: materialised})
	if action != autoupdate.DrainNone && c.report != nil {
		c.report(action.String(), candidate, live)
	}
}

// Metric projects the controller for publication. A node draining on purpose
// must never be indistinguishable from one that is merely refusing admission
// for pressure, which is what `fleet status` would otherwise show.
func (c *updateDrainController) Metric() telemetry.UpdateDrainMetric {
	if c == nil {
		return telemetry.UpdateDrainMetric{}
	}
	return telemetry.UpdateDrainMetric{Draining: c.state.Draining(), Candidate: c.candidate,
		PendingSince: c.pendingSince, Since: c.state.Since()}
}

// installationRoot derives the release root this daemon's updater writes to.
//
// The running executable decides when it lives in a release layout
// (<root>/releases/<generation>/fleet): that is exactly where the updater
// materialises the next generation, wherever the configuration is kept. A node
// started from a candidate or experiment file outside <root>/state would
// otherwise read a directory with no releases/, never see the waiting
// candidate, never drain, and leave the updater's quiescence gate refusing
// forever. Outside a release layout the configuration path decides, so two
// installations on one machine still never read each other's candidates.
func installationRoot(executable, configPath string) string {
	if executable != "" {
		generation := filepath.Dir(executable)
		releases := filepath.Dir(generation)
		if filepath.Base(executable) == "fleet" &&
			filepath.Base(releases) == "releases" && filepath.Base(generation) != "releases" {
			return filepath.Dir(releases)
		}
	}
	if configPath == "" {
		return ""
	}
	// <root>/state/fleet.json
	return filepath.Dir(filepath.Dir(configPath))
}
