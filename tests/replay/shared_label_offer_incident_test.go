package replay_test

import (
	"math"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// Issue #397 on the Mac mini, 2026-10-20, replayed from its fleet.db (read
// only) and the scale-set inbox delivery times:
//
//	13:54:31Z  linux-2x4  budgie "Publish a release"      running until 14:14:46
//	14:00:48Z  maestro    budgie "Seed iOS native"        running until 14:24:37
//	14:06:04Z  delivered  budgie linux-4x8 "Compare native runtime fingerprint"
//	14:06:33Z  delivered  rnw    maestro   "Maestro (screen-chrome, bare)"
//	           -> waited on the mini until 14:27:xx; the Studio ran nothing
//	              from 14:06 to 14:27.
//
// The mini's budget is 10 CPU / 23 GiB. With a maestro (4 CPU) and a 2x4
// (2 CPU) running, 4 CPU were free, which is enough for ONE of a maestro or a
// 4x8. Every set was offered its own profile's fit, computed alone: each of the
// seven maestro sets was offered one maestro, each of the five 4x8 sets one 4x8,
// and each of the five 2x4 sets two 2x4s. GitHub placed both the 4x8 and the
// maestro. The 4x8 started and the maestro waited 21 minutes, while the
// Studio, which advertises the same maestro labels, was idle.
//
// The replay is the tick before 14:06:04Z, the moment the mini sized its offers.

func miniOfferConfig() scheduler.Config {
	maestro := domain.Profile{ID: "maestro", Platform: domain.PlatformMacOS, Route: "trf-macos-arm64-4x7",
		Resources: domain.Resources{CPU: 4, MemoryMB: 7_168, Slots: 1}, MaxActive: 2}
	builder := domain.Profile{ID: "builder", Platform: domain.PlatformMacOS, Route: "trf-macos-arm64-6x12",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}, MaxActive: 1}
	small := domain.Profile{ID: "linux-2x4", Platform: domain.PlatformLinux, Route: "trf-linux-arm64-2x4",
		Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}}
	large := domain.Profile{ID: "linux-4x8", Platform: domain.PlatformLinux, Route: "trf-linux-arm64-4x8",
		Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}}
	return scheduler.Config{
		LinuxCapacity:          domain.Resources{CPU: 10, MemoryMB: 20_480, Slots: 4},
		HostBudget:             domain.Resources{CPU: 10, MemoryMB: 23_552},
		FairnessAge:            5 * time.Minute,
		AssignedTimeout:        15 * time.Minute,
		ElasticHostEnvelope:    true,
		MixedPlatformAdmission: true,
		MixedProfileCohorts:    true,
		Profiles: map[domain.ProfileID]domain.Profile{"maestro": maestro, "builder": builder,
			"linux-2x4": small, "linux-4x8": large},
	}
}

func miniOfferInstance(now time.Time, id, profile string, job int64) domain.Instance {
	definition := miniOfferConfig().Profiles[domain.ProfileID(profile)]
	return domain.Instance{ID: id, Repo: "budgie-at/budgie", Platform: definition.Platform,
		Profile: definition.ID, Route: definition.Route, Resources: definition.Resources,
		State: domain.InstanceRunning, Power: domain.InstancePowerRunning, RunningSince: now.Add(-5 * time.Minute),
		Demand: domain.DemandKey{Repo: "budgie-at/budgie", RunID: 39_000, Attempt: 1, JobID: job}}
}

// TestSharedLabelOfferIncidentOffersTheMinisResidualOnce is the decisive tick.
// The mini's seventeen sets are sized together. What they are offered beyond
// what they hold has to start on the mini at once, so a maestro and a 4x8 can
// no longer both be invited into the 4 CPU that fit one of them.
func TestSharedLabelOfferIncidentOffersTheMinisResidualOnce(t *testing.T) {
	now := time.Date(2026, 10, 20, 14, 5, 50, 0, time.UTC)
	in := scheduler.Input{Now: now, Config: miniOfferConfig(),
		Demands: domain.Fresh([]domain.Demand{}, now),
		Instances: domain.Fresh([]domain.Instance{
			miniOfferInstance(now, "trf-linux-2x4-aeca2e", "linux-2x4", 1),
			miniOfferInstance(now, "trf-maestro-9a9f6c", "maestro", 2),
		}, now),
		Host: domain.Fresh(domain.Host{
			Capacity:  domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4},
			Available: domain.Resources{CPU: 10, MemoryMB: 24_576, Slots: 4}}, now),
	}
	// The mini's configured sets on 2026-10-20: a maestro set in each of seven
	// scopes, and a 2x4 and a 4x8 set in each of the five scopes that run Linux
	// there. Room is each set's configured capacity less what it holds.
	var claims []scheduler.Claim
	for range 7 {
		claims = append(claims, scheduler.Claim{Profile: "maestro", Room: 2})
	}
	for range 5 {
		claims = append(claims, scheduler.Claim{Profile: "linux-2x4", Room: 4},
			scheduler.Claim{Profile: "linux-4x8", Room: 2})
	}
	shares, ok := scheduler.PartitionHeadroom(in, claims)
	if !ok {
		t.Fatal("the replayed observations are usable")
	}
	offered := domain.Resources{}
	for index, share := range shares {
		for range share {
			offered = offered.Add(miniOfferConfig().Profiles[claims[index].Profile].Resources)
		}
	}
	if offered.CPU == 0 || offered.CPU > 4 {
		t.Fatalf("the mini offered %+v beyond what it held (shares %v); its 4 free CPU start exactly one maestro, "+
			"one 4x8, or two 2x4s, and it must offer one of those", offered, shares)
	}

	// The figure the incident ran on: each set sized alone against the same
	// residual. It is what GitHub was invited to place: 68 CPU into 4.
	alone := domain.Resources{}
	for _, claim := range claims {
		single, _ := scheduler.PartitionHeadroom(in, []scheduler.Claim{{Profile: claim.Profile, Room: math.MaxInt}})
		for range min(single[0], claim.Room) {
			alone = alone.Add(miniOfferConfig().Profiles[claim.Profile].Resources)
		}
	}
	if alone.CPU != 68 {
		t.Fatalf("sized one set at a time the mini offers %d CPU; the replay no longer reconstructs the incident", alone.CPU)
	}
}
