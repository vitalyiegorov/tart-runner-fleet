package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
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

// TestSchedulerLoopRetriesContentionWithoutSpendingTheErrorBackoff is the
// regression case for the 2026-08-01 saturation incident. A tick that lost an
// optimistic-concurrency race was paced identically to a dead database: the loop
// waited the full five-second error backoff on a five-second tick interval, so
// every self-healing conflict cost the queue a whole scheduling round on top of
// the one it had already lost. The daemon logged that outcome once a minute for
// eight consecutive minutes while queued jobs aged past their SLO.
//
// A transient failure must be retried promptly, the delay must grow so a conflict
// that is not clearing degrades to the ordinary backoff instead of spinning
// against the store it contends with, and a success must reset the escalation.
// Nothing about reporting changes: every failure still reaches the hook.
func TestSchedulerLoopRetriesContentionWithoutSpendingTheErrorBackoff(t *testing.T) {
	outcomes := []error{
		tickFailure{reason: ReasonPlanCommitContended, err: operations.ErrConflict},
		tickFailure{reason: ReasonPlanCommitContended, err: operations.ErrConflict},
		nil,
		tickFailure{reason: ReasonPlanCommitContended, err: operations.ErrConflict},
		tickFailure{reason: ReasonPlanCommitFailed, err: errors.New("database is locked")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempt int
	var delays []time.Duration
	var reasons []string
	service := Service{TickInterval: time.Minute, ErrorBackoff: 5 * time.Second,
		Ticker: tickFunc(func(context.Context) error {
			if attempt >= len(outcomes) {
				cancel()
				return nil
			}
			err := outcomes[attempt]
			attempt++
			return err
		}),
		OnFailure: func(_, reason string) { reasons = append(reasons, reason) },
		After: func(delay time.Duration) <-chan time.Time {
			delays = append(delays, delay)
			ready := make(chan time.Time, 1)
			ready <- time.Time{}
			return ready
		}}
	service.schedulerLoop(ctx, make(chan struct{}))

	// Two consecutive conflicts escalate 250ms -> 500ms; the success between them
	// resets the escalation, so the fourth tick's conflict starts over at 250ms;
	// the store failure is not transient and pays the full backoff. Successful
	// ticks contribute the ordinary tick interval.
	wantDelays := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Minute,
		250 * time.Millisecond, 5 * time.Second, time.Minute}
	if !reflect.DeepEqual(delays, wantDelays) {
		t.Fatalf("delays = %v, want %v", delays, wantDelays)
	}
	wantReasons := []string{ReasonPlanCommitContended, ReasonPlanCommitContended,
		ReasonPlanCommitContended, ReasonPlanCommitFailed}
	if !reflect.DeepEqual(reasons, wantReasons) {
		t.Fatalf("reported reasons = %v, want %v", reasons, wantReasons)
	}
}

// TestContentionDelayIsBoundedByTheErrorBackoff proves the escalation saturates:
// a conflict that never clears must neither out-wait nor out-spin the backoff the
// loop already applies to every other failure.
func TestContentionDelayIsBoundedByTheErrorBackoff(t *testing.T) {
	service := Service{ErrorBackoff: time.Second}
	for _, attempt := range []int{0, 1, 2, 3, 4, 5, 9, 64, 1 << 20} {
		delay := service.contentionDelay(attempt)
		if delay < contentionRetryBase || delay > time.Second {
			t.Fatalf("contentionDelay(%d) = %v, want within [%v, %v]", attempt, delay, contentionRetryBase, time.Second)
		}
	}
	if got := service.contentionDelay(1); got != contentionRetryBase {
		t.Fatalf("first retry = %v, want %v", got, contentionRetryBase)
	}
}

// TestAFastNilIngestIsPacedNotSpun is issue #314's mechanism, found by a
// SIGQUIT goroutine dump on the live studio: all 36 per-binding ingest loops
// simultaneously RUNNABLE inside the bookkeeping after IngestChanged.
//
// The success path trusted the source to block — a broker long poll spends ~50s
// per empty answer, so `continue` with no pause was free. But `Handle` returns
// nil the moment `Next` yields no message, and on a pressured host those empty
// answers can come back immediately. Then the loop's pacing is whatever the
// network feels like, which is to say none: one core pegged, a continuous read
// stream against the store, health revisions in the billions — and not one log
// line, because nothing ever failed.
func TestAFastNilIngestIsPacedNotSpun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var waits []time.Duration
	calls := 0
	now := time.Unix(0, 0)
	service := Service{
		Ticker: tickFunc(func(context.Context) error { return nil }),
		Now:    func() time.Time { return now },
		After: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			done := make(chan time.Time, 1)
			done <- time.Time{}
			return done
		},
	}
	ingester := changeIngestFunc(func(context.Context) (bool, error) {
		calls++
		if calls >= 4 {
			cancel()
		}
		return false, nil // an instantly empty poll: no message, no error
	})

	service.ingestLoop(ctx, ingester, make(chan struct{}, 1))

	mu.Lock()
	defer mu.Unlock()
	paced := 0
	for _, wait := range waits {
		if wait == minIngestSpacing {
			paced++
		}
	}
	if paced < 3 {
		t.Fatalf("an instantly returning source must be paced between polls: %d paced waits in %v", paced, waits)
	}
}

