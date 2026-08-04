package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Incident 2026-08-01 .. 2026-08-04 (issue #165). GitHub restarted the broker
// message-id sequence for scale set 8077185082566234948 at 18:32Z. The fleet's
// inbox still held ids 100000004..100000086 written between 13 and 20 July, so
// every message the new sequence delivered from 100000004 onward collided with a
// July row whose content differed. ApplyDemandBatch keyed idempotency on
// (scale_set_id, message_id) alone, returned ErrConflict, ScaleSet.Handle
// nacked, ADR 0009 recreated the session, GitHub redelivered the same id, and
// the collision repeated for three days. Every linux-large job in
// vitalyiegorov/knee-doctor stayed queued on GitHub and was never seen.
//
// A broker message id is not a durable identity: it is unique only within one
// sequence. These tests pin the contract that replaces it -- a message id is
// deduped against the inbox GENERATION it was delivered in, and the one
// unambiguous piece of evidence that the sequence restarted (the ledger holds
// this id under different content) adopts the next generation instead of
// refusing the message forever.
const incidentScaleSet = 8077185082566234948

// julyEvent reproduces a message of the sequence that was live in July.
func julyEvent(requestID int64) operations.DemandEvent {
	return operations.DemandEvent{Kind: operations.DemandJobAvailable, RunnerRequestID: requestID,
		Owner: "vitalyiegorov", Repository: "knee-doctor", WorkflowRunID: 29189348157, JobID: "july-job",
		DisplayName: "Build", WorkflowRef: "refs/heads/main", EventName: "pull_request",
		Labels:    []string{"self-hosted", "linux-tiered", "linux-large"},
		QueueTime: time.Date(2026, 7, 13, 21, 46, 20, 0, time.UTC)}
}

// augustEvent reproduces a message of the sequence GitHub restarted on 1 August.
func augustEvent(requestID int64, minute int) operations.DemandEvent {
	return operations.DemandEvent{Kind: operations.DemandJobAvailable, RunnerRequestID: requestID,
		Owner: "vitalyiegorov", Repository: "knee-doctor", WorkflowRunID: 30712769676, JobID: "august-job",
		DisplayName: "Build", WorkflowRef: "refs/heads/main", EventName: "pull_request",
		Labels:    []string{"self-hosted", "linux-tiered", "linux-large"},
		QueueTime: time.Date(2026, 8, 1, 18, minute, 53, 0, time.UTC)}
}

// seedRetiredSequence writes the July sequence exactly as production held it:
// ids 100000004..100000010 with the cursor at the high-water mark.
func seedRetiredSequence(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	for id := int64(100000004); id <= 100000010; id++ {
		if _, err := store.ApplyDemandBatch(ctx, incidentScaleSet, id, []operations.DemandEvent{julyEvent(id)}); err != nil {
			t.Fatalf("seed retired sequence %d: %v", id, err)
		}
	}
	cursor, err := store.DemandCursor(ctx, incidentScaleSet)
	if err != nil || cursor != 100000010 {
		t.Fatalf("retired cursor: %d %v", cursor, err)
	}
}

// TestRestartedSequenceIsAdoptedNotRefusedForever is the incident itself,
// replayed in production's exact shape: the restarted sequence opens below the
// retired ledger's floor, so its first messages land unopposed, and the fleet
// only meets the retired sequence when the ids catch up to it.
func TestRestartedSequenceIsAdoptedNotRefusedForever(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	seedRetiredSequence(t, store)

	// 2026-08-01T18:32:52Z: the restarted sequence's first message. It is below
	// everything the retired sequence recorded, so it simply applies -- a lower
	// id is not on its own evidence of anything.
	opened, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000001, []operations.DemandEvent{augustEvent(6597795989104146686, 32)})
	if err != nil || !opened.Applied {
		t.Fatalf("the restarted sequence's first message: %#v %v", opened, err)
	}
	if opened.Reset.Detected {
		t.Fatalf("an id the ledger has never held is not a restart: %#v", opened.Reset)
	}

	// 2026-08-04T19:12Z: the id that collided with a 13 July row for three days.
	// It carries a different job, and it must reach the durable queue.
	collided, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000004, []operations.DemandEvent{augustEvent(7880094454290370577, 37)})
	if err != nil {
		t.Fatalf("the redelivered message still collides with the retired sequence: %v", err)
	}
	if !collided.Applied {
		t.Fatal("the redelivered message was deduped against a retired sequence")
	}
	if !collided.Reset.Detected || collided.Reset.Generation != 1 ||
		collided.Reset.RetiredMessageID != 100000010 || collided.Reset.AdoptedMessageID != 100000004 {
		t.Fatalf("the restart must name the generation it adopted and the sequence it retired: %#v", collided.Reset)
	}
	// The retired sequence stays readable under its own generation: those rows
	// are the evidence that diagnosed the incident.
	var retired int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scale_set_inbox WHERE scale_set_id=? AND generation=0`, incidentScaleSet).
		Scan(&retired); err != nil || retired != 8 {
		t.Fatalf("retired generation rows: %d %v", retired, err)
	}
	records, err := store.ActiveDemands(ctx, incidentScaleSet)
	if err != nil {
		t.Fatalf("active demands: %v", err)
	}
	var found bool
	for _, record := range records {
		if record.RunnerRequestID == 7880094454290370577 && record.Status == operations.DemandJobAvailable {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stranded job never reached the durable queue: %#v", records)
	}

	// The cursor must follow the live sequence. Left at the retired high-water
	// mark it asks the broker for messages after an id the new sequence will not
	// reach for thousands of jobs.
	cursor, err := store.DemandCursor(ctx, incidentScaleSet)
	if err != nil || cursor != 100000004 {
		t.Fatalf("cursor did not track the live sequence: %d %v", cursor, err)
	}
}

// TestRedeliveryInsideOneSequenceStillDedupes is the property the fix must not
// weaken. At-least-once delivery is safe only because a redelivered message is
// recognized and applied once; adopting a generation per delivery would turn
// every retry into a re-application.
func TestRedeliveryInsideOneSequenceStillDedupes(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	events := []operations.DemandEvent{augustEvent(4242, 32)}
	first, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000001, events)
	if err != nil || !first.Applied {
		t.Fatalf("first delivery: %#v %v", first, err)
	}
	// A commit whose acknowledgement failed: GitHub redelivers the same message.
	repeat, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000001, events)
	if err != nil {
		t.Fatalf("redelivery must be idempotent: %v", err)
	}
	if repeat.Applied {
		t.Fatal("a redelivered message was applied twice")
	}
	if repeat.Reset.Detected {
		t.Fatalf("a redelivery is not a sequence reset: %#v", repeat.Reset)
	}
	// And a message id equal to the cursor is a redelivery, not a regression.
	cursor, err := store.DemandCursor(ctx, incidentScaleSet)
	if err != nil || cursor != 100000001 {
		t.Fatalf("cursor: %d %v", cursor, err)
	}
}

// TestDivergentContentIsAppliedWithNoCursorAtAll proves the contract does not
// depend on the cursor row existing. An operator who cleared it, or a database
// restored from before it was written, must still never see a delivered job
// refused forever.
func TestDivergentContentIsAppliedWithNoCursorAtAll(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000004, []operations.DemandEvent{julyEvent(7880094454290370576)}); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM scale_set_cursors WHERE scale_set_id=?`, incidentScaleSet); err != nil {
		t.Fatalf("retire the cursor: %v", err)
	}
	result, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000004, []operations.DemandEvent{augustEvent(6597795989104146686, 37)})
	if err != nil {
		t.Fatalf("divergent content must not be refused forever: %v", err)
	}
	if !result.Applied {
		t.Fatal("divergent content was deduped against the retired sequence")
	}
	if !result.Reset.Detected || result.Reset.RetiredMessageID != 0 {
		t.Fatalf("a lost cursor retires nothing but still adopts a generation: %#v", result.Reset)
	}
}

// TestRetiredGenerationsAreBoundedAndReadable keeps the ledger from growing
// without limit across restarts while retaining the evidence that diagnosed this
// incident: the generation immediately before the live one stays readable, which
// is exactly the row set that named the defect on 2026-08-04.
func TestRetiredGenerationsAreBoundedAndReadable(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	// Four sequences in a row, each proving itself by contradicting the ledger
	// under the same id.
	for restart := 0; restart < 4; restart++ {
		if _, err := store.ApplyDemandBatch(ctx, incidentScaleSet, 100000004, []operations.DemandEvent{augustEvent(int64(restart+1), 32)}); err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
	}
	var generations int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT generation) FROM scale_set_inbox WHERE scale_set_id=?`, incidentScaleSet).
		Scan(&generations); err != nil {
		t.Fatalf("count generations: %v", err)
	}
	if generations != 2 {
		t.Fatalf("the ledger must retain the live generation and the one it retired, got %d", generations)
	}
}
