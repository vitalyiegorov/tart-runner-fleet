package simulation_test

import (
	"testing"

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

// linuxSpawns counts the guests launched, and signalled reports that the image was named.
func (w *world) linuxSpawns() int {
	total := 0
	for _, observation := range w.observations {
		if !observation.Applied {
			continue
		}
		for _, operation := range observation.Plan.Operations {
			if operation.Kind == scheduler.OperationSpawn && w.platformOfJob(operation.Demand.JobID) == domain.PlatformLinux {
				total++
			}
		}
	}
	return total
}

func (w *world) signalled(name string) bool {
	for _, observation := range w.observations {
		for _, signal := range observation.ImageSignals {
			if signal.Name == name {
				return true
			}
		}
	}
	return false
}

// TestARefusedImageRetriesAtThePerDemandRateAndIsNamed is the accepted trade-off in
// numbers. Nothing bounds a refused image as such, because exits cannot tell it from
// dispatch elsewhere: it launches at the per-demand retry rate, and that is the replay's
// count. What changed is that the outage has a name by the end of the first deadline.
func TestARefusedImageRetriesAtThePerDemandRateAndIsNamed(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 240, 100, linuxBacklog(12)...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	launched := w.linuxSpawns()
	t.Logf("a refused image launched %d guests in two simulated hours across 12 jobs", launched)
	if launched > 28 {
		t.Fatalf("a refused image launched %d guests in two hours: past even the per-demand rate", launched)
	}
	for _, name := range []string{domain.SignalImageNotReceivingJobs, domain.SignalRunnerVersionRefused} {
		if !w.signalled(name) {
			t.Fatalf("the outage was never named %s", name)
		}
	}
	final := w.observations[len(w.observations)-1]
	if final.Queued != 12 {
		t.Fatalf("the backlog must stay visible: queued=%d", final.Queued)
	}
	for _, job := range w.jobs {
		if job.status != jobQueued {
			t.Fatalf("nothing may touch a job at GitHub: %d is %s", job.requestID, job.status)
		}
	}
}

// On a Tart node nothing can be read, and the generation is named all the same.
func TestAnUnreadableImageIsStillNamedByItsExits(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 120, 100, linuxBacklog(12)...))
	defer w.close()
	w.refusalUnreadable = true
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	if !w.signalled(domain.SignalImageNotReceivingJobs) || w.signalled(domain.SignalRunnerVersionRefused) {
		t.Fatalf("named %v and %v", w.signalled(domain.SignalImageNotReceivingJobs), w.signalled(domain.SignalRunnerVersionRefused))
	}
}

// A healthy node says nothing, however busy.
func TestAHealthyNodeRaisesNoImageSignal(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, simTrace{Seed: 20_261_105, Ticks: 120, Config: cfg.Name, Events: linuxBacklog(12)})
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("violated a property: %s", findings[0])
	}
	for _, observation := range w.observations {
		if len(observation.ImageSignals) != 0 || len(observation.EmptyExitBackoffs) != 0 {
			t.Fatalf("t%03d: %+v %+v", observation.Tick, observation.ImageSignals, observation.EmptyExitBackoffs)
		}
	}
}

// TestARefusalThatLiftsReleasesTheBacklog: a refusal can end by itself, because
// GitHub's enforcement began as four-hour brownouts (ADR 0041). A retry is what finds
// out, and every job runs.
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
