package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// TestResidualAdvertiserLowersOnlyOnAFreshReadyView pins the fail-open rule of
// issue #388: a set advertises below its configured capacity only on a fresh
// view from a ready tick, and never below what it holds.
func TestResidualAdvertiserLowersOnlyOnAFreshReadyView(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	advertiser := newResidualAdvertiser(func() time.Time { return now }, 2*time.Minute)
	full := advertiser.provider(18, 2)
	holding := advertiser.provider(20, 2)
	stranger := advertiser.provider(99, 3)
	if full() != 2 || holding() != 2 {
		t.Fatalf("before any tick the configured capacity is advertised: %d %d", full(), holding())
	}
	result := app.TickResult{At: now, Headroom: []app.ScaleSetHeadroom{
		{StoreKey: 18, Held: 0, Fit: 0}, {StoreKey: 20, Held: 1, Fit: 0}}}
	advertiser.observe(result, true)
	if full() != 0 || holding() != 1 || stranger() != 3 {
		t.Fatalf("fresh view: advertised %d %d %d, want 0 1 3", full(), holding(), stranger())
	}
	now = now.Add(3 * time.Minute)
	if full() != 2 {
		t.Fatalf("a stale view must fail open to the configured capacity, got %d", full())
	}
	now = now.Add(-3 * time.Minute)
	advertiser.observe(result, false)
	if full() != 2 || holding() != 2 {
		t.Fatalf("a tick that was not ready forgets the view: %d %d", full(), holding())
	}
	advertiser.observe(app.TickResult{At: now}, true)
	if full() != 2 {
		t.Fatalf("a ready tick without headroom is unknown, got %d", full())
	}
	advertiser.observe(result, true)
	now = now.Add(-time.Second)
	if full() != 2 {
		t.Fatalf("a view from the future is not trusted, got %d", full())
	}
	var absent *residualAdvertiser
	absent.observe(result, true)
}

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

// TestResidualAdvertiserKeepsItsLastViewThroughATickThatWasNotReady is issue
// #397's third cause. A tick that could not plan used to drop the view, and every
// set then advertised its configured maximum: a node too unsure of itself to
// admit anything invited the most work. A node that cannot plan cannot start
// what it is offered, so it keeps offering what it last knew it could.
func TestResidualAdvertiserKeepsItsLastViewThroughATickThatWasNotReady(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	advertiser := newResidualAdvertiser(func() time.Time { return now }, 2*time.Minute)
	full := advertiser.provider(18, 2)
	advertiser.observe(app.TickResult{At: now, Headroom: []app.ScaleSetHeadroom{{StoreKey: 18, Held: 1}}}, true)
	advertiser.observe(app.TickResult{}, false)
	if got := full(); got != 1 {
		t.Fatalf("a tick that was not ready advertised %d, want the last view's 1", got)
	}
	now = now.Add(time.Hour)
	if got := full(); got != 1 {
		t.Fatalf("an hour of unready ticks advertised %d, want the last view's 1", got)
	}
}
