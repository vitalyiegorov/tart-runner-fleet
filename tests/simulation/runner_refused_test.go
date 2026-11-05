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

func (w *world) spawnsOn(platform domain.Platform) int {
	count := 0
	for _, observation := range w.observations {
		if !observation.Applied {
			continue
		}
		for _, operation := range observation.Plan.Operations {
			if operation.Kind == scheduler.OperationSpawn && w.platformOfJob(operation.Demand.JobID) == platform {
				count++
			}
		}
	}
	return count
}

func (w *world) platformOfJob(requestID int64) domain.Platform {
	job := w.jobByRequest(requestID)
	if job == nil {
		return ""
	}
	return w.engine.Bindings[job.binding].Profile.Platform
}

// TestARefusedImageStopsLaunching is the red replay. Over two hours of a refusal a
// node with no notion of it re-fills every slot it has each time the assignment
// deadline frees one; a node that read the refusal from its guest holds its launches
// and spends one probe per half hour finding out whether the image still works.
func TestARefusedImageStopsLaunching(t *testing.T) {
	t.Parallel()
	cfg := defaultWorld()
	w := newWorld(t, cfg, refusedTrace(cfg, 240, 100, linuxBacklog(8)...))
	defer w.close()
	if findings := w.run(); len(findings) > 0 {
		t.Fatalf("a refused image violated a property: %s", findings[0])
	}
	spawns := w.spawnsOn(domain.PlatformLinux)
	first := 0
	for _, instance := range w.observations[len(w.observations)/8].Instances {
		if instance.Platform == domain.PlatformLinux {
			first++
		}
	}
	if first == 0 {
		t.Fatal("the harness never launched the first batch")
	}
	if spawns > first+3 {
		t.Fatalf("a refused image launched %d guests in two hours after a first batch of %d: nothing stopped it", spawns, first)
	}
	final := w.observations[len(w.observations)-1]
	if final.Queued != 8 {
		t.Fatalf("holding launches must not hide the backlog: queued=%d", final.Queued)
	}
	refusal, refused := final.RunnerRefusals[domain.PlatformLinux]
	if !refused || refusal.Version != simRefusedVersion || refusal.Instance == "" {
		t.Fatalf("the image verdict must name the version and the guest that said so: %+v", final.RunnerRefusals)
	}
	for _, job := range w.jobs {
		if job.status != jobQueued {
			t.Fatalf("holding launches must not touch a job at GitHub: %d is %s", job.requestID, job.status)
		}
	}
}

// TestARefusalThatLiftsReleasesTheBacklog: a refusal can end by itself, because
// GitHub's enforcement began as four-hour brownouts (ADR 0041). The probe is what
// finds out, and one started job clears the verdict for everything behind it.
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
		t.Fatalf("a started job must clear the verdict: %+v", last.RunnerRefusals)
	}
}

// TestARefusedLinuxImageDoesNotHoldMacOSWork: the verdict is about one image.
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
// stop a node's launches. Two jobs doing it must change nothing, because no guest
// that took a job is ever asked.
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
		if len(observation.RunnerRefusals) != 0 {
			t.Fatalf("t%03d: a job's own log line became a verdict on the image: %+v", observation.Tick, observation.RunnerRefusals)
		}
	}
	if next := w.jobs[2]; next.status != jobDone {
		t.Fatalf("a later job was held by a forged refusal: %s", next.status)
	}
}
