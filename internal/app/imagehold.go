package app

import (
	"sort"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// bindingQueue is one binding's queue as this tick read it.
type bindingQueue struct {
	binding Binding
	queued  []domain.Demand
	err     error
}

// judgeImages sums the exits since each generation's last assignment across its queue
// (ADR 0094). Only queued demands count: an exit on a job another runner took, or one
// that was cancelled, is the ordinary race and is simply absent from the sum. Nothing
// here reads an instance's state, so no guest, stranded or otherwise, can suppress
// anything: the hold is dated by the last exit or launch, and it ends by time alone.
func judgeImages(queues []bindingQueue, exits map[domain.EmptyExitKey]domain.EmptyExits,
	images map[domain.Platform]string,
) map[domain.Platform]EmptyExitBackoff {
	type tally struct {
		exits, demands int
		last, launched time.Time
	}
	tallies := map[domain.Platform]*tally{}
	for _, queue := range queues {
		for _, demand := range queue.queued {
			spent := exits[domain.EmptyExitKey{Demand: demand.Key, Image: images[demand.Platform]}]
			if spent.Unassigned == 0 {
				continue
			}
			current := tallies[demand.Platform]
			if current == nil {
				current = &tally{}
				tallies[demand.Platform] = current
			}
			current.exits += spent.Unassigned
			current.demands++
			current.last = later(current.last, spent.Last)
			current.launched = later(current.launched, spent.Launched)
		}
	}
	holds := map[domain.Platform]EmptyExitBackoff{}
	for platform, current := range tallies {
		spent := domain.EmptyExits{Count: current.exits, Last: current.last}
		if current.demands < domain.ImageHoldMinDemands || !domain.ImageEmptyExitBackoff.Exceeded(spent) {
			continue
		}
		dated := spent
		dated.Last = later(current.last, current.launched)
		holds[platform] = EmptyExitBackoff{Image: images[platform], Platform: platform, Demands: current.demands,
			Exits: current.exits, LastAt: current.last, RetryAt: domain.ImageEmptyExitBackoff.RetryAt(dated)}
	}
	return holds
}

func later(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

// withholdHeldImages adds a held image's queue to the withheld set, and returns the
// demands that may retry it once its time has come. It decides only what is planned:
// the rows, the job at GitHub and the queue SLO are untouched.
func withholdHeldImages(queued []domain.Demand, holds map[domain.Platform]EmptyExitBackoff,
	withheld map[domain.DemandKey]bool, now time.Time,
) []domain.Demand {
	var retries []domain.Demand
	for _, demand := range queued {
		hold, held := holds[demand.Platform]
		if !held || withheld[demand.Key] {
			continue
		}
		withheld[demand.Key] = true
		if !now.Before(hold.RetryAt) {
			retries = append(retries, demand)
		}
	}
	return retries
}

// planWithRetries plans the tick, then offers each retry candidate to the scheduler,
// keeping the first plan per platform that actually spawns it. Feasibility has one
// author, so a job that cannot fit never blocks one that can. One launch per platform
// is what keeps a retry from being a whole batch across many demands, and the launch
// itself spends the interval, durably and whatever becomes of the guest, because the
// next tick dates the hold from it. The job that has cost the least goes first, then the
// oldest, so one that keeps failing cannot take every retry from one a runner would take.
func planWithRetries(input scheduler.Input, candidates []domain.Demand, exits map[domain.EmptyExitKey]domain.EmptyExits,
	images map[domain.Platform]string,
) (scheduler.Plan, []domain.Demand) {
	plan := scheduler.PlanTick(input)
	spent := func(demand domain.Demand) int {
		return exits[domain.EmptyExitKey{Demand: demand.Key, Image: images[demand.Platform]}].Count
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if left, right := spent(candidates[i]), spent(candidates[j]); left != right {
			return left < right
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	retried := map[domain.Platform]bool{}
	for _, candidate := range candidates {
		if retried[candidate.Platform] {
			continue
		}
		trial := input
		trial.Demands = domain.Fresh(append(append([]domain.Demand(nil), input.Demands.Value...), candidate), input.Demands.ObservedAt)
		next := scheduler.PlanTick(trial)
		for _, operation := range next.Operations {
			if operation.Kind == scheduler.OperationSpawn && operation.Demand == candidate.Key {
				plan, input, retried[candidate.Platform] = next, trial, true
				break
			}
		}
	}
	return plan, input.Demands.Value
}
