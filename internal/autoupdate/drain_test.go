package autoupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func drainPolicy() DrainPolicy {
	return DrainPolicy{Enabled: true, PendingFor: 30 * time.Minute, MaxWait: 2 * time.Hour, Cooldown: time.Hour}
}

var drainStart = time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)

func TestDrainStartsOnlyAfterACandidateHasWaited(t *testing.T) {
	state := NewDrainState(drainPolicy())
	if action := state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 2}); action != DrainNone {
		t.Fatalf("drained on the first tick a candidate appeared: %s", action)
	}
	if action := state.Observe(DrainFacts{At: drainStart.Add(29 * time.Minute), CandidatePending: true, LiveInstances: 2}); action != DrainNone {
		t.Fatalf("drained before the pending bound elapsed: %s", action)
	}
	if state.Draining() {
		t.Fatal("reported draining before it started")
	}
	if action := state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 2}); action != DrainStart {
		t.Fatalf("never drained for a candidate that waited the full bound: %s", action)
	}
	if !state.Draining() {
		t.Fatal("a drain in progress is not reportable")
	}
	if repeat := state.Observe(DrainFacts{At: drainStart.Add(31 * time.Minute), CandidatePending: true, LiveInstances: 1}); repeat != DrainNone {
		t.Fatalf("started the same drain twice: %s", repeat)
	}
}

// A node with no work still has to wait out the pending bound: an update is not
// urgent enough to interrupt the normal rhythm of a node that is simply between
// jobs, and the updater's own gate will apply the moment it is quiescent anyway.
func TestDrainDoesNotShortCircuitOnAnIdleNode(t *testing.T) {
	state := NewDrainState(drainPolicy())
	if action := state.Observe(DrainFacts{At: drainStart, CandidatePending: true}); action != DrainNone {
		t.Fatalf("idle node drained immediately: %s", action)
	}
	if action := state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true}); action != DrainStart {
		t.Fatalf("idle node never drained: %s", action)
	}
}

