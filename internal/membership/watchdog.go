// Package membership is the pure policy of ADR 0061: what a node owes the fleet
// when a scale set it serves stops taking work, and -- in adoption.go -- what a
// sibling may do about a set whose owner has gone.
//
// Nothing here performs I/O or reads a clock. The daemon hands in one tick's
// evidence and carries the returned state forward; the actions it gets back are
// decisions, never effects, so the whole ladder is testable against a clock
// instead of against GitHub (contributor rule 3).
package membership

import (
	"fmt"
	"sort"
	"time"
)

// Mode is how a node declares its membership. The zero value is Off: a node
// that declares nothing runs exactly the code it ran before ADR 0061.
type Mode string

const (
	// Off is a node with no membership watchdog.
	Off Mode = ""
	// Stationary is a node that is meant to be present: a wedged set gets the
	// longer ladder because the node is expected to recover in place.
	Stationary Mode = "stationary"
	// Roaming is a node that comes and goes -- a laptop. Its sets climb a
	// shorter ladder, because a roaming node that is not serving is more likely
	// to be leaving than to be recovering.
	Roaming Mode = "roaming"
)

// ParseMode reads the configured spelling. An unknown spelling is refused
// rather than read as Off: a typo must not silently disable the watchdog.
func ParseMode(raw string) (Mode, error) {
	switch mode := Mode(raw); mode {
	case Off, Stationary, Roaming:
		return mode, nil
	default:
		return Off, fmt.Errorf("membership must be %q or %q, got %q", Stationary, Roaming, raw)
	}
}

// Enabled reports whether this mode runs the watchdog at all.
func (m Mode) Enabled() bool { return m != Off }

// Key identifies one scale set this node serves.
type Key struct {
	Scope string
	ID    int
}

// Stage is where one bound set stands on the ladder.
type Stage string

const (
	// Serving is a set with nothing wrong that this node knows of.
	Serving Stage = "serving"
	// Healing is a wedged set whose session this node is recycling, a bounded
	// number of times, each attempt judged by an audit taken after it.
	Healing Stage = "healing"
	// Excluded is a set whose session this node has released because healing
	// did not work: GitHub stops binding new jobs to it, so a sibling that
	// advertises the same labels takes them instead (ADR 0047's measurement).
	Excluded Stage = "excluded"
	// Probation is a set that rejoined after an exclusion and has not yet been
	// judged by an audit taken after the rejoin.
	Probation Stage = "probation"
)

// severity orders the stages for the node summary: the worst one is the
// node's state.
func (s Stage) severity() int {
	switch s {
	case Excluded:
		return 3
	case Healing:
		return 2
	case Probation:
		return 1
	default:
		return 0
	}
}

// Action is what the policy asks the daemon to do with one set's session.
type Action string

const (
	// Heal closes and reopens the set's session: the repair a daemon restart
	// performs, applied to one binding (ADR 0009's recovery path).
	Heal Action = "heal"
	// Exclude releases the set's session and keeps it released (ADR 0047's
	// withdrawal, applied to one binding).
	Exclude Action = "exclude"
	// Rejoin opens the set's session again.
	Rejoin Action = "rejoin"
)

const (
	stationaryHealAttempts = 2
	roamingHealAttempts    = 1
	stationaryExclusion    = 4
	roamingExclusion       = 2
	// exclusionCapCadences bounds the doubling. A set that keeps failing
	// probation is a set ADR 0056 says an operator must recreate; holding it
	// out for longer than this would only hide that from the next probation.
	exclusionCapCadences = 16
	// freshCadences is how many audit cadences old evidence may be and still
	// describe the present. One cadence is the ordinary gap; the second absorbs
	// a slow audit without letting a dead auditor's last answer stand forever.
	freshCadences = 2
)

// Policy is the whole ladder. Every bound is derived from the mode and from the
// audit cadence the node already declares (github.parkedScaleSetAuditMinutes):
// evidence only arrives once a cadence, so no bound can usefully be shorter,
// and deriving them adds no knob (ADR 0061, the three questions).
type Policy struct {
	Mode    Mode
	Cadence time.Duration
}

// HealAttempts is how many times a wedged set's session is recycled before the
// set is excluded.
func (p Policy) HealAttempts() int {
	if p.Mode == Roaming {
		return roamingHealAttempts
	}
	return stationaryHealAttempts
}

// ExcludedFor is how long the n-th consecutive exclusion lasts. It doubles with
// each probation the set fails, up to a cap, so a set that is genuinely broken
// spends less and less of its time attracting jobs it cannot run.
func (p Policy) ExcludedFor(n int) time.Duration {
	base := stationaryExclusion
	if p.Mode == Roaming {
		base = roamingExclusion
	}
	cadences := base
	for i := 1; i < n && cadences < exclusionCapCadences; i++ {
		cadences *= 2
	}
	return time.Duration(min(cadences, exclusionCapCadences)) * p.Cadence
}

