package daemon

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// macOSOnlyNode is the Mac ADR 0055's retirement procedure leaves behind: it
// boots macOS guests and declares no Linux execution of any kind.
func macOSOnlyNode() config.Config {
	cfg := config.Default()
	linux := cfg.Linux
	// The envelope survives the retirement: `maxLinuxCpu` and friends are the
	// node's shared cross-platform admission bound (ADR 0012), not a Linux-only
	// one, and a macOS guest is charged against them.
	cfg.Linux = config.Linux{VMPrefix: linux.VMPrefix, MaxInstances: linux.MaxInstances, Capacity: linux.Capacity}
	return cfg
}

// TestAMacWithNoLinuxProfilesBootsNoLinuxGuests is the guest-console predicate
// after ADR 0055. Before this change the check asked whether `linux.baseVm` was
// set, so a Mac that had retired every Linux profile — and could therefore
// never boot a guest whose kernel might die — went on being told to configure a
// serial sink for guests it does not have.
// The discriminating case is a Mac PART WAY through the procedure: the
// profiles are gone but the base image name is still in the file, which is
// exactly what an operator's first edit leaves behind. A guest that cannot be
// booted cannot lose a console, and the field alone cannot tell the two apart.
func TestAMacWithNoLinuxProfilesBootsNoLinuxGuests(t *testing.T) {
	if got := guestConsole("darwin", macOSOnlyNode()); got.BootsLinuxGuests {
		t.Fatalf("a Mac with no Linux profiles boots no Linux guests: %#v", got)
	}
	retiring := macOSOnlyNode()
	retiring.Linux.BaseVM = "linux-runner-base-go"
	if got := guestConsole("darwin", retiring); got.BootsLinuxGuests {
		t.Fatalf("an unrouted base image is not a booted guest: %#v", got)
	}
}

// TestAMacThatStillRunsLinuxKeepsItsConsoleObligation is the other half: the
// relaxation must not silence the check on a node that has not retired yet,
// which is what three incidents (#236, #258, #259) bought it for.
func TestAMacThatStillRunsLinuxKeepsItsConsoleObligation(t *testing.T) {
	cfg := config.Default()
	cfg.Linux.BaseVM = "linux-runner-base-go"
	if got := guestConsole("darwin", cfg); !got.BootsLinuxGuests {
		t.Fatalf("a Mac with Linux profiles still boots Linux guests: %#v", got)
	}
}

// TestAMacOSOnlyNodeSchedulesOnlyMacOSProfiles is what makes `fleet instances`
// and `fleet queues` show no `linux-*` row: every published per-profile row is
// keyed by the scheduler's profile map, so retiring the profiles retires the
// rows. It is asserted here rather than assumed because the alternative — a
// node advertising a Linux profile it cannot boot — is a queue that fills and
// never drains.
func TestAMacOSOnlyNodeSchedulesOnlyMacOSProfiles(t *testing.T) {
	cfg := macOSOnlyNode()
	scheduled := app.BuildSchedulerConfig(cfg)
	if len(scheduled.Profiles) != 2 {
		t.Fatalf("scheduler profiles = %+v, want exactly the two macOS profiles", scheduled.Profiles)
	}
	for id, profile := range scheduled.Profiles {
		if profile.Platform != domain.PlatformMacOS {
			t.Fatalf("profile %s is on platform %s, want macOS only", id, profile.Platform)
		}
	}
	// The admission envelope must survive: it is what every macOS guest on this
	// node is charged against, and a zero here admits nothing at all.
	if scheduled.LinuxCapacity.CPU <= 0 || scheduled.LinuxCapacity.MemoryMB <= 0 || scheduled.LinuxCapacity.Slots <= 0 {
		t.Fatalf("admission envelope = %+v, want the node's shared envelope intact", scheduled.LinuxCapacity)
	}
}

// TestAMacOSOnlyNodePublishesOnlyTheImageItBoots keeps rule 4 the right way
// round at the telemetry seam. The absence of a Linux image here is proved from
// the configuration, not unread from a host, so it is published as no row — not
// as an unknown version the runner-version check would have to call a
// compliance hole on the one node that did the retirement correctly.
func TestAMacOSOnlyNodePublishesOnlyTheImageItBoots(t *testing.T) {
	images := runnerImages(macOSOnlyNode())
	if len(images) != 1 || images[0].Platform != "macOS" {
		t.Fatalf("runnerImages() = %+v, want exactly the macOS image", images)
	}
	if images[0].Version == "" && images[0].Reason == "" {
		t.Fatalf("the surviving row must still state its posture: %+v", images[0])
	}
}
