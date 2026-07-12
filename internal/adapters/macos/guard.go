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
	if g.MaxSwapUsedMB > 0 && snapshot.SwapUsedMB > g.MaxSwapUsedMB {
		return Decision{Reason: "swap pressure"}
	}
	if g.MaxLoadAverage > 0 && snapshot.LoadAverage > g.MaxLoadAverage && snapshot.CPUidlePercent < g.MinCPUidlePercent {
		return Decision{Reason: "cpu pressure"}
	}
	return Decision{Allowed: true, Reason: "capacity available"}
}
