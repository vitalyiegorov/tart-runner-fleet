package telemetry

import (
	"testing"
	"time"
)

// The two name lists are the health surface's vocabulary: a duplicate in either
// would make one observation silently shadow another, and a check that reports
// the wrong loop is worse than one that reports none. Construction refuses it
// rather than deduplicating, because a configuration that says a thing twice
// does not know what it wants.
func TestNewHealthRefusesADuplicateNameInEitherVocabulary(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0).UTC()}
	for _, test := range []struct {
		name   string
		config HealthConfig
	}{
		{"duplicate critical observation", HealthConfig{Profiles: []string{"small"},
			CriticalObservations: []string{"scheduler", "scheduler"}}},
		{"duplicate failure component", HealthConfig{Profiles: []string{"small"},
			FailureComponents: []string{"ingest", "ingest"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHealth(clock, test.config); err == nil {
				t.Fatal("NewHealth accepted a vocabulary that names one thing twice")
			}
		})
	}

	if _, err := NewHealth(clock, HealthConfig{Profiles: []string{"small"},
		CriticalObservations: []string{"scheduler", "operations"},
		FailureComponents:    []string{"ingest", "scheduler"}}); err != nil {
		t.Fatalf("NewHealth rejected a valid vocabulary: %v", err)
	}
}
