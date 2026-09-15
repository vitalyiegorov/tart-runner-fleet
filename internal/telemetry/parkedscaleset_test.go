package telemetry

import (
	"strings"
	"testing"
	"time"
)

var auditedOn = time.Date(2026, 8, 4, 18, 30, 0, 0, time.UTC)

// studioSet is `trf-sudoku-builder-studio` on 2026-08-04: parked on the node
// whose configuration no longer names it, holding two assigned jobs and two busy
// runners that GitHub will offer to nobody else.
func studioSet() ParkedScaleSetMetric {
	return ParkedScaleSetMetric{Scope: "suuudokuuu", ScaleSetID: 7, Name: "trf-sudoku-builder-studio",
		Assigned: 2, Busy: 2, ObservedAt: auditedOn}
}

func parkedSnapshot(rows []ParkedScaleSetMetric, at time.Time) Snapshot {
	return Snapshot{Now: auditedOn, ParkedScaleSets: rows, ParkedScaleSetsAuditedAt: at}
}

// TestAParkedScaleSetHoldingWorkFailsTheCheck is issue #164's Case A as the
// doctor row an operator reads. Two jobs sat assigned to set 7 for 4.5 hours
// while `fleet doctor` returned PASS.
func TestAParkedScaleSetHoldingWorkFailsTheCheck(t *testing.T) {
	result := parkedScaleSetResult(parkedSnapshot([]ParkedScaleSetMetric{studioSet()}, auditedOn))

	if result.OK {
		t.Fatal("work GitHub has routed to a set nobody polls must not read as healthy")
	}
	detail := strings.Join(result.Reasons, " ")
	for _, want := range []string{"suuudokuuu", "scale set 7", "trf-sudoku-builder-studio",
		"2 assigned job(s)", "nothing can be listening", "cancel and re-run the workflow, or bind the set on a node"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the finding must say %q: %q", want, detail)
		}
	}
}

// A parked set holding nothing is the ordinary shape of shared-label federation
// (ADR 0034): the sibling node owns it, and this node cannot read the sibling's
// configuration. It is reported as a row and is never a finding.
func TestAnIdleParkedSetIsNotAFinding(t *testing.T) {
	idle := studioSet()
	idle.Assigned, idle.Busy = 0, 0

	if result := parkedScaleSetResult(parkedSnapshot([]ParkedScaleSetMetric{idle}, auditedOn)); !result.OK {
		t.Fatalf("a sibling's idle set is not this node's fault: %v", result.Reasons)
	}
}

// A busy runner with no assigned job still means something is listening, so it
// is the same finding: both halves are read, and either alone is enough.
func TestABusyRunnerAloneIsStillAFinding(t *testing.T) {
	busy := studioSet()
	busy.Assigned = 0

	if result := parkedScaleSetResult(parkedSnapshot([]ParkedScaleSetMetric{busy}, auditedOn)); result.OK {
		t.Fatal("a parked set running a job must be reported")
	}
}

// A node that has never audited is not a node that found nothing. An observe
// daemon never audits at all, and rendering that as a pass is exactly the
// silence issue #164 spent 4.5 hours inside.
func TestANodeThatNeverAuditedIsNotAPass(t *testing.T) {
	result := parkedScaleSetResult(parkedSnapshot([]ParkedScaleSetMetric{studioSet()}, time.Time{}))

	if !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("an unaudited node states nothing: %#v", result)
	}
}

// Findings are ordered, so two runs of one state read identically.
func TestParkedFindingsAreDeterministicallyOrdered(t *testing.T) {
	first, second := studioSet(), studioSet()
	first.Scope, second.Scope = "z-repo", "a-repo"

	result := parkedScaleSetResult(parkedSnapshot([]ParkedScaleSetMetric{first, second}, auditedOn))

	if len(result.Reasons) != 2 || !strings.HasPrefix(result.Reasons[0], "a-repo") {
		t.Fatalf("reasons must be ordered: %v", result.Reasons)
	}
}

