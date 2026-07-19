package config

import (
	"os"
	"testing"
)

func TestFourLaneMaestroExperimentConfigIsValid(t *testing.T) {
	file, err := os.Open("../../config/experiments/four-lane-maestro.json") // #nosec G304 -- fixed repository fixture.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	cfg, err := Decode(file)
	if err != nil {
		t.Fatalf("experiment config must decode and validate: %v", err)
	}

	if cfg.MacOS.Maestro.MemoryMiB != 9216 {
		t.Fatalf("maestro guests must be 9216 MiB, got %d", cfg.MacOS.Maestro.MemoryMiB)
	}
	if cfg.MacOS.Maestro.CPU != 4 {
		t.Fatalf("maestro guests must be 4 vCPU, got %d", cfg.MacOS.Maestro.CPU)
	}
	if cfg.MacOS.Maestro.MaxActive != 2 {
		t.Fatalf("exactly 2 macOS Maestro VMs (Apple kernel quota), got %d", cfg.MacOS.Maestro.MaxActive)
	}
	if cfg.MacOS.AdmissionPolicy != MacOSAdmissionExclusive {
		t.Fatalf("experiment runs macos-exclusive admission, got %q", cfg.MacOS.AdmissionPolicy)
	}
	if cfg.MacOS.RootDiskOptions != "sync=none" {
		t.Fatalf("disposable clones run sync=none, got %q", cfg.MacOS.RootDiskOptions)
	}
	if cfg.Guards.MaxSwapUsedMiB != 3072 {
		t.Fatalf("experiment swap guard is 3072 MiB, got %d", cfg.Guards.MaxSwapUsedMiB)
	}
}
