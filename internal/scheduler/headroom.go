package scheduler

import "github.com/vitalyiegorov/tart-runner-fleet/internal/domain"

// Headroom reports how many more instances of one profile the node's current
// residual admission envelope can start, once every demand already waiting on
// this node has been charged its own vector (issue #388, ADR 0072).
//
// It is the answer a scale set needs before it tells GitHub how much capacity
// it has. A node has one envelope -- slots, CPU, memory, the host budget, and the
// physical bound -- and many scale sets drawing on it, one per scope and
// profile. Each set's configured capacity is truthful for its scope alone, so
// GitHub summed them and placed work on a node whose envelope could not start
// it. This is the envelope `planLinux` and `appendMacSpawns` admit against, read
// through the same functions, so an advertisement and an admission can never be
// judged against different capacity:
//
//   - The hard-bounds envelope (`linuxFreeAged`, and `agedLinuxEnvelope` with its
//     four-slot production ceiling for Linux). A job placed now is admitted once
//     it ages if it is not admitted sooner, and the advisory CPU-idle clamp that
//     young work waits on is not a reason to send work elsewhere.
//   - Every queued demand not already incarnated (`queuedVectors`) is charged
//     first, wherever it is queued: assigned work that has not launched will take
//     the residual before anything newly placed can. That also covers the aged
//     head's reservation, which is one of those demands, without charging it
//     twice. A dimension a demand cannot be charged in full is exhausted rather
//     than refused, so one oversized waiting head shrinks the residual instead
//     of hiding it.
//   - A macOS profile is also bounded by its own MaxActive less the instances
//     and queued demands it already has, exactly as `appendMacSpawns` bounds it.
//
// The platform lanes (exclusive macOS, handoffs, drain-and-switch) are not
// modelled: they decide which of two feasible things starts first, not whether
// the envelope can hold one more, and modelling them could only ever advertise
// less. The bounds that are modelled are the ones that decided issue #388.
//
// ok is false when the answer cannot be computed: an unusable observation or a
// profile this node does not define. Unknown is never zero; the caller falls
// back to its configured capacity.
func Headroom(in Input, profile domain.ProfileID) (int, bool) {
	definition, known := in.Config.Profiles[profile]
	if !known || definition.Resources == (domain.Resources{}) ||
		!in.Demands.Usable() || !in.Instances.Usable() || !in.Host.Usable() {
		return 0, false
	}
	free := linuxFreeAged(in)
	if definition.Platform == domain.PlatformLinux {
		free = agedLinuxEnvelope(in)
	}
	waiting := normalizedDemands(in)
	for _, vector := range queuedVectors(in.Config, in.Instances.Value, waiting) {
		free = exhaust(free, vector)
	}
	count := 0
	for free.CanFit(definition.Resources) {
		free, _ = free.Sub(definition.Resources)
		count++
	}
	if definition.Platform == domain.PlatformMacOS {
		count = min(count, max(0, macProfileLimit(definition)-cohortSize(in, profile, waiting)))
	}
	return count, true
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