// An audit that found nothing parked is still an audit: it publishes an empty
// set and a completion time, which is what separates "nothing is parked" from
// "nobody looked".
func TestAnAuditThatFoundNothingIsStillPublished(t *testing.T) {
	health, err := NewHealth(&fakeClock{now: auditedOn}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}

	if err := health.SetParkedScaleSets(nil, auditedOn); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot()
	if snapshot.ParkedScaleSetsAuditedAt.IsZero() || len(snapshot.ParkedScaleSets) != 0 {
		t.Fatalf("an empty audit is still an audit: %#v", snapshot.ParkedScaleSets)
	}
	if result := parkedScaleSetResult(snapshot); !result.OK {
		t.Fatalf("nothing parked is a pass: %v", result.Reasons)
	}

	if err := health.SetParkedScaleSets([]ParkedScaleSetMetric{studioSet()}, auditedOn); err != nil {
		t.Fatal(err)
	}
	if result := parkedScaleSetResult(health.Snapshot()); result.OK {
		t.Fatalf("the published audit must reach the check: %#v", health.Snapshot().ParkedScaleSets)
	}
}

// A mangled row is refused outright rather than published, for the reason every
// other metric refuses one: a nonsense count must never masquerade as a reading,
// and a row with no scale set names nothing an operator could act on.
func TestAMangledParkedRowIsRefused(t *testing.T) {
	health, err := NewHealth(&fakeClock{now: auditedOn}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]ParkedScaleSetMetric{
		"no scope":          {ScaleSetID: 7, Assigned: 1},
		"no id":             {Scope: "repo", Assigned: 1},
		"negative assigned": {Scope: "repo", ScaleSetID: 7, Assigned: -1},
		"negative busy":     {Scope: "repo", ScaleSetID: 7, Busy: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := health.SetParkedScaleSets([]ParkedScaleSetMetric{row}, auditedOn); err == nil {
				t.Fatal("an ungrammatical row must be refused")
			}
		})
	}
	if err := health.SetParkedScaleSets(nil, time.Time{}); err == nil {
		t.Fatal("an audit with no completion time is not an audit")
	}
	if !health.Snapshot().ParkedScaleSetsAuditedAt.IsZero() {
		t.Fatal("a refused audit must not mark the node as having audited")
	}
}

// The status document carries the audit and the sets it found, and a node that
// never audited carries neither -- absence is what the surfaces read as "not
// audited" rather than as health. The metric is per scale set, because the
// finding IS one set.
func TestTheStatusDocumentAndMetricCarryTheAudit(t *testing.T) {
	health, err := NewHealth(&fakeClock{now: auditedOn}, HealthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	passing := HealthResult{OK: true}

	envelope := statusEnvelope(health.Snapshot(), "v", "authority", passing, passing, passing, passing,
		passing, passing, passing, passing, passing)
	if envelope.Data.ParkedScaleSets != nil || envelope.Data.ParkedScaleSetsAuditedAt != nil {
		t.Fatalf("an unaudited node publishes neither: %#v", envelope.Data.ParkedScaleSets)
	}
	if envelope.Data.ParkedScaleSetCheck == nil || !envelope.Data.ParkedScaleSetCheck.OK {
		t.Fatalf("the check itself is always published: %#v", envelope.Data.ParkedScaleSetCheck)
	}
	if strings.Contains(renderMetrics(health.Snapshot()), "fleet_parked_scale_set") {
		t.Fatal("a node that never audited exports no parked series")
	}

	if err := health.SetParkedScaleSets([]ParkedScaleSetMetric{studioSet()}, auditedOn); err != nil {
		t.Fatal(err)
	}
	envelope = statusEnvelope(health.Snapshot(), "v", "authority", passing, passing, passing, passing,
		passing, passing, passing, passing, passing)
	if len(envelope.Data.ParkedScaleSets) != 1 || envelope.Data.ParkedScaleSets[0].ScaleSetID != 7 ||
		envelope.Data.ParkedScaleSets[0].Assigned != 2 || envelope.Data.ParkedScaleSets[0].Busy != 2 {
		t.Fatalf("the row must travel verbatim: %#v", envelope.Data.ParkedScaleSets)
	}
	if envelope.Data.ParkedScaleSetsAuditedAt == nil || !envelope.Data.ParkedScaleSetsAuditedAt.Equal(auditedOn) {
		t.Fatalf("the audit time must travel: %#v", envelope.Data.ParkedScaleSetsAuditedAt)
	}
	if envelope.Data.ParkedScaleSetCheck.OK {
		t.Fatal("a parked set holding work must fail the published check")
	}
	metrics := renderMetrics(health.Snapshot())
	for _, want := range []string{
		`fleet_parked_scale_set_assigned_jobs{scope="suuudokuuu",scale_set="7"} 2`,
		`fleet_parked_scale_set_busy_runners{scope="suuudokuuu",scale_set="7"} 2`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("the metric must carry %q:\n%s", want, metrics)
		}
	}
}
