package daemon

import (
	"path/filepath"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
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
	report        func(action, version string, instances int)
	pendingSince  time.Time
	candidate     string
}

func newUpdateDrainController(policy autoupdate.DrainPolicy, root, running string,
	readDir func(string) ([]string, error), reporter *failureReporter) *updateDrainController {
	controller := &updateDrainController{state: autoupdate.NewDrainState(policy),
		root: root, running: running, readDir: readDir}
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
	if !pending {
		c.pendingSince = time.Time{}
	} else if c.pendingSince.IsZero() {
		c.pendingSince = at
	}
	action := c.state.Observe(autoupdate.DrainFacts{At: at, CandidatePending: pending, LiveInstances: live})
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

// installationRoot derives the release root from the configuration path the
// daemon was started with, rather than from the ambient user's default layout:
// a daemon told to run from an explicit path must read the releases beside that
// path, not some other installation's.
func installationRoot(configPath string) string {
	if configPath == "" {
		return ""
	}
	// <root>/state/fleet.json
	return filepath.Dir(filepath.Dir(configPath))
}
