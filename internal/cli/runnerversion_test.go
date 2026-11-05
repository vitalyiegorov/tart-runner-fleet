package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func currentImages() []adminapi.RunnerImage {
	return []adminapi.RunnerImage{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.336.0", Floor: "2.329.0"},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Version: "2.336.0", Floor: "2.329.0"},
	}
}

// TestDoctorFailsOnABaseImageBelowTheFloor is the check issue #206 exists for.
// A runner GitHub will not let register produces no error anywhere else on this
// fleet: the jobs queue and the nodes idle, which is what a quiet Sunday looks
// like.
func TestDoctorFailsOnABaseImageBelowTheFloor(t *testing.T) {
	const reason = `linux base image "linux-runner-base-go" carries actions/runner 2.335.1, below the 2.336.0 floor`
	status := healthyStatus()
	status.Data.RunnerImages = []adminapi.RunnerImage{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.335.1", Floor: "2.336.0",
			BelowFloor: true, Reason: reason},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Version: "2.336.0", Floor: "2.336.0"},
	}
	status.Data.RunnerVersionCheck = &adminapi.Check{Reasons: []string{reason}}
	// The asserted fragment carries no quotes: `fleet doctor --output json` escapes
	// them, and a substring that only matches the table would be half a check.
	assertDoctorNames(t, status, "runner version", "carries actions/runner 2.335.1, below the 2.336.0 floor")
}

// TestDoctorFailsOnAnUndeclaredRunnerVersion is the second half of the same
// rule. An image nobody has vouched for is not a passing image: for two months
// both nodes were in exactly that state and nothing said so.
func TestDoctorFailsOnAnUndeclaredRunnerVersion(t *testing.T) {
	const reason = `macOS base image "macos-tartelet-base-go" declares no runner version, ` +
		`so its brownout compliance cannot be judged`
	status := healthyStatus()
	status.Data.RunnerImages = []adminapi.RunnerImage{
		{Platform: "linux", VM: "linux-runner-base-go", Version: "2.336.0", Floor: "2.329.0"},
		{Platform: "macOS", VM: "macos-tartelet-base-go", Floor: "2.329.0", BelowFloor: true, Reason: reason},
	}
	status.Data.RunnerVersionCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "runner version", "declares no runner version")
}

func TestDoctorPassesWhenEveryImageIsCurrent(t *testing.T) {
	status := healthyStatus()
	status.Data.RunnerImages = currentImages()
	status.Data.RunnerVersionCheck = &adminapi.Check{OK: true}
	assertDoctorPasses(t, status, "runner version")
}

// TestDoctorNamesTheVersionEvenWhenItPasses is the observability half of the
// acceptance criterion: the version in service must be readable without SSH-ing
// into a guest, so a passing check still prints what each image carries and what
// bar it cleared.
func TestDoctorNamesTheVersionEvenWhenItPasses(t *testing.T) {
	status := healthyStatus()
	status.Data.RunnerImages = currentImages()
	status.Data.RunnerVersionCheck = &adminapi.Check{OK: true}
	client := &fakeClient{status: status, metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "", &stdout, &stderr); code != exitSuccess {
		t.Fatalf("current images are healthy, got exit %d: %s", code, stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"runner version", "linux 2.336.0", "macOS 2.336.0", "floor 2.329.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor must name the version it passed on, missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorPassesWhenTheDaemonPredatesTheCheck is the mandatory handoff half: a
// daemon that never looked must not be rendered as a fleet that is behind, and
// must say plainly that it did not look rather than imply a measured pass.
func TestDoctorPassesWhenTheDaemonPredatesTheCheck(t *testing.T) {
	status := healthyStatus()
	status.Data.RunnerImages, status.Data.RunnerVersionCheck = nil, nil
	client := &fakeClient{status: status, metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "", &stdout, &stderr); code != exitSuccess {
		t.Fatalf("an older daemon publishes no image and that is not a fault: %s", stdout.String())
	}
	if out := stdout.String(); !strings.Contains(out, "not reported by this daemon") {
		t.Fatalf("doctor must say so plainly:\n%s", out)
	}
}

// A held demand fails its check and a signal fails nothing, and each is named in doctor
// and above the queues in status, because the queue alone looks merely busy (ADR 0094).
func TestDoctorAndStatusNameAHeldDemandAndADisplayOnlySignal(t *testing.T) {
	const held = "demand_empty_exit_backoff: o/r/1/1/7 (s/maestro): 3 instances exited without a job"
	const signal = "image_generation_not_receiving_jobs: base@2.336.0: 4 instances. This changes no launch decisions."
	status := healthyStatus()
	status.Data.EmptyExitCheck = &adminapi.Check{Reasons: []string{held}}
	assertDoctorNames(t, status, "empty exits", "demand_empty_exit_backoff")

	status = healthyStatus()
	status.Data.ImageSignals = []adminapi.ImageSignal{{Name: "image_generation_not_receiving_jobs", Image: "base@2.336.0", Detail: signal}}
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var doctor, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"doctor"}, &doctor, &stderr, deps); code != 0 {
		t.Fatalf("a display-only signal failed doctor, exit %d:\n%s", code, doctor.String())
	}
	if !strings.Contains(doctor.String(), "image_generation_not_receiving_jobs") {
		t.Fatalf("doctor must name it:\n%s", doctor.String())
	}
	status.Data.EmptyExitCheck = &adminapi.Check{Reasons: []string{held}}
	var output strings.Builder
	renderStatus(&output, status)
	for _, want := range []string{"WITHHELD", "demand_empty_exit_backoff", "IMAGE SIGNALS (display only)", "image_generation_not_receiving_jobs"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("status lacks %q:\n%s", want, output.String())
		}
	}
}
