package reconcile

import "testing"

func TestPromotionRequiresSequentialEvidence(t *testing.T) {
	all := Evidence{FreshObservations: true, ShadowMatches: true, CanaryPassed: true, RollbackReady: true}
	tests := []struct {
		from, to Mode
		evidence Evidence
		want     bool
	}{
		{Observe, Shadow, all, true},
		{Shadow, Canary, all, true},
		{Canary, Authority, all, true},
		{Observe, Authority, all, false},
		{Shadow, Authority, all, false},
		{Observe, Shadow, Evidence{}, false},
		{Shadow, Canary, Evidence{FreshObservations: true, RollbackReady: true}, false},
		{Canary, Authority, Evidence{FreshObservations: true, ShadowMatches: true, CanaryPassed: true}, false},
		{Authority, Authority, Evidence{}, true},
	}
	for _, tt := range tests {
		if got := CanTransition(tt.from, tt.to, tt.evidence); got != tt.want {
			t.Errorf("CanTransition(%s,%s,%+v) = %v, want %v", tt.from, tt.to, tt.evidence, got, tt.want)
		}
	}
}

func TestRollbackAndInvalidModes(t *testing.T) {
	for _, from := range []Mode{Shadow, Canary, Authority} {
		if !CanTransition(from, Observe, Evidence{}) {
			t.Errorf("rollback from %s rejected", from)
		}
	}
	if CanTransition(Mode("bad"), Observe, Evidence{}) || CanTransition(Observe, Mode("bad"), Evidence{}) {
		t.Fatal("invalid mode accepted")
	}
	for _, mode := range []Mode{Observe, Shadow, Canary, Authority} {
		if !mode.Valid() {
			t.Errorf("%s is invalid", mode)
		}
	}
	if Mode("bad").Valid() {
		t.Fatal("bad mode is valid")
	}
}
