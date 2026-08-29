package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestEveryUsablePlanPublishesTheEnvelopeItJudged is issue #263's ask.
//
// The mac studio held a reservation on the `vector` axis for 75 minutes for a
// head of 2 CPU / 4096 MiB while every published field said that head fitted.
// Answering "against what?" meant re-deriving the envelope from the host budget,
// the Linux-when-macOS-idle cap, the capacity model, the guard clamps and the
// live cohort — over SSH, by hand, and the answer that came back was wrong. A
// tick that judged anything has to say what it judged against.
func TestEveryUsablePlanPublishesTheEnvelopeItJudged(t *testing.T) {
	plan := PlanTick(input([]domain.Demand{demand("a/repo", 1, time.Minute, "small")}, nil, State{}))

	if plan.Envelope.AgedFree == (domain.Resources{}) {
		t.Fatal("a usable tick must publish the envelope its decision rests on")
	}
	if plan.Envelope.Free.CPU > plan.Envelope.AgedFree.CPU {
		t.Fatalf("the young envelope can never exceed the aged one: %#v", plan.Envelope)
	}
}

// A tick that could not read its own world judged nothing, and must publish no
// envelope rather than a zero one: absence reads as "not judged", and a zero row
// would read as "no capacity".
func TestABlockedPlanPublishesNoEnvelope(t *testing.T) {
	blocked := input([]domain.Demand{demand("a/repo", 1, time.Minute, "small")}, nil, State{})
	blocked.Demands = domain.Stale(blocked.Demands.Value, testNow.Add(-time.Hour), "stale")

	if plan := PlanTick(blocked); plan.Envelope != (Envelope{}) {
		t.Fatalf("a blocked tick judged nothing and must publish nothing: %#v", plan.Envelope)
	}
}

// The envelope is diagnostic, so it must not enter the plan identity. If it did,
// every corpus digest in ADR 0031's harness would move for a field no decision
// reads back.
func TestTheEnvelopeDoesNotEnterThePlanIdentity(t *testing.T) {
	in := input([]domain.Demand{demand("a/repo", 1, time.Minute, "small")}, nil, State{})
	planned := PlanTick(in)

	bare := planTick(in)
	if bare.ID != planned.ID {
		t.Fatalf("attaching the envelope moved the plan identity: %q vs %q", bare.ID, planned.ID)
	}
}
