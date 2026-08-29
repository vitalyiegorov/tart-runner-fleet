package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The mini's shape at the time of the incident, widened enough that a Linux
// profile as large as the macOS builder exists: the whole bug is a candidate
// that could take the head's vector whole, and testConfig has no Linux profile
// that large.
func agedHeadConfig() Config {
	config := testConfig()
	config.MixedPlatformAdmission = true
	config.LinuxCapacity = domain.Resources{CPU: 16, MemoryMB: 32_768, Slots: 8}
	config.Profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "legacy",
		Resources: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 1}}
	return config
}

func agedHeadInput(demands []domain.Demand, instances []domain.Instance) Input {
	return Input{
		Now:       testNow,
		Config:    agedHeadConfig(),
		Demands:   domain.Fresh(demands, testNow),
		Instances: domain.Fresh(instances, testNow),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 16, MemoryMB: 32_768, Slots: 8}}, testNow),
	}
}

func agedHeadDemand(repo string, jobID int64, age time.Duration, profile domain.ProfileID) domain.Demand {
	built := demand(repo, jobID, age, profile)
	built.Profile = profile
	built.Route = agedHeadConfig().Profiles[profile].Route
	built.Platform = agedHeadConfig().Profiles[profile].Platform
	return built
}

// The live builder holds the profile's only active slot, so the queued builder
// head cannot spawn no matter how much of the envelope is free. That is the
// shape of the 2026-08-09 incident: a macOS release waiting on a macOS instance
// to finish, with room to spare beside it.
func liveBuilder() domain.Instance {
	return domain.Instance{ID: "builder-live", Repo: "c/repo", Platform: domain.PlatformMacOS,
		Profile: "builder", Route: "macos-builder",
		Resources: domain.Resources{CPU: 8, MemoryMB: 12_288, Slots: 1}, State: domain.InstanceRunning}
}

// Production, 2026-08-09 (issue #225): a 6 CPU / 12288 MiB vector freed on the
// mac mini. A macOS App Store release had waited 2h01m for exactly that vector;
// a Linux pull-request job had waited 1h20m for exactly that vector. Five
// seconds later the scheduler admitted the Linux job, and the older macOS demand
// kept waiting — it got the host only when an operator cancelled the Linux job
// by hand.
//
// Identical vector, identical host, identical envelope. The only difference was
// platform, and the younger demand won by forty minutes: the remainder planner
// behind an infeasible macOS head handed planLinux the WHOLE envelope, and
// reservations are authored only over Linux demands, so nothing checked the
// macOS head at all.
func TestAgedMacHeadIsNotJumpedByYoungerLinuxWantingItsVector(t *testing.T) {
	macHead := agedHeadDemand("a/repo", 1, 2*time.Hour+time.Minute, "builder")
	linuxYounger := agedHeadDemand("b/repo", 2, 80*time.Minute, "xl")

	plan := PlanTick(agedHeadInput([]domain.Demand{macHead, linuxYounger}, []domain.Instance{liveBuilder()}))

	for _, key := range spawnedKeys(plan) {
		if key == linuxYounger.Key {
			t.Fatal("a younger Linux demand took the vector an aged macOS head was waiting for")
		}
	}
}

// The bound is ordering only. Work that fits BESIDE the aged head — that cannot
// delay it by a tick — is admitted exactly as before, because ADR 0045 charges
// the head's vector against nobody. Free beside the live builder is 8 CPU /
// 20480 MiB; the head wants 8 / 12288 of it, leaving room a `medium` fits in.
func TestWorkThatFitsBesideAnAgedMacHeadIsStillAdmitted(t *testing.T) {
	macHead := agedHeadDemand("a/repo", 1, 2*time.Hour, "builder")
	linuxSmall := agedHeadDemand("b/repo", 2, time.Minute, "medium")

	plan := PlanTick(agedHeadInput([]domain.Demand{macHead, linuxSmall}, []domain.Instance{liveBuilder()}))

	var admitted bool
	for _, key := range spawnedKeys(plan) {
		if key == linuxSmall.Key {
			admitted = true
		}
	}
	if !admitted {
		t.Fatal("an aged macOS head idled capacity it was not waiting for")
	}
}

// A young macOS head holds nothing up: within a pass the fairness age is what
// turns waiting into precedence, and the same threshold has to govern across the
// platform boundary or the bound would be a platform preference rather than an
// ordering rule.
func TestAYoungMacHeadDoesNotHoldUpLinux(t *testing.T) {
	macHead := agedHeadDemand("a/repo", 1, time.Minute, "builder")
	linux := agedHeadDemand("b/repo", 2, 30*time.Second, "xl")

	plan := PlanTick(agedHeadInput([]domain.Demand{macHead, linux}, []domain.Instance{liveBuilder()}))

	var admitted bool
	for _, key := range spawnedKeys(plan) {
		if key == linux.Key {
			admitted = true
		}
	}
	if !admitted {
		t.Fatal("a young macOS head blocked Linux work: the fairness age confers precedence, not the platform")
	}
}
