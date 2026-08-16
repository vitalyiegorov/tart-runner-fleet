package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// The check an operator runs first must name the condition. Through all eight of
// issue #236's runner deaths `fleet doctor` had nothing to say: the daemon never
// asked a guest anything, so there was nothing to publish and nothing to check.
func TestDoctorFailsOnAGuestThatStoppedAnswering(t *testing.T) {
	const reason = "instance trf-xl-0aacdbcc6653bd8a of profile xl stopped answering its guest probe 2m0s ago"
	status := healthyStatus()
	status.Data.GuestSilences = []adminapi.GuestSilence{{Instance: "trf-xl-0aacdbcc6653bd8a", Profile: "xl",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMiB: 12_288, Refusals: 5, SilenceSeconds: 120,
		RequiredRefusals: 5, WindowSeconds: 90, Unresponsive: true, RunID: 31_939_037_119, JobID: 93_540_000_001}}
	status.Data.GuestLivenessCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "guest liveness", reason)
}

// The handoff half. An older daemon probed nothing, and `fleet doctor` must not
// start failing against every host that has not been upgraded yet.
func TestDoctorPassesWhenTheDaemonNeverProbedAGuest(t *testing.T) {
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		status := healthyStatus()
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	if got := executeWith(context.Background(), []string{"doctor"}, &stdout, &stderr, deps); got != exitSuccess {
		t.Fatalf("code=%d, want exitSuccess; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS   guest liveness") || !strings.Contains(stdout.String(), "RESULT PASS") {
		t.Fatalf("doctor did not pass an unmeasured guest-liveness check:\n%s", stdout.String())
	}
}

// A guest inside both bounds is the fleet watching, not the fleet reporting. It
// must never turn `fleet doctor` red, or the check is unreadable within a week.
func TestDoctorPassesWhileAGuestIsMerelyBeingWatched(t *testing.T) {
	status := healthyStatus()
	status.Data.GuestSilences = []adminapi.GuestSilence{{Instance: "trf-small-9a1c", Profile: "linux-small",
		Refusals: 2, SilenceSeconds: 40, RequiredRefusals: 5, WindowSeconds: 90}}
	status.Data.GuestLivenessCheck = &adminapi.Check{OK: true, Reasons: []string{}}
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	if got := executeWith(context.Background(), []string{"doctor"}, &stdout, &stderr, deps); got != exitSuccess {
		t.Fatalf("code=%d, want exitSuccess; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS   guest liveness") {
		t.Fatalf("a watched guest must not fail the check:\n%s", stdout.String())
	}
}
