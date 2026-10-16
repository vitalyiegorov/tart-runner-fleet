package membership

import (
	"testing"
	"time"
)

var (
	t0      = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	cadence = 15 * time.Minute
	set19   = Key{Scope: "knee", ID: 19}
	set20   = Key{Scope: "knee", ID: 20}
)

func stationary() Policy { return Policy{Mode: Stationary, Cadence: cadence} }

// audit is one completed scale-set audit taken at `at`, read by the watchdog at
// `now`, in which `wedged` are the sets the ADR 0056 detector judged wedged.
func audit(now, at time.Time, wedged ...Key) Evidence {
	return Evidence{At: now, AuditedAt: at, Bound: []Key{set19, set20}, Wedged: wedged}
}

func TestParseModeAcceptsTheVocabularyAndRefusesEverythingElse(t *testing.T) {
	for raw, want := range map[string]Mode{"": Off, "stationary": Stationary, "roaming": Roaming} {
		got, err := ParseMode(raw)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseMode("laptop"); err == nil {
		t.Fatal("an unknown membership mode must be refused, not silently read as off")
	}
}

func TestTheLadderIsDerivedFromTheModeAndTheAuditCadence(t *testing.T) {
	s, r := stationary(), Policy{Mode: Roaming, Cadence: cadence}
	if s.HealAttempts() != 2 || r.HealAttempts() != 1 {
		t.Fatalf("heal attempts stationary=%d roaming=%d", s.HealAttempts(), r.HealAttempts())
	}
	if s.ExcludedFor(1) != 4*cadence || r.ExcludedFor(1) != 2*cadence {
		t.Fatalf("first exclusion stationary=%s roaming=%s", s.ExcludedFor(1), r.ExcludedFor(1))
	}
	if s.ExcludedFor(2) != 8*cadence || s.ExcludedFor(9) != 16*cadence || r.ExcludedFor(9) != 16*cadence {
		t.Fatalf("exclusion must double and stop at the cap: %s %s %s", s.ExcludedFor(2), s.ExcludedFor(9), r.ExcludedFor(9))
	}
	if s.ExcludedFor(0) != 4*cadence {
		t.Fatalf("a zero count is the first exclusion, got %s", s.ExcludedFor(0))
	}
	if Off.Enabled() || !Stationary.Enabled() || !Roaming.Enabled() {
		t.Fatal("only an absent mode is off")
	}
}

// A wedged set is healed a bounded number of times, each attempt judged by an
// audit taken AFTER it, then excluded, then rejoined on probation.
func TestAWedgedSetIsHealedThenExcludedThenRejoinedOnProbation(t *testing.T) {
	p := stationary()
	var state map[Key]Binding
	step := func(ev Evidence, want map[Key]Action) {
		t.Helper()
		var actions map[Key]Action
		state, actions = Step(p, state, ev)
		if len(actions) != len(want) {
			t.Fatalf("at %s actions = %v, want %v", ev.At.Sub(t0), actions, want)
		}
		for key, action := range want {
			if actions[key] != action {
				t.Fatalf("at %s action[%v] = %q, want %q (state %+v)", ev.At.Sub(t0), key, actions[key], action, state[key])
			}
		}
	}
	step(audit(t0, t0, set19), map[Key]Action{set19: Heal})
	if state[set19].Stage != Healing || state[set19].Attempts != 1 || state[set20].Stage != Serving {
		t.Fatalf("after the first wedged audit: %+v", state)
	}
	// The same audit read again is not new evidence: the heal it caused has not
	// been judged yet, so nothing may happen.
	step(audit(t0.Add(time.Minute), t0, set19), nil)
	step(audit(t0.Add(cadence), t0.Add(cadence), set19), map[Key]Action{set19: Heal})
	if state[set19].Attempts != 2 {
		t.Fatalf("second attempt not counted: %+v", state[set19])
	}
	step(audit(t0.Add(2*cadence), t0.Add(2*cadence), set19), map[Key]Action{set19: Exclude})
	if state[set19].Stage != Excluded || state[set19].Exclusions != 1 {
		t.Fatalf("bounded attempts exhausted must exclude: %+v", state[set19])
	}
	// Excluded is held for its bound whatever the audits say: the bound job is
	// still assigned and will keep the set reading wedged.
	step(audit(t0.Add(5*cadence), t0.Add(5*cadence), set19), nil)
	step(audit(t0.Add(6*cadence), t0.Add(5*cadence), set19), map[Key]Action{set19: Rejoin})
	if state[set19].Stage != Probation {
		t.Fatalf("an elapsed exclusion must rejoin on probation: %+v", state[set19])
	}
	// The audit that predates the rejoin cannot judge it.
	step(audit(t0.Add(6*cadence+time.Minute), t0.Add(5*cadence), set19), nil)
	// Wedged again after rejoining: excluded for twice as long.
	step(audit(t0.Add(7*cadence), t0.Add(7*cadence), set19), map[Key]Action{set19: Exclude})
	if state[set19].Exclusions != 2 {
		t.Fatalf("a probation failure must count a second exclusion: %+v", state[set19])
	}
	step(audit(t0.Add(14*cadence), t0.Add(14*cadence), set19), nil)
	step(audit(t0.Add(15*cadence), t0.Add(14*cadence)), map[Key]Action{set19: Rejoin})
	step(audit(t0.Add(16*cadence), t0.Add(16*cadence)), nil)
	if state[set19].Stage != Serving || state[set19].Exclusions != 0 || state[set19].Attempts != 0 {
		t.Fatalf("a clean audit after rejoining must restore full membership: %+v", state[set19])
	}
}

func TestAHealThatWorkedReturnsTheSetToServing(t *testing.T) {
	p := stationary()
	state, _ := Step(p, nil, audit(t0, t0, set19))
	state, actions := Step(p, state, audit(t0.Add(cadence), t0.Add(cadence)))
	if len(actions) != 0 || state[set19].Stage != Serving || state[set19].Attempts != 0 {
		t.Fatalf("a set no longer wedged must serve again: %+v %v", state[set19], actions)
	}
}

// Contributor rule 4: an audit that never completed, or one too old to describe
// the present, is not evidence that a set is healthy and not evidence that it is
// wedged. The ladder neither climbs nor descends on it.
func TestAnUnavailableOrStaleAuditNeverMovesTheLadder(t *testing.T) {
	p := stationary()
	if state, actions := Step(p, nil, Evidence{At: t0, Bound: []Key{set19}, Wedged: []Key{set19}}); len(actions) != 0 || state[set19].Stage != Serving {
		t.Fatalf("a node that never completed an audit must not heal: %+v %v", state, actions)
	}
	state, _ := Step(p, nil, audit(t0, t0, set19))
	stale := audit(t0.Add(4*cadence), t0.Add(cadence), set19)
	if p.Fresh(stale) {
		t.Fatal("an audit older than two cadences must not be fresh")
	}
	next, actions := Step(p, state, stale)
	if len(actions) != 0 || next[set19] != state[set19] {
		t.Fatalf("a stale audit must freeze the ladder: %+v -> %+v %v", state[set19], next[set19], actions)
	}
	cleanButStale := audit(t0.Add(4*cadence), t0.Add(cadence))
	if next, _ = Step(p, state, cleanButStale); next[set19].Stage != Healing {
		t.Fatalf("a stale clean audit must not be read as recovery: %+v", next[set19])
	}
	future := audit(t0, t0.Add(time.Hour), set19)
	if p.Fresh(future) {
		t.Fatal("an audit from the future is a clock fault, not evidence")
	}
}

func TestOnlyBoundSetsAreTrackedAndADroppedBindingIsForgotten(t *testing.T) {
	p := stationary()
	foreign := Key{Scope: "knee", ID: 7}
	state, actions := Step(p, nil, audit(t0, t0, foreign))
	if len(actions) != 0 {
		t.Fatalf("a set this node does not serve is never its to heal: %v", actions)
	}
	if _, ok := state[foreign]; ok || len(state) != 2 {
		t.Fatalf("state must hold exactly the bound sets: %+v", state)
	}
	state, _ = Step(p, state, Evidence{At: t0.Add(time.Minute), AuditedAt: t0, Bound: []Key{set20}})
	if _, ok := state[set19]; ok {
		t.Fatalf("a binding that is no longer configured must be forgotten: %+v", state)
	}
}

func TestAnExcludedSetRejoinsOnTimeEvenWithoutAFreshAudit(t *testing.T) {
	// Rejoining is the direction back to the node's configured behaviour, so it
	// needs elapsed time and nothing else: an audit outage must never hold a set
	// outside the fleet indefinitely.
	p := Policy{Mode: Roaming, Cadence: cadence}
	state, _ := Step(p, nil, audit(t0, t0, set19))
	state, actions := Step(p, state, audit(t0.Add(cadence), t0.Add(cadence), set19))
	if actions[set19] != Exclude {
		t.Fatalf("roaming allows one heal attempt: %v %+v", actions, state[set19])
	}
	_, actions = Step(p, state, Evidence{At: t0.Add(3 * cadence), Bound: []Key{set19, set20}})
	if actions[set19] != Rejoin {
		t.Fatalf("an elapsed exclusion must rejoin without an audit: %v", actions)
	}
}

func TestAClockThatMovedBackwardsDoesNotSatisfyAnExclusionBound(t *testing.T) {
	p := Policy{Mode: Roaming, Cadence: cadence}
	state, _ := Step(p, nil, audit(t0, t0, set19))
	state, _ = Step(p, state, audit(t0.Add(cadence), t0.Add(cadence), set19))
	if _, actions := Step(p, state, Evidence{At: t0.Add(-time.Hour), Bound: []Key{set19}}); len(actions) != 0 {
		t.Fatalf("a backwards clock must not rejoin: %v", actions)
	}
}

func TestSummaryIsTheWorstStageAndListsOnlyBindingsNotServing(t *testing.T) {
	if got := Summarize(nil); got.State != Serving || len(got.Bindings) != 0 {
		t.Fatalf("no bindings summarise as serving: %+v", got)
	}
	state := map[Key]Binding{
		set19:               {Stage: Probation, Since: t0},
		set20:               {Stage: Serving},
		{"a", 1}:            {Stage: Excluded, Since: t0, Exclusions: 1},
		{"a", 2}:            {Stage: Healing, Since: t0, Attempts: 1},
		{Scope: "b", ID: 3}: {Stage: Healing},
	}
	got := Summarize(state)
	if got.State != Excluded || len(got.Bindings) != 4 {
		t.Fatalf("summary = %+v", got)
	}
	if got.Bindings[0].Key != (Key{"a", 1}) || got.Bindings[3].Key != set19 {
		t.Fatalf("rows must be ordered by scope then id: %+v", got.Bindings)
	}
	keys := []Key{set20, {Scope: "a", ID: 9}, set19}
	SortKeys(keys)
	if keys[0].Scope != "a" || keys[1] != set19 || keys[2] != set20 {
		t.Fatalf("SortKeys = %v", keys)
	}
	if Summarize(map[Key]Binding{set19: {Stage: Healing}, set20: {Stage: Probation}}).State != Healing {
		t.Fatal("healing outranks probation")
	}
}
