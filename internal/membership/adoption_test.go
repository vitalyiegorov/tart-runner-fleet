package membership

import (
	"testing"
	"time"
)

func strandedSighting(since, audited time.Time) Sighting {
	return Sighting{Key: set19, Equivalent: true, Assigned: 1, Busy: 1, StrandedSince: since, AuditedAt: audited}
}

func TestASiblingMayAskOnlyOnItsOwnFreshEvidenceAfterTheGrace(t *testing.T) {
	p := AdoptionPolicy{Cadence: cadence}
	if p.Grace() != 2*cadence || p.Lease() != 4*cadence {
		t.Fatalf("grace=%s lease=%s", p.Grace(), p.Lease())
	}
	now := t0.Add(2 * cadence)
	if !p.MayAsk(strandedSighting(t0, now), now, true) {
		t.Fatal("a set stranded past the grace on fresh evidence may be asked for")
	}
	cases := map[string]func(*Sighting) (time.Time, bool){
		"within the grace":        func(s *Sighting) (time.Time, bool) { s.AuditedAt = t0.Add(cadence); return now, true },
		"one audit, then silence": func(s *Sighting) (time.Time, bool) { s.AuditedAt = t0; return t0.Add(2 * cadence), true },
		"own admission refuses":   func(s *Sighting) (time.Time, bool) { return now, false },
		"not equivalent":          func(s *Sighting) (time.Time, bool) { s.Equivalent = false; return now, true },
		"a runner is registered":  func(s *Sighting) (time.Time, bool) { s.Registered = 1; return now, true },
		"holds no work":           func(s *Sighting) (time.Time, bool) { s.Assigned, s.Busy = 0, 0; return now, true },
		"never seen stranded":     func(s *Sighting) (time.Time, bool) { s.StrandedSince = time.Time{}; return now, true },
		"audit never completed":   func(s *Sighting) (time.Time, bool) { s.AuditedAt = time.Time{}; return now, true },
		"audit too old":           func(s *Sighting) (time.Time, bool) { return now.Add(3 * cadence), true },
		"stranded after audited":  func(s *Sighting) (time.Time, bool) { s.StrandedSince = now.Add(time.Minute); return now, true },
		"busy alone with runner":  func(s *Sighting) (time.Time, bool) { s.Assigned = 0; s.Registered = 2; return now, true },
		"assigned alone, no busy": func(s *Sighting) (time.Time, bool) { s.Busy = 0; s.Registered = 1; return now, true },
	}
	for name, mutate := range cases {
		s := strandedSighting(t0, now)
		at, admission := mutate(&s)
		if p.MayAsk(s, at, admission) {
			t.Fatalf("%s: a sibling must not ask", name)
		}
	}
}

func TestOnlyAnObtainedSessionAdopts(t *testing.T) {
	p := AdoptionPolicy{Cadence: cadence}
	for _, answer := range []Arbiter{Held, Unanswered} {
		if _, ok := p.Arbitrate(set19, answer, t0); ok {
			t.Fatalf("%s must not adopt: a 409 proves the owner alive and an error proves nothing", answer)
		}
	}
	adoption, ok := p.Arbitrate(set19, Obtained, t0)
	if !ok || adoption.Key != set19 || adoption.Until != t0.Add(4*cadence) {
		t.Fatalf("obtained = %+v %v", adoption, ok)
	}
	if adoption.Release(t0.Add(time.Minute), false) {
		t.Fatal("a busy adoption within its lease is kept")
	}
	if !adoption.Release(t0.Add(time.Minute), true) || !adoption.Release(adoption.Until, false) {
		t.Fatal("an adoption is released when idle and in any case at its lease")
	}
}
