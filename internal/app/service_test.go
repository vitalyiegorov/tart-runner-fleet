package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type tickFunc func(context.Context) error

func (f tickFunc) Tick(ctx context.Context) error { return f(ctx) }

type ingestFunc func(context.Context) error

func (f ingestFunc) Ingest(ctx context.Context) error { return f(ctx) }

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
		}, OnFailure: func(component string) { failures <- component }}
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
		}, OnFailure: func(s string) { failures <- s },
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
		TickInterval: time.Hour, OnFailure: func(component string) { failures <- component; cancel() }}
	if err := service.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := <-failures; got != "operations" {
		t.Fatalf("worker failure=%q", got)
	}
}
