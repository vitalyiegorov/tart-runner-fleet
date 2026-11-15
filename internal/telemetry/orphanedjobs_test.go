package telemetry

import (
	"testing"
	"time"
)

func TestIssue520DoctorReportsUnassignedRESTJobs(t *testing.T) {
	h, clock := newTestHealth(t)
	name := "github"
	h.SetUndeliveredJobs(name, []string{"budgie run 420 job 42: cancel and rerun"})
	if !h.Ingest().OK {
		t.Fatal("unobserved inventory must not be trusted")
	}
	if err := h.RecordObservation(name, ObservationFresh); err != nil {
		t.Fatal(err)
	}
	if h.Ingest().OK {
		t.Fatal("doctor silently passed an unassigned REST job")
	}
	snapshot := h.Snapshot()
	snapshot.UndeliveredJobs[0] = "mutated"
	if h.Ingest().Reasons[0] == "mutated" {
		t.Fatal("snapshot aliases health")
	}
	clock.Advance(time.Hour)
	if !h.Ingest().OK {
		t.Fatal("stale inventory cannot prove a current orphan")
	}
	if err := h.RecordObservation(name, ObservationFresh); err != nil {
		t.Fatal(err)
	}
	h.SetUndeliveredJobs(name, nil)
	if !h.Ingest().OK {
		t.Fatal("complete refresh must clear resolved jobs")
	}
}
