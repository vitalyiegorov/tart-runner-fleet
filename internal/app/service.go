package app

import (
	"context"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
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
	// OnFailure deliberately receives only a bounded component name. This
	// prevents an upstream error from reflecting a token or JIT secret to logs.
	OnFailure func(component string)
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
	for ctx.Err() == nil {
		if err := s.Ticker.Tick(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.failure("scheduler")
			if !s.wait(ctx, s.errorBackoff()) {
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
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-s.after(s.tickInterval()):
		}
	}
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
		s.failure("ingest")
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
			s.failure(component)
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

func (s Service) failure(component string) {
	if s.OnFailure != nil {
		s.OnFailure(component)
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
