package replay_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

const issue7QueueToPlanBound = time.Second

type issue7Replay struct {
	mu             sync.Mutex
	demands        []domain.Demand
	ingestCalls    int
	idleObserved   chan struct{}
	burstCommitted chan struct{}
	planReady      chan scheduler.Plan
	profiles       map[domain.ProfileID]domain.Profile
	now            time.Time
}

func (r *issue7Replay) Tick(context.Context) error {
	r.mu.Lock()
	demands := append([]domain.Demand(nil), r.demands...)
	r.mu.Unlock()
	if len(demands) == 0 {
		select {
		case <-r.idleObserved:
		default:
			close(r.idleObserved)
		}
		return nil
	}
	plan := scheduler.PlanTick(scheduler.Input{
		Now: r.now,
		Config: scheduler.Config{
			LinuxCapacity: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4},
			FairnessAge:   5 * time.Minute,
			RepoCaps:      map[string]int{"a/repo": 4},
			Profiles:      r.profiles,
		},
		Demands:   domain.Fresh(demands, r.now),
		Instances: domain.Fresh([]domain.Instance(nil), r.now),
		Host:      domain.Fresh(domain.Host{Available: domain.Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}}, r.now),
	})
	select {
	case r.planReady <- plan:
	default:
	}
	return nil
}

func (r *issue7Replay) Ingest(ctx context.Context) error {
	r.mu.Lock()
	r.ingestCalls++
	call := r.ingestCalls
	r.mu.Unlock()
	if call > 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.idleObserved:
	}
	r.mu.Lock()
	r.demands = issue7Burst(r.now, r.profiles)
	r.mu.Unlock()
	close(r.burstCommitted)
	return nil
}

func issue7Burst(now time.Time, profiles map[domain.ProfileID]domain.Profile) []domain.Demand {
	profileIDs := []domain.ProfileID{"medium", "medium", "large"}
	result := make([]domain.Demand, 0, len(profileIDs))
	for index, profileID := range profileIDs {
		profile := profiles[profileID]
		result = append(result, domain.Demand{
			Key:       domain.DemandKey{Repo: "a/repo", RunID: 7, Attempt: 1, JobID: int64(index + 1)},
			CreatedAt: now.Add(-time.Duration(len(profileIDs)-index) * time.Second),
			Platform:  profile.Platform,
			Profile:   profile.ID,
			Route:     profile.Route,
			Event:     domain.EventPullRequest,
			RunStatus: domain.RunQueued,
		})
	}
	return result
}

// TestIssue7IdleHostBurstPlansBeforePeriodicTick is intentionally red until a
// committed demand wakes reconciliation instead of waiting for the next poll.
func TestIssue7IdleHostBurstPlansBeforePeriodicTick(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	profiles := map[domain.ProfileID]domain.Profile{
		"medium": {ID: "medium", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}},
		"large":  {ID: "large", Platform: domain.PlatformLinux, Route: "tiered", Resources: domain.Resources{CPU: 4, MemoryMB: 8_192, Slots: 1}},
	}
	replay := &issue7Replay{
		idleObserved:   make(chan struct{}),
		burstCommitted: make(chan struct{}),
		planReady:      make(chan scheduler.Plan, 1),
		profiles:       profiles,
		now:            now,
	}
	periodicTick := make(chan time.Time)
	timerArmed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (app.Service{
			Ticker:       replay,
			Ingesters:    []app.Ingester{replay},
			TickInterval: 20 * time.Second,
			After: func(time.Duration) <-chan time.Time {
				select {
				case timerArmed <- struct{}{}:
				default:
				}
				return periodicTick
			},
		}).Run(ctx)
	}()

	<-replay.burstCommitted
	<-timerArmed
	timer := time.NewTimer(issue7QueueToPlanBound)
	defer timer.Stop()
	var plan scheduler.Plan
	metBound := true
	select {
	case plan = <-replay.planReady:
	case <-timer.C:
		metBound = false
		periodicTick <- now
		plan = <-replay.planReady
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("service shutdown: %v", err)
	}
	want := []domain.DemandKey{
		{Repo: "a/repo", RunID: 7, Attempt: 1, JobID: 1},
		{Repo: "a/repo", RunID: 7, Attempt: 1, JobID: 2},
		{Repo: "a/repo", RunID: 7, Attempt: 1, JobID: 3},
	}
	got := make([]domain.DemandKey, 0, len(plan.Operations))
	for _, operation := range plan.Operations {
		if operation.Kind == scheduler.OperationSpawn {
			got = append(got, operation.Demand)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issue #7 burst plan = %#v, want %#v", got, want)
	}
	if !metBound {
		t.Fatalf("issue #7: idle-host medium/medium/large burst exceeded %s queue-to-plan bound; scheduler planned only after the periodic tick", issue7QueueToPlanBound)
	}
}
