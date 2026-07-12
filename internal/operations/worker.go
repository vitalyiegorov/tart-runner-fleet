package operations

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func errorsJoin(executionErr, storeErr error) error {
	return fmt.Errorf("execution failed: %v; persist retry: %w", executionErr, storeErr)
}

type Executor interface {
	Execute(context.Context, Operation) error
}

type ExecutorFunc func(context.Context, Operation) error

func (f ExecutorFunc) Execute(ctx context.Context, operation Operation) error {
	return f(ctx, operation)
}

// Worker claims a bounded batch and waits for every claimed operation to reach
// a durable terminal or retry state before returning.
type Worker struct {
	Store             Store
	Executors         map[string]Executor
	Owner             string
	MaxConcurrent     int
	LeaseFor          time.Duration
	RenewEvery        time.Duration
	OperationDeadline time.Duration
	Retry             RetryPolicy
	Now               func() time.Time
	After             func(time.Duration) <-chan time.Time
}

func (w Worker) RunOnce(ctx context.Context) (int, error) {
	if w.Store == nil || w.Owner == "" {
		return 0, ErrInvalid
	}
	claimed, err := w.Store.Claim(ctx, w.Owner, w.concurrency(), w.now(), w.leaseFor())
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	var mutex sync.Mutex
	completed := 0
	var firstErr error
	for _, operation := range claimed {
		operation := operation
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := w.runOperation(ctx, operation)
			mutex.Lock()
			defer mutex.Unlock()
			if done {
				completed++
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}()
	}
	wg.Wait()
	return completed, firstErr
}

func (w Worker) runOperation(parent context.Context, operation Operation) (bool, error) {
	executor, ok := w.Executors[operation.Kind]
	if !ok {
		err := w.Store.Retry(context.WithoutCancel(parent), operation.ID, w.Owner, "unknown operation kind", w.now(), true)
		return false, err
	}
	ctx := parent
	cancel := func() {}
	if deadline := w.operationDeadline(); deadline > 0 {
		ctx, cancel = context.WithTimeout(parent, deadline)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	defer cancel()

	stopRenewal := make(chan struct{})
	renewalDone := make(chan error, 1)
	go w.renewLease(ctx, cancel, operation.ID, stopRenewal, renewalDone)
	executionErr := executeSafely(ctx, executor, operation)
	close(stopRenewal)
	renewalErr := <-renewalDone
	if renewalErr != nil {
		return false, renewalErr
	}
	now := w.now()
	if executionErr != nil {
		next, retry := w.Retry.Next(operation.Attempts+1, now)
		if err := w.Store.Retry(context.WithoutCancel(parent), operation.ID, w.Owner, executionErr.Error(), next, !retry); err != nil {
			return false, errorsJoin(executionErr, err)
		}
		return false, nil
	}
	_, err := w.Store.Complete(context.WithoutCancel(parent), operation.ID, w.Owner, operation.EffectKey, now)
	return err == nil, err
}

func executeSafely(ctx context.Context, executor Executor, operation Operation) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("executor panic: %v", recovered)
		}
	}()
	return executor.Execute(ctx, operation)
}

func (w Worker) renewLease(ctx context.Context, cancel context.CancelFunc, id string, stop <-chan struct{}, done chan<- error) {
	for {
		select {
		case <-stop:
			done <- nil
			return
		case <-ctx.Done():
			done <- nil
			return
		case <-w.after(w.renewEvery()):
			if err := w.Store.RenewOperation(context.WithoutCancel(ctx), id, w.Owner, w.now(), w.leaseFor()); err != nil {
				cancel()
				done <- err
				return
			}
		}
	}
}

func (w Worker) concurrency() int {
	if w.MaxConcurrent <= 0 {
		return 1
	}
	return w.MaxConcurrent
}

func (w Worker) leaseFor() time.Duration {
	if w.LeaseFor <= 0 {
		return time.Minute
	}
	return w.LeaseFor
}

func (w Worker) renewEvery() time.Duration {
	if w.RenewEvery <= 0 || w.RenewEvery >= w.leaseFor() {
		return w.leaseFor() / 3
	}
	return w.RenewEvery
}

func (w Worker) operationDeadline() time.Duration {
	if w.OperationDeadline < 0 {
		return 0
	}
	if w.OperationDeadline == 0 {
		return 30 * time.Second
	}
	return w.OperationDeadline
}

func (w Worker) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}
	return w.Now().UTC()
}

func (w Worker) after(duration time.Duration) <-chan time.Time {
	if w.After == nil {
		return time.After(duration)
	}
	return w.After(duration)
}
