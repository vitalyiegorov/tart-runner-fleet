package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// Dead-cleanup recovery of a running VM may only retry a stop once the host's
// own guardrails pass again (ADR 0081). An update drain's refusal is not host
// pressure; stale, unavailable or pressured evidence is never healthy.
func TestHostHealthyIsAFreshObservationWhoseGuardrailsPass(t *testing.T) {
	pressured := productionSnapshot()
	pressured.FreeDiskGB = 1
	stale := productionSnapshot()
	stale.Freshness = executor.Stale
	for name, test := range map[string]struct {
		observation domain.Observation[domain.Host]
		want        bool
	}{
		"healthy":         {hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), false, domain.Resources{}, false), true},
		"update drain":    {hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), false, domain.Resources{}, true), true},
		"pressured":       {hostObservation(pressured, configuredCapacity(), productionGuards(), false, domain.Resources{}, false), false},
		"pressured drain": {hostObservation(pressured, configuredCapacity(), productionGuards(), false, domain.Resources{}, true), false},
		"stale":           {hostObservation(stale, configuredCapacity(), productionGuards(), false, domain.Resources{}, false), false},
		"unavailable":     {domain.Unavailable[domain.Host]("host probe unavailable"), false},
		"stale value":     {domain.Stale(domain.Host{Pressure: domain.HostPressure{AdmissionAllowed: true}}, time.Unix(1, 0), "old"), false},
	} {
		if got := hostHealthy(test.observation); got != test.want {
			t.Errorf("%s: hostHealthy=%v want %v", name, got, test.want)
		}
	}
}
