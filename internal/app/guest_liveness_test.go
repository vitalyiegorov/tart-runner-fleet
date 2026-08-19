package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// fakeGuestProbe answers per instance, in order, and records what it was asked.
type fakeGuestProbe struct {
	mu       sync.Mutex
	answers  map[string][]domain.GuestLiveness
	fallback domain.GuestLiveness
	asked    []string
}

func (f *fakeGuestProbe) Probe(_ context.Context, id string) domain.GuestLiveness {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, id)
	queue := f.answers[id]
	if len(queue) == 0 {
		return f.fallback
	}
	f.answers[id] = queue[1:]
	return queue[0]
}

func (f *fakeGuestProbe) probed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func trackerPolicy() domain.GuestLivenessPolicy {
	return domain.GuestLivenessPolicy{ConsecutiveRefusals: 3, Window: time.Minute}
}

func TestTheTrackerAccumulatesRefusalsAcrossTicks(t *testing.T) {
	probe := &fakeGuestProbe{fallback: domain.GuestLivenessRefused}
	start := time.Date(2026, 8, 16, 17, 48, 26, 0, time.UTC)
	clock := start
	tracker := &GuestLivenessTracker{Probe: probe, Policy: trackerPolicy(), Now: func() time.Time { return clock }}

	var state domain.GuestLivenessState
	for tick := range 3 {
		clock = start.Add(time.Duration(tick) * 30 * time.Second)
		states := tracker.Observe(context.Background(), []string{"trf-xl-1"})
		state = states["trf-xl-1"]
	}
	if state.Refusals != 3 || state.RefusedSince != start {
		t.Fatalf("three ticks of refusal must accumulate; got %#v", state)
	}
	if !trackerPolicy().Confirmed(state, start.Add(time.Minute)) {
		t.Fatalf("three refusals over a minute must satisfy the bound; got %#v", state)
	}
}

// An instance the tick no longer reports is forgotten, so a long-lived daemon
// cannot accumulate one accumulator per instance it has ever seen.
func TestTheTrackerForgetsAnInstanceItIsNoLongerAskedAbout(t *testing.T) {
	probe := &fakeGuestProbe{fallback: domain.GuestLivenessRefused}
	now := time.Now().UTC()
	clock := now
	tracker := &GuestLivenessTracker{Probe: probe, Policy: trackerPolicy(), Now: func() time.Time { return clock }}
	tracker.Observe(context.Background(), []string{"trf-xl-1", "trf-xl-2"})
	clock = now.Add(time.Minute)
	tracker.Observe(context.Background(), []string{"trf-xl-1"})
	clock = now.Add(2 * time.Minute)
	states := tracker.Observe(context.Background(), []string{"trf-xl-1", "trf-xl-2"})
	if states["trf-xl-1"].Refusals != 3 {
		t.Fatalf("a continuously reported instance keeps its run; got %#v", states["trf-xl-1"])
	}
	if states["trf-xl-2"].Refusals != 1 {
		t.Fatalf("an instance that vanished and returned starts over; got %#v", states["trf-xl-2"])
	}
	if len(states) != 2 {
		t.Fatalf("the accumulator holds exactly the instances it was asked about; got %#v", states)
	}
}

func TestTheTrackerProbesNothingWithoutABoundOrAProbe(t *testing.T) {
	probe := &fakeGuestProbe{fallback: domain.GuestLivenessRefused}
	for _, test := range []struct {
		name    string
		tracker *GuestLivenessTracker
		ids     []string
	}{
		{name: "nil tracker", tracker: nil, ids: []string{"trf-xl-1"}},
		{name: "no probe", tracker: &GuestLivenessTracker{Policy: trackerPolicy()}, ids: []string{"trf-xl-1"}},
		{name: "no bound", tracker: &GuestLivenessTracker{Probe: probe}, ids: []string{"trf-xl-1"}},
		{name: "nothing to probe", tracker: &GuestLivenessTracker{Probe: probe, Policy: trackerPolicy()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if states := test.tracker.Observe(context.Background(), test.ids); len(states) != 0 {
				t.Fatalf("%s must report nothing; got %#v", test.name, states)
			}
		})
	}
	if asked := probe.probed(); len(asked) != 0 {
		t.Fatalf("a disabled tracker must not probe at all; asked %v", asked)
	}
}

// The probe must never run against an instance whose recovery needs no probe: a
// stopped or absent VM is already reclaimable, and an instance that has not
// reached Running has no guest worth this question.
func TestOnlyARunningPoweredOnGuestIsProbed(t *testing.T) {
	probe := &fakeGuestProbe{fallback: domain.GuestLivenessAlive}
	running := inventoryInstance(operations.StateRunning)
	assigned := inventoryInstance(operations.StateAssigned)
	assigned.ID = "trf-small-2"
	stopped := inventoryInstance(operations.StateRunning)
	stopped.ID = "trf-small-3"
	now := time.Now().UTC()

	inv := ProductionInventory{
		Store: fakeInstances{values: []operations.Instance{running, assigned, stopped}},
		Executor: fakeTart{values: []executor.Instance{{Name: running.ID, Power: domain.InstancePowerRunning},
			{Name: assigned.ID, Power: domain.InstancePowerRunning}, {Name: stopped.ID, Power: domain.InstancePowerStopped}}},
		Host:     fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4},
		Guards:   executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2048},
		Guest:    &GuestLivenessTracker{Probe: probe, Policy: trackerPolicy()},
	}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() {
		t.Fatalf("instances = %#v", instances)
	}
	if asked := probe.probed(); len(asked) != 1 || asked[0] != running.ID {
		t.Fatalf("only the running, powered-on guest may be probed; asked %v", asked)
	}
	for _, instance := range instances.Value {
		probed := instance.Guest != (domain.GuestLivenessState{})
		if probed != (instance.ID == running.ID) {
			t.Fatalf("instance %s carries guest state %#v", instance.ID, instance.Guest)
		}
	}
}

// The whole mechanism must be absent when it is not wired: an unprobed guest is
// an unavailable observation, and an unavailable observation must never look
// like a measured refusal.
func TestAnUnwiredTrackerLeavesEveryGuestUnjudged(t *testing.T) {
	now := time.Now().UTC()
	inv := ProductionInventory{
		Store:    fakeInstances{values: []operations.Instance{inventoryInstance(operations.StateRunning)}},
		Executor: fakeTart{values: []executor.Instance{{Name: "trf-small-1", Power: domain.InstancePowerRunning}}},
		Host:     fakeHost{healthySnapshot(now)},
		Capacity: domain.Resources{CPU: 8, MemoryMB: 16384, Slots: 4},
		Guards:   executor.Guardrails{MinFreeDiskGB: 60, MinAvailableMemoryMB: 2048},
	}
	instances, _ := inv.Observe(context.Background())
	if !instances.Usable() || instances.Value[0].Guest != (domain.GuestLivenessState{}) {
		t.Fatalf("an unwired tracker must leave the guest state zero; got %#v", instances.Value)
	}
}
