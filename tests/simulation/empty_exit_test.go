package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// Issue #493. On 2026-11-05 the Mac mini spent every maestro slot it could free
// on one job: `validate (scene)` of pony-labirinth run 41979555666, queued in
// GitHub's view and corroborated by REST on every snapshot. Each VM cloned and
// booted, its runner took nothing, its guest powered itself off, and a tick later
// the fleet's stopped-power recovery reclaimed it and spawned the next.

// emptyExitWorld is the incident's shape on a one-slot node: the wasted slot is
// the only slot, so whatever the loop consumes nothing else can have.
func emptyExitWorld() worldConfig {
	cfg := defaultWorld()
	maestro := cfg.Scheduler.Profiles["maestro"]
	maestro.MaxActive = 1
	cfg.Scheduler.Profiles["maestro"] = maestro
	return cfg
}

func emptyExitTrace(cfg worldConfig, ticks, exits int, extra ...simEvent) simTrace {
	events := []simEvent{
		{Tick: 1, Kind: eventEmptyExit, Count: exits},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"},
	}
	return simTrace{Seed: 20_261_105, Ticks: ticks, Config: cfg.Name, Events: append(events, extra...)}
}

// admissionsOf counts the applied spawns one workflow job received.
func (w *world) admissionsOf(job *simJob) int {
	count := 0
	for _, observation := range w.observations {
		if !observation.Applied {
			continue
		}
		for _, key := range observation.spawns() {
			if key.JobID == job.requestID {
				count++
			}
		}
	}
	return count
}

// TestARunnerlessJobDoesNotBootVMsInALoop is the red replay of the incident. The
// job never gets a runner in this window, which is what production measured: two
// hours of it. A loop with no backoff boots one VM per lifecycle, about one every
// twelve ticks; a bounded backoff boots far fewer and leaves the queue visible.
func TestARunnerlessJobDoesNotBootVMsInALoop(t *testing.T) {
	t.Parallel()
	cfg := emptyExitWorld()
	w := newWorld(t, cfg, emptyExitTrace(cfg, 240, 1_000))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the empty-exit loop violated a property: %s", findings[0])
	}
	job := w.jobs[0]
	if job.exitedEmpty < domain.EmptyExitBackoff.Tolerated {
		t.Fatalf("the harness never reproduced the loop: %d empty exits", job.exitedEmpty)
	}
	if boots := w.admissionsOf(job); boots > 7 {
		t.Fatalf("one runnerless job booted %d VMs in two hours (%d exited empty): the loop has no bound", boots, job.exitedEmpty)
	}
	final := w.observations[len(w.observations)-1]
	if final.Queued != 1 {
		t.Fatalf("backing off must not hide the job from the queue: queued=%d", final.Queued)
	}
	if job.status != jobQueued {
		t.Fatalf("backing off must not touch the job at GitHub: status=%s", job.status)
	}
}

// TestAHeldJobConsumesNoSlotBetweenAttempts: between attempts the job holds
// nothing at all, so the slot is free for other demand instead of being spent on a
// VM that is going to power itself off. A younger job on the same scale set runs.
func TestAHeldJobConsumesNoSlotBetweenAttempts(t *testing.T) {
	t.Parallel()
	cfg := emptyExitWorld()
	w := newWorld(t, cfg, emptyExitTrace(cfg, 240, 1_000,
		simEvent{Tick: 30, Kind: eventArrive, Repo: "a/repo", Profile: "maestro", Event: "push"}))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the empty-exit loop violated a property: %s", findings[0])
	}
	if len(w.jobs) != 2 {
		t.Fatalf("expected the two jobs of the trace, got %d", len(w.jobs))
	}
	if other := w.jobs[1]; other.status != jobDone {
		t.Fatalf("a younger job never got the slot: status=%s", other.status)
	}
	occupied := 0
	for _, observation := range w.observations {
		for _, instance := range observation.Instances {
			if instance.Demand.JobID == w.jobs[0].requestID && instance.ConsumesHostResources() {
				occupied++
				break
			}
		}
	}
	if occupied > len(w.observations)/2 {
		t.Fatalf("the runnerless job held a slot for %d of %d ticks: it keeps consuming capacity between attempts",
			occupied, len(w.observations))
	}
}

