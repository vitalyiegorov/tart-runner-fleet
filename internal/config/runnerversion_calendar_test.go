package config

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// runnerCalendar is GitHub's actual calendar when v2.336.0 was refused on 2026-11-05.
func runnerCalendar() []RunnerRelease {
	at := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			panic(err)
		}
		return parsed
	}
	return []RunnerRelease{
		{Version: "2.338.2", PublishedAt: at("2026-10-26T14:45:12Z")},
		{Version: "2.338.0", PublishedAt: at("2026-10-06T13:55:11Z")},
		{Version: "2.337.0", PublishedAt: at("2026-08-26T14:33:29Z")},
		{Version: "2.336.0", PublishedAt: at("2026-07-20T17:45:55Z")},
	}
}

// The incident's own image at the incident's own floor: the floor said nothing,
// and the calendar had been saying so since 25 September.
func TestTheCalendarWarnsWhereTheStaticFloorDidNot(t *testing.T) {
	image := RunnerImage{Platform: "linux", VM: "base", Version: "2.336.0", Floor: "2.336.0", Releases: runnerCalendar()}

	image.Now = time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	if reason := image.Reason(); reason != "" {
		t.Fatalf("inside the window: %s", reason)
	}
	image.Now = time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)
	reason := image.Reason()
	for _, want := range []string{"2.336.0", "2.337.0 was published 2026-08-26", "closed 2026-09-25"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
}

func TestACalendarNobodyReadJudgesNothing(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	if reason := (RunnerImage{Platform: "linux", VM: "base", Version: "2.336.0", Floor: "2.336.0", Now: now}).Reason(); reason != "" {
		t.Fatalf("no calendar: %s", reason)
	}
	if reason := (RunnerImage{Platform: "linux", VM: "base", Version: "2.336.0", Floor: "2.336.0", Releases: runnerCalendar()}).Reason(); reason != "" {
		t.Fatalf("no clock: %s", reason)
	}
}

func TestTheNewestReleasePassesTheCalendar(t *testing.T) {
	image := RunnerImage{Platform: "linux", VM: "base", Version: "2.338.2", Floor: "2.336.0", Releases: runnerCalendar(),
		Now: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	if reason := image.Reason(); reason != "" {
		t.Fatalf("nothing newer exists: %s", reason)
	}
}

func TestARefusalOutranksEveryOtherReasonAndIsNamed(t *testing.T) {
	refusal := domain.RunnerRefusal{Version: "2.336.0", Instance: "trf-linux-2x4-abc",
		At: time.Date(2026, 11, 5, 8, 54, 0, 0, time.UTC)}
	reason := RunnerImage{Platform: "linux", VM: "base", Floor: "2.336.0", Refusal: &refusal}.Reason()
	for _, want := range []string{"runner_version_refused", "advisory", "a job can write", "2.336.0", "trf-linux-2x4-abc", "2026-11-05T08:54:00Z"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason %q lacks %q", reason, want)
		}
	}
	if len(reason) > 512 {
		t.Fatalf("reason exceeds what telemetry will carry: %d bytes", len(reason))
	}
}

// The floor verdict is about the floor. A refusal or a closed window fails the check
// through Reason without claiming the image is below a floor it clears.
func TestTheFloorVerdictIsAboutTheFloorAlone(t *testing.T) {
	refusal := domain.RunnerRefusal{Version: "2.336.0", Instance: "trf-a", At: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)}
	for name, test := range map[string]struct {
		image RunnerImage
		below bool
	}{
		"clears it":   {image: RunnerImage{Version: "2.336.0", Floor: "2.336.0"}},
		"refused":     {image: RunnerImage{Version: "2.336.0", Floor: "2.336.0", Refusal: &refusal}},
		"past window": {image: RunnerImage{Version: "2.336.0", Floor: "2.336.0", Releases: runnerCalendar(), Now: refusal.At}},
		"below it":    {image: RunnerImage{Version: "2.335.1", Floor: "2.336.0"}, below: true},
		"undeclared":  {image: RunnerImage{Floor: "2.336.0"}, below: true},
	} {
		if got := test.image.BelowFloor(); got != test.below {
			t.Errorf("%s: below floor = %v, want %v", name, got, test.below)
		}
	}
}
