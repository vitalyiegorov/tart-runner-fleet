package operations

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type workerStore struct {
	Store
	mutex          sync.Mutex
	claimed        []Operation
	claimErr       error
	completeErr    error
	retryErr       error
	renewErr       error
	claimLimit     int
	completed      int
	retried        int
	dead           int
	renewed        int
	renewedSignal  chan struct{}
	retryAvailable time.Time
}

func (s *workerStore) Claim(_ context.Context, _ string, limit int, _ time.Time, _ time.Duration) ([]Operation, error) {
	s.claimLimit = limit
	return s.claimed, s.claimErr
}

func (s *workerStore) Complete(context.Context, string, string, string, time.Time) (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.completed++
	return true, s.completeErr
}

func (s *workerStore) Retry(_ context.Context, _, _, _ string, available time.Time, dead bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.retried++
	s.retryAvailable = available
	if dead {
		s.dead++
	}
	return s.retryErr
}

func (s *workerStore) RenewOperation(context.Context, string, string, time.Time, time.Duration) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.renewed++
	if s.renewed == 1 && s.renewedSignal != nil {
		close(s.renewedSignal)
	}
	return s.renewErr
}

func workerOperation(id, kind string) Operation {
	return Operation{ID: id, Kind: kind, EffectKey: id, Status: OperationClaimed}
}

func TestWorkerBoundedConcurrentExecutionAndCompletion(t *testing.T) {
	store := &workerStore{claimed: []Operation{workerOperation("one", "run"), workerOperation("two", "run")}}
	barrier := make(chan struct{})
	started := make(chan struct{}, 2)
	worker := Worker{Store: store, Owner: "worker", MaxConcurrent: 2, OperationDeadline: time.Second, Executors: map[string]Executor{
		"run": ExecutorFunc(func(context.Context, Operation) error { started <- struct{}{}; <-barrier; return nil }),
	}}
	done := make(chan struct{})
	var completed int
	var err error
	go func() { completed, err = worker.RunOnce(context.Background()); close(done) }()
	<-started
	<-started
	close(barrier)
	<-done
	if err != nil || completed != 2 || store.completed != 2 || store.claimLimit != 2 {
		t.Fatalf("bounded worker: completed=%d store=%d limit=%d err=%v", completed, store.completed, store.claimLimit, err)
	}
}

func TestWorkerRetriesTimeoutPanicUnknownAndPersistenceFailure(t *testing.T) {
	now := time.Unix(700, 0).UTC()
	tests := []struct {
		name     string
		op       Operation
		executor Executor
		dead     int
	}{
		{"timeout", workerOperation("timeout", "run"), ExecutorFunc(func(ctx context.Context, _ Operation) error { <-ctx.Done(); return ctx.Err() }), 0},
		{"panic", workerOperation("panic", "run"), ExecutorFunc(func(context.Context, Operation) error { panic("boom") }), 0},
		{"unknown", workerOperation("unknown", "missing"), nil, 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &workerStore{claimed: []Operation{test.op}}
			executors := map[string]Executor{}
			if test.executor != nil {
				executors["run"] = test.executor
			}
			worker := Worker{Store: store, Owner: "worker", OperationDeadline: time.Millisecond, Executors: executors, Now: func() time.Time { return now }, Retry: RetryPolicy{MaxAttempts: 2}}
			if completed, err := worker.RunOnce(context.Background()); err != nil || completed != 0 || store.retried != 1 || store.dead != test.dead {
				t.Fatalf("result completed=%d retry=%d dead=%d err=%v", completed, store.retried, store.dead, err)
			}
		})
	}
	store := &workerStore{claimed: []Operation{workerOperation("failure", "run")}, retryErr: errors.New("disk full")}
	worker := Worker{Store: store, Owner: "worker", Executors: map[string]Executor{"run": ExecutorFunc(func(context.Context, Operation) error { return errors.New("effect") })}}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("retry persistence failure ignored")
	}
}

func TestWorkerRenewsLeaseAndFencesLostLease(t *testing.T) {
	renewed := make(chan struct{})
	store := &workerStore{claimed: []Operation{workerOperation("renew", "run")}, renewedSignal: renewed}
	tick := func(time.Duration) <-chan time.Time {
		channel := make(chan time.Time, 1)
		channel <- time.Now()
		return channel
	}
	worker := Worker{Store: store, Owner: "worker", LeaseFor: time.Minute, RenewEvery: time.Second, After: tick, Executors: map[string]Executor{
		"run": ExecutorFunc(func(context.Context, Operation) error { <-renewed; return nil }),
	}}
	if completed, err := worker.RunOnce(context.Background()); err != nil || completed != 1 || store.renewed == 0 {
		t.Fatalf("lease not renewed: %d %d %v", completed, store.renewed, err)
	}
	store = &workerStore{claimed: []Operation{workerOperation("lost", "run")}, renewErr: ErrLeaseLost, renewedSignal: make(chan struct{})}
	worker.Store = store
	worker.Executors["run"] = ExecutorFunc(func(context.Context, Operation) error { <-store.renewedSignal; return nil })
	if completed, err := worker.RunOnce(context.Background()); completed != 0 || !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("lost lease not fenced: %d %v", completed, err)
	}
}

func TestWorkerValidationDefaultsAndHelpers(t *testing.T) {
	if _, err := (Worker{}).RunOnce(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid worker: %v", err)
	}
	store := &workerStore{claimErr: errors.New("database down")}
	worker := Worker{Store: store, Owner: "worker"}
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("claim error ignored")
	}
	if worker.concurrency() != 1 || worker.leaseFor() != time.Minute || worker.renewEvery() != 20*time.Second || worker.operationDeadline() != 30*time.Second || worker.now().Location() != time.UTC {
		t.Fatal("worker defaults mismatch")
	}
	worker.OperationDeadline = -1
	worker.RenewEvery = 2 * time.Minute
	if worker.operationDeadline() != 0 || worker.renewEvery() != 20*time.Second {
		t.Fatal("worker override validation mismatch")
	}
	if err := executeSafely(context.Background(), nil, Operation{}); err == nil {
		t.Fatal("nil executor panic escaped")
	}
	store = &workerStore{claimed: []Operation{workerOperation("no-deadline", "run")}}
	worker = Worker{Store: store, Owner: "worker", OperationDeadline: -1, Executors: map[string]Executor{"run": ExecutorFunc(func(context.Context, Operation) error { return nil })}}
	if completed, err := worker.RunOnce(context.Background()); err != nil || completed != 1 {
		t.Fatalf("deadline disabled: %d %v", completed, err)
	}
}
