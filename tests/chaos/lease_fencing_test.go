package chaos

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func TestLeaseStormHasOneAuthority(t *testing.T) {
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(2_000, 0).UTC()
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.AcquireLease(context.Background(), "authority", string(rune('a'+i)), now, time.Minute); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, operations.ErrLeaseHeld) {
				t.Errorf("unexpected claim error: %v", err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("authority winners=%d", winners.Load())
	}
}
