package macos

import "fmt"

type Guardrails struct {
	MinFreeDiskGB        int64
	MinAvailableMemoryMB int64
	MaxSwapUsedMB        int64
	MaxLoadAverage       float64
	MinCPUidlePercent    float64
}

type Request struct {
	MemoryMB int64
}

type Decision struct {
	Allowed bool
	Reason  string
}

// activelyPagingOut reports whether the host is paging out now, not merely
// carrying swap from an earlier burst. Exceeding the swap ceiling is a necessary
// but insufficient condition to refuse admission: macOS does not eagerly reclaim
// swap, so a level-only gate latches the fleet off a healthy host for as long as
// the residue persists.
//
// An unmeasured rate fails closed. Without a prior sample the level is the only
// evidence there is, and it must still block.
func activelyPagingOut(snapshot Snapshot) bool {
	if !snapshot.SwapOutRateObserved {
		return true
	}
	return snapshot.SwapOutRatePerSecond > 0
}

func (g Guardrails) Evaluate(snapshot Snapshot, request Request) Decision {
	if snapshot.Freshness != Fresh {
		return Decision{Reason: fmt.Sprintf("host observation %s", snapshot.Freshness)}
	}
	if request.MemoryMB < 0 {
		return Decision{Reason: "invalid requested memory"}
	}
	if snapshot.FreeDiskGB < g.MinFreeDiskGB {
		return Decision{Reason: "disk reserve"}
	}
	if snapshot.AvailableMemoryMB-request.MemoryMB < g.MinAvailableMemoryMB {
		return Decision{Reason: "memory reserve"}
	}
	if g.MaxSwapUsedMB > 0 && snapshot.SwapUsedMB > g.MaxSwapUsedMB && activelyPagingOut(snapshot) {
		return Decision{Reason: "swap pressure"}
	}
	if g.MaxLoadAverage > 0 && snapshot.LoadAverage > g.MaxLoadAverage && snapshot.CPUidlePercent < g.MinCPUidlePercent {
		return Decision{Reason: "cpu pressure"}
	}
	return Decision{Allowed: true, Reason: "capacity available"}
}
