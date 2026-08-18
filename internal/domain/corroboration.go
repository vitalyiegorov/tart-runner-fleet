package domain

import "time"

// A destructive recovery may not rest on a single observation that the fleet's
// own re-verification is expected to contradict.
//
// On 2026-08-18 one instance on this fleet was planned for a destructive
// recovery drain eighty-six times in nine minutes, and every one of those drains
// was aborted at execution time because `VMControl.Running` — the same
// `tart list` the observation came from, two seconds later — answered that the VM
// was running after all. The night before, the same storm ran a hundred and
// fifteen times on a different instance. Nothing was wrong with either runner.
//
// The premise was one uncorroborated power reading. `tart` reports a VM it cannot
// open the configuration of as `"Running": false` — its `running()` swallows the
// error and returns "not running" — so a transient failure that has nothing to do
// with the machine arrives at the fleet as a confident statement that the machine
// is off, indistinguishable from the real thing (issue #246).
//
// Every other recovery cause in this fleet states a bound before it will act:
// the assignment deadline and the idle-runner deadline require elapsed time, the
// occupancy budget requires a configured ceiling, and the guest-liveness verdict
// (ADR 0040) requires an unbroken run of refusals over a window. `Power ==
// Stopped` required nothing at all. This file gives it the same shape as the
// others, stated once.
//
// The accumulator and the bound are ADR 0040's, reused rather than rebuilt: they
// were always about "how much unbroken hostile observation, over how long, before
// the fleet acts" rather than about guests specifically. The names below say so.

// ObservationRun is an unbroken run of hostile observations about one instance:
// how many, since when, and when the last contrary observation cleared it. It is
// the accumulator ADR 0040 introduced for guest probes, named here for what it
// is, because a second observation needed the identical rule.
type ObservationRun = GuestLivenessState

// CorroborationPolicy is how much unbroken hostile observation, over how long,
// the fleet requires before it will act destructively on what that observation
// claims. Both bounds must be met: the count bounds a momentary glitch, and the
// window bounds a control loop that happens to tick quickly.
//
// A zero policy states no bound and can confirm nothing. A destructive premise is
// never inferred from a policy that does not state one.
type CorroborationPolicy = GuestLivenessPolicy

// PowerSignal folds one backend power reading into the three-valued observation a
// CorroborationPolicy accumulates.
//
// Only `stopped` is hostile. `running` clears the run outright, which is what
// makes an aborted recovery self-limiting: the drain returns the instance to
// Running and the next reading that agrees resets the premise to nothing. Absent
// and unknown are neither — an owned VM missing from an enumeration is a
// different fact with its own gate, and an unread power state has established
// nothing and must never accumulate toward destroying a runner.
func PowerSignal(power InstancePower) GuestLiveness {
	switch power {
	case InstancePowerRunning:
		return GuestLivenessAlive
	case InstancePowerStopped:
		return GuestLivenessRefused
	default:
		return GuestLivenessUnknown
	}
}

// PowerCorroboration is the bound a backend's claim that a live instance's VM is
// powered off must meet before the fleet will destroy that instance for it.
//
// It is deliberately NOT configurable. A knob here could only ever be turned
// towards acting on a single reading, which is the behaviour issue #246 is about;
// and unlike ADR 0040's bound this one cannot end a healthy job on its own — it
// can only delay a reclaim the fleet would otherwise perform immediately. Three
// readings over forty-five seconds costs a genuinely powered-off instance well
// under a minute of extra vector hold, against an occupancy budget measured in
// hours, and it is longer than any storm this fleet has ever survived by accident.
var PowerCorroboration = CorroborationPolicy{ConsecutiveRefusals: 3, Window: 45 * time.Second}

// PowerRetractedFactor is how much longer a power premise must hold once the
// fleet has already disproven it about this instance.
//
// An aborted stopped recovery is the strongest evidence that exists about a power
// reading: the drain re-read the same source at the moment of acting and got the
// opposite answer. Discarding that is what turned nine minutes into eighty-six
// drains — the fleet re-derived the identical operation from the identical
// reading every time the run refilled, having learned nothing from disproving it.
//
// It is a single step rather than a growing backoff because there is nothing for
// a second step to learn: the first retraction already establishes that this
// instance's readings cannot be trusted at the ordinary bound, and a ladder would
// add a knob, a counter and an edge to detect, for a distinction no operator can
// act on differently. Six minutes is longer than either production storm's
// inter-drain gap and two orders of magnitude inside the occupancy budget that
// backstops a genuinely stopped VM.
const PowerRetractedFactor = 8

// CorroboratedPower is the power reading the fleet is allowed to act on: the raw
// one, unless it says `stopped` and the run behind it has not met the bound, in
// which case it is Unknown — nothing was established.
//
// The downgrade happens HERE, at classification, rather than at the one gate that
// plans a kill, and that is the whole of the second half of issue #246. The
// scheduler does not only destroy instances for a stopped power reading; it also
// stops charging the host for them (ConsumesHostResources), and admits work into
// the vector it thinks came back. A misreported instance therefore double-books
// the machine — the simulator produced exactly that, a conservation violation
// five slots over a four-slot ceiling, the first time this fault existed. One
// rule at the classification protects every consumer; one rule at the gate would
// have protected one of them.
//
// Unknown rather than Running is deliberate. The fleet has not established that
// the VM is running either, and Unknown is the value that already means "nothing
// was read" everywhere it appears: ProvenIdle excludes it by construction, so an
// uncorroborated instance goes on charging the host, which is the conservative
// direction on both axes.
//
// A stopped reading is only surprising for an instance the fleet has not decided
// to tear down, so a tearing-down instance is exempt. There the fleet's own
// decision corroborates the reading, and requiring a second source would delay
// releasing the vector of a VM everyone agrees is finished — on every teardown,
// which is the capacity turnaround this fleet runs on. Holding that exemption is
// also what keeps every corpus digest byte-identical to the merge base: no trace
// that is never handed a misreport sees any behaviour change at all.
//
// It is fail-closed on every other input: an unmeasurable run is not a
// measurement, and a run that has not met both halves of the bound is the fleet
// still watching rather than the fleet deciding.
func CorroboratedPower(power InstancePower, state InstanceState, run ObservationRun, retracted bool, now time.Time) InstancePower {
	if power != InstancePowerStopped || state.TearingDown() || powerBound(retracted).Confirmed(run, now) {
		return power
	}
	return InstancePowerUnknown
}

// powerBound is the corroboration this instance's power premise must meet: the
// ordinary one, or the retracted one for an instance whose stopped recovery the
// drain's own re-read has already sent back to Running.
func powerBound(retracted bool) CorroborationPolicy {
	policy := PowerCorroboration
	if retracted {
		policy.Window *= PowerRetractedFactor
	}
	return policy
}
