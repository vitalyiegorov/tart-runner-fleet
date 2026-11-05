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
	// and Assigned when that was. Neither ever affects a launch: they feed one
	// display-only signal (ADR 0094).
	Unassigned int
	Assigned   time.Time
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
// PowerCorroboration is not: nothing about a node changes the right answer. It holds one
// demand and no other, because a job can collect exits through no fault of its own while
// GitHub dispatches elsewhere. Cap is set by that case: whatever is left of the hold
// stands between such a job and a runner once it can run, which measured 34 minutes at a
// cap of 30 and 19 at 15. Against the 2026-11-05 incident's roughly forty VMs an hour on
// one job this settles to a retry cadence of about four an hour. It slows a failing
// demand and does not cap its total.
var EmptyExitBackoff = EmptyExitPolicy{Tolerated: 2, Base: 5 * time.Minute, Cap: 15 * time.Minute}

// Display-only findings about an image generation (ADR 0094). Nothing that reads these
// may reach a plan. Empty exits cannot tell a bad image from normal dispatch elsewhere,
// and a job can write the log the other is read from, so a signal is all either earns.
const (
	SignalRunnerVersionRefused  = "runner_version_refused"
	SignalImageNotReceivingJobs = "image_generation_not_receiving_jobs"
	// ImageSignalWindow is how recent a reading or exit must be to be shown, and how
	// long a generation must have gone without an assignment to be named.
	ImageSignalWindow = 30 * time.Minute
	// ImageSignalMinExits is how many never-assigned exits on queued jobs "many" means.
	ImageSignalMinExits = 4
)

// Holds reports that a demand must wait at this instant. An exit dated ahead of the clock
// establishes nothing, as after a clock correction across a restart, so it holds nothing
// and can never put a retry beyond the bound.
func (p EmptyExitPolicy) Holds(exits EmptyExits, now time.Time) bool {
	return p.Exceeded(exits) && !exits.Last.After(now) && now.Before(p.RetryAt(exits))
}

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
