package telemetry

import "testing"

// The guest-console check exists because three incidents in eight days — #236,
// #258, and #259 — each ended at *trigger unidentified* for the same reason:
// the dead guest's own console across the death did not exist. The serial sink
// is implemented end to end and defaults to off, so a node that forgets it is
// silent by construction, and silence is what every one of those investigations
// paid for.

func TestGuestConsoleFailsWhenLinuxGuestsHaveNowhereToWriteTheirConsole(t *testing.T) {
	health := runnerVersionHealth(t)
	health.SetGuestConsole(GuestConsoleMetric{BootsLinuxGuests: true})
	result := health.GuestConsole()
	if result.OK {
		t.Fatalf("a node booting Linux guests with no serial log directory must fail, got %+v", result)
	}
	if len(result.Reasons) == 0 {
		t.Fatalf("the failure must say why, got %+v", result)
	}
}

func TestGuestConsolePassesOnceTheSerialSinkIsConfigured(t *testing.T) {
	health := runnerVersionHealth(t)
	health.SetGuestConsole(GuestConsoleMetric{BootsLinuxGuests: true, SerialLogConfigured: true})
	if result := health.GuestConsole(); !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("want a passing check with no reasons, got %+v", result)
	}
}

func TestGuestConsolePassesOnANodeThatBootsNoLinuxGuests(t *testing.T) {
	health := runnerVersionHealth(t)
	health.SetGuestConsole(GuestConsoleMetric{})
	if result := health.GuestConsole(); !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("a node with no Linux guests has no console to lose, got %+v", result)
	}
}

// TestGuestConsolePassesUnpublishedForAnOlderDaemon is the handoff half every
// check in this package carries: a daemon that predates the field never looked,
// and a CLI must not render that as a measured failure of a live node.
func TestGuestConsolePassesUnpublishedForAnOlderDaemon(t *testing.T) {
	health := runnerVersionHealth(t)
	if result := health.GuestConsole(); !result.OK {
		t.Fatalf("an unpublished metric is an unspecified check, not a failure, got %+v", result)
	}
}
