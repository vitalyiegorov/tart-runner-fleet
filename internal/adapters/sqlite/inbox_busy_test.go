package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// TestIssue465BrokerBatchWaitsForALeaseWriterInsteadOfFailing replays the fleet-wide
// message_poll_failed flood with no HTTP status: every broker message commits
// through ApplyDemandBatch, a deferred transaction that read its inbox cursor
// before it first wrote. When the authority lease is mid-write on its own
// connection at that moment, SQLite refuses the read-to-write upgrade with plain
// SQLITE_BUSY at once, skipping busy_timeout, and the ingest loop reports the
// refusal as a failed long poll. This is issue #458's defect on the broker path.
// The batch must instead wait out the brief lease write and commit.
func TestIssue465BrokerBatchWaitsForALeaseWriterInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var result operations.DemandBatchResult
	code, err := replayBriefLeaseWrite(t, store, "inbox.", func() error {
		var err error
		result, err = store.ApplyDemandBatch(ctx, 9, 1, []operations.DemandEvent{demandEvent(operations.DemandJobAvailable, 41)})
		return err
	})
	if err != nil {
		t.Fatalf("broker batch failed against a brief lease write (code %d): %v", code, err)
	}
	if !result.Applied {
		t.Fatal("the batch committed nothing")
	}
}
