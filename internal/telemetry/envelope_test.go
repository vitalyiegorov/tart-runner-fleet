package telemetry

import "testing"

// The envelope is published so an operator can do arithmetic on it without
// checking it first, which is only safe if an ungrammatical reading is refused
// rather than stored.
func TestSetEnvelopeRefusesANegativeComponent(t *testing.T) {
	health, _ := newTestHealth(t)
	for name, envelope := range map[string]EnvelopeMetric{
		"cpu":         {CPU: -1},
		"memory":      {MemoryMiB: -1},
		"slots":       {Slots: -1},
		"aged cpu":    {AgedCPU: -1},
		"aged memmib": {AgedMemoryMiB: -1},
		"aged slots":  {AgedSlots: -1},
	} {
		if err := health.SetEnvelope(envelope); err == nil {
			t.Fatalf("a negative %s was accepted", name)
		}
	}
	if got := health.Snapshot().Envelope; got != (EnvelopeMetric{}) {
		t.Fatalf("a refused envelope must not be stored, got %#v", got)
	}
}

// The published envelope must survive into the snapshot verbatim, and bump the
// revision like every other metric so a watcher sees the tick.
func TestSetEnvelopePublishesTheTicksCapacity(t *testing.T) {
	health, _ := newTestHealth(t)
	before := health.Snapshot().Revision
	envelope := EnvelopeMetric{CPU: 2, MemoryMiB: 9216, Slots: 3, AgedCPU: 6, AgedMemoryMiB: 9216, AgedSlots: 4}

	if err := health.SetEnvelope(envelope); err != nil {
		t.Fatalf("SetEnvelope: %v", err)
	}

	snapshot := health.Snapshot()
	if snapshot.Envelope != envelope {
		t.Fatalf("envelope = %#v, want %#v", snapshot.Envelope, envelope)
	}
	if snapshot.Revision == before {
		t.Fatal("publishing an envelope must bump the revision")
	}
}

// A tick whose observation was unusable computes no envelope, and the DTO must
// say "not judged" by absence rather than "no capacity" by a zero row.
func TestEnvelopeRowIsAbsentUntilATickHasJudgedOne(t *testing.T) {
	if row := envelopeRow(Snapshot{}); row != nil {
		t.Fatalf("an unjudged tick must publish no envelope row, got %#v", row)
	}
	row := envelopeRow(Snapshot{Envelope: EnvelopeMetric{CPU: 2, MemoryMiB: 9216, Slots: 3,
		AgedCPU: 6, AgedMemoryMiB: 9216, AgedSlots: 4}})
	if row == nil || row.CPU != 2 || row.MemoryMiB != 9216 || row.Slots != 3 ||
		row.AgedCPU != 6 || row.AgedMemoryMiB != 9216 || row.AgedSlots != 4 {
		t.Fatalf("envelope row = %#v", row)
	}
}
