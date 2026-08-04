package executor

import (
	"context"
	"fmt"
	"time"
)

// This file is the second half of the executor seam: how a node measures the
// machine it runs on. It was internal/adapters/macos's own vocabulary, which
// made the host contract a macOS type and left ADR 0034's Linux node with a
// probe that would have had to impersonate a macOS package to be read by the
// scheduler. The measurements are not macOS facts — available memory, free
// disk, swap pressure, load, idle CPU, and the machine's physical totals are
// what every operating system reports and what admission has always been
// decided on.
//
// The guardrail evaluation moves with the values on purpose. It is pure policy
// over a snapshot, it is identical on every node, and leaving it in the macOS
// adapter would have forced a Linux probe to either duplicate it or import it.

type Freshness string

const (
	Fresh       Freshness = "fresh"
	Stale       Freshness = "stale"
	Unavailable Freshness = "unavailable"
)

type HostSnapshot struct {
	Freshness         Freshness
	ObservedAt        time.Time
	AvailableMemoryMB int64
	FreeDiskGB        int64
	SwapUsedMB        int64
	SwapOuts          int64
	CPUidlePercent    float64
	LoadAverage       float64
	// SwapOutRatePerSecond is the page-out rate since the previous fresh
	// observation, the signal that distinguishes a host paging RIGHT NOW from one
	// merely carrying residue: macOS does not eagerly reclaim swap, so SwapUsedMB
	// behaves closer to a high-water mark than a current pressure reading.
	//
	// SwapOutRateObserved separates a measured zero from "no prior sample to
	// measure against". A rate needs two observations, so the first one after a
	// daemon start cannot have it, and consumers must fail closed on the level
	// rather than read an unmeasured rate as a quiet host.
	SwapOutRatePerSecond float64
	SwapOutRateObserved  bool
	// PhysicalCPU and PhysicalMemoryMB are the machine's real totals, used to
	// bound aggregate fleet reservations by the host that actually exists rather
	// than by a configured constant. Zero means the fact could not be read and
	// consumers must fall back to configuration: these are advisory, so an
	// unreadable total never degrades the observation and never masquerades as a
	// measurement of a zero-resource machine.
	PhysicalCPU      int64
	PhysicalMemoryMB int64
	Cause            error
}

// HostProbe is what a node's host measurement must provide: one whole-machine
// observation, taken now. It returns no error because an unreadable machine is
// not an absent one — the snapshot itself carries Freshness and Cause, so a
// probe that cannot read the host reports a stale or unavailable observation
// rather than a zero-resource machine (AGENTS.md §4).
type HostProbe interface {
	Snapshot(context.Context) HostSnapshot
}

type Guardrails struct {
	MinFreeDiskGB        int64
	MinAvailableMemoryMB int64
	MaxSwapUsedMB        int64
	MaxLoadAverage       float64
	MinCPUidlePercent    float64
}

type AdmissionRequest struct {
	MemoryMB int64
}

type AdmissionDecision struct {
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
func activelyPagingOut(snapshot HostSnapshot) bool {
	if !snapshot.SwapOutRateObserved {
		return true
	}
	return snapshot.SwapOutRatePerSecond > 0
}

func (g Guardrails) Evaluate(snapshot HostSnapshot, request AdmissionRequest) AdmissionDecision {
	if snapshot.Freshness != Fresh {
		return AdmissionDecision{Reason: fmt.Sprintf("host observation %s", snapshot.Freshness)}
	}
	if request.MemoryMB < 0 {
		return AdmissionDecision{Reason: "invalid requested memory"}
	}
	if snapshot.FreeDiskGB < g.MinFreeDiskGB {
		return AdmissionDecision{Reason: "disk reserve"}
	}
	if snapshot.AvailableMemoryMB-request.MemoryMB < g.MinAvailableMemoryMB {
		return AdmissionDecision{Reason: "memory reserve"}
	}
	if g.MaxSwapUsedMB > 0 && snapshot.SwapUsedMB > g.MaxSwapUsedMB && activelyPagingOut(snapshot) {
		return AdmissionDecision{Reason: "swap pressure"}
	}
	if g.MaxLoadAverage > 0 && snapshot.LoadAverage > g.MaxLoadAverage && snapshot.CPUidlePercent < g.MinCPUidlePercent {
		return AdmissionDecision{Reason: "cpu pressure"}
	}
	return AdmissionDecision{Allowed: true, Reason: "capacity available"}
}
