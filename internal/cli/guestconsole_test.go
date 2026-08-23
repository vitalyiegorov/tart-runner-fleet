package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// The guest-console check is issue #259's answer to the same silence #236 and
// #258 paid for: a Tart node whose Linux guests write their consoles nowhere
// cannot produce evidence when a guest kernel dies, so the lapse itself must be
// visible to `fleet doctor` rather than remembered by an operator.

func TestDoctorFailsWhenALinuxGuestHasNowhereToWriteItsConsole(t *testing.T) {
	const reason = "this node boots Linux guests but writes no guest console: set linuxSerialLogDirectory"
	status := healthyStatus()
	status.Data.GuestConsole = &adminapi.GuestConsole{BootsLinuxGuests: true}
	status.Data.GuestConsoleCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "guest console", "writes no guest console")
}

func TestDoctorPassesOnceTheSerialSinkIsConfigured(t *testing.T) {
	status := healthyStatus()
	status.Data.GuestConsole = &adminapi.GuestConsole{BootsLinuxGuests: true, SerialLogConfigured: true}
	status.Data.GuestConsoleCheck = &adminapi.Check{OK: true}
	assertDoctorPasses(t, status, "guest console")
	// The posture must be readable even on a pass: a bare "ok" would leave the
	// question — can a dead guest leave evidence? — as invisible as it was.
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout bytes.Buffer
	if got := executeWith(context.Background(), []string{"doctor"}, &stdout, &bytes.Buffer{}, deps); got != exitSuccess {
		t.Fatalf("code=%d; stdout=%q", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), "serial console captured") {
		t.Fatalf("doctor did not state the posture on a pass:\n%s", stdout.String())
	}
}

func TestDoctorPassesOnANodeThatBootsNoLinuxGuests(t *testing.T) {
	status := healthyStatus()
	status.Data.GuestConsole = &adminapi.GuestConsole{}
	status.Data.GuestConsoleCheck = &adminapi.Check{OK: true}
	assertDoctorPasses(t, status, "guest console")
}

// TestDoctorPassesUnpublishedForAnOlderDaemon keeps the handoff rule: an older
// daemon that never published the field must render as unspecified, not as a
// measured failure.
func TestDoctorPassesUnpublishedForAnOlderDaemon(t *testing.T) {
	status := healthyStatus()
	assertDoctorPasses(t, status, "guest console")
}
