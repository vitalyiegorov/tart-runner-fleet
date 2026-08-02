package telemetry

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func failureHealth(t *testing.T) (*Health, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(70_000, 0).UTC()}
	health, err := NewHealth(clock, HealthConfig{Profiles: []string{"small"},
		FailureComponents: []string{"scheduler", "ingest", "operations"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return health, clock
}

// TestComponentFailuresCountEveryOccurrence is the counter the 2026-08-02
// incident needed. The failure reporter logs at most one line per component and
// reason per minute, so the only durable record of a hot loop understated it by
// the tick rate: eight log lines stood for roughly a hundred failed ticks, and
// nothing distinguished a loop failing once from one failing continuously.
func TestComponentFailuresCountEveryOccurrence(t *testing.T) {
	health, _ := failureHealth(t)

	for range 5 {
		health.RecordComponentFailure("scheduler", "plan_commit_failed")
	}
	health.RecordComponentFailure("scheduler", "plan_commit_contended")
	health.RecordComponentFailure("ingest", "message_poll_failed")

	want := []ComponentFailure{
		{Component: "ingest", Reason: "message_poll_failed", Count: 1},
		{Component: "scheduler", Reason: "plan_commit_contended", Count: 1},
		{Component: "scheduler", Reason: "plan_commit_failed", Count: 5},
	}
	if got := health.Snapshot().ComponentFailures; !reflect.DeepEqual(got, want) {
		t.Fatalf("ComponentFailures = %#v, want %#v (sorted by component then reason)", got, want)
	}
}

// TestComponentFailureCardinalityIsBounded keeps the label space closed. The
// component is a static literal authored in internal/app and the reason is a
// closed vocabulary, so anything else is a wiring mistake and must not become a
// new time series.
func TestComponentFailureCardinalityIsBounded(t *testing.T) {
	health, _ := failureHealth(t)

	for _, testCase := range []struct {
		name               string
		component, reason  string
		wantComponentCount int
	}{
		{name: "an unconfigured component is dropped", component: "intruder", reason: "plan_commit_failed"},
		{name: "an empty component is dropped", reason: "plan_commit_failed"},
		{name: "a configured component is counted", component: "ingest", reason: "session_expired", wantComponentCount: 1},
		{name: "an unclassified failure is still counted", component: "ingest", wantComponentCount: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			health.RecordComponentFailure(testCase.component, testCase.reason)
			if got := len(health.Snapshot().ComponentFailures); got != testCase.wantComponentCount {
				t.Fatalf("series = %d, want %d", got, testCase.wantComponentCount)
			}
		})
	}
	// A failure the classifier could not name still has to be visible, under a
	// bounded token rather than an empty label.
	found := false
	for _, failure := range health.Snapshot().ComponentFailures {
		if failure.Reason == UnclassifiedFailureReason {
			found = true
		}
	}
	if !found {
		t.Fatalf("unclassified failure missing: %#v", health.Snapshot().ComponentFailures)
	}
}

// TestComponentFailuresAreRenderedAsACounter pins the exposition. The existing
// fleet_observation_fresh gauge self-heals within one tick, so a failure loop
// that recovers between scrapes is invisible; a monotonic counter is what an
// alert can rate().
func TestComponentFailuresAreRenderedAsACounter(t *testing.T) {
	health, _ := failureHealth(t)
	health.RecordComponentFailure("scheduler", "plan_commit_rejected")
	health.RecordComponentFailure("scheduler", "plan_commit_rejected")

	rendered := renderMetrics(health.Snapshot())

	for _, want := range []string{
		"# TYPE fleet_component_failures_total counter",
		`fleet_component_failures_total{component="scheduler",reason="plan_commit_rejected"} 2`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("metrics missing %q:\n%s", want, rendered)
		}
	}
}

// TestComponentFailuresAreAbsentUntilOneHappens keeps a healthy fleet's
// exposition free of empty series, matching how fleet_operation_failures is
// only written when there is something to report.
func TestComponentFailuresAreAbsentUntilOneHappens(t *testing.T) {
	health, _ := failureHealth(t)

	if rendered := renderMetrics(health.Snapshot()); strings.Contains(rendered, "fleet_component_failures_total") {
		t.Fatalf("healthy fleet exposes a failure series:\n%s", rendered)
	}
}
