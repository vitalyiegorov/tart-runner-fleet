package scheduler

import (
	"math"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// Claim is one scale set's request on the node's residual envelope (issue #397).
type Claim struct {
	Profile domain.ProfileID
	// Room is how many more instances the set may advertise: its configured
	// capacity less what it already holds. Its share never exceeds it, so a set
	// already at its ceiling does not absorb a share it cannot advertise.
	Room int
	// Waiting is how many jobs GitHub shows queued for the set that it has not
	// delivered here. It is the one evidence a node has that a scope is waiting
	// for capacity right now, so it is served before any share is speculative.
	Waiting int
}

// Unsized is the share of a claim whose profile the scheduler cannot size: an
// unknown profile or one with no resource vector. Unknown is never zero; the
// caller falls back to its configured capacity.
const Unsized = -1

// shareRotation is how long one set keeps its place at the front of the
// speculative pass. It is one REST refresh, so a scope with a job the rotation
// has not reached is normally seen waiting first, and served by the first pass.
const shareRotation = 30 * time.Second

// PartitionHeadroom divides the node's residual admission envelope among the
// scale sets that draw on it, so that everything the sets are offered beyond
// what they hold could start here at once (issues #388 and #397, ADR 0073).
//
// A node has one envelope -- slots, CPU, memory, the host budget, and the
// physical bound -- and one scale set per scope and profile drawing on it.
// GitHub fills each set to the capacity it last advertised and cannot move a job
// it has placed. Handing every set the whole residual offered one free slot once
// per scope: on 2026-10-19..22 a Mac with no free slot received 80 maestro jobs
// while the other Mac had room, and sat idle 22 slot-hours while its sibling's
// queue waited.
//
// The residual is the one `planLinux` and `appendMacSpawns` admit against, read
// through the same functions, so an offer and an admission are never judged
// against different capacity:
//
//   - the hard-bounds envelope (`linuxFreeAged`) with the node's true slot
//     count (ADR 0077). A job placed now is admitted once it ages if it is not
//     admitted sooner, and neither the advisory CPU-idle clamp that young work
//     waits on nor the per-tick admission pace (`admissionsPerTick`) is a reason
//     to send work elsewhere;
//   - every queued demand not already incarnated (`queuedVectors`) is charged
//     first, wherever it is queued, and that includes the aged head's
//     reservation. A dimension a demand cannot be charged in full is exhausted
//     rather than refused, so one oversized head shrinks the residual instead of
//     hiding it;
//   - a macOS profile is also bounded by its own MaxActive less the instances and
//     queued demands it already has, exactly as `appendMacSpawns` bounds it.
//
// Each unit granted to a claim is then charged exactly like one more waiting
// demand, so two profiles drawing on one budget share it too. Units are granted
// one at a time round robin, in two passes: first to the sets with work GitHub
// shows waiting for them, up to that work, then the remainder to every set up to
// its room. The round starts at a position that rotates every shareRotation, so
// no set is permanently last for a speculative share.
//
// The platform lanes (exclusive macOS, handoffs, drain-and-switch) are not
// modelled: they decide which of two feasible things starts first, not whether
// the envelope can hold one more, and modelling them could only offer less.
//
// ok is false when no answer can be computed from unusable observations.
func PartitionHeadroom(in Input, claims []Claim) ([]int, bool) {
	if !in.Demands.Usable() || !in.Instances.Usable() || !in.Host.Usable() {
		return nil, false
	}
	waiting := normalizedDemands(in)
	free := linuxFreeAged(in)
	for _, vector := range queuedVectors(in.Config, in.Instances.Value, waiting) {
		free = exhaust(free, vector)
	}
	cohort := map[domain.ProfileID]int{}
	shares := make([]int, len(claims))
	for index, claim := range claims {
		definition, known := in.Config.Profiles[claim.Profile]
		if !known || definition.Resources == (domain.Resources{}) {
			shares[index] = Unsized
			continue
		}
		if _, counted := cohort[claim.Profile]; definition.Platform == domain.PlatformMacOS && !counted {
			cohort[claim.Profile] = max(0, macProfileLimit(definition)-cohortSize(in, claim.Profile, waiting))
		}
	}
	grant := func(index int) bool {
		profile := claims[index].Profile
		definition := in.Config.Profiles[profile]
		if !free.CanFit(definition.Resources) {
			return false
		}
		if definition.Platform == domain.PlatformMacOS {
			if cohort[profile] == 0 {
				return false
			}
			cohort[profile]--
		}
		free = exhaust(free, definition.Resources)
		shares[index]++
		return true
	}
	start := 0
	if len(claims) > 0 {
		start = int(in.Now.Unix()/int64(shareRotation/time.Second)) % len(claims)
	}
	for _, limit := range []func(Claim) int{
		func(claim Claim) int { return min(claim.Room, claim.Waiting) },
		func(claim Claim) int { return claim.Room },
	} {
		for granted := true; granted; {
			granted = false
			for offset := range claims {
				index := (start + offset) % len(claims)
				if shares[index] != Unsized && shares[index] < limit(claims[index]) && grant(index) {
					granted = true
				}
			}
		}
	}
	return shares, true
}

// exhaust charges a vector against an envelope one dimension at a time, floored
// at zero. Sub refuses a vector that does not fit at all, which is the right
// question for an admission and the wrong one for "what is left once this is
// owed".
func exhaust(free, owed domain.Resources) domain.Resources {
	return domain.Resources{CPU: max(0, free.CPU-owed.CPU), MemoryMB: max(0, free.MemoryMB-owed.MemoryMB),
		Slots: max(0, free.Slots-owed.Slots)}
}

// cohortSize is how much of a macOS profile's MaxActive is already spoken for:
// its live instances, counted as `appendMacSpawns` counts them, and its queued
// demands that no live instance incarnates yet.
func cohortSize(in Input, profile domain.ProfileID, waiting []domain.Demand) int {
	size := 0
	incarnated := make(map[domain.DemandKey]bool, len(in.Instances.Value))
	for _, instance := range in.Instances.Value {
		if instance.IncarnatesDemand() {
			incarnated[instance.Demand] = true
		}
		if instance.ConsumesHostResources() && instance.Profile == profile {
			size++
		}
	}
	for _, demand := range waiting {
		if demand.Profile == profile && !incarnated[demand.Key] {
			size++
		}
	}
	return size
}

// FitsOnceLinuxDrains reports whether one instance of a Linux profile could
// start on this node once every Linux guest and queued Linux demand here has
// run to its end (issue #439, ADR 0085). It is the promise ADR 0082's standing
// offer of one makes to the job it attracts: the job waits for this node's own
// Linux work, which Linux admission and aging bound.
//
// What Linux work cannot release stays charged: every live guest of another
// platform, and every queued macOS demand no guest incarnates yet, which takes
// its vector before a later Linux job can. A macOS guest holds its vector for up
// to its occupancy budget, hours on a maestro suite, so a Linux job assigned to
// wait behind one waits for that, while GitHub never moves it to a sibling with
// room.
//
// The envelope is `freeCapacity`'s, with the same hard bounds the planner
// admits against -- the Linux-only cap, the physical total and the host budget
// -- asked of the occupancy that outlasts Linux drain. The measured residual is
// not: it is net of the very Linux guests that drain, and a host tenant's
// pressure is ADR 0084's gate, not this rule.
//
// A macOS profile, or one the scheduler cannot size, is always true: ADR 0082
// is unchanged for it.
func FitsOnceLinuxDrains(in Input, profile domain.ProfileID) bool {
	definition, known := in.Config.Profiles[profile]
	if !known || definition.Platform != domain.PlatformLinux {
		return true
	}
	outlasting := make([]domain.Instance, 0, len(in.Instances.Value))
	for _, instance := range in.Instances.Value {
		if instance.Platform != domain.PlatformLinux {
			outlasting = append(outlasting, instance)
		}
	}
	drained := in
	drained.Host.Value.Available = domain.Resources{CPU: math.MaxInt, MemoryMB: math.MaxInt, Slots: math.MaxInt}
	free := freeCapacity(drained, outlasting, true)
	_, macos := partition(normalizedDemands(in))
	for _, vector := range queuedVectors(in.Config, in.Instances.Value, macos) {
		free = exhaust(free, vector)
	}
	return free.CanFit(definition.Resources)
}
