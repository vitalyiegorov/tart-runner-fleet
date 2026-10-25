package operations

import (
	"testing"
	"time"
)

// Automatic cleanup recovery is bounded and backs off (ADR 0081): the first
// resumption is immediate, each later one waits twice as long after the
// successor died, and the effect is handed to an operator after the bound.
func TestCleanupResumptionIsBoundedAndBacksOff(t *testing.T) {
	died := time.Unix(10_000, 0).UTC()
	for name, test := range map[string]struct {
		resumed int
		deadAt  time.Time
		now     time.Time
		want    bool
	}{
		"first is immediate":      {0, died, died, true},
		"first without a stamp":   {0, time.Time{}, died, true},
		"second waits":            {1, died, died.Add(CleanupResumptionBackoff - time.Nanosecond), false},
		"second after backoff":    {1, died, died.Add(CleanupResumptionBackoff), true},
		"third doubles":           {2, died, died.Add(CleanupResumptionBackoff), false},
		"third after double":      {2, died, died.Add(2 * CleanupResumptionBackoff), true},
		"bound reached":           {MaxCleanupResumptions, died, died.Add(24 * time.Hour), false},
		"negative":                {-1, died, died.Add(24 * time.Hour), false},
		"unstamped later attempt": {1, time.Time{}, died, false},
		"future death":            {1, died.Add(time.Hour), died, false},
	} {
		if got := CleanupResumptionDue(test.resumed, test.deadAt, test.now); got != test.want {
			t.Errorf("%s: due=%v want %v", name, got, test.want)
		}
	}
}
