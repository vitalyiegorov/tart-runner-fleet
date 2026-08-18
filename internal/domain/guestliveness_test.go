package domain

import (
	"testing"
	"time"
)

var probeNow = time.Date(2026, 8, 16, 17, 43, 20, 0, time.UTC)

func probePolicy() GuestLivenessPolicy {
	return GuestLivenessPolicy{ConsecutiveRefusals: 5, Window: 90 * time.Second}
}

// The host-side reproduction of issue #236: the container started at 17:43:20,
// the guest panicked 306 seconds later, and from that instant every `tart exec`
// was refused immediately. Five refusals thirty seconds apart cross both bounds.
func TestAnUnbrokenRunOfRefusalsDeclaresTheGuestDead(t *testing.T) {
	policy, state := probePolicy(), GuestLivenessState{}
	panicAt := probeNow.Add(306 * time.Second)
	for probe := range 5 {
		at := panicAt.Add(time.Duration(probe) * 30 * time.Second)
		state = policy.Observe(state, GuestLivenessRefused, at)
		if probe < 4 && policy.Confirmed(state, at) {
			t.Fatalf("probe %d of 5 must not yet be a verdict; got %#v", probe+1, state)
		}
	}
	last := panicAt.Add(4 * 30 * time.Second)
	if !policy.Confirmed(state, last) {
		t.Fatalf("five refusals over 120s must satisfy a 5/90s bound; got %#v", state)
	}
	if state.RefusedSince != panicAt || state.LastProbe != last {
		t.Fatalf("the accumulator must carry the probe timeline; got %#v", state)
	}
	if silence, measured := state.Silence(last); !measured || silence != 120*time.Second {
		t.Fatalf("the silence must be measurable as 120s; got %v measured=%v", silence, measured)
	}
}

// The design risk, at the level the policy owns it. An inconclusive probe is not
// a hard failure, so it can never extend a run toward a verdict — a saturated
// guest whose probe keeps running out of its own deadline stays alive forever.
func TestAnInconclusiveProbeClearsTheRunItCannotConfirm(t *testing.T) {
	policy := probePolicy()
	state := GuestLivenessState{}
	for probe := range 4 {
		state = policy.Observe(state, GuestLivenessRefused, probeNow.Add(time.Duration(probe)*time.Minute))
	}
	state = policy.Observe(state, GuestLivenessUnknown, probeNow.Add(4*time.Minute))
	if state.Refusals != 0 || !state.RefusedSince.IsZero() {
		t.Fatalf("an unknown outcome must clear the run; got %#v", state)
	}
	if policy.Confirmed(state, probeNow.Add(time.Hour)) {
		t.Fatalf("a cleared run can never be a verdict; got %#v", state)
	}
	// Ten more inconclusive probes over an hour still establish nothing.
	for probe := range 10 {
		state = policy.Observe(state, GuestLivenessUnknown, probeNow.Add(time.Duration(probe)*6*time.Minute))
		if policy.Confirmed(state, probeNow.Add(time.Hour)) {
			t.Fatalf("inconclusive probes must never accumulate; got %#v", state)
		}
	}
}

func TestOneAnswerEndsTheRunAndRecordsTheGuestAlive(t *testing.T) {
	policy := probePolicy()
	state := GuestLivenessState{}
	for probe := range 4 {
		state = policy.Observe(state, GuestLivenessRefused, probeNow.Add(time.Duration(probe)*time.Minute))
	}
	alive := probeNow.Add(4 * time.Minute)
	state = policy.Observe(state, GuestLivenessAlive, alive)
	if state.Refusals != 0 || !state.RefusedSince.IsZero() || state.LastAlive != alive {
		t.Fatalf("an answered probe must clear the run and record the answer; got %#v", state)
	}
	if _, measured := state.Silence(alive); measured {
		t.Fatalf("a guest that answered is not silent at all; got %#v", state)
	}
}

func TestGuestLivenessVerdictIsFailClosed(t *testing.T) {
	dead := GuestLivenessState{Refusals: 9, RefusedSince: probeNow.Add(-time.Hour), LastProbe: probeNow}
	for _, test := range []struct {
		name   string
		policy GuestLivenessPolicy
		state  GuestLivenessState
		now    time.Time
	}{
		{name: "no bound at all", policy: GuestLivenessPolicy{}, state: dead, now: probeNow},
		{name: "count without a window", policy: GuestLivenessPolicy{ConsecutiveRefusals: 5}, state: dead, now: probeNow},
		{name: "window without a count", policy: GuestLivenessPolicy{Window: time.Minute}, state: dead, now: probeNow},
		{name: "never probed", policy: probePolicy(), state: GuestLivenessState{}, now: probeNow},
		{name: "run start unrecorded", policy: probePolicy(), now: probeNow,
			state: GuestLivenessState{Refusals: 9, LastProbe: probeNow}},
		{name: "run start in the future", policy: probePolicy(), now: probeNow,
			state: GuestLivenessState{Refusals: 9, RefusedSince: probeNow.Add(time.Hour)}},
		{name: "count met, window not", policy: probePolicy(), now: probeNow,
			state: GuestLivenessState{Refusals: 9, RefusedSince: probeNow.Add(-89 * time.Second)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.policy.Confirmed(test.state, test.now) {
				t.Fatalf("%s must not be a verdict; got %#v against %#v", test.name, test.state, test.policy)
			}
		})
	}
	if (GuestLivenessPolicy{}).Enabled() || !probePolicy().Enabled() {
		t.Fatal("a policy is enabled exactly when it states both bounds")
	}
}