// TestAJobThatBouncesTwiceIsNotBackedOff: an empty exit is ordinary. Another
// runner taking the job first, a host restart, or a guest that crashed once all
// look like this, and none of them may delay the job.
func TestAJobThatBouncesTwiceIsNotBackedOff(t *testing.T) {
	t.Parallel()
	cfg := emptyExitWorld()
	exits := domain.EmptyExitBackoff.Tolerated
	w := newWorld(t, cfg, emptyExitTrace(cfg, 80, exits))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("an ordinary empty exit violated a property: %s", findings[0])
	}
	job := w.jobs[0]
	if job.status != jobDone || job.exitedEmpty != exits {
		t.Fatalf("job should have run after %d empty exits: status=%s exited=%d", exits, job.status, job.exitedEmpty)
	}
	for _, observation := range w.observations {
		if len(observation.EmptyExitBackoffs) > 0 {
			t.Fatalf("t%03d backed off after an ordinary empty exit: %+v", observation.Tick, observation.EmptyExitBackoffs)
		}
	}
}

// TestABackedOffJobThatIsLaterAssignedStillRuns: the backoff is a delay, never a
// verdict. Once GitHub hands the job to a runner it runs to completion, and it does
// so without anything having been cancelled, expired, or rewritten.
func TestABackedOffJobThatIsLaterAssignedStillRuns(t *testing.T) {
	t.Parallel()
	cfg := emptyExitWorld()
	w := newWorld(t, cfg, emptyExitTrace(cfg, 240, 5))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the empty-exit loop violated a property: %s", findings[0])
	}
	job := w.jobs[0]
	if job.status != jobDone || job.exitedEmpty != 5 {
		t.Fatalf("a job GitHub eventually assigned must run: status=%s exited=%d", job.status, job.exitedEmpty)
	}
	held := 0
	for _, observation := range w.observations {
		held += len(observation.EmptyExitBackoffs)
	}
	if held == 0 {
		t.Fatal("the job was never held, so this trace proved nothing about a held job")
	}
	if last := w.observations[len(w.observations)-1]; len(last.EmptyExitBackoffs) != 0 {
		t.Fatalf("a finished job must leave no hold behind: %+v", last.EmptyExitBackoffs)
	}
}

// simImages is one generation of every image a simulated node boots.
func simImages(generation string) map[domain.Platform]string {
	return map[domain.Platform]string{domain.PlatformLinux: "linux@" + generation, domain.PlatformMacOS: "macos@" + generation}
}

// TestARebuiltImageIsNotChargedForTheOldOnesExits: the operator's remedy must work
// at once. Exits on the image that was replaced say nothing about its replacement,
// so the first launch after a swap may not wait out a hold the old image earned.
func TestARebuiltImageIsNotChargedForTheOldOnesExits(t *testing.T) {
	t.Parallel()
	cfg := emptyExitWorld()
	const swap = 32
	w := newWorld(t, cfg, emptyExitTrace(cfg, 90, 1_000, simEvent{Tick: swap, Kind: eventImageSwap}))
	defer w.close()
	w.engine.Images = simImages("original")
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the swap violated a property: %s", findings[0])
	}
	before := w.observations[swap-2]
	if len(before.EmptyExitBackoffs) != 1 || !before.EmptyExitBackoffs[0].RetryAt.After(simEpoch.Add((swap+4)*simTick)) {
		t.Fatalf("the trace must swap well inside a hold, or it proves nothing: %+v", before.EmptyExitBackoffs)
	}
	job := w.jobs[0]
	for _, observation := range w.observations[swap-1 : swap+3] {
		for _, key := range observation.spawns() {
			if key.JobID == job.requestID {
				return
			}
		}
	}
	t.Fatalf("the rebuilt image waited out the old image's hold: job status %s", job.status)
}
