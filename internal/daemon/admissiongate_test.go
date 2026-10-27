package daemon

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

var gateClock = time.Date(2026, 10, 27, 6, 20, 0, 0, time.UTC)

func gateTick(at time.Time, allowed bool, reason string) app.TickResult {
	return app.TickResult{At: at, Host: domain.Host{Pressure: domain.HostPressure{AvailableMemoryMB: 8192,
		FreeDiskGB: 200, CPUIdlePercent: 50, LoadAverage: 1, AdmissionAllowed: allowed, AdmissionReason: reason}}}
}

func gateLines(buffer *bytes.Buffer) []string {
	text := strings.TrimSpace(buffer.String())
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// TestAdmissionGateLogsEachTransitionOnce is issue #436. Omarchy's swap gate
// flapped for two hours on 2026-10-27 and stderr said nothing, so no "delivered
// while gated" sighting could be dated against the gate. Each edge is one line;
// a steady gate is silent however many ticks it holds.
func TestAdmissionGateLogsEachTransitionOnce(t *testing.T) {
	var buffer bytes.Buffer
	health := reservationHealthForTest(t)
	ticker := engineTicker{health: health, reporter: newFailureReporter(&buffer, nil), gate: &admissionGate{}}

	ticker.recordMetrics(gateTick(gateClock, true, "capacity available"))
	ticker.recordMetrics(gateTick(gateClock.Add(time.Minute), true, "capacity available"))
	if lines := gateLines(&buffer); len(lines) != 0 {
		t.Fatalf("an open gate the daemon started with is not a transition: %q", lines)
	}
	if since := health.Snapshot().HostPressure.AdmissionSince; !since.Equal(gateClock) {
		t.Fatalf("admissionSince=%v, want the first observation %v", since, gateClock)
	}

	closed := gateClock.Add(15 * time.Minute)
	opened := gateClock.Add(25 * time.Minute)
	reclosed := gateClock.Add(95 * time.Minute)
	ticks := []app.TickResult{
		gateTick(closed, false, "swap pressure"),
		gateTick(closed.Add(time.Minute), false, "swap pressure"),
		gateTick(opened, true, "capacity available"),
		gateTick(opened.Add(time.Minute), true, "capacity available"),
		gateTick(reclosed, false, "swap pressure"),
		gateTick(reclosed.Add(time.Minute), false, "swap pressure"),
	}
	for _, tick := range ticks {
		ticker.recordMetrics(tick)
	}
	lines := gateLines(&buffer)
	if len(lines) != 3 {
		t.Fatalf("closed->open->closed must be exactly three lines, got %d: %q", len(lines), lines)
	}
	for index, want := range [][]string{
		{"admission gate closed", `reason="swap pressure"`},
		{"admission gate opened", `after="swap pressure"`, "closedFor=10m0s"},
		{"admission gate closed", `reason="swap pressure"`, "openFor=1h10m0s"},
	} {
		for _, fragment := range want {
			if !strings.Contains(lines[index], fragment) {
				t.Fatalf("line %d missing %q: %q", index, fragment, lines[index])
			}
		}
	}
	if since := health.Snapshot().HostPressure.AdmissionSince; !since.Equal(reclosed) {
		t.Fatalf("admissionSince=%v, want the last transition %v", since, reclosed)
	}
}

// A closed gate whose reason changes is a new refusal: the operator chasing swap
// must learn the node is now out of disk, and the clock restarts with it.
func TestAdmissionGateLogsAChangedReasonWhileClosed(t *testing.T) {
	var buffer bytes.Buffer
	health := reservationHealthForTest(t)
	ticker := engineTicker{health: health, reporter: newFailureReporter(&buffer, nil), gate: &admissionGate{}}

	ticker.recordMetrics(gateTick(gateClock, false, "swap pressure"))
	ticker.recordMetrics(gateTick(gateClock.Add(time.Minute), false, "disk reserve"))
	ticker.recordMetrics(gateTick(gateClock.Add(2*time.Minute), false, "disk reserve"))

	lines := gateLines(&buffer)
	if len(lines) != 2 || !strings.Contains(lines[0], `reason="swap pressure"`) ||
		!strings.Contains(lines[1], `reason="disk reserve"`) || !strings.Contains(lines[1], `after="swap pressure"`) {
		t.Fatalf("a changed refusal must be named once: %q", lines)
	}
	if since := health.Snapshot().HostPressure.AdmissionSince; !since.Equal(gateClock.Add(time.Minute)) {
		t.Fatalf("admissionSince=%v, want the reason change", since)
	}
}

// An unobserved host has no gate state. It is neither a transition nor a reset:
// a probe that went stale for one tick must not restart the clock or log twice.
func TestAdmissionGateIgnoresAnUnobservedHost(t *testing.T) {
	var buffer bytes.Buffer
	ticker := engineTicker{health: reservationHealthForTest(t), reporter: newFailureReporter(&buffer, nil),
		gate: &admissionGate{}}

	ticker.recordMetrics(gateTick(gateClock, false, "swap pressure"))
	ticker.recordMetrics(app.TickResult{At: gateClock.Add(time.Minute)})
	ticker.recordMetrics(gateTick(gateClock.Add(2*time.Minute), false, "swap pressure"))

	if lines := gateLines(&buffer); len(lines) != 1 {
		t.Fatalf("an unobserved tick split one closure into %d lines: %q", len(lines), lines)
	}
}

// The update drain closes the gate on a healthy host. It is a refusal like any
// other, and the line must say so rather than leave the drain to be inferred.
func TestAdmissionGateNamesAnUpdateDrain(t *testing.T) {
	var buffer bytes.Buffer
	ticker := engineTicker{health: reservationHealthForTest(t), reporter: newFailureReporter(&buffer, nil),
		gate: &admissionGate{}}

	ticker.recordMetrics(gateTick(gateClock, true, "capacity available"))
	ticker.recordMetrics(gateTick(gateClock.Add(time.Minute), false, "update drain"))

	if lines := gateLines(&buffer); len(lines) != 1 || !strings.Contains(lines[0], `reason="update drain"`) {
		t.Fatalf("the drain closure was not named: %q", lines)
	}
}

// A ticker without a gate — every harness that predates issue #436 — publishes
// pressure and says nothing.
func TestAdmissionGateIsOptional(t *testing.T) {
	var buffer bytes.Buffer
	health := reservationHealthForTest(t)
	ticker := engineTicker{health: health, reporter: newFailureReporter(&buffer, nil)}

	ticker.recordMetrics(gateTick(gateClock, false, "swap pressure"))
	if buffer.Len() != 0 || !health.Snapshot().HostPressure.AdmissionSince.IsZero() {
		t.Fatalf("a gateless ticker logged %q or dated the gate", buffer.String())
	}
	var absent *failureReporter
	absent.reportAdmissionGate(admissionTransition{Reason: "swap pressure"})
	var unset *admissionGate
	if _, changed := unset.Observe(gateClock, false, "swap pressure"); changed || !unset.Since().IsZero() {
		t.Fatal("a nil gate must observe nothing")
	}
}
