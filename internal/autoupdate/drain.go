package autoupdate

import (
	"os"
	"path/filepath"
	"time"
)

// DrainAction is what the update-drain policy asks the node to do with its own
// admission. It is a decision, not an effect: the state machine is pure so the
// rule can be tested against a clock instead of against a fleet.
type DrainAction int

const (
	DrainNone DrainAction = iota
	// DrainStart stops admitting new work so the live instances standing between
	// this node and its update can finish and not be replaced.
	DrainStart
	// DrainStop resumes admission, either because the update went through, the
	// candidate went away, or the node waited longer than the deadline allows.
	DrainStop
)

func (a DrainAction) String() string {
	switch a {
	case DrainStart:
		return "start"
	case DrainStop:
		return "stop"
	default:
		return "none"
	}
}

// DrainPolicy bounds how a node reaches the quiescence its updater needs.
//
// ADR 0011 refuses to swap a generation out from under running work, which is
// right, and left the fleet with a gate it could only pass by coincidence: a
// node that is doing its job is never idle, so the busiest nodes ran the oldest
// code. Issue #230 measured 1011 consecutive refusals on one node; issue #282
// found both Macs 26 releases behind for the same reason.
//
// Draining does not weaken the guarantee. Running instances still finish their
// jobs untouched — what stops is the arrival of their replacements, which is the
// only thing standing between a busy node and a quiescent one.
type DrainPolicy struct {
	Enabled bool
	// PendingFor is how long a newer generation must have been sitting on disk,
	// unapplied, before this node starts refusing admission to reach it.
	PendingFor time.Duration
	// MaxWait bounds the drain. A node that cannot reach quiescence within it —
	// a two-hour Maestro guest, a job that will not end — resumes admission
	// rather than starving its own queue for a release that can wait.
	MaxWait time.Duration
	// Cooldown is how long a node serves normally after abandoning a drain,
	// so an unreachable quiescence cannot become a permanent half-capacity node.
	Cooldown time.Duration
}

// DrainFacts is one tick's view of whether this node needs, and can reach, the
// quiescence an update requires.
type DrainFacts struct {
	At time.Time
	// CandidatePending reports that a generation newer than the running one is
	// already on disk and has not been applied.
	CandidatePending bool
	// LiveInstances is what stands between this node and quiescence. Queued jobs
	// deliberately do not count: they are not interruptible work, and counting
	// them makes the drain self-defeating, since draining is what grows a queue.
	LiveInstances int
}

// DrainState tracks the current phase. The zero value is a node serving normally
// that has never observed anything, which is what a fresh daemon is.
type DrainState struct {
	policy DrainPolicy
	// draining is this node's own refusal of admission, not the host's.
	draining bool
	// since marks when the current phase began: pending-but-not-yet-draining,
	// draining, or cooling down.
	since     time.Time
	cooldown  bool
	observed  bool
	pendingAt time.Time
}

// NewDrainState binds a policy to a fresh state.
func NewDrainState(policy DrainPolicy) *DrainState { return &DrainState{policy: policy} }

// Draining reports whether this node is currently refusing admission to reach an
// update. It exists to be published: a node at half capacity on purpose must
// never look like one that is merely quiet.
func (s *DrainState) Draining() bool { return s != nil && s.draining }

// Since reports when the current phase began, so an operator reads how long a
// drain has been waiting rather than only that it is waiting.
func (s *DrainState) Since() time.Time {
	if s == nil {
		return time.Time{}
	}
	return s.since
}

// Observe folds one tick into the state machine and returns the action owed.
// It is total: every input yields a decision, and the same inputs always yield
// the same one.
func (s *DrainState) Observe(facts DrainFacts) DrainAction {
	if s == nil {
		return DrainNone
	}
	if !s.policy.Enabled {
		if s.draining {
			s.reset(facts.At)
			return DrainStop
		}
		return DrainNone
	}
	// A candidate that went away — rolled back, or applied by something else —
	// ends the drain immediately. There is nothing left to reach.
	if !facts.CandidatePending {
		s.pendingAt = time.Time{}
		if s.draining {
			s.reset(facts.At)
			return DrainStop
		}
		s.cooldown = false
		return DrainNone
	}
	if !s.observed || s.pendingAt.IsZero() || facts.At.Before(s.pendingAt) {
		s.observed = true
		s.pendingAt = facts.At
		s.since = facts.At
	}
	if s.draining {
		// Quiescence reached: hold the drain until the updater applies and this
		// process is replaced. Releasing admission here would let a new instance
		// start in the window before the swap and put the node straight back
		// where it started.
		if facts.At.Sub(s.since) >= s.policy.MaxWait {
			s.draining = false
			s.cooldown = true
			s.since = facts.At
			return DrainStop
		}
		return DrainNone
	}
	if s.cooldown {
		if facts.At.Sub(s.since) >= s.policy.Cooldown {
			s.cooldown = false
			s.since = facts.At
		}
		return DrainNone
	}
	if facts.At.Sub(s.pendingAt) >= s.policy.PendingFor {
		s.draining = true
		s.since = facts.At
		return DrainStart
	}
	return DrainNone
}

func (s *DrainState) reset(at time.Time) {
	s.draining = false
	s.cooldown = false
	s.since = at
}

// PendingCandidate reports whether a generation newer than running is already
// materialised under rootDir/releases, and names the newest one.
//
// This is the signal a node can read about itself without coordinating with the
// updater process: the updater downloads and verifies a candidate into its own
// release directory *before* the quiescence gate refuses it, so a newer
// directory that is still not running is exactly "an update is waiting on me".
//
// It is deliberately conservative. An unreadable directory, an unparseable
// version, or no releases at all reports nothing pending, because a node must
// never refuse admission on the strength of a fact it could not establish.
func PendingCandidate(rootDir, running string, readDir func(string) ([]string, error)) (bool, string) {
	if rootDir == "" || running == "" || readDir == nil {
		return false, ""
	}
	names, err := readDir(filepath.Join(rootDir, "releases"))
	if err != nil {
		return false, ""
	}
	newest := ""
	for _, name := range names {
		if name == running {
			continue
		}
		order, err := compareVersions(name, running)
		if err != nil || order <= 0 {
			continue
		}
		if newest == "" {
			newest = name
			continue
		}
		if later, err := compareVersions(name, newest); err == nil && later > 0 {
			newest = name
		}
	}
	return newest != "", newest
}

// ReleaseDirNames lists the release directories beneath root. It is the
// production reader PendingCandidate takes, kept separate so the policy can be
// tested without a filesystem.
func ReleaseDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}
