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
	// FreeInodes and TotalInodes are the inode counts of the same filesystem
	// FreeDiskGB measures. Free bytes do not say whether a file can be created:
	// on 2026-10-25 a node with 29 GiB free had no inode left, and every guest
	// it started failed with ENOSPC (issue #423, ADR 0083). TotalInodes zero is
	// a filesystem with no fixed inode limit — btrfs allocates them on demand
	// and reports none — for which the inode floor does not apply. It is never
	// an unread value: a probe that cannot read inodes degrades the observation.
	FreeInodes     int64
	TotalInodes    int64
	SwapUsedMB     int64
	SwapOuts       int64
	CPUidlePercent float64
	LoadAverage    float64
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
	// MemoryStallPercent is the share of the last ten seconds in which at least
	// one task waited on memory: Linux PSI's `some avg10` from
	// /proc/pressure/memory. It is what says whether swap over the ceiling is
	// pressure or cold residue the kernel keeps rotating (issue #480, ADR 0102).
	//
	// It is advisory. MemoryStallObserved false is a host that does not report
	// it (macOS, a kernel without PSI) or a reading the probe could not parse;
	// it never degrades the observation, and the swap guard then decides exactly
	// as it did before the stall existed.
	MemoryStallPercent  float64
	MemoryStallObserved bool
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
	MinFreeDiskGB int64
	// MinFreeInodePercent is the share of the guest-storage filesystem's inodes
	// that must stay free. Zero is an unset floor.
	MinFreeInodePercent  float64
	MinAvailableMemoryMB int64
	MaxSwapUsedMB        int64
	// SwapOutPagesPerSecond is the swap-out rate at or above which a host over
	// its swap ceiling is paging out. Zero is DefaultSwapOutPagesPerSecond.
	SwapOutPagesPerSecond float64
	// SwapOutHardPagesPerSecond is the swap-out rate at or above which a host
	// over its swap ceiling is under pressure whatever its memory stall reads.
	// Zero is DefaultSwapOutHardPagesPerSecond.
	SwapOutHardPagesPerSecond float64
	// MemoryStallPercent is the observed memory stall at or above which swap
	// over the ceiling and paging below the hard rate is pressure. Zero is
	// DefaultMemoryStallPercent.
	MemoryStallPercent float64
	MaxLoadAverage     float64
	MinCPUidlePercent  float64
}

// DefaultSwapOutPagesPerSecond is the swap-out rate, averaged over the interval
// between two observations, that counts as paging out (issue #451, ADR 0089).
// It is ten pages a second: 200 pages across a 20-second poll, against the one
// page every twenty seconds (0.046/s) a Linux kernel trickles out of a host
// with 23 GB available, and far below the thousands a thrashing host pages out.
const DefaultSwapOutPagesPerSecond = 10

// DefaultMemoryStallPercent is the observed memory stall (PSI `some avg10`) at
// or above which a host over its swap ceiling and paging out below the hard
// rate is under pressure (issue #480, ADR 0102). Cold residue on 2026-11-19 read 0.54% and 0.18%;
// Android's lmkd treats about 7% as medium pressure and systemd-oomd acts at
// 60% sustained for 30 seconds, so 10% sits above residue and below the point
// where either acts. It is not calibrated against a recorded thrash.
const DefaultMemoryStallPercent = 10

// DefaultSwapOutHardPagesPerSecond is the swap-out rate at or above which no
// memory stall reading can exempt a host (issue #480, ADR 0102). Omarchy paged
// out 9563 pages/s at a stall of 1.67% on 2026-11-19 while a model trained, so
// a low stall does not rule out real paging; cold residue rotated out at about
// 50 pages/s. 1000 is the rate the owner's stopgap already trusted as the line
// between the two.
const DefaultSwapOutHardPagesPerSecond = 1000

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
// Paging out means a rate at or above the threshold, not any page at all. A host
// that holds old swap keeps trickling pages out in the background, and reading
// that noise floor as pressure flapped the gate all day on a node with 23 GB
// available (issue #451, ADR 0089).
//
// An unmeasured rate fails closed. Without a prior sample the level is the only
// evidence there is, and it must still block, unless an observed memory stall
// says the level is residue (pagingCorroborated, ADR 0102).
func (g Guardrails) activelyPagingOut(snapshot HostSnapshot) bool {
	if !snapshot.SwapOutRateObserved {
		return true
	}
	threshold := g.SwapOutPagesPerSecond
	if threshold <= 0 {
		threshold = DefaultSwapOutPagesPerSecond
	}
	return snapshot.SwapOutRatePerSecond >= threshold
}

// pagingCorroborated reports whether paging out over the ceiling is pressure
// rather than cold residue. Swap used and a modest swap-out rate both
// mis-measure a host whose swap is full of cold pages: the level never drains
// and the kernel keeps rotating those pages out, at 50 a second on
// ubuntu-vitalii on 2026-11-19 while no task waited on memory (issue #480, ADR
// 0102). The stall measures the waiting, but only below the hard rate: Omarchy
// paged out 9563 pages/s at a 1.67% stall the same day, so a measured rate at
// or above SwapOutHardPagesPerSecond is pressure whatever the stall reads.
//
// An unobserved stall corroborates, so a host that does not report one keeps
// ADR 0089's rule exactly. A stall observed below the threshold also overrides
// an unmeasured rate: it is a positive single-sample reading that the level is
// residue, so the first tick after a restart admits a quiet host.
func (g Guardrails) pagingCorroborated(snapshot HostSnapshot) bool {
	hard := g.SwapOutHardPagesPerSecond
	if hard <= 0 {
		hard = DefaultSwapOutHardPagesPerSecond
	}
	if !snapshot.MemoryStallObserved || (snapshot.SwapOutRateObserved && snapshot.SwapOutRatePerSecond >= hard) {
		return true
	}
	threshold := g.MemoryStallPercent
	if threshold <= 0 {
		threshold = DefaultMemoryStallPercent
	}
	return snapshot.MemoryStallPercent >= threshold
}

// inodesBelowFloor reports whether the filesystem's free inodes are under the
// configured share of its total. A filesystem with no fixed inode limit
// (TotalInodes zero) has no floor to fall below.
func (g Guardrails) inodesBelowFloor(snapshot HostSnapshot) bool {
	if g.MinFreeInodePercent <= 0 || snapshot.TotalInodes <= 0 {
		return false
	}
	return float64(snapshot.FreeInodes)*100 < g.MinFreeInodePercent*float64(snapshot.TotalInodes)
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
	if g.inodesBelowFloor(snapshot) {
		return AdmissionDecision{Reason: "inode reserve"}
	}
	if snapshot.AvailableMemoryMB-request.MemoryMB < g.MinAvailableMemoryMB {
		return AdmissionDecision{Reason: "memory reserve"}
	}
	if g.MaxSwapUsedMB > 0 && snapshot.SwapUsedMB > g.MaxSwapUsedMB && g.activelyPagingOut(snapshot) &&
		g.pagingCorroborated(snapshot) {
		return AdmissionDecision{Reason: "swap pressure"}
	}
	if g.MaxLoadAverage > 0 && snapshot.LoadAverage > g.MaxLoadAverage && snapshot.CPUidlePercent < g.MinCPUidlePercent {
		return AdmissionDecision{Reason: "cpu pressure"}
	}
	return AdmissionDecision{Allowed: true, Reason: "capacity available"}
}
