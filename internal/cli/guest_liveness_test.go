package cli

import (
	"testing"

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
	assertDoctorPasses(t, healthyStatus(), "guest liveness")
}

// A guest inside both bounds is the fleet watching, not the fleet reporting. It
// must never turn `fleet doctor` red, or the check is unreadable within a week.
func TestDoctorPassesWhileAGuestIsMerelyBeingWatched(t *testing.T) {
	status := healthyStatus()
	status.Data.GuestSilences = []adminapi.GuestSilence{{Instance: "trf-small-9a1c", Profile: "linux-small",
		Refusals: 2, SilenceSeconds: 40, RequiredRefusals: 5, WindowSeconds: 90}}
	status.Data.GuestLivenessCheck = &adminapi.Check{OK: true, Reasons: []string{}}
	assertDoctorPasses(t, status, "guest liveness")
}
