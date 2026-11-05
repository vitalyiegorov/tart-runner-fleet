package app

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// RunnerRefusals is what this node's guests have said about its images, per
// platform, because a node boots one image per platform (ADR 0094).
//
// A verdict takes two distinct guests, each of which never took a job
// (RunnerRefusalReader). The log is writable by whatever ran in the guest, so one
// reading is only ever a witness, and a job from a fork must not be able to stop a
// node.
//
// Its memory is deliberately in-process, like GuestLivenessTracker's. A restart
// forgets the verdict and later teardowns learn it again, which is fail-open:
// forgetting costs a few launches, and a rebuilt image only reaches this daemon
// through a restart, so a stale verdict can never outlive the image it names.
type RunnerRefusals struct {
	// Now must be the clock the scheduler plans on, for the reason
	// PowerCorroborator's must. Nil is the wall clock.
	Now       func() time.Time
	mu        sync.Mutex
	state     map[domain.Platform]domain.RunnerRefusal
	witnesses map[domain.Platform]domain.RunnerRefusal
}

func (r *RunnerRefusals) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

// Record notes a guest's log naming its version as refused. Under a standing verdict
// one guest renews it, because only one probe ever runs at a time; otherwise it takes
// a second guest within the window. It is safe on a nil receiver, which is a node that
// reads no guest.
func (r *RunnerRefusals) Record(platform domain.Platform, version, instance string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[domain.Platform]domain.RunnerRefusal{}
		r.witnesses = map[domain.Platform]domain.RunnerRefusal{}
	}
	refusal := domain.RunnerRefusal{Version: version, Instance: instance, At: r.now()}
	if _, standing := r.state[platform]; standing {
		r.state[platform] = refusal
		return
	}
	first, seen := r.witnesses[platform]
	fresh := seen && first.Version == version && refusal.At.Sub(first.At) <= domain.RunnerRefusalWitnessWindow
	switch {
	case fresh && first.Instance == instance:
	case fresh:
		r.state[platform] = refusal
		delete(r.witnesses, platform)
	default:
		r.witnesses[platform] = refusal
	}
}

// Observe returns the standing verdicts, first clearing any that a job has
// disproved, and any lone witness with it. Only a job that STARTED after the reading
// disproves it: one already running when the cutoff arrived says nothing about the
// image today.
func (r *RunnerRefusals) Observe(instances []domain.Instance) map[domain.Platform]domain.RunnerRefusal {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, instance := range instances {
		if instance.State != domain.InstanceRunning {
			continue
		}
		if refusal, refused := r.state[instance.Platform]; refused && instance.RunningSince.After(refusal.At) {
			delete(r.state, instance.Platform)
		}
		if witness, seen := r.witnesses[instance.Platform]; seen && instance.RunningSince.After(witness.At) {
			delete(r.witnesses, instance.Platform)
		}
	}
	standing := make(map[domain.Platform]domain.RunnerRefusal, len(r.state))
	for platform, refusal := range r.state {
		standing[platform] = refusal
	}
	return standing
}

// RunnerJobEvidence answers whether the broker ever gave an instance a job.
type RunnerJobEvidence interface {
	InstanceTookJob(context.Context, string) (bool, error)
}

// RunnerRefusalReader takes a guest's word only if no job ever ran in that guest. A
// real refusal never receives one, and a guest that did may say anything at all, so
// it is not asked. A question it cannot answer is no evidence: this can only ever
// cost a late verdict, never a held node.
type RunnerRefusalReader struct {
	Evidence RunnerJobEvidence
	// Read returns the guest's refusal line. An error is no such line.
	Read     func(context.Context, string) ([]byte, error)
	Refusals *RunnerRefusals
}

// ReadRunnerRefusal is lifecycle.RunnerRefusalReader.
func (r RunnerRefusalReader) ReadRunnerRefusal(ctx context.Context, instance operations.Instance) {
	took, err := r.Evidence.InstanceTookJob(ctx, instance.ID)
	if err != nil || took {
		return
	}
	output, err := r.Read(ctx, instance.ID)
	if err != nil {
		return
	}
	if version, refused := domain.ParseRunnerRefusal(output); refused {
		r.Refusals.Record(instance.Platform, version, instance.ID)
	}
}

// probeInFlight reports, per platform, a live guest that could still answer the
// question: one that has not started a job and is not being torn down. A guest in
// cleanup can never answer, and a stuck cleanup or a parked dead letter can stay that
// way indefinitely, so it does not stand in for a probe.
func probeInFlight(instances []domain.Instance) map[domain.Platform]bool {
	probing := map[domain.Platform]bool{}
	for _, instance := range instances {
		if instance.IncarnatesDemand() && instance.State != domain.InstanceRunning && !instance.State.TearingDown() {
			probing[instance.Platform] = true
		}
	}
	return probing
}

// withholdRefused adds a refused image's queue to the withheld set, and returns the
// demands that may probe it: all of them once the interval has passed and no guest is
// already answering, none otherwise. Which one probes is planWithProbes' decision. It
// decides only what is planned: the rows, the job at GitHub and the queue SLO are
// untouched.
func withholdRefused(queued []domain.Demand, refused map[domain.Platform]domain.RunnerRefusal,
	probing map[domain.Platform]bool, withheld map[domain.DemandKey]bool, now time.Time,
) []domain.Demand {
	var probes []domain.Demand
	for _, demand := range queued {
		refusal, isRefused := refused[demand.Platform]
		if !isRefused || withheld[demand.Key] {
			continue
		}
		withheld[demand.Key] = true
		if !now.Before(refusal.RetryAt()) && !probing[demand.Platform] {
			probes = append(probes, demand)
		}
	}
	return probes
}

// planWithProbes plans the tick, then offers each candidate to the scheduler, oldest
// first, keeping the first plan per platform that actually spawns it. Feasibility has
// one author, so an unschedulable head cannot hold the allowance, and nothing is spent
// until a spawn is selected: the next tick asks again, and once the spawn exists
// probeInFlight sees it. Candidates are absent from the input, so a kept plan starts
// one guest from that image however many fit.
func planWithProbes(input scheduler.Input, candidates []domain.Demand) (scheduler.Plan, []domain.Demand) {
	plan := scheduler.PlanTick(input)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })
	probed := map[domain.Platform]bool{}
	for _, candidate := range candidates {
		if probed[candidate.Platform] {
			continue
		}
		trial := input
		trial.Demands = domain.Fresh(append(append([]domain.Demand(nil), input.Demands.Value...), candidate), input.Demands.ObservedAt)
		next := scheduler.PlanTick(trial)
		for _, operation := range next.Operations {
			if operation.Kind == scheduler.OperationSpawn && operation.Demand == candidate.Key {
				plan, input, probed[candidate.Platform] = next, trial, true
				break
			}
		}
	}
	return plan, input.Demands.Value
}
