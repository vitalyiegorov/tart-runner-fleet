package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type tickFunc func(context.Context) error

func (f tickFunc) Tick(ctx context.Context) error { return f(ctx) }

type ingestFunc func(context.Context) error

func (f ingestFunc) Ingest(ctx context.Context) error { return f(ctx) }

type changeIngestFunc func(context.Context) (bool, error)

func (f changeIngestFunc) Ingest(ctx context.Context) error {
	_, err := f(ctx)
	return err
}
func (f changeIngestFunc) IngestChanged(ctx context.Context) (bool, error) { return f(ctx) }

type workFunc func(context.Context) error

func (f workFunc) Work(ctx context.Context) error { return f(ctx) }

func TestServiceRunsAllLoopsAndStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	seen := map[string]int{}
	mark := func(name string) {
		mu.Lock()
		seen[name]++
		total := seen["tick"] + seen["ingest"] + seen["work"]
		mu.Unlock()
		if total >= 3 {
			cancel()
		}
	}
	service := Service{
		Ticker:       tickFunc(func(context.Context) error { mark("tick"); return nil }),
		Ingesters:    []Ingester{ingestFunc(func(context.Context) error { mark("ingest"); <-ctx.Done(); return ctx.Err() })},
		Worker:       workFunc(func(context.Context) error { mark("work"); return nil }),
		TickInterval: time.Hour, WorkInterval: time.Hour, ErrorBackoff: time.Hour,
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"tick", "ingest", "work"} {
		if seen[name] == 0 {
			t.Fatalf("%s loop did not run: %#v", name, seen)
		}
	}
}

func TestServiceReportsGenericFailuresAndBacksOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backoff := make(chan time.Time, 1)
	backoff <- time.Now()
	failures := make(chan string, 3)
	calls := 0
	service := Service{Ticker: tickFunc(func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("secret detail")
		}
		cancel()
		return nil
	}), TickInterval: time.Hour, ErrorBackoff: time.Second,
		After: func(duration time.Duration) <-chan time.Time {
			if duration == time.Second {
				return backoff
			}
			return make(chan time.Time)
		}, OnFailure: func(component, _ string) { failures <- component }}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || <-failures != "scheduler" {
		t.Fatalf("calls=%d failures=%v", calls, failures)
	}
}

func TestServiceValidationDefaultsAndCanceledWait(t *testing.T) {
	if err := (Service{}).Run(context.Background()); err == nil {
		t.Fatal("nil ticker accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := Service{Ticker: tickFunc(func(context.Context) error { t.Fatal("called after cancellation"); return nil })}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	service = Service{Ticker: tickFunc(func(context.Context) error { cancel(); return nil })}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServiceIngestAndWorkerErrorPaths(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failures := make(chan string, 4)
	backoff := make(chan time.Time, 1)
	backoff <- time.Now()
	ingestCalls := 0
	service := Service{
		Ticker: tickFunc(func(context.Context) error { return nil }),
		Ingesters: []Ingester{ingestFunc(func(context.Context) error {
			ingestCalls++
			if ingestCalls == 1 {
				return errors.New("x")
			}
			cancel()
			return nil
		})},
		TickInterval: time.Minute, ErrorBackoff: time.Second,
		After: func(d time.Duration) <-chan time.Time {
			if d == time.Second {
				return backoff
			}
			return make(chan time.Time)
		}, OnFailure: func(s, _ string) { failures <- s },
	}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ingestCalls != 2 {
		t.Fatalf("ingest calls=%d", ingestCalls)
	}
	got := map[string]bool{}
	for len(failures) > 0 {
		got[<-failures] = true
	}
	if !got["ingest"] {
		t.Fatalf("failures=%#v", got)
	}

	ctx, cancel = context.WithCancel(context.Background())
	service = Service{Ticker: tickFunc(func(context.Context) error { return nil }), Worker: workFunc(func(context.Context) error { return errors.New("x") }),
		TickInterval: time.Hour, OnFailure: func(component, _ string) { failures <- component; cancel() }}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-failures; got != "operations" {
		t.Fatalf("worker failure=%q", got)
	}
}

func TestServiceEventLoopCancellationAndCoalescingEdges(t *testing.T) {
	changed, err := ingest(context.Background(), changeIngestFunc(func(context.Context) (bool, error) { return true, nil }))
	if !changed || err != nil {
		t.Fatalf("change-aware ingest = %v, %v", changed, err)
	}
	if !(Service{}).wait(context.Background(), 0) {
		t.Fatal("zero-delay wait did not continue")
	}

	ctx, cancel := context.WithCancel(context.Background())
	service := Service{Ticker: tickFunc(func(context.Context) error {
		cancel()
		return errors.New("stopped")
	})}
	service.schedulerLoop(ctx, make(chan struct{}))

	ctx, cancel = context.WithCancel(context.Background())
	blocked := make(chan time.Time)
	waiting := make(chan struct{})
	service = Service{Ticker: tickFunc(func(context.Context) error { return errors.New("retry") }),
		ErrorBackoff: time.Hour, After: func(time.Duration) <-chan time.Time {
			close(waiting)
			return blocked
		}}
	done := make(chan struct{})
	go func() { service.schedulerLoop(ctx, make(chan struct{})); close(done) }()
	<-waiting
	cancel()
	<-done

	ctx, cancel = context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	service = Service{}
	service.ingestLoop(ctx, changeIngestFunc(func(context.Context) (bool, error) {
		cancel()
		return true, nil
	}), wake)

	ctx, cancel = context.WithCancel(context.Background())
	service.ingestLoop(ctx, changeIngestFunc(func(context.Context) (bool, error) {
		cancel()
		return false, context.Canceled
	}), make(chan struct{}, 1))

	ctx, cancel = context.WithCancel(context.Background())
	service.loop(ctx, func(context.Context) error {
		cancel()
		return errors.New("stopped")
	}, "worker", func() time.Duration { return time.Hour })

	ctx, cancel = context.WithCancel(context.Background())
	service.loop(ctx, func(context.Context) error {
		cancel()
		return nil
	}, "worker", func() time.Duration { return 0 })
}

type reasonedFailure struct{ reason string }

func (e reasonedFailure) Error() string         { return "bounded failure" }
func (e reasonedFailure) FailureReason() string { return e.reason }

// Regression: a wedged ingest binding logged only "component=ingest" for four
// hours. The bounded hook must also carry the closed-vocabulary reason.
func TestServiceIngestFailureCarriesBoundedReason(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		reason string
	}{
		{name: "classified failure", err: fmt.Errorf("ingest: %w", reasonedFailure{reason: "session_expired"}),
			reason: "session_expired"},
		{name: "unclassified failure", err: errors.New("secret upstream detail")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			reasons := make(chan string, 1)
			backoff := make(chan time.Time, 1)
			backoff <- time.Now()
			calls := 0
			service := Service{
				Ticker: tickFunc(func(context.Context) error { return nil }),
				Ingesters: []Ingester{ingestFunc(func(context.Context) error {
					calls++
					if calls == 1 {
						return test.err
					}
					cancel()
					return nil
				})},
				TickInterval: time.Hour, ErrorBackoff: time.Second,
				After: func(d time.Duration) <-chan time.Time {
					if d == time.Second {
						return backoff
					}
					return make(chan time.Time)
				},
				OnFailure: func(component, reason string) {
					if component == "ingest" {
						select {
						case reasons <- reason:
						default:
						}
					}
				},
			}
			if err := service.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := <-reasons; got != test.reason {
				t.Fatalf("reason = %q, want %q", got, test.reason)
			}
		})
	}
}
