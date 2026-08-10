package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// wedgedDrain is the 2026-08-10 incident (issue #233) as the CLI receives it.
func wedgedDrain() adminapi.Stalled {
	return adminapi.Stalled{Operation: "event-drain-trf-macos-6x12-f458a747883b9a0d", Kind: "deregister",
		Code: "stop", Instance: "trf-macos-6x12-f458a747883b9a0d", Attempts: 67, RetryingSeconds: 4939,
		DrainState: "deregistering", HeldSeconds: 4939}
}

func stalledStatus(rows []adminapi.Stalled, check *adminapi.Check) adminapi.StatusEnvelope {
	status := healthyStatus()
	status.Data.Stalled = rows
	status.Data.ProgressCheck = check
	return status
}

// TestStalledTableNamesEverythingTheOperatorHadToReadFromSQLite is the whole
// remedy for defect 4: the instance, the step, the attempt count, and the
// elapsed time, on one line, without SSH.
func TestStalledTableNamesEverythingTheOperatorHadToReadFromSQLite(t *testing.T) {
	var buffer bytes.Buffer
	renderStalled(&buffer, []adminapi.Stalled{wedgedDrain()})
	out := buffer.String()
	for _, want := range []string{"INSTANCE", "OPERATION", "STEP", "ATTEMPTS", "RETRYING", "DRAIN STATE", "HELD",
		"trf-macos-6x12-f458a747883b9a0d", "event-drain-trf-macos-6x12-f458a747883b9a0d", "stop", "67",
		"1h22m19s", "deregistering"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stalled table missing %q:\n%s", want, out)
		}
	}
}

// TestStalledTableRendersAParkedInstanceWithoutAnOperation proves an instance
// whose drain has already dead-lettered still gets a row. It is the row that
// matters most and the one with the least to say.
func TestStalledTableRendersAParkedInstanceWithoutAnOperation(t *testing.T) {
	var buffer bytes.Buffer
	renderStalled(&buffer, []adminapi.Stalled{{Instance: "trf-macos-6x12-f458a747883b9a0d",
		DrainState: "deregistering", HeldSeconds: 10800}})
	out := buffer.String()
	if !strings.Contains(out, "trf-macos-6x12-f458a747883b9a0d") || !strings.Contains(out, "3h0m0s") {
		t.Fatalf("parked instance row missing:\n%s", out)
	}
	if strings.Count(out, "-") == 0 {
		t.Fatalf("an absent operation must render as a dash, not as an empty column:\n%s", out)
	}
	if got := orDash(""); got != "-" {
		t.Fatalf("orDash(\"\") = %q", got)
	}
}

func TestStatusReportsAStalledDrainAsDegraded(t *testing.T) {
	reason := "operation event-drain-trf-macos-6x12-f458a747883b9a0d (deregister) has failed 67 times at stop"
	status := stalledStatus([]adminapi.Stalled{wedgedDrain()}, &adminapi.Check{Reasons: []string{reason}})
	var buffer bytes.Buffer
	renderStatus(&buffer, status)
	out := buffer.String()
	for _, want := range []string{"TART RUNNER FLEET — DEGRADED", "progress: " + reason, "\nSTALLED\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
}

func TestStatusOmitsTheStalledSectionWhenEverythingProgresses(t *testing.T) {
	var buffer bytes.Buffer
	renderStatus(&buffer, healthyStatus())
	if out := buffer.String(); strings.Contains(out, "STALLED") || strings.Contains(out, "progress:") {
		t.Fatalf("a healthy fleet rendered a stalled section:\n%s", out)
	}
}

// TestDoctorFailsOnADrainThatIsNotProgressing is the check an operator runs
// first. On 2026-08-10 it reported `queue_incident,queue_slo_breached` — the
// symptom — plus PASS occupancy and PASS reservation, and named nothing.
func TestDoctorFailsOnADrainThatIsNotProgressing(t *testing.T) {
	const reason = "operation event-drain-trf-macos-6x12-f458a747883b9a0d (deregister) has failed 67 times at stop over 1h22m19s, holding instance trf-macos-6x12-f458a747883b9a0d"
	assertDoctorNames(t, stalledStatus([]adminapi.Stalled{wedgedDrain()},
		&adminapi.Check{Reasons: []string{reason}}), "drain progress", reason)
}

// TestDoctorPassesWhenTheDaemonNeverMeasuredProgress is the handoff half: an
// older daemon omits the check entirely and must not start failing every host
// that has not been upgraded yet.
func TestDoctorPassesWhenTheDaemonNeverMeasuredProgress(t *testing.T) {
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		status := healthyStatus()
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	if got := executeWith(context.Background(), []string{"doctor"}, &stdout, &stderr, deps); got != exitSuccess {
		t.Fatalf("code=%d, want exitSuccess; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS   drain progress") {
		t.Fatalf("doctor did not pass an unmeasured progress check:\n%s", stdout.String())
	}
}
