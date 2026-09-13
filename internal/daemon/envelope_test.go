package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

// TestEngineTickerPublishesTheEnvelopeTheTickComputed is the other half of
// issue #226's lesson, learned again as issue #263.
//
// The axis says why a head was refused. It does not say what it was refused
// AGAINST, and on 2026-08-23 that second fact took hours of SSH archaeology
// across six configuration knobs and was still got wrong. The envelope comes
// from the plan rather than being recomputed here, for the same reason the axis
// does: an operator must read the numbers the scheduler decided on.
func TestEngineTickerPublishesTheEnvelopeTheTickComputed(t *testing.T) {
	health := reservationHealthForTest(t)
	result := app.TickResult{At: time.Unix(1_700_000_000, 0).UTC(), Plan: scheduler.Plan{
		Status: scheduler.PlanReady,
		Envelope: scheduler.Envelope{
			Free:     domain.Resources{CPU: 2, MemoryMB: 9216, Slots: 3},
			AgedFree: domain.Resources{CPU: 6, MemoryMB: 9216, Slots: 4},
		},
	}}

	engineTicker{health: health}.recordEnvelope(result)

	published := health.Snapshot().Envelope
	if published.CPU != 2 || published.MemoryMiB != 9216 || published.Slots != 3 {
		t.Fatalf("the young envelope must be published verbatim: %#v", published)
	}
	if published.AgedCPU != 6 || published.AgedMemoryMiB != 9216 || published.AgedSlots != 4 {
		t.Fatalf("the aged envelope is the one a vector hold is judged in: %#v", published)
	}
}

// A tick whose observation was unusable computes no envelope, and the recorder
// must publish that absence rather than a zero row an operator would read as
// "no capacity".
func TestEngineTickerPublishesNothingForATickThatJudgedNoEnvelope(t *testing.T) {
	health := reservationHealthForTest(t)

	engineTicker{health: health}.recordEnvelope(app.TickResult{
		At: time.Unix(1_700_000_000, 0).UTC(), Plan: scheduler.Plan{Status: scheduler.PlanBlockedObservation}})

	if published := health.Snapshot().Envelope; published.CPU != 0 || published.AgedCPU != 0 {
		t.Fatalf("a blocked tick must publish no envelope: %#v", published)
	}
}

// A tick that retired dead rows says so. The rows leave the queue silently
// otherwise, and issue #315 asked for a named reason rather than silence.
func TestEngineTickerSaysWhenOverdueDemandWasExpired(t *testing.T) {
	var buffer bytes.Buffer
	reporter := newFailureReporter(&buffer, nil)
	ticker := engineTicker{health: reservationHealthForTest(t), reporter: reporter}

	ticker.recordOverdueExpiry(app.TickResult{ExpiredOverdue: 3})
	line := buffer.String()
	for _, want := range []string{"overdue demand expired", "count=3", "job_start_bound_passed"} {
		if !strings.Contains(line, want) {
			t.Fatalf("expiry line missing %q: %q", want, line)
		}
	}

	buffer.Reset()
	ticker.recordOverdueExpiry(app.TickResult{})
	if buffer.Len() != 0 {
		t.Fatalf("a tick that expired nothing must say nothing: %q", buffer.String())
	}
}
