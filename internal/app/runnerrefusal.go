package app

import (
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// RunnerRefusals is what this node's guests have said about its images, per
// platform, because a node boots one image per platform (ADR 0094).
//
// Its memory is deliberately in-process, like GuestLivenessTracker's. A restart
// forgets the verdict and the next guest's teardown learns it again, which is
// fail-open: forgetting costs a few launches, and a rebuilt image only reaches this
// daemon through a restart, so a stale verdict can never outlive the image it names.
type RunnerRefusals struct {
	// Now must be the clock the scheduler plans on, for the reason
	// PowerCorroborator's must. Nil is the wall clock.
	Now   func() time.Time
	mu    sync.Mutex
	state map[domain.Platform]domain.RunnerRefusal
}

func (r *RunnerRefusals) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

// Record notes a guest's log naming its version as refused. It is safe on a nil
// receiver, which is a node that reads no guest.
func (r *RunnerRefusals) Record(platform domain.Platform, version, instance string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[domain.Platform]domain.RunnerRefusal{}
	}
	r.state[platform] = domain.RunnerRefusal{Version: version, Instance: instance, At: r.now()}
}

// Observe returns the standing verdicts, first clearing any that a job has
// disproved. Only a job that STARTED after the verdict disproves it: one that was
// already running when the cutoff arrived says nothing about the image today.
func (r *RunnerRefusals) Observe(instances []domain.Instance) map[domain.Platform]domain.RunnerRefusal {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, instance := range instances {
		if refusal, refused := r.state[instance.Platform]; refused &&
			instance.State == domain.InstanceRunning && instance.RunningSince.After(refusal.At) {
			delete(r.state, instance.Platform)
		}
	}
	standing := make(map[domain.Platform]domain.RunnerRefusal, len(r.state))
	for platform, refusal := range r.state {
		standing[platform] = refusal
	}
	return standing
}

// probeInFlight reports, per platform, a live guest that has not started a job.
// Under a verdict that guest is already the probe, whichever batch it came from: it
// will either start a job or be read at its own teardown.
func probeInFlight(instances []domain.Instance) map[domain.Platform]bool {
	probing := map[domain.Platform]bool{}
	for _, instance := range instances {
		if instance.IncarnatesDemand() && instance.State != domain.InstanceRunning {
			probing[instance.Platform] = true
		}
	}
	return probing
}

// withholdRefused adds a refused image's queue to the withheld set, leaving out at
// most one demand per platform once the probe interval has passed and no guest is
// already answering the question. It decides only what is planned: the rows, the
// job at GitHub and the queue SLO are untouched.
func withholdRefused(queued []domain.Demand, refused map[domain.Platform]domain.RunnerRefusal,
	probing map[domain.Platform]bool, withheld map[domain.DemandKey]bool, now time.Time,
) {
	for _, demand := range queued {
		refusal, isRefused := refused[demand.Platform]
		if !isRefused || withheld[demand.Key] {
			continue
		}
		if !now.Before(refusal.RetryAt()) && !probing[demand.Platform] {
			probing[demand.Platform] = true
			continue
		}
		withheld[demand.Key] = true
	}
}