// The drain is bounded. A guest that will not finish inside MaxWait must not
// hold the node at zero admission indefinitely — that trades a stale binary for
// a starved queue, which is worse.
func TestDrainAbandonsAtTheDeadlineAndCoolsDownBeforeRetrying(t *testing.T) {
	state := NewDrainState(drainPolicy())
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	if action := state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainStart {
		t.Fatalf("setup did not drain: %s", action)
	}
	drainingSince := drainStart.Add(30 * time.Minute)
	if action := state.Observe(DrainFacts{At: drainingSince.Add(119 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainNone {
		t.Fatalf("abandoned before the deadline: %s", action)
	}
	abandoned := drainingSince.Add(2 * time.Hour)
	if action := state.Observe(DrainFacts{At: abandoned, CandidatePending: true, LiveInstances: 1}); action != DrainStop {
		t.Fatalf("never abandoned an unreachable drain: %s", action)
	}
	if state.Draining() {
		t.Fatal("still reports draining after abandoning")
	}
	// Cooldown: the node serves normally rather than immediately re-draining.
	if action := state.Observe(DrainFacts{At: abandoned.Add(59 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainNone {
		t.Fatalf("re-drained inside the cooldown: %s", action)
	}
	// After the cooldown the pending bound applies again before a second attempt.
	if action := state.Observe(DrainFacts{At: abandoned.Add(time.Hour), CandidatePending: true, LiveInstances: 1}); action != DrainNone {
		t.Fatalf("re-drained without serving out the pending bound: %s", action)
	}
	if action := state.Observe(DrainFacts{At: abandoned.Add(time.Hour + 30*time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainStart {
		t.Fatalf("never retried the drain after cooling down: %s", action)
	}
}

// A candidate that disappears — rolled back, or applied by an operator — ends
// the drain at once. There is nothing left to reach.
func TestDrainStopsWhenTheCandidateGoesAway(t *testing.T) {
	state := NewDrainState(drainPolicy())
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1})
	if !state.Draining() {
		t.Fatal("setup did not drain")
	}
	if action := state.Observe(DrainFacts{At: drainStart.Add(31 * time.Minute), CandidatePending: false}); action != DrainStop {
		t.Fatalf("kept draining for a candidate that no longer exists: %s", action)
	}
	if state.Draining() {
		t.Fatal("still draining with no candidate")
	}
}

func TestDrainDisabledPolicyNeverDrainsAndReleasesAnActiveDrain(t *testing.T) {
	off := NewDrainState(DrainPolicy{})
	for minute := 0; minute <= 240; minute += 10 {
		if action := off.Observe(DrainFacts{At: drainStart.Add(time.Duration(minute) * time.Minute), CandidatePending: true}); action != DrainNone {
			t.Fatalf("disabled policy acted: %s", action)
		}
	}

	state := NewDrainState(drainPolicy())
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1})
	state.policy.Enabled = false
	if action := state.Observe(DrainFacts{At: drainStart.Add(31 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainStop {
		t.Fatalf("disabling the policy left the node refusing admission: %s", action)
	}
	if state.Draining() {
		t.Fatal("still draining after the policy was disabled")
	}
}

// A clock correction must not hand the bound elapsed time nobody waited through.
func TestDrainTreatsABackwardClockAsARestartedWait(t *testing.T) {
	state := NewDrainState(drainPolicy())
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	if action := state.Observe(DrainFacts{At: drainStart.Add(-time.Hour), CandidatePending: true, LiveInstances: 1}); action != DrainNone {
		t.Fatalf("a backward clock produced an action: %s", action)
	}
	corrected := drainStart.Add(-time.Hour)
	if action := state.Observe(DrainFacts{At: corrected.Add(29 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainNone {
		t.Fatalf("drained on time the corrected clock never measured: %s", action)
	}
	if action := state.Observe(DrainFacts{At: corrected.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainStart {
		t.Fatalf("never drained after the corrected clock earned the bound: %s", action)
	}
}

func TestDrainNilStateIsInert(t *testing.T) {
	var absent *DrainState
	if absent.Observe(DrainFacts{At: drainStart, CandidatePending: true}) != DrainNone {
		t.Fatal("a nil state acted")
	}
	if absent.Draining() || !absent.Since().IsZero() {
		t.Fatal("a nil state reported a drain")
	}
	if got := DrainNone.String(); got != "none" {
		t.Fatalf("DrainNone renders as %q", got)
	}
}

func TestPendingCandidateNamesTheNewestUnappliedGeneration(t *testing.T) {
	releases := func(names ...string) func(string) ([]string, error) {
		return func(string) ([]string, error) { return names, nil }
	}
	for _, test := range []struct {
		name    string
		running string
		dir     func(string) ([]string, error)
		want    string
	}{
		{"one newer", "v0.1.498+main.abc", releases("v0.1.498+main.abc", "v0.1.510+main.def"), "v0.1.510+main.def"},
		{"newest of several", "v0.1.461+main.a", releases("v0.1.461+main.a", "v0.1.487+main.b", "v0.1.510+main.c", "v0.1.498+main.d"), "v0.1.510+main.c"},
		{"nothing newer", "v0.1.510+main.c", releases("v0.1.461+main.a", "v0.1.510+main.c"), ""},
		{"only the running one", "v0.1.510+main.c", releases("v0.1.510+main.c"), ""},
		{"older siblings only", "v0.1.510+main.c", releases("v0.1.498+main.a", "v0.1.461+main.b"), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending, newest := PendingCandidate("/root", test.running, test.dir)
			if newest != test.want || pending != (test.want != "") {
				t.Fatalf("PendingCandidate = (%v, %q), want (%v, %q)", pending, newest, test.want != "", test.want)
			}
		})
	}
}

// A fact this node could not establish must never become a reason to refuse
// admission, so every unreadable input reports nothing pending.
func TestPendingCandidateReportsNothingItCannotEstablish(t *testing.T) {
	failing := func(string) ([]string, error) { return nil, errors.New("unreadable") }
	garbage := func(string) ([]string, error) { return []string{"not-a-version", "", "v..", "current"}, nil }
	for _, test := range []struct {
		name    string
		root    string
		running string
		dir     func(string) ([]string, error)
	}{
		{"unreadable releases directory", "/root", "v0.1.1+main.a", failing},
		{"unparseable directory names", "/root", "v0.1.1+main.a", garbage},
		{"no root", "", "v0.1.1+main.a", garbage},
		{"no running version", "/root", "", garbage},
		{"no reader", "/root", "v0.1.1+main.a", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if pending, newest := PendingCandidate(test.root, test.running, test.dir); pending || newest != "" {
				t.Fatalf("PendingCandidate = (%v, %q), want (false, \"\")", pending, newest)
			}
		})
	}
}

func TestReleaseDirNamesListsOnlyDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v0.1.510+main.a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	names, err := ReleaseDirNames(root)
	if err != nil {
		t.Fatalf("ReleaseDirNames: %v", err)
	}
	if len(names) != 1 || names[0] != "v0.1.510+main.a" {
		t.Fatalf("ReleaseDirNames = %v, want just the release directory", names)
	}
	if _, err := ReleaseDirNames(filepath.Join(root, "absent")); err == nil {
		t.Fatal("an absent directory was reported as readable")
	}
}

func TestDrainActionsRenderForTheOperatorLog(t *testing.T) {
	for action, want := range map[DrainAction]string{DrainNone: "none", DrainStart: "start", DrainStop: "stop"} {
		if got := action.String(); got != want {
			t.Fatalf("DrainAction(%d).String() = %q, want %q", action, got, want)
		}
	}
}

// Since dates the current phase so an operator reads how long a drain has been
// waiting, not merely that it is.
func TestDrainSinceTracksTheCurrentPhase(t *testing.T) {
	state := NewDrainState(drainPolicy())
	if !state.Since().IsZero() {
		t.Fatal("a fresh state dated a phase that never began")
	}
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	if !state.Since().Equal(drainStart) {
		t.Fatalf("pending phase dated %v, want %v", state.Since(), drainStart)
	}
	drained := drainStart.Add(30 * time.Minute)
	state.Observe(DrainFacts{At: drained, CandidatePending: true, LiveInstances: 1})
	if !state.Since().Equal(drained) {
		t.Fatalf("drain dated %v, want the moment it started (%v)", state.Since(), drained)
	}
}

// A candidate that disappears during a cooldown clears it, so the node is not
// still serving out a penalty for a drain that no longer has a target when the
// next candidate arrives.
func TestDrainCooldownClearsWithTheCandidate(t *testing.T) {
	state := NewDrainState(drainPolicy())
	state.Observe(DrainFacts{At: drainStart, CandidatePending: true, LiveInstances: 1})
	state.Observe(DrainFacts{At: drainStart.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1})
	abandoned := drainStart.Add(2*time.Hour + 30*time.Minute)
	if action := state.Observe(DrainFacts{At: abandoned, CandidatePending: true, LiveInstances: 1}); action != DrainStop {
		t.Fatalf("setup did not abandon: %s", action)
	}

	// The candidate goes away mid-cooldown.
	if action := state.Observe(DrainFacts{At: abandoned.Add(time.Minute), CandidatePending: false}); action != DrainNone {
		t.Fatalf("clearing a candidate during cooldown produced an action: %s", action)
	}

	// A new candidate now only has to serve out the pending bound.
	fresh := abandoned.Add(2 * time.Minute)
	state.Observe(DrainFacts{At: fresh, CandidatePending: true, LiveInstances: 1})
	if action := state.Observe(DrainFacts{At: fresh.Add(30 * time.Minute), CandidatePending: true, LiveInstances: 1}); action != DrainStart {
		t.Fatalf("a new candidate was held behind a stale cooldown: %s", action)
	}
}
