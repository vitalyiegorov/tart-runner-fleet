package simulation_test

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// simRefusedVersion is what a refused guest's log names. The world's image carries it.
const simRefusedVersion = "2.336.0"

// ADR 0094. At about 08:05Z on 2026-11-05 GitHub began refusing actions/runner
// 2.336.0. Every x86 runner on two nodes still registered, logged
//
//	An error occurred: Runner version v2.336.0 is deprecated and cannot receive messages.
//
// and idled for the fifteen-minute assignment deadline. The fleet then spawned the
// next one, so each node looked busy, bound no job, and never said why.
func refusedTrace(cfg worldConfig, ticks, refusedFor int, arrivals ...simEvent) simTrace {
	events := []simEvent{{Tick: 1, Kind: eventRunnerRefused, Count: refusedFor}}
	return simTrace{Seed: 20_261_105, Ticks: ticks, Config: cfg.Name, Events: append(events, arrivals...)}
}

func linuxBacklog(jobs int) []simEvent {
	arrivals := make([]simEvent, 0, jobs)
	for index := range jobs {
		arrivals = append(arrivals, simEvent{Tick: 1 + index, Kind: eventArrive, Repo: "a/repo", Profile: "medium", Event: "push"})
	}
	return arrivals
}

func (w *world) platformOfJob(requestID int64) domain.Platform {
	job := w.jobByRequest(requestID)
	if job == nil {
		return ""
	}
	return w.engine.Bindings[job.binding].Profile.Platform
}

// assertImageBound is what a bound on the image means across a queue. Up to the fourth
// exit the node launches as ever, which on this host is a first batch and a refill.
// Once the image is held only retries launch, and those are at most two an hour.
func (w *world) assertImageBound(t *testing.T) {
	t.Helper()
	total, retries, held := 0, 0, false
	for _, observation := range w.observations {
		for _, backoff := range observation.EmptyExitBackoffs {
			held = held || backoff.Demand == (domain.DemandKey{})
		}
		if !observation.Applied {
			continue
		}
		for _, operation := range observation.Plan.Operations {
			if operation.Kind == scheduler.OperationSpawn {
				total++
				if held {
					retries++
				}
			}
		}
	}
	if !held {
		t.Fatal("the image was never held")
	}
	if total > 9 || retries > 3 {
		t.Fatalf("a refused image launched %d guests in two hours, %d of them while held: nothing bounds the image", total, retries)
	}
}

// TestARefusedImageStopsLaunching is the red replay. Over two hours of a refusal a
// node with no notion of it re-fills every slot it has each time the assignment
// deadline frees one; a node that counts never-assigned exits holds its launches and
// retries one guest at a time, and its guests' logs name the reason.
func TestARefusedImageStopsLaunching(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 240, 100, linuxBacklog(8)...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	w.assertImageBound(t)
	final := w.observations[len(w.observations)-1]
	if final.Queued != 8 {
		t.Fatalf("holding launches must not hide the backlog: queued=%d", final.Queued)
	}
	refusal, refused := final.RunnerRefusals[domain.PlatformLinux]
	if !refused || refusal.Version != simRefusedVersion || refusal.Instance == "" {
		t.Fatalf("the reading must name the version and the guest that said so: %+v", final.RunnerRefusals)
	}
	for _, job := range w.jobs {
		if job.status != jobQueued {
			t.Fatalf("holding launches must not touch a job at GitHub: %d is %s", job.requestID, job.status)
		}
	}
}

// TestARefusalThatLiftsReleasesTheBacklog: a refusal can end by itself, because
// GitHub's enforcement began as four-hour brownouts (ADR 0041). A retry is what finds
// out, and one started job clears the hold for everything behind it.
func TestARefusalThatLiftsReleasesTheBacklog(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 300, 9, linuxBacklog(8)...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a brownout violated a property: %s", findings[0])
	}
	for _, job := range w.jobs {
		if job.status != jobDone {
			t.Fatalf("job %d never ran after the refusal lifted: %s", job.requestID, job.status)
		}
	}
	if last := w.observations[len(w.observations)-1]; len(last.RunnerRefusals) != 0 {
		t.Fatalf("a started job must clear the reading: %+v", last.RunnerRefusals)
	}
}

// TestOneAssignmentReleasesTheQueueAtOnce: the end to end form of what the unit tests
// state. A retry takes a job once the refusal lifts, the broker names its runner, and
// the hold is gone on the next tick, with no half hour to wait out.
func TestOneAssignmentReleasesTheQueueAtOnce(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 240, 9, linuxBacklog(8)...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a brownout violated a property: %s", findings[0])
	}
	var lastHeld time.Time
	for _, observation := range w.observations {
		for _, backoff := range observation.EmptyExitBackoffs {
			if backoff.Demand == (domain.DemandKey{}) {
				lastHeld = observation.Now
			}
		}
	}
	if lastHeld.IsZero() {
		t.Fatal("the image was never held, so this trace proved nothing")
	}
	var firstStart time.Time
	for _, job := range w.jobs {
		if firstStart.IsZero() || job.startedAt.Before(firstStart) {
			firstStart = job.startedAt
		}
	}
	if lastHeld.After(firstStart.Add(4 * simTick)) {
		t.Fatalf("the first job started at %s and the image was still held at %s", firstStart, lastHeld)
	}
}

