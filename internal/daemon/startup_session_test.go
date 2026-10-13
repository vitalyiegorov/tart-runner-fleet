package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// On 2026-10-13 the MacBook's controller exited sixteen times in a row because
// one of its bindings timed out opening a broker session while the home uplink
// was saturated, and launchd restarted it into the same timeout. These tests pin
// the distinction the fix rests on: the network's state defers one binding, and
// everything else still stops the node.

// brokerTimeout is the shape the official client returned that day: its own
// wrapping, around a *url.Error, around the HTTP client's deadline.
func brokerTimeout() error {
	return fmt.Errorf("create official message session: failed to send request: %w",
		&url.Error{Op: "Post", URL: "https://broker.actions.githubusercontent.com/sessions", Err: context.DeadlineExceeded})
}

// dialTimeout is a net.Error timeout that is not context.DeadlineExceeded, the
// shape a TCP dial that never completes returns.
type dialTimeout struct{}

func (dialTimeout) Error() string   { return "i/o timeout" }
func (dialTimeout) Timeout() bool   { return true }
func (dialTimeout) Temporary() bool { return true }

func TestTransientStartupFailureSeparatesTheNetworkFromTheNode(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "broker timeout", err: brokerTimeout(), want: true},
		{name: "dial timeout", err: fmt.Errorf("wrapped: %w", &url.Error{Op: "Post", URL: "https://api.github.com", Err: dialTimeout{}}), want: true},
		{name: "deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: true},
		{name: "dns", err: fmt.Errorf("wrapped: %w", &net.DNSError{Err: "no such host", Name: "api.github.com"}), want: true},
		{name: "refused", err: fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), want: true},
		{name: "rejected key", err: errors.New("create official GitHub App client: invalid private key")},
		{name: "deleted scale set", err: errors.New(`request POST .../runnerscalesets/15/sessions failed(status="404 Not Found")`)},
		{name: "invalid", err: operations.ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transientStartupFailure(test.err); got != test.want {
				t.Fatalf("transientStartupFailure(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestAPendingBindingOpensItsSessionOnALaterPoll(t *testing.T) {
	var opens atomic.Int32
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{pending: true,
		open: func(context.Context) (scaleSetSource, error) {
			if opens.Add(1) == 1 {
				return nil, brokerTimeout()
			}
			return &successfulSessionSource{}, nil
		}, limiter: make(chan struct{}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	commit := func(context.Context, githubscaleset.Demand) error { return nil }
	if _, err := source.Registered(context.Background(), "trf-x"); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a binding with no session must refuse control calls, got %v", err)
	}
	err = source.Handle(context.Background(), commit)
	var failure *githubscaleset.SessionFailure
	if !errors.As(err, &failure) || failure.Reason != githubscaleset.ReasonSessionCreateFailed {
		t.Fatalf("first poll = %v, want a session_create_failed ingest failure", err)
	}
	if err := source.Handle(context.Background(), commit); err != nil {
		t.Fatalf("second poll = %v, want the session opened and polled", err)
	}
	if err := source.Handle(context.Background(), commit); err != nil || opens.Load() != 2 {
		t.Fatalf("a healthy session must not be reopened; err=%v opens=%d", err, opens.Load())
	}
}

func TestAPendingBindingCanBeSuspendedAndClosedWithoutASession(t *testing.T) {
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{pending: true,
		open: func(context.Context) (scaleSetSource, error) { return &successfulSessionSource{}, nil }, limiter: make(chan struct{}, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Suspend(context.Background()); err != nil || !source.Suspended() {
		t.Fatalf("suspend = %v suspended=%v", err, source.Suspended())
	}
	if source.pendingOpen() {
		t.Fatal("a suspended binding must not open a session behind the yield controller's back")
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("close = %v", err)
	}
	if err := source.openPending(context.Background()); err != nil {
		t.Fatalf("open after close = %v", err)
	}
}

func TestAPendingBindingRequiresExactlyOneStartingShape(t *testing.T) {
	open := func(context.Context) (scaleSetSource, error) { return &successfulSessionSource{}, nil }
	limiter := make(chan struct{}, 1)
	if _, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{open: open, limiter: limiter}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("no session and not pending = %v", err)
	}
	if _, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &successfulSessionSource{}, pending: true, open: open, limiter: limiter}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a session and pending = %v", err)
	}
}

func TestAPendingOpenStopsWhenItsContextDoes(t *testing.T) {
	limiter := make(chan struct{}, 1)
	limiter <- struct{}{}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{pending: true,
		open: func(context.Context) (scaleSetSource, error) { return &successfulSessionSource{}, nil }, limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := source.Handle(ctx, func(context.Context, githubscaleset.Demand) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("handle with a cancelled context = %v", err)
	}
}

// The node-level behaviour: a transient failure on one binding no longer ends
// the run, and a non-transient one still does (TestRunCleansPartialScaleSets...).
func TestRunDefersABindingTheNetworkCouldNotReach(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var readyOnce sync.Once
	d.inventory = func(_ runtimeStore, _ config.Config, _ app.RecoveryObserver, _ *updateDrainController) app.Inventory {
		return notifyingInventory{ready: ready, once: &readyOnce}
	}
	var calls atomic.Int32
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		if calls.Add(1) == 2 {
			return nil, brokerTimeout()
		}
		return &fakeSource{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{Mode: reconcile.Authority, ConfigPath: writeConfig(t, true),
			DatabasePath: filepath.Join(t.TempDir(), "fleet.db"), HealthAddress: "127.0.0.1:0"}, d)
	}()
	select {
	case <-ready:
		cancel()
		<-done
	case err := <-done:
		t.Fatalf("a transient session failure ended the run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not start")
	}
	if calls.Load() < 2 {
		t.Fatalf("expected the failing binding to have been attempted; calls=%d", calls.Load())
	}
}