// Evidence is one tick's view of the node's own bound sets.
type Evidence struct {
	// At is now.
	At time.Time
	// AuditedAt is when the latest completed scale-set audit finished; zero
	// means none has. Wedged is that audit's ADR 0056 verdict and is
	// meaningless without it.
	AuditedAt time.Time
	Wedged    []Key
	// Bound is every set this node serves. A set absent from Bound is
	// forgotten: it is no longer this node's to heal.
	Bound []Key
}

// Fresh is the one statement of contributor rule 4 on this path: evidence is
// usable only if an audit completed, recently, and not in the future.
func (p Policy) Fresh(e Evidence) bool {
	if e.AuditedAt.IsZero() || e.AuditedAt.After(e.At) {
		return false
	}
	return e.At.Sub(e.AuditedAt) <= freshCadences*p.Cadence
}

// Binding is one bound set's place on the ladder.
type Binding struct {
	Stage Stage
	// Since is when the current stage began.
	Since time.Time
	// Attempts counts heals in the current Healing stage; Exclusions counts
	// consecutive exclusions without an intervening clean audit.
	Attempts, Exclusions int
	// JudgedAt is the AuditedAt of the evidence behind the last transition. The
	// next transition needs a strictly newer audit, so an action is always
	// judged by evidence gathered after it was taken.
	JudgedAt time.Time
}

// Step folds one tick's evidence into the ladder. It is total and
// deterministic: the same state and evidence always produce the same result.
func Step(p Policy, previous map[Key]Binding, e Evidence) (map[Key]Binding, map[Key]Action) {
	wedged := make(map[Key]bool, len(e.Wedged))
	for _, key := range e.Wedged {
		wedged[key] = true
	}
	fresh := p.Fresh(e)
	next := make(map[Key]Binding, len(e.Bound))
	actions := map[Key]Action{}
	for _, key := range e.Bound {
		current, ok := previous[key]
		if !ok {
			current = Binding{Stage: Serving}
		}
		judged := fresh && e.AuditedAt.After(current.JudgedAt)
		updated, action := p.advance(current, judged, wedged[key], e)
		next[key] = updated
		if action != "" {
			actions[key] = action
		}
	}
	return next, actions
}

// advance is one binding's transition. judged reports that the evidence is
// fresh AND newer than whatever caused this binding's last transition.
func (p Policy) advance(b Binding, judged, wedged bool, e Evidence) (Binding, Action) {
	switch b.Stage {
	case Excluded:
		// Time, and only time, ends an exclusion: the bound job keeps the set
		// reading wedged while it is out, and rejoining is the direction back
		// to configured behaviour, so an audit outage must never hold it out.
		if !e.At.Before(b.Since) && e.At.Sub(b.Since) >= p.ExcludedFor(b.Exclusions) {
			return Binding{Stage: Probation, Since: e.At, Exclusions: b.Exclusions, JudgedAt: e.AuditedAt}, Rejoin
		}
		return b, ""
	case Healing:
		if !judged {
			return b, ""
		}
		if !wedged {
			return Binding{Stage: Serving, Since: e.At, JudgedAt: e.AuditedAt}, ""
		}
		if b.Attempts < p.HealAttempts() {
			b.Attempts++
			b.JudgedAt = e.AuditedAt
			return b, Heal
		}
		return Binding{Stage: Excluded, Since: e.At, Exclusions: b.Exclusions + 1, JudgedAt: e.AuditedAt}, Exclude
	case Probation:
		if !judged {
			return b, ""
		}
		if wedged {
			return Binding{Stage: Excluded, Since: e.At, Exclusions: b.Exclusions + 1, JudgedAt: e.AuditedAt}, Exclude
		}
		return Binding{Stage: Serving, Since: e.At, JudgedAt: e.AuditedAt}, ""
	default:
		if !judged || !wedged {
			return b, ""
		}
		return Binding{Stage: Healing, Since: e.At, Attempts: 1, Exclusions: b.Exclusions, JudgedAt: e.AuditedAt}, Heal
	}
}

// Row is one bound set that is not serving, as the node publishes it.
type Row struct {
	Key
	Binding
}

// Summary is the node's membership as `fleet status` reports it: the worst
// stage of any bound set, and the sets that are not serving.
type Summary struct {
	State    Stage
	Bindings []Row
}

// Summarize projects the ladder for publication. Serving sets are left out so
// the section is finding-shaped rather than an inventory.
func Summarize(state map[Key]Binding) Summary {
	summary := Summary{State: Serving, Bindings: []Row{}}
	for key, binding := range state {
		if binding.Stage.severity() == 0 {
			continue
		}
		summary.Bindings = append(summary.Bindings, Row{Key: key, Binding: binding})
		if binding.Stage.severity() > summary.State.severity() {
			summary.State = binding.Stage
		}
	}
	sort.Slice(summary.Bindings, func(i, j int) bool {
		a, b := summary.Bindings[i].Key, summary.Bindings[j].Key
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.ID < b.ID
	})
	return summary
}
