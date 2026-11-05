package app

import (
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// RunnerRefusals is the latest reading of what this node's guests' logs say about their
// runner, per platform (ADR 0094). It is display only. A job runs as the user that owns
// that log and can write the line, so no plan reads this: it can name the reason for a
// hold the fleet already has from evidence a job cannot produce, and nothing more.
//
// Its memory is in-process: a restart forgets a reading, which costs an operator one
// sentence until the next teardown.
type RunnerRefusals struct {
	// Now dates a reading. Nil is the wall clock.
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

// Observe returns the standing readings, first dropping any that a job has since
// contradicted by starting on that platform.
func (r *RunnerRefusals) Observe(instances []domain.Instance) map[domain.Platform]domain.RunnerRefusal {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, instance := range instances {
		if refusal, read := r.state[instance.Platform]; read && instance.State == domain.InstanceRunning && instance.RunningSince.After(refusal.At) {
			delete(r.state, instance.Platform)
		}
	}
	standing := make(map[domain.Platform]domain.RunnerRefusal, len(r.state))
	for platform, refusal := range r.state {
		standing[platform] = refusal
	}
	return standing
}
