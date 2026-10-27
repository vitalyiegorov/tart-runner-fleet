package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// Issue #404, seed 498 tick 148 of the container arm: the reserved head keeps
// its turn when its own scope's backfill puts a guest on the node.
//
// `b`'s `xl` was the aged head when it reserved the vector, and ADR 0057's key
// agreed: neither `b` nor `a` held a slot, so age decided. Backfill behind the
// reservation then admitted a `small` of `b`'s in the residual, so on the tick
// the vector freed `b` held one slot and `a` held none. Re-ranking the head by
// that occupancy would let the backfill take the head's turn, which ADR 0017
// forbids ("Backfill may use stranded capacity; it may never take the head's
// turn"); ADR 0057 leaves the reserved head it mints as it was, and amends the
// no-jump clause only to lend a CAP-held head's vector (ADR 0038). Property (s)
// called the admission a pass-over; that was an oracle defect.
func TestABackfilledScopeKeepsItsReservedHeadsTurn(t *testing.T) {
	reserved := demand("b/repo", 1, 66*time.Minute, "large")
	waiting := demand("a/repo", 2, 63*time.Minute, "large")
	prior := State{Reservation: &domain.Reservation{Demand: reserved.Key, Profile: reserved.Profile,
		Resources: testConfig().Profiles["large"].Resources, Since: testNow.Add(-10 * time.Minute)}}
	backfilled := []domain.Instance{runningLinux("b-small", "b/repo", "small")}
	// Seven cores free: one `large` fits, two do not.
	keys := spawnedKeys(PlanTick(input([]domain.Demand{reserved, waiting}, backfilled, prior)))
	if len(keys) != 1 || keys[0] != reserved.Key {
		t.Fatalf("admitted %#v, want only the reserved head %s", keys, reserved.Key)
	}
}
