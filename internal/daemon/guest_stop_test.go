package daemon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// knee-doctor#1352: a runner that receives a shutdown signal must be findable in
// the controller log by its name. Every guest the fleet stops gets one line with
// the cause, the rung, the memory it held and the job it was spawned for, so a
// shutdown with no such line is one the fleet did not make.
func TestEveryGuestStopNamesItsCauseAndJob(t *testing.T) {
	instance := operations.Instance{ID: "trf-macos-2x4-0a1b2c3d4e5f6a7b", Repo: "owner/pony", Profile: "macos-2x4",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}, State: operations.StateDeregistering,
		Demand:     domain.DemandKey{Repo: "owner/pony", RunID: 17, Attempt: 1, JobID: 41},
		DrainPhase: operations.DrainPhaseLingeringRunner}
	reporter, logged := silenceReporter()

	reporter.reportGuestStop(lifecycle.GuestStop{Instance: instance,
		Phase: lifecycle.DrainPhaseName(instance.DrainPhase), Rung: lifecycle.StopForced.String()})

	for _, fragment := range []string{"instance guest stopped", "instance=trf-macos-2x4-0a1b2c3d4e5f6a7b",
		"profile=macos-2x4", "repo=owner/pony", "cause=lingering_runner", "rung=forced", "state=deregistering",
		"memoryMiB=4096", "runId=17", "jobId=41"} {
		if !strings.Contains(logged.String(), fragment) {
			t.Fatalf("stop log missing %q: %q", fragment, logged.String())
		}
	}
}

// The swap gate tripped within a minute of most of the unexplained shutdowns,
// but its line said only "swap pressure". A closure now carries what the guests
// hold against what the host has, so memory overcommit is readable from the log.
func TestAdmissionGateClosureCarriesGuestAndHostMemory(t *testing.T) {
	var buffer bytes.Buffer
	ticker := engineTicker{health: reservationHealthForTest(t), reporter: newFailureReporter(&buffer, nil),
		gate: &admissionGate{}}
	tick := gateTick(gateClock, false, "swap pressure")
	tick.Host.Pressure.SwapUsedMB, tick.Host.Pressure.PhysicalMemoryMB = 3072, 24576
	tick.Instances = []domain.Instance{
		{ID: "trf-a", State: domain.InstanceRunning, Resources: domain.Resources{CPU: 4, MemoryMB: 8192, Slots: 1}},
		{ID: "trf-b", State: domain.InstanceRunning, Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}},
		{ID: "trf-c", State: domain.InstanceDeleted, Resources: domain.Resources{CPU: 2, MemoryMB: 4096, Slots: 1}},
	}

	ticker.recordMetrics(tick)

	lines := gateLines(&buffer)
	if len(lines) != 1 {
		t.Fatalf("want one closure line, got %q", lines)
	}
	for _, fragment := range []string{`reason="swap pressure"`, "guests=2", "guestMemoryMiB=12288",
		"hostMemoryMiB=24576", "availableMemoryMiB=8192", "swapUsedMiB=3072"} {
		if !strings.Contains(lines[0], fragment) {
			t.Fatalf("closure missing %q: %q", fragment, lines[0])
		}
	}
	ticker.recordMetrics(gateTick(gateClock.Add(1), true, "capacity available"))
	if opened := gateLines(&buffer)[1]; strings.Contains(opened, "guestMemoryMiB") {
		t.Fatalf("an opening is not a refusal and carries no memory: %q", opened)
	}
}
