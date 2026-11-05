package domain

import (
	"testing"
	"time"
)

func TestEmptyExitBackoffIsToleratedThenDoublesToItsCap(t *testing.T) {
	last := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		count    int
		exceeded bool
		hold     time.Duration
	}{
		{count: 0}, {count: 1}, {count: 2},
		{count: 3, exceeded: true, hold: 5 * time.Minute},
		{count: 4, exceeded: true, hold: 10 * time.Minute},
		{count: 5, exceeded: true, hold: 20 * time.Minute},
		{count: 6, exceeded: true, hold: 30 * time.Minute},
		{count: 500, exceeded: true, hold: 30 * time.Minute},
	} {
		exits := EmptyExits{Count: test.count, Last: last}
		if got := EmptyExitBackoff.Exceeded(exits); got != test.exceeded {
			t.Errorf("count %d: exceeded = %v", test.count, got)
		}
		if got := EmptyExitBackoff.RetryAt(exits).Sub(last); got != test.hold {
			t.Errorf("count %d: hold = %s, want %s", test.count, got, test.hold)
		}
	}
}

func TestAnUnstatedEmptyExitBoundHoldsNothing(t *testing.T) {
	last := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	if (EmptyExitPolicy{}).Exceeded(EmptyExits{Count: 100, Last: last}) {
		t.Fatal("a zero policy states no bound and may hold nothing")
	}
	if EmptyExitBackoff.Exceeded(EmptyExits{Count: 100}) {
		t.Fatal("exits with no instant cannot date a hold")
	}
}
