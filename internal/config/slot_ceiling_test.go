package config

import (
	"strings"
	"testing"
)

// wideLinuxNode is issue #393's node-b after its re-budget: 24 threads and 60 GiB
// of which the fleet is promised 20 CPU / 44 GiB, serving the 2x4 and 4x8
// profiles and no macOS. Its envelope holds ten 2x4 guests (20/2 CPU, 45056/4096
// MiB rounds down to eleven), so ten is the most slots it may declare.
func wideLinuxNode(slots int) Config {
	cfg := Default()
	cfg.MacOS = MacOS{}
	cfg.Linux.MaxInstances = slots
	cfg.Linux.Capacity = Resources{CPU: 20, MemoryMiB: 45056}
	cfg.HostBudget = Resources{CPU: 20, MemoryMiB: 45056}
	cfg.Linux.Profiles = []Profile{
		{ID: "linux-2x4", Label: "trf-linux-arm64-2x4", Resources: Resources{CPU: 2, MemoryMiB: 4096}, DiskGiB: 50},
		{ID: "linux-4x8", Label: "trf-linux-arm64-4x8", Resources: Resources{CPU: 4, MemoryMiB: 8192}, DiskGiB: 50},
	}
	return cfg
}

func TestValidateAcceptsASlotCountTheEnvelopeCanFill(t *testing.T) {
	for _, slots := range []int{1, 4, 5, 10} {
		if err := wideLinuxNode(slots).Validate(); err != nil {
			t.Errorf("Validate() with %d slots on a ten-guest envelope = %v", slots, err)
		}
	}
}

// TestValidateRefusesSlotsTheEnvelopeCannotFill is the resource-derived half of
// the ceiling. A slot no guest can ever occupy is capacity the node advertises
// and cannot serve, so the file is refused and the error names both figures.
func TestValidateRefusesSlotsTheEnvelopeCannotFill(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"cpu binds": {func(c *Config) { c.Linux.MaxInstances = 11 },
			"11 slots exceed the 10 the envelope can fill"},
		"memory binds": {func(c *Config) {
			c.Linux.MaxInstances = 6
			c.HostBudget = Resources{CPU: 20, MemoryMiB: 20480}
		}, "6 slots exceed the 5 the envelope can fill"},
		"the budget narrows the linux cap": {func(c *Config) {
			c.Linux.MaxInstances = 8
			c.HostBudget = Resources{CPU: 12, MemoryMiB: 45056}
		}, "8 slots exceed the 6 the envelope can fill"},
		"unbudgeted capacity binds alone": {func(c *Config) {
			c.Linux.MaxInstances = 11
			c.HostBudget = Resources{}
		}, "11 slots exceed the 10 the envelope can fill"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := wideLinuxNode(10)
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestValidateKeepsAFixedSafetyCeiling: however large the envelope, a node
// declares at most sixteen slots. The bound is on what one controller drives
// concurrently -- lifecycle workers, guest boots, runner registrations -- not on
// what the allocator searches, which is bounded per tick on its own.
func TestValidateKeepsAFixedSafetyCeiling(t *testing.T) {
	cfg := wideLinuxNode(16)
	cfg.Linux.Capacity = Resources{CPU: 64, MemoryMiB: 262144}
	cfg.HostBudget = Resources{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with 16 slots = %v", err)
	}
	cfg.Linux.MaxInstances = 17
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "between 1 and 16") {
		t.Fatalf("Validate() with 17 slots = %v, want the safety ceiling", err)
	}
}

// TestValidateKeepsEveryFourSlotConfiguration: every production file was written
// under the old 1..4 rule, and some declare more slots than their vectors fill
// (the Mac Studio: four slots, an 8-CPU envelope, 4-CPU maestro guests). A
// release must not refuse the configuration its node is running, because the
// updater validates the existing config with the candidate binary and would
// never adopt it.
func TestValidateKeepsEveryFourSlotConfiguration(t *testing.T) {
	cfg := Default()
	cfg.Linux.Profiles = nil
	cfg.Linux.BaseVM = ""
	cfg.Linux.MaxInstances = 4
	cfg.Linux.Capacity = Resources{CPU: 8, MemoryMiB: 16384}
	cfg.HostBudget = Resources{CPU: 8, MemoryMiB: 16384}
	cfg.MacOS.Builder.Resources = Resources{CPU: 6, MemoryMiB: 12288}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on the Studio's four-slot shape = %v", err)
	}
	cfg.Linux.MaxInstances = 5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "5 slots exceed the 0 the envelope can fill") {
		t.Fatalf("Validate() with a fifth unbacked slot = %v", err)
	}
}

// TestOnlyLinuxGuestsBackExtraSlots: a macOS guest occupies a slot of the shared
// envelope (ADR 0012), but Apple's Virtualization framework runs at most two of
// them and `maxActive` bounds each profile, so they never need a fifth slot. A
// slot above four is backed by the node's smallest Linux guest alone; a node
// that boots none keeps the four it could always declare.
func TestOnlyLinuxGuestsBackExtraSlots(t *testing.T) {
	cfg := wideLinuxNode(4)
	cfg.MacOS = Default().MacOS
	cfg.MacOS.Maestro.Resources = Resources{CPU: 1, MemoryMiB: 1024}
	cfg.Linux.Profiles = cfg.Linux.Profiles[1:]
	cfg.Linux.MaxInstances = 6
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "6 slots exceed the 5 the envelope can fill") {
		t.Fatalf("Validate() = %v, want the 4x8 guest to bound the slots", err)
	}
}
