package telemetry

import (
	"strings"
	"testing"
	"time"
)

func ingestSnapshot(rows []ScopeQueueMetrics, slo time.Duration) Snapshot {
	return Snapshot{Now: time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC), ScopeQueues: rows, QueueSLO: slo}
}

// starvedSet is `trf-fleet-small` on the mac mini at 2026-08-26 01:50: GitHub
// showed a matching job queued for four hours, and the node's own session had
// delivered none of it.
func starvedSet(shared bool) ScopeQueueMetrics {
	return ScopeQueueMetrics{Scope: "ops/fleet", Profile: "small", ScaleSetID: 7,
		Count: 1, Delivered: 0, Observed: 1, SharedLabels: shared,
		OldestEnqueuedAt: time.Date(2026, 8, 25, 21, 50, 0, 0, time.UTC)}
}

// TestASetGitHubHasStoppedOfferingJobsToIsAFinding is issue #292's first ask.
//
// Every signal read healthy from the node's own chair: the session polled fine
// so its observations were `fresh`, the node's queue for the profile was 0 so
// the queue SLO had nothing to breach, and doctor returned PASS. The only
// observer who could see it was a human reading GitHub, four hours later.
func TestASetGitHubHasStoppedOfferingJobsToIsAFinding(t *testing.T) {
	result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{starvedSet(false)}, 5*time.Minute))

	if result.OK {
		t.Fatal("work GitHub is holding that this node was never offered must not read as healthy")
	}
	detail := strings.Join(result.Reasons, " ")
	for _, want := range []string{"ops/fleet", "small", "not delivered", "4h0m0s"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the finding must name the set and the wait, missing %q: %q", want, detail)
		}
	}
}

// A set whose labels are shared with a sibling is NAMED as such rather than
// blamed. Under ADR 0034 GitHub may bind a matching job to either node, so
// undelivered work here is not evidence of a fault here — but work no node has
// taken for longer than the SLO is still a fleet-level fact worth a line.
func TestASharedLabelSetIsNamedRatherThanBlamed(t *testing.T) {
	result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{starvedSet(true)}, 5*time.Minute))

	if result.OK {
		t.Fatal("a shared-label set is still reported; no node has taken the work")
	}
	if !strings.Contains(strings.Join(result.Reasons, " "), "shared with another node") {
		t.Fatalf("the sibling must be named so this node is not blamed: %v", result.Reasons)
	}
}

// A set mid-delivery diverges for seconds on every ordinary tick. Firing on that
// would be noise in the artifact this check exists to make readable.
func TestAMomentaryDivergenceIsNotAFinding(t *testing.T) {
	row := starvedSet(false)
	row.OldestEnqueuedAt = time.Date(2026, 8, 26, 1, 49, 30, 0, time.UTC)

	if result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{row}, 5*time.Minute)); !result.OK {
		t.Fatalf("a 30-second divergence must not fire: %v", result.Reasons)
	}
}

// A set the node HAS been offered its work for is silent, however deep its queue
// is. A backlog the node is working through is the queue SLO's business.
func TestADeliveredQueueIsNotAnIngestFinding(t *testing.T) {
	row := starvedSet(false)
	row.Delivered, row.Observed, row.Count = 12, 12, 12

	if result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{row}, 5*time.Minute)); !result.OK {
		t.Fatalf("a delivered backlog is not an ingest fault: %v", result.Reasons)
	}
}

// A node with no REST observation reports 0 observed, which must never read as
// "GitHub has nothing" turning into a negative divergence or a finding.
func TestAnUnobservedSetIsNotAFinding(t *testing.T) {
	row := starvedSet(false)
	row.Delivered, row.Observed = 3, 0

	if result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{row}, 5*time.Minute)); !result.OK {
		t.Fatalf("no REST observation is not evidence of starvation: %v", result.Reasons)
	}
}

// Without a configured SLO there is no threshold to judge against, and the check
// says nothing rather than inventing one.
func TestWithoutAQueueSLOTheCheckIsSilent(t *testing.T) {
	if result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{starvedSet(false)}, 0)); !result.OK {
		t.Fatalf("no SLO, no finding: %v", result.Reasons)
	}
}

// Findings are ordered so two runs of one state read identically.
func TestFindingsAreDeterministicallyOrdered(t *testing.T) {
	first, second := starvedSet(false), starvedSet(false)
	first.Scope, second.Scope = "z/repo", "a/repo"

	result := ingestResult(ingestSnapshot([]ScopeQueueMetrics{first, second}, 5*time.Minute))

	if len(result.Reasons) != 2 || !strings.HasPrefix(result.Reasons[0], "a/repo") {
		t.Fatalf("reasons must be ordered: %v", result.Reasons)
	}
}
