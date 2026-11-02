package daemon

import (
	"reflect"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// TestIssue470ScaleSetInstanceMetricsProjectsEveryRow covers the per-set
// projection deterministically. It used to run only when an authority tick in
// some other test happened to observe live instances, so its four statements
// flipped the 99% coverage gate from run to run on 2026-11-02.
func TestIssue470ScaleSetInstanceMetricsProjectsEveryRow(t *testing.T) {
	if got := scaleSetInstanceMetrics(nil); got != nil {
		t.Fatalf("an unobserved tick must stay nil, got %#v", got)
	}
	rows := []app.ScopeInstance{
		{Scope: "fleet-repo", Profile: "linux-4x8", ScaleSetID: 27, Count: 2},
		{Scope: "knee-repo", Profile: "linux-2x4", ScaleSetID: 23, Count: 0},
	}
	want := []telemetry.ScaleSetInstanceMetric{
		{Scope: "fleet-repo", ScaleSetID: 27, Profile: "linux-4x8", Count: 2},
		{Scope: "knee-repo", ScaleSetID: 23, Profile: "linux-2x4", Count: 0},
	}
	if got := scaleSetInstanceMetrics(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("metrics = %#v, want %#v", got, want)
	}
}