// A source that genuinely long-polled owes nothing: charging the floor on top of
// a 50-second poll would slow real delivery for no protection. The spacing is
// only the part of the floor the source did not already spend.
func TestALongPollingIngestIsNotChargedTheFloor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var waits []time.Duration
	calls := 0
	now := time.Unix(0, 0)
	service := Service{
		Ticker: tickFunc(func(context.Context) error { return nil }),
		Now:    func() time.Time { return now },
		After: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			waits = append(waits, d)
			mu.Unlock()
			done := make(chan time.Time, 1)
			done <- time.Time{}
			return done
		},
	}
	ingester := changeIngestFunc(func(context.Context) (bool, error) {
		now = now.Add(50 * time.Second) // the long poll itself consumed the time
		calls++
		if calls >= 3 {
			cancel()
		}
		return false, nil
	})

	service.ingestLoop(ctx, ingester, make(chan struct{}, 1))

	mu.Lock()
	defer mu.Unlock()
	for _, wait := range waits {
		if wait > 0 && wait <= minIngestSpacing {
			t.Fatalf("a slow poll was charged spacing it already spent: %v", waits)
		}
	}
}

// Cancellation during the spacing wait ends the loop, exactly as it does in
// every other wait this file has.
func TestIngestSpacingWaitHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Unix(0, 0)
	service := Service{
		Ticker: tickFunc(func(context.Context) error { return nil }),
		Now:    func() time.Time { return now },
		After: func(time.Duration) <-chan time.Time {
			cancel()                    // the shutdown arrives mid-wait
			return make(chan time.Time) // and the timer never fires
		},
	}
	calls := 0
	ingester := changeIngestFunc(func(context.Context) (bool, error) { calls++; return false, nil })

	service.ingestLoop(ctx, ingester, make(chan struct{}, 1))

	if calls != 1 {
		t.Fatalf("a cancelled spacing wait must end the loop after one poll, got %d", calls)
	}
}

// An ingest error that arrives because the shutdown interrupted it is a
// shutdown, not a failure: nothing is reported and nothing is retried.
func TestIngestErrorDuringShutdownIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reported := 0
	service := Service{
		Ticker:    tickFunc(func(context.Context) error { return nil }),
		OnFailure: func(string, string) { reported++ },
		After:     func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	}
	ingester := changeIngestFunc(func(context.Context) (bool, error) {
		cancel()
		return false, errors.New("poll interrupted by shutdown")
	})

	service.ingestLoop(ctx, ingester, make(chan struct{}, 1))

	if reported != 0 {
		t.Fatalf("a shutdown-interrupted poll was reported as a failure %d times", reported)
	}
}

// The error backoff honours cancellation the same way the spacing wait does.
func TestIngestErrorBackoffHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	service := Service{
		Ticker: tickFunc(func(context.Context) error { return nil }),
		After: func(time.Duration) <-chan time.Time {
			cancel()
			return make(chan time.Time)
		},
	}
	ingester := changeIngestFunc(func(context.Context) (bool, error) {
		calls++
		return false, errors.New("broker refused the poll")
	})

	service.ingestLoop(ctx, ingester, make(chan struct{}, 1))

	if calls != 1 {
		t.Fatalf("a cancelled error backoff must end the loop after one poll, got %d", calls)
	}
}
