package config

import (
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

func budget(seconds int) *int { return &seconds }

// A profile's occupancy ceiling has three distinguishable states, and the
// difference between "unstated" and "stated as zero" is the difference between
// a default and an operator's deliberate exemption (ADR 0036, issue #223).
func TestOccupancyBudgetResolvesAgainstThePlatformDefault(t *testing.T) {
	for _, test := range []struct {
		name     string
		profile  Profile
		platform domain.Platform
		want     time.Duration
	}{
		{name: "unstated linux takes the linux default", profile: Profile{ID: "small"},
			platform: domain.PlatformLinux, want: DefaultOccupancyBudget(domain.PlatformLinux)},
		{name: "unstated macOS takes the longer macOS default", profile: Profile{ID: "builder"},
			platform: domain.PlatformMacOS, want: DefaultOccupancyBudget(domain.PlatformMacOS)},
		{name: "stated zero is a deliberate exemption", profile: Profile{ID: "builder", OccupancyBudgetSeconds: budget(0)},
			platform: domain.PlatformMacOS, want: 0},
		{name: "stated value wins", profile: Profile{ID: "xl", OccupancyBudgetSeconds: budget(2_700)},
			platform: domain.PlatformLinux, want: 45 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.profile.OccupancyBudget(test.platform); got != test.want {
				t.Fatalf("OccupancyBudget = %s, want %s", got, test.want)
			}
		})
	}
	// The defaults must sit above the longest healthy job each platform runs and
	// below GitHub's own six-hour maximum, or the budget is either a job killer
	// or unreachable.
	if macos, linux := DefaultOccupancyBudget(domain.PlatformMacOS), DefaultOccupancyBudget(domain.PlatformLinux); macos <= linux ||
		linux < MinOccupancyBudget || macos > MaxOccupancyBudget {
		t.Fatalf("defaults are not ordered and bounded: macos=%s linux=%s", macos, linux)
	}
}

func TestOccupancyBudgetRejectsACeilingThatCouldNeverBeRight(t *testing.T) {
	for _, test := range []struct {
		name    string
		seconds *int
		valid   bool
	}{
		{name: "unstated", seconds: nil, valid: true},
		{name: "explicitly unbounded", seconds: budget(0), valid: true},
		{name: "at the floor", seconds: budget(300), valid: true},
		{name: "at the ceiling", seconds: budget(21_600), valid: true},
		{name: "below the floor", seconds: budget(299), valid: false},
		{name: "above GitHub's job maximum", seconds: budget(21_601), valid: false},
		{name: "negative", seconds: budget(-1), valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			linux := Default()
			linux.Linux.Profiles[0].OccupancyBudgetSeconds = test.seconds
			macos := Default()
			macos.MacOS.Builder.OccupancyBudgetSeconds = test.seconds
			for label, candidate := range map[string]Config{"linux": linux, "macos": macos} {
				err := candidate.Validate()
				if test.valid && err != nil {
					t.Fatalf("%s: valid ceiling rejected: %v", label, err)
				}
				if !test.valid && !errors.Is(err, errOccupancyBudgetRange) {
					t.Fatalf("%s: invalid ceiling accepted or misreported: %v", label, err)
				}
			}
		})
	}
}

// A ceiling is a pointer, so a clone that shared it would let one node's
// configuration edit another's.
func TestCloneCopiesTheOccupancyBudgetByValue(t *testing.T) {
	original := Default()
	original.Linux.Profiles[0].OccupancyBudgetSeconds = budget(1_800)
	original.MacOS.Builder.OccupancyBudgetSeconds = budget(3_600)
	original.MacOS.Maestro.OccupancyBudgetSeconds = nil

	clone := original.Clone()
	*clone.Linux.Profiles[0].OccupancyBudgetSeconds = 900
	*clone.MacOS.Builder.OccupancyBudgetSeconds = 900

	if *original.Linux.Profiles[0].OccupancyBudgetSeconds != 1_800 || *original.MacOS.Builder.OccupancyBudgetSeconds != 3_600 {
		t.Fatal("a clone must not alias the original's occupancy ceiling")
	}
	if clone.MacOS.Maestro.OccupancyBudgetSeconds != nil {
		t.Fatal("an unstated ceiling must stay unstated through a clone")
	}
}
