package daemon

import (
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
)

// residualAdvertiser carries the scheduler's latest view of the node's residual
// envelope to every scale-set session, so each long poll advertises only what
// that set holds plus what the node can still start (issue #388, ADR 0073).
//
// It is one value for the whole node, written by the ticker and read by every
// binding's poll, for the same reason the session-yield state is: the envelope
// is a property of the machine, not of any one scale set, and every set must
// read the same conclusion the scheduler admitted on.
//
// Sets that poll concurrently each read the same residual and may each
// advertise it, so a node can be offered that residual once per set, and only
// until the poll that carried the stale figure ends: the first placement is
// charged on the next tick and every later poll advertises net of it. That
// bound is deliberate and ADR 0073 says why: dividing the residual between sets
// would leave a single-node scope waiting on an idle machine, and serialising it
// through a lease would let a set with no work hold the residual hostage for
// its whole poll.
type residualAdvertiser struct {
	now    func() time.Time
	maxAge time.Duration

	mu       sync.Mutex
	at       time.Time
	headroom map[int64]app.ScaleSetHeadroom
}

func newResidualAdvertiser(now func() time.Time, maxAge time.Duration) *residualAdvertiser {
	return &residualAdvertiser{now: now, maxAge: maxAge}
}

// observe records one tick. A tick that did not produce a ready plan forgets
// the previous view rather than keeping it: every set falls back to its
// configured capacity until the scheduler can see the node again.
func (a *residualAdvertiser) observe(result app.TickResult, ready bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.at, a.headroom = time.Time{}, nil
	if !ready || result.Headroom == nil {
		return
	}
	a.at = result.At
	a.headroom = make(map[int64]app.ScaleSetHeadroom, len(result.Headroom))
	for _, row := range result.Headroom {
		a.headroom[row.StoreKey] = row
	}
}

// provider is the per-poll capacity of one scale set. A view older than maxAge
// is unknown: a scheduler that has stopped ticking must not pin every set to
// the residual of a moment long past.
func (a *residualAdvertiser) provider(storeKey int64, configured int) func() int {
	return func() int {
		a.mu.Lock()
		defer a.mu.Unlock()
		row, known := a.headroom[storeKey]
		if known {
			now := a.now()
			known = !a.at.After(now) && now.Sub(a.at) <= a.maxAge
		}
		return app.AdvertisedCapacity(configured, row, known)
	}
}
