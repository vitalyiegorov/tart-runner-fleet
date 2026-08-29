package app

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// A drain has to outrank a healthy host, because a host with capacity to spare
// is exactly the case that would keep replacing the instances the node is
// waiting on — and the node would never reach the quiescence its update needs
// (ADR 0048, issues #230 and #282).
func TestUpdateDrainRefusesAdmissionOnAnOtherwiseHealthyHost(t *testing.T) {
	serving := hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), false,
		domain.Resources{}, false)
	if !serving.Value.Pressure.AdmissionAllowed {
		t.Fatalf("fixture host is not admitting: %q", serving.Value.Pressure.AdmissionReason)
	}

	draining := hostObservation(productionSnapshot(), configuredCapacity(), productionGuards(), false,
		domain.Resources{}, true)
	if draining.Value.Pressure.AdmissionAllowed {
		t.Fatal("a draining node kept admitting: the instances it waits on would be replaced forever")
	}
	if draining.Value.Pressure.AdmissionReason != "update drain" {
		t.Fatalf("admission reason = %q, want it to name the drain so an idle node is never an unexplained refusal",
			draining.Value.Pressure.AdmissionReason)
	}
}

// A drain never rescues a host that is already refusing for pressure: the
// operator-facing reason must stay the one that is actually blocking, or the
// drain would mask a disk reserve.
func TestUpdateDrainDoesNotOverwriteARealPressureRefusal(t *testing.T) {
	snapshot := productionSnapshot()
	snapshot.FreeDiskGB = 1
	refused := hostObservation(snapshot, configuredCapacity(), productionGuards(), false, domain.Resources{}, true)
	if refused.Value.Pressure.AdmissionAllowed {
		t.Fatal("a host under its disk reserve admitted")
	}
	if refused.Value.Pressure.AdmissionReason != "disk reserve" {
		t.Fatalf("admission reason = %q, want the pressure that actually blocks", refused.Value.Pressure.AdmissionReason)
	}
}
