package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	driver "modernc.org/sqlite"
)

// replayBriefLeaseWrite runs operation while the authority lease connection
// holds an uncommitted write. The write is taken when the operation's first
// statement after its begin point runs (any fault point under prefix other than
// prefix+"begin") and commits 150 ms later, well inside busy_timeout. It fails
// the test if the lease write was never held or could not commit, and returns
// the operation's own error and its SQLite result code (0 if none).
// A setup failure is delivered on released too, so the replay reports it
// instead of blocking on a release that never comes.
func replayBriefLeaseWrite(t *testing.T, store *Store, prefix string, operation func() error) (int, error) {
	t.Helper()
	ctx := context.Background()
	held := false
	released := make(chan error, 1)
	store.injectFault = func(point string) error {
		if held || point == prefix+"begin" || !strings.HasPrefix(point, prefix) {
			return nil
		}
		held = true
		leaseTx, err := store.lease().BeginTx(ctx, nil)
		if err != nil {
			released <- err
			return err
		}
		if _, err := leaseTx.ExecContext(ctx, `UPDATE scheduler_state SET updated_at=updated_at+1 WHERE singleton=1`); err != nil {
			_ = leaseTx.Rollback()
			released <- err
			return err
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			released <- leaseTx.Commit()
		}()
		return nil
	}
	err := operation()
	if !held {
		t.Fatal("the replay never held the lease write")
	}
	if leaseErr := <-released; leaseErr != nil {
		t.Fatalf("the lease write failed to commit: %v", leaseErr)
	}
	var refusal *driver.Error
	code := 0
	if errors.As(err, &refusal) {
		code = refusal.Code()
	}
	return code, err
}
