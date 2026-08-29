package simulation_test

import (
	"fmt"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ---------------------------------------------------------------------------
// (r) No demand takes the vector an older cross-platform head is waiting for.
// ---------------------------------------------------------------------------

// crossPlatformInversionChecker fails when a tick admits a demand that could
// take the WHOLE vector an older, aged demand of the OTHER platform is waiting
// for, and that could not have been admitted beside that demand instead.
//
// It is the issue #225 oracle. On 2026-08-09 a 6 CPU / 12288 MiB vector freed on
// the mac mini; a macOS App Store release had waited 2h01m for exactly that
// vector and a Linux pull-request job 1h20m, and the scheduler gave it to the
// Linux job five seconds later. Every property this harness had was silent.
// Property (l) is confined to one platform in as many words — a tier ranks
// inside a lane — and property (b) counts a pass-over as starvation only after
// StarvationN ticks of it, which a two-hour wait on a live builder never trips
// because the head is not feasible on the ticks it is passed over.
//
// The rule it checks is ADR 0045's, not a new one: a head withholds ORDER, never
// a vector. So the two exemptions are ADR 0045's own, and they are what keeps
// this from degenerating into "macOS first".
//
// Work that fits `free - head` sits BESIDE the head and cannot delay it by a
// tick, so it is never an inversion however young it is. That exemption is why
// the property does not sterilize the host, and it is the same distinction
// `jumpsTheReservedHead` draws for the within-Linux case.
//
// The head must be AGED. Within a pass it is the fairness age that turns waiting
// into precedence (ADR 0004's global FIFO lane), and a rule that gave a
// one-minute-old macOS demand the same standing would be a platform preference
// wearing an ordering rule's clothes.
//
// A head its own REPOSITORY CAP holds out is excluded, because ADR 0038 decides
// that such a head lends its vector: freeing the vector cannot admit it, so
// nothing that takes the vector delays it. This is the same term
// `chargeReservedHead` applies, recomputed here from the plan's own instances so
// ADR 0031's independence rule holds.
//
// The head is deliberately NOT required to be feasible this tick. That is the
// whole difference between this oracle and every other one here, and it is what
// the incident was: the head could not spawn because a live builder held the
// profile's only active slot, and the vector it was waiting for went to a
// younger job of the other platform instead of to it when the builder finished.
// An oracle that judged only feasible heads would have watched that happen for
// two hours and reported nothing.
//
// Only the HEAD of each platform's aged band is judged, and that restriction is
// load-bearing rather than a convenience. ADR 0045's rule is about the demand a
// reservation would be held for -- `priorityOrder`'s first -- and nothing behind
// it is entitled to a vector at all. A first draft of this oracle judged every
// aged waiting demand and failed on seed 1 tick 99 of the tiered world, where a
// 5m30s `maestro` sitting THIRD in the queue was called wronged by an 8m0s
// `large` that had not touched the actual head's vector. The queue was doing
// exactly what a queue does.
func crossPlatformInversionChecker(cfg worldConfig) checker {
	return func(w *world, observation tickObservation) []finding {
		if w.tearingDown(observation) {
			return nil
		}
		admitted := admittedDemands(observation)
		if len(admitted) == 0 {
			return nil
		}
		free, ok := tickFreeEnvelope(cfg, observation, true)
		if !ok {
			return nil
		}
		capped := reposAtCap(cfg, observation)
		var findings []finding
		for _, head := range agedHeadsByPlatform(cfg, observation, admitted) {
			if capped[head.Key.Repo] {
				continue
			}
			headVector := cfg.Scheduler.Profiles[head.Profile].Resources
			residual, residualExists := free.Sub(headVector)
			for _, chosen := range admitted {
				chosenVector := cfg.Scheduler.Profiles[chosen.Profile].Resources
				if chosen.Platform == head.Platform || !chosenVector.CanFit(headVector) ||
					(residualExists && residual.CanFit(chosenVector)) ||
					!outranksInAgedFIFO(cfg, observation, head, chosen) {
					continue
				}
				findings = append(findings, finding{Kind: findingCrossPlatformInversion, Tick: observation.Tick,
					Detail: fmt.Sprintf("%s %s (%s old) left waiting while %s %s (%s old) took its %d cpu / %d MiB vector\n%s",
						head.Platform, head.Key, observation.Now.Sub(head.CreatedAt),
						chosen.Platform, chosen.Key, observation.Now.Sub(chosen.CreatedAt),
						headVector.CPU, headVector.MemoryMB, w.dumpPlan(observation))})
			}
		}
		return findings
	}
}

// agedHeadsByPlatform names the one waiting demand per platform that
// `priorityOrder` would rank first: the aged band comes before every other lane
// (ADR 0004), and inside it the order is effective tier then age (ADR 0037 §4).
// A platform with no aged waiting demand contributes no head, which is the tick
// where this oracle has nothing to say.
func agedHeadsByPlatform(cfg worldConfig, observation tickObservation, admitted []domain.Demand) []domain.Demand {
	heads := map[domain.Platform]domain.Demand{}
	for _, demand := range observation.Demands {
		if !aged(cfg, observation.Now, demand) || containsDemand(admitted, demand) {
			continue
		}
		current, known := heads[demand.Platform]
		if !known || outranksInAgedFIFO(cfg, observation, demand, current) {
			heads[demand.Platform] = demand
		}
	}
	ordered := make([]domain.Demand, 0, len(heads))
	for _, platform := range []domain.Platform{domain.PlatformLinux, domain.PlatformMacOS} {
		if head, known := heads[platform]; known {
			ordered = append(ordered, head)
		}
	}
	return ordered
}

// outranksInAgedFIFO reports whether the head is entitled to go first under the
// order ADR 0037 §4 gives the aged band: effective tier, then age. The chosen
// demand outranks it whenever it is aged and ranks ahead on that order, which is
// the ordinary case of a queue doing its job.
func outranksInAgedFIFO(cfg worldConfig, observation tickObservation, head, chosen domain.Demand) bool {
	if !aged(cfg, observation.Now, chosen) {
		return true
	}
	headTier := effectiveTierOf(cfg, observation.Now, head)
	chosenTier := effectiveTierOf(cfg, observation.Now, chosen)
	if headTier != chosenTier {
		return headTier > chosenTier
	}
	return head.CreatedAt.Before(chosen.CreatedAt)
}

// reposAtCap names the repositories ADR 0012's cap already holds out, which is
// the term ADR 0038 turns into "such a head lends its vector".
func reposAtCap(cfg worldConfig, observation tickObservation) map[string]bool {
	counts := map[string]int{}
	for _, instance := range observation.Instances {
		if instance.ConsumesHostResources() && !instance.State.TearingDown() &&
			instance.State != domain.InstanceOnlineIdle {
			counts[instance.Repo]++
		}
	}
	capped := map[string]bool{}
	for repo, count := range counts {
		if count >= repoCap(cfg, repo) {
			capped[repo] = true
		}
	}
	return capped
}

// tickFreeEnvelope is the admission envelope one tick had, recomputed from the
// plan's own instance list and the host reading rather than from anything the
// scheduler published (ADR 0031). It is shared with feasibleDemands so the two
// oracles can never disagree about how much room a tick had.
func tickFreeEnvelope(cfg worldConfig, observation tickObservation, aged bool) (domain.Resources, bool) {
	live := domain.Resources{}
	for _, instance := range observation.Instances {
		if instance.ConsumesHostResources() {
			live = live.Add(instance.Resources)
		}
	}
	headroom, ok := cfg.hostCeiling().Sub(live)
	if !ok {
		return domain.Resources{}, false
	}
	if aged {
		// An aged demand escapes the advisory CPU-idle clamp and is judged on the
		// physical residual alone (`linuxFreeAged`). This oracle only ever judges
		// aged heads, so clamping here would compare the scheduler's decision
		// against an envelope the scheduler did not use -- and did: seed 3 tick 55
		// of the mac-mini world read a 6-core head as unable to fit five available
		// cores, when the tick had judged it against ten.
		return headroom, true
	}
	return domain.Resources{CPU: min(headroom.CPU, observation.Host.Available.CPU),
		MemoryMB: min(headroom.MemoryMB, observation.Host.Available.MemoryMB), Slots: headroom.Slots}, true
}
