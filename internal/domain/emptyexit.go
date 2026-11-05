package domain

import "time"

// EmptyExits is what the durable record holds about the instances a demand has
// already spent: how many were deleted while still bound to it, and when the last
// of them was.
//
// While a demand is still queued that count IS the number of instances that left
// without taking its job, with no further evidence needed. Demand rank only ever
// moves forward (ADR 0026), so a job that had started on any runner would no longer
// be queued, and alignRunnerDemand moves a runner's binding to whatever GitHub
// actually handed it. An instance that ran something else is bound to that instead.
type EmptyExits struct {
	Count int
	Last  time.Time
	// Unassigned is the part of Count since this generation last had a job assigned,
	// and Launched when it last launched any instance at all. Both are the image-wide
	// bound's, not this demand's.
	Unassigned int
	Launched   time.Time
}

// EmptyExitKey is what exits are counted against: one demand on one image generation.
// A rebuilt image owes nothing for its predecessor's exits, because the operator's
// remedy must work at once.
type EmptyExitKey struct {
	Demand DemandKey
	Image  string
}

// EmptyExitPolicy is how many empty exits a queued demand may cost before the
// fleet stops spending instances on it back to back, and how long it then waits
// between attempts (issue #493, ADR 0094).
//
// A zero policy tolerates nothing and holds for nothing, so it never holds a demand.
type EmptyExitPolicy struct {
	// Tolerated is how many empty exits are ordinary. A sibling taking the job
	// first, a host restart, and a guest that crashed once all look like this, and
	// none of them may delay the job.
	Tolerated int
	// Base is the first hold, doubled per further exit up to Cap. Cap is the
	// promise that a job GitHub starts dispatching again is never kept from a
	// runner for longer than this.
	Base, Cap time.Duration
}

// EmptyExitBackoff is the fleet's policy. It is deliberately not configurable, as
// PowerCorroboration is not: nothing about a node changes the right answer. Against
// the 2026-11-05 incident's roughly forty VMs an hour this settles to a retry cadence of
// two an hour per job. It slows a failing demand and does not cap its total.
var EmptyExitBackoff = EmptyExitPolicy{Tolerated: 2, Base: 5 * time.Minute, Cap: 30 * time.Minute}

// ImageEmptyExitBackoff bounds one image generation across its whole queue. A demand's
// own bound cannot: a refused image charges each of many demands one exit at a time, so
// it runs through every slot long before any one demand earns a hold.
//
// It reads only what a job cannot produce. Running a job means being assigned, so a job
// cannot manufacture an exit that counts. A broker event that went missing can at worst
// slow launches a little, and the next job to bind clears the hold at once. It tolerates
// more than a demand's bound does, and needs ImageHoldMinDemands behind it, so that a
// host restart or one job no runner will take never holds back the rest of the queue.
//
// Its first hold is the assignment deadline's default, because a retry on a guest that
// idles until reclaimed cannot report back any sooner, and a shorter hold would launch
// the next one before the last had answered.
var ImageEmptyExitBackoff = EmptyExitPolicy{Tolerated: 3, Base: 15 * time.Minute, Cap: 30 * time.Minute}

// ImageHoldMinDemands is how many queued demands must share those exits.
const ImageHoldMinDemands = 2

// Exceeded reports that these exits are no longer ordinary.
func (p EmptyExitPolicy) Exceeded(exits EmptyExits) bool {
	return p.Base > 0 && exits.Count > p.Tolerated && !exits.Last.IsZero()
}

// RetryAt is the instant the demand may next be planned. Its age is measured from
// the last exit alone, so a hold ends by itself, whatever else happens: nothing has
// to clear it, and a daemon restart can neither lengthen nor forget it.
func (p EmptyExitPolicy) RetryAt(exits EmptyExits) time.Time {
	if !p.Exceeded(exits) {
		return exits.Last
	}
	hold := p.Base
	for range exits.Count - p.Tolerated - 1 {
		if hold >= p.Cap {
			break
		}
		hold *= 2
	}
	return exits.Last.Add(min(hold, p.Cap))
}
