package telemetry

import (
	"strings"
	"testing"
)

// studioPressure is the mac studio at the moment of the 2026-08-25 incident:
// admitting nothing on its disk reserve, with roughly 58 GiB free against a
// 60 GiB floor, while every other guardrail was comfortable.
func studioPressure() HostPressureMetric {
	return HostPressureMetric{AvailableMemoryMiB: 8192, FreeDiskGiB: 58, SwapUsedMiB: 1024,
		CPUIdlePercent: 62, LoadAverage: 4.1, AdmissionAllowed: false, AdmissionReason: "disk reserve"}
}

func studioFloors() AdmissionFloors {
	return AdmissionFloors{MinFreeDiskGiB: 60, MinAvailableMemoryMiB: 2048,
		MaxSwapUsedMiB: 8192, MaxLoadAverage: 24}
}

// TestANodeAdmittingNothingIsAFinding is issue #286.
//
// The studio starved the whole fleet for about two and a half hours while
// `admissionAllowed: false` and `admissionReason: "disk reserve"` sat in
// `fleet status` and `fleet doctor` reported 10 of 11 checks OK, naming only the
// downstream queue-SLO symptom. The operator learned that jobs were slow. What
// was true is that the node was refusing every job, and why.
func TestANodeAdmittingNothingIsAFinding(t *testing.T) {
	result := admissionResult(Snapshot{HostPressure: studioPressure(), AdmissionFloors: studioFloors()})

	if result.OK {
		t.Fatal("a node refusing every job must not report a passing check")
	}
	detail := strings.Join(result.Reasons, " ")
	for _, want := range []string{"admitting no work", "disk reserve", "58", "60"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the finding must name the reason and the measurement against its floor, missing %q: %q", want, detail)
		}
	}
}

// A blocked node reports the guard its OWN reason names, not whichever guard
// happens to have the least headroom. The reason is what refused it.
func TestABlockedNodeReportsTheGuardThatRefusedIt(t *testing.T) {
	pressure := studioPressure()
	pressure.AdmissionReason = "memory reserve"
	pressure.AvailableMemoryMiB = 1024

	detail := strings.Join(admissionResult(Snapshot{HostPressure: pressure, AdmissionFloors: studioFloors()}).Reasons, " ")

	if !strings.Contains(detail, "memory") || !strings.Contains(detail, "1024") {
		t.Fatalf("the refusing guard must be the one measured: %q", detail)
	}
}

// TestAnAdmittingNodeStillShowsHowCloseItIs is the warning half.
//
// The studio's free disk slid 72 -> 54 GiB over about six hours. That was
// obvious in hindsight and invisible at the time. A margin printed on every
// passing doctor run is the cheapest form of a warning and costs no threshold
// nobody would agree on.
func TestAnAdmittingNodeStillShowsHowCloseItIs(t *testing.T) {
	pressure := studioPressure()
	pressure.AdmissionAllowed = true
	pressure.AdmissionReason = "capacity available"
	pressure.FreeDiskGiB = 72

	result := admissionResult(Snapshot{HostPressure: pressure, AdmissionFloors: studioFloors()})

	if !result.OK {
		t.Fatal("an admitting node passes")
	}
	detail := strings.Join(result.Reasons, " ")
	if !strings.Contains(detail, "nearest floor") || !strings.Contains(detail, "disk 72") {
		t.Fatalf("a passing check must still show the tightest margin: %q", detail)
	}
}

// The nearest floor is the guard with the least RELATIVE headroom, so a node
// close on memory is not reported as close on disk merely because the numbers
// are larger.
func TestTheNearestFloorIsTheTightestOneNotTheLargestNumber(t *testing.T) {
	pressure := studioPressure()
	pressure.AdmissionAllowed = true
	pressure.AdmissionReason = "capacity available"
	pressure.FreeDiskGiB = 600
	pressure.AvailableMemoryMiB = 2100

	detail := strings.Join(admissionResult(Snapshot{HostPressure: pressure, AdmissionFloors: studioFloors()}).Reasons, " ")

	if !strings.Contains(detail, "memory") {
		t.Fatalf("the tightest guard is memory here: %q", detail)
	}
}

// A floor the operator never configured is not rendered. A node with no disk
// floor is not zero GiB away from one.
func TestAnUnconfiguredFloorIsNotReported(t *testing.T) {
	pressure := studioPressure()
	pressure.AdmissionAllowed = true
	pressure.AdmissionReason = "capacity available"

	result := admissionResult(Snapshot{HostPressure: pressure})

	if !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("with no configured floors there is nothing to report: %#v", result)
	}
}

// A refusal whose reason is outside the closed vocabulary is still a refusal.
// The check must never render upstream text, and must never pass because it
// failed to recognise a word.
func TestAnUnrecognisedRefusalIsStillAFinding(t *testing.T) {
	pressure := studioPressure()
	pressure.AdmissionReason = "Bearer AAABBBCCC"

	result := admissionResult(Snapshot{HostPressure: pressure, AdmissionFloors: studioFloors()})

	if result.OK {
		t.Fatal("an unrecognised reason must not turn a refusal into a pass")
	}
	if strings.Contains(strings.Join(result.Reasons, " "), "Bearer") {
		t.Fatalf("unclassified text reached the finding: %v", result.Reasons)
	}
}

// The accessor and the status document are the same function of one snapshot,
// so they can never disagree about whether this node is taking work.
func TestTheAccessorAgreesWithTheDocument(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetHostPressure(studioPressure()); err != nil {
		t.Fatalf("SetHostPressure: %v", err)
	}

	if health.Admission().OK {
		t.Fatal("the accessor must report the same refusal the document does")
	}
}
