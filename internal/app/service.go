package app

import (
	"context"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type TickRunner interface{ Tick(context.Context) error }
type Ingester interface{ Ingest(context.Context) error }
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
	var wg sync.WaitGroup
	start := func(run func(context.Context) error, component string, successDelay func() time.Duration) {
		wg.Add(1)
		go func() { defer wg.Done(); s.loop(ctx, run, component, successDelay) }()
	}
	start(s.Ticker.Tick, "scheduler", s.tickInterval)
	for _, ingester := range s.Ingesters {
		if ingester != nil {
			start(ingester.Ingest, "ingest", func() time.Duration { return 0 })
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

func (s Service) loop(ctx context.Context, run func(context.Context) error, component string, successDelay func() time.Duration) {
	for ctx.Err() == nil {
		delay := successDelay()
		if err := run(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if s.OnFailure != nil {
				s.OnFailure(component)
			}
			delay = s.errorBackoff()
		}
		if delay <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.after(delay):
		}
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
