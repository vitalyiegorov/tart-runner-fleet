package daemon

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// TestGuestConsoleAsksTheOperatingSystem pins the predicate the doctor check
// rides on. `linux.baseVm` is a Tart base VM name that the schema keeps in
// every node's file, so a Linux observe-only node or a podman container node
// carries one it will never clone (ADR 0034) — and the Linux smoke test runs
// exactly such a config. A predicate on the field alone would fail every Linux
// node for a console no such node can lose.
func TestGuestConsoleAsksTheOperatingSystem(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"

	apple := guestConsole("darwin", cfg)
	if !apple.BootsLinuxGuests {
		t.Fatalf("an Apple node with a Linux base VM boots Linux guests: %#v", apple)
	}

	linux := guestConsole("linux", cfg)
	if linux.BootsLinuxGuests || linux.SerialLogConfigured {
		t.Fatalf("a Linux node with an uncloned base VM name boots no Tart guests: %#v", linux)
	}
}

// TestGuestConsoleSeesTheSerialSink is the half the check exists for: the same
// Apple node with the sink configured passes, and without it fails.
func TestGuestConsoleSeesTheSerialSink(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"

	if got := guestConsole("darwin", cfg); got.SerialLogConfigured {
		t.Fatalf("no directory configured means no sink: %#v", got)
	}
	cfg.Linux.SerialLogDirectory = "/var/db/tart-runner-fleet/serial"
	if got := guestConsole("darwin", cfg); !got.SerialLogConfigured {
		t.Fatalf("a configured directory is a sink: %#v", got)
	}
}
