package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// TestRunWiresResidualCapacityIntoTruthfulScaleSets: under canonical inventory
// every session is handed a capacity provider bounded by its configured value.
func TestRunWiresResidualCapacityIntoTruthfulScaleSets(t *testing.T) {
	d := testDependencies(t)
	var mu sync.Mutex
	opened := map[int]githubscaleset.GitHubAppScaleSetConfig{}
	all := make(chan struct{})
	d.newScaleSet = func(_ context.Context, cfg githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		mu.Lock()
		defer mu.Unlock()
		opened[cfg.ScaleSetID] = cfg
		if len(opened) == 5 {
			close(all)
		}
		return &fakeSource{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{
			ConfigPath: writeConfig(t, true), DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Shadow,
		}, d)
	}()
	select {
	case <-all:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("scale-set sessions were not opened")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, cfg := range opened {
		if cfg.Capacity == nil {
			t.Fatalf("scale set %d has no residual capacity provider", id)
		}
		if got := cfg.Capacity(); got < 0 || got > cfg.MaxCapacity {
			t.Fatalf("scale set %d advertises %d outside [0, %d]", id, got, cfg.MaxCapacity)
		}
	}
}