// TestARefusedLinuxImageDoesNotHoldMacOSWork: a hold is about one image.
func TestARefusedLinuxImageDoesNotHoldMacOSWork(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 180, 100, append(linuxBacklog(4),
		simEvent{Tick: 60, Kind: eventArrive, Repo: "b/repo", Profile: "maestro", Event: "push"})...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	if mac := w.jobs[len(w.jobs)-1]; mac.status != jobDone {
		t.Fatalf("a macOS job waited on a refused Linux image: %s", mac.status)
	}
}

// TestAJobCannotForgeARefusal: the guest's log is writable by the user that runs
// jobs, so a pull request from a fork could otherwise append the refusal line and
// stop a node's launches. Two jobs doing it must change nothing, because no plan reads
// that log at all.
func TestAJobCannotForgeARefusal(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, simTrace{Seed: 20_261_105, Ticks: 90, Config: cfg.Name, Events: []simEvent{
		{Tick: 1, Kind: eventForgedRefusal},
		{Tick: 1, Kind: eventArrive, Repo: "a/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 20, Kind: eventForgedRefusal},
		{Tick: 20, Kind: eventArrive, Repo: "a/repo", Profile: "medium", Event: "pull_request"},
		{Tick: 40, Kind: eventArrive, Repo: "b/repo", Profile: "medium", Event: "push"},
	}})
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("forged refusals violated a property: %s", findings[0])
	}
	for _, observation := range w.observations {
		if len(observation.EmptyExitBackoffs) != 0 {
			t.Fatalf("t%03d: a job's own log line became a hold: %+v", observation.Tick, observation.EmptyExitBackoffs)
		}
	}
	if next := w.jobs[2]; next.status != jobDone {
		t.Fatalf("a later job was held by a forged refusal: %s", next.status)
	}
}

// TestARefusedImageIsBoundedAcrossManyDemandsWithoutReadingAGuest: on a Tart node
// nothing can be read, and a log must not be needed anyway. Exits alone bound the
// image: a first batch, then a retry at a time, across every job in the queue.
func TestARefusedImageIsBoundedAcrossManyDemandsWithoutReadingAGuest(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 240, 100, linuxBacklog(12)...))
	defer w.close()
	w.refusalUnreadable = true
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	w.assertImageBound(t)
	if final := w.observations[len(w.observations)-1]; final.Queued != 12 {
		t.Fatalf("holding launches must not hide the backlog: queued=%d", final.Queued)
	}
}

// TestALogSayingRefusedOnEveryGuestChangesNothing: every guest's log carries the line
// while every job runs normally, which is what jobs writing it would look like with
// their broker events lost. Every plan must be byte-identical to a world with no line.
func TestALogSayingRefusedOnEveryGuestChangesNothing(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	plans := func(forge bool) []string {
		w := newWorld(t, cfg, simTrace{Seed: 20_261_105, Ticks: 120, Config: cfg.Name, Events: linuxBacklog(8)})
		defer w.close()
		w.forgeEveryLog = forge
		if findings := w.run(); len(findings) > 0 {
			t.Fatalf("violated a property: %s", findings[0])
		}
		ids := make([]string, 0, len(w.observations))
		for _, observation := range w.observations {
			ids = append(ids, observation.Plan.ID)
		}
		return ids
	}
	clean, forged := plans(false), plans(true)
	for index := range clean {
		if clean[index] != forged[index] {
			t.Fatalf("tick %d: a log line changed the plan", index+1)
		}
	}
}

// TestASwapStartsTheNewGenerationClean: the operator's remedy works on the first tick,
// whatever the old image cost, and the whole queue runs.
func TestASwapStartsTheNewGenerationClean(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	const swap = 46
	w := newWorld(t, cfg, refusedTrace(cfg, 120, 100, append(linuxBacklog(8), simEvent{Tick: swap, Kind: eventImageSwap})...))
	defer w.close()
	w.engine.Images = simImages("original")
	w.refusalUnreadable = true
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("the swap violated a property: %s", findings[0])
	}
	held := false
	for _, backoff := range w.observations[swap-2].EmptyExitBackoffs {
		held = held || backoff.Demand == (domain.DemandKey{})
	}
	if !held {
		t.Fatalf("the trace must swap during an image hold, or it proves nothing: %+v", w.observations[swap-2].EmptyExitBackoffs)
	}
	for _, job := range w.jobs {
		if job.status != jobDone {
			t.Fatalf("job %d waited on the old generation: %s", job.requestID, job.status)
		}
	}
}
