package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const (
	// contentionRetryBase is the first retry delay after a self-healing commit
	// conflict: short enough that the tick is not lost, long enough that the
	// winning writer's transaction has drained off the single store connection.
	contentionRetryBase = 250 * time.Millisecond
	// contentionRetryShiftLimit caps the doubling so the shift can never
	// overflow; the error backoff clamps the resulting delay anyway.
	contentionRetryShiftLimit = 8
)

type TickRunner interface{ Tick(context.Context) error }
type Ingester interface{ Ingest(context.Context) error }
type ChangeIngester interface {
	IngestChanged(context.Context) (bool, error)
}
type WorkRunner interface{ Work(context.Context) error }

type Service struct {
	Ticker       TickRunner
	Ingesters    []Ingester
	Worker       WorkRunner
	TickInterval time.Duration
	WorkInterval time.Duration
	ErrorBackoff time.Duration
	After        func(time.Duration) <-chan time.Time
	// OnFailure deliberately receives only a bounded component name and a
	// closed-vocabulary reason. This prevents an upstream error from reflecting
	// a token or JIT secret to logs while still telling an operator why a
	// component keeps failing. Reason is empty when the failure carries none.
	OnFailure func(component, reason string)
}

// FailureReason lets an error carry a bounded, closed-vocabulary diagnostic to
// the failure hook. Implementations must return a value from a closed
// vocabulary; concrete upstream text never travels this path.
type FailureReason interface {
	FailureReason() string
}

// TransientFailure lets an error declare that it clears by itself: the losing
// side of a race whose winner already advanced the durable state. Nothing else
// may claim it, and claiming it changes only retry pacing — never whether the
// failure is reported.
type TransientFailure interface {
	Transient() bool
}

func failureReason(err error) string {
	var reason FailureReason
	if errors.As(err, &reason) {
		return reason.FailureReason()
	}
	return ""
}

func transientFailure(err error) bool {
	var transient TransientFailure
	return errors.As(err, &transient) && transient.Transient()
}

func (s Service) Run(ctx context.Context) error {
	if s.Ticker == nil {
		return operations.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	schedulerWake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	start := func(run func(context.Context) error, component string, successDelay func() time.Duration) {
		wg.Add(1)
		go func() { defer wg.Done(); s.loop(ctx, run, component, successDelay) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.schedulerLoop(ctx, schedulerWake) }()
	for _, ingester := range s.Ingesters {
		if ingester != nil {
			wg.Add(1)
			go func() { defer wg.Done(); s.ingestLoop(ctx, ingester, schedulerWake) }()
		}
	}
	if s.Worker != nil {
		start(s.Worker.Work, "operations", s.workInterval)
	}
	<-ctx.Done()
	cancel()
	wg.Wait()
	return nil
}

func (s Service) schedulerLoop(ctx context.Context, wake <-chan struct{}) {
	contended := 0
	for ctx.Err() == nil {
		if err := s.Ticker.Tick(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.failure("scheduler", failureReason(err))
			delay := s.errorBackoff()
			if transientFailure(err) {
				contended++
				delay = s.contentionDelay(contended)
			} else {
				contended = 0
			}
			if !s.wait(ctx, delay) {
				return
			}
			// The next tick observes every durable change accumulated during the
			// backoff, so consume the coalesced edge before retrying.
			select {
			case <-wake:
			default:
			}
			continue
		}
		contended = 0
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-s.after(s.tickInterval()):
		}
	}
}

// contentionDelay paces the retry after a scheduler tick lost an optimistic
// concurrency race. The winning writer has already committed, so the state the
// next tick reads is newer and the condition has usually cleared in
// milliseconds; charging the full error backoff instead throws away that tick's
// admission decisions entirely. On 2026-08-01 a saturated host produced a
// commit conflict at least once a minute for eight consecutive minutes, and each
// one bought a five-second stall on a five-second tick — the queue paid twice
// for a condition that repaired itself.
//
// The delay doubles per consecutive conflict and saturates at the ordinary error
// backoff, so a conflict that is NOT clearing degrades to exactly today's
// behaviour rather than spinning against the store it is contending with. The
// growth also walks the retry off the fixed cadence of the operations worker
// whose writes produce these conflicts, without introducing randomness into a
// loop AGENTS.md requires to stay deterministic.
func (s Service) contentionDelay(attempt int) time.Duration {
	backoff := s.errorBackoff()
	delay := contentionRetryBase << min(max(attempt, 1)-1, contentionRetryShiftLimit)
	return min(delay, backoff)
}

func (s Service) ingestLoop(ctx context.Context, ingester Ingester, wake chan<- struct{}) {
	for ctx.Err() == nil {
		changed, err := ingest(ctx, ingester)
		if changed {
			// A capacity-one edge coalesces a burst without losing the fact that
			// reconciliation must observe newly durable demand.
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.failure("ingest", failureReason(err))
		if !s.wait(ctx, s.errorBackoff()) {
			return
		}
	}
}

func ingest(ctx context.Context, ingester Ingester) (bool, error) {
	if changeIngester, ok := ingester.(ChangeIngester); ok {
		return changeIngester.IngestChanged(ctx)
	}
	err := ingester.Ingest(ctx)
	return err == nil, err
}

func (s Service) loop(ctx context.Context, run func(context.Context) error, component string, successDelay func() time.Duration) {
	for ctx.Err() == nil {
		delay := successDelay()
		if err := run(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.failure(component, failureReason(err))
			delay = s.errorBackoff()
		}
		if delay <= 0 {
			continue
		}
		if !s.wait(ctx, delay) {
			return
		}
	}
}

func (s Service) failure(component, reason string) {
	if s.OnFailure != nil {
		s.OnFailure(component, reason)
	}
}

func (s Service) wait(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.after(delay):
		return true
	}
}

func (s Service) tickInterval() time.Duration {
	if s.TickInterval <= 0 {
		return 20 * time.Second
	}
	return s.TickInterval
}
func (s Service) workInterval() time.Duration {
	if s.WorkInterval <= 0 {
		return 2 * time.Second
	}
	return s.WorkInterval
}
func (s Service) errorBackoff() time.Duration {
	if s.ErrorBackoff <= 0 {
		return 5 * time.Second
	}
	return s.ErrorBackoff
}
func (s Service) after(delay time.Duration) <-chan time.Time {
	if s.After == nil {
		return time.After(delay)
	}
	return s.After(delay)
}
