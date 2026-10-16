package membership

import "time"

// This file is rung (c) of ADR 0063's ladder: what a sibling may do about a
// scale set whose owner has gone. It is the executable statement of the rule,
// and the deterministic simulation (tests/simulation/membership_test.go)
// exercises it against a model of GitHub's session arbiter.
//
// It has NO production caller yet, deliberately. ADR 0063's spike could not
// settle from code or documentation that a sibling's new session is handed the
// work already assigned to the set, nor how long GitHub keeps an unpolled
// session alive. Until the live experiments the ADR lists have answered both,
// the daemon does not open a session on any set it does not own. The policy is
// here so the follow-up that wires it has a proven rule to satisfy rather than
// a paragraph to interpret.

// Arbiter is GitHub's answer to "open a message session on this set". It is the
// only cross-node arbiter the fleet has (ADR 0034) and the corroboration ADR
// 0042 requires before a sibling acts on another node's set.
type Arbiter string

const (
	// Held is a 409: another session holds the set, so its owner is alive --
	// possibly wedged, possibly behind its own capacity, but present.
	Held Arbiter = "held"
	// Obtained is a session GitHub granted: no other session holds the set.
	Obtained Arbiter = "obtained"
	// Unanswered is everything else: a timeout, a 5xx, a refused credential. It
	// proves nothing about the owner (contributor rule 4).
	Unanswered Arbiter = "unanswered"
)

// Sighting is what a sibling's audit saw of one set it does NOT serve.
type Sighting struct {
	Key
	// Equivalent reports that the sighting node serves a set advertising the
	// identical label list in the same scope. ADR 0034 amendment c makes a
	// shared label a promise of equivalent guest capability, which is the only
	// ground a sibling has for believing it can run the work at all.
	Equivalent bool
	// Assigned, Busy and Registered are GitHub's own counters for the set.
	Assigned, Busy, Registered int
	// StrandedSince is when this sibling first saw the set holding work with no
	// registered runner (scalesetaudit.Track over the parked reading); zero if
	// it is not stranded now.
	StrandedSince time.Time
	// AuditedAt is when the audit that produced the sighting completed.
	AuditedAt time.Time
}

// stranded is the ADR 0054 parked reading: work held, no runner registered.
func (s Sighting) stranded() bool { return (s.Assigned > 0 || s.Busy > 0) && s.Registered == 0 }

// AdoptionPolicy bounds when a sibling may ask the arbiter and for how long it
// may keep what it was granted.
type AdoptionPolicy struct {
	// Cadence is the sibling's own audit cadence, the period of its evidence.
	Cadence time.Duration
}

// Grace is how long a set must have read stranded before a sibling asks for its
// session, measured between audits rather than against the clock: the audit
// that grants the ask must itself have been taken a grace after the first one
// that saw the set stranded, so at least three consecutive audits agree and a
// single reading followed by an audit outage can never satisfy it. A sibling cannot read its
// owner's configuration, so the owner's declared mode cannot shorten this; a
// roaming owner shortens its own exposure instead, by yielding (ADR 0063 e).
func (p AdoptionPolicy) Grace() time.Duration { return freshCadences * p.Cadence }

// Lease is the longest a sibling holds an adopted session before handing it
// back, idle or not, so a returning owner is never locked out for longer than
// this.
func (p AdoptionPolicy) Lease() time.Duration { return stationaryExclusion * p.Cadence }

// MayAsk is the whole premise a sibling needs before it may ask GitHub for a
// set's session: the set is equivalent to one it serves, the evidence is fresh,
// the set has read stranded for longer than the grace, and its own admission
// would admit work right now. Every term is a fact this node holds; the claim
// about the owner is left entirely to the arbiter.
func (p AdoptionPolicy) MayAsk(s Sighting, now time.Time, admissionAllowed bool) bool {
	if !admissionAllowed || !s.Equivalent || !s.stranded() || s.StrandedSince.IsZero() {
		return false
	}
	if !(Policy{Cadence: p.Cadence}).Fresh(Evidence{At: now, AuditedAt: s.AuditedAt}) {
		return false
	}
	return !s.AuditedAt.Before(s.StrandedSince) && s.AuditedAt.Sub(s.StrandedSince) >= p.Grace()
}

// Adoption is a session a sibling holds on a set it does not own.
type Adoption struct {
	Key
	Since, Until time.Time
}

// Arbitrate turns the arbiter's answer into what the sibling holds. Only
// Obtained adopts; Held proves the owner is alive and Unanswered proves
// nothing, and neither leaves the sibling holding anything.
func (p AdoptionPolicy) Arbitrate(key Key, answer Arbiter, now time.Time) (Adoption, bool) {
	if answer != Obtained {
		return Adoption{}, false
	}
	return Adoption{Key: key, Since: now, Until: now.Add(p.Lease())}, true
}

// Release reports that an adopted session must be handed back: the adopter has
// nothing of the set's left to run, or the lease is up. Running work keeps the
// session only until the lease, because the session is how a runner's
// completion is reported and the owner's return must still be bounded.
func (a Adoption) Release(now time.Time, idle bool) bool {
	return idle || !now.Before(a.Until)
}
