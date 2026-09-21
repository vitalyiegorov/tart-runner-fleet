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

// strandedSet is node-b's `trf-budgie-linux-amd64-4x8` (id 17) on 2026-09-21:
// GitHub held three jobs assigned and three runners busy for it with no runner
// registered, the node's queue for it was empty, no instance existed, and
// nothing was delivered all day (#336).
func strandedSet() StrandedScaleSetMetric {
	return StrandedScaleSetMetric{Scope: "budgie", ScaleSetID: 17, Name: "trf-budgie-linux-amd64-4x8",
		Profile: "linux-4x8", Assigned: 3, Busy: 3,
		HoldingSince: time.Date(2026, 8, 25, 21, 50, 0, 0, time.UTC),
		ObservedAt:   time.Date(2026, 8, 26, 1, 50, 0, 0, time.UTC),
		Reason: "budgie scale set 17 (trf-budgie-linux-amd64-4x8) is bound here and has held 3 assigned job(s) " +
			"and 3 busy runner(s) for 4h0m0s with no runner registered and no instance on this node: " +
			"GitHub is delivering nothing for this set -- recreate the set"}
}

// TestAStrandedBoundSetFailsTheIngestCheck is issue #336 at the row an operator
// reads. The node's own queue was empty, so nothing in the delivery gap above
// could fire: the fault is that GitHub's counters for a set this node BINDS say
// it holds work while nothing is ever handed over. That is an ingest-delivery
// failure and must not render as "every set is being offered the work GitHub
// has for it".
func TestAStrandedBoundSetFailsTheIngestCheck(t *testing.T) {
	snapshot := ingestSnapshot(nil, 5*time.Minute)
	snapshot.StrandedScaleSets = []StrandedScaleSetMetric{strandedSet()}

	result := ingestResult(snapshot)

	if result.OK {
		t.Fatal("a bound set GitHub has stopped delivering for must fail the ingest check")
	}
	detail := strings.Join(result.Reasons, " ")
	for _, want := range []string{"budgie", "scale set 17", "trf-budgie-linux-amd64-4x8", "recreate the set"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the failure must name the set and the remedy, missing %q: %q", want, detail)
		}
	}

	// And a node whose queue SLO is disabled still reports it: the finding is
	// not about a queue this node can see.
	if ingestResult(Snapshot{StrandedScaleSets: []StrandedScaleSetMetric{strandedSet()}}).OK {
		t.Fatal("the finding does not depend on the queue SLO")
	}
}

// A published audit that found nothing stranded is exactly the old pass.
func TestAnAuditWithNoStrandedBoundSetKeepsTheIngestCheckPassing(t *testing.T) {
	if result := ingestResult(ingestSnapshot(nil, 5*time.Minute)); !result.OK || len(result.Reasons) != 0 {
		t.Fatalf("no stranded set is still a pass: %v", result.Reasons)
	}
}

// The publication is rejected rather than trusted when it is not a finding: a
// row with no set or no sentence would fail the node on nothing.
func TestAMalformedStrandedPublicationIsRefused(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetStrandedScaleSets([]StrandedScaleSetMetric{{Scope: "budgie"}}); err == nil {
		t.Fatal("a row naming no scale set must be refused")
	}
	row := strandedSet()
	if err := health.SetStrandedScaleSets([]StrandedScaleSetMetric{row}); err != nil {
		t.Fatal(err)
	}
	snapshot := health.Snapshot()
	if len(snapshot.StrandedScaleSets) != 1 || snapshot.StrandedScaleSets[0].ScaleSetID != 17 {
		t.Fatalf("the finding must reach the snapshot: %#v", snapshot.StrandedScaleSets)
	}
	if err := health.SetStrandedScaleSets(nil); err != nil {
		t.Fatal(err)
	}
	if len(health.Snapshot().StrandedScaleSets) != 0 {
		t.Fatal("a later audit that found nothing clears the finding")
	}
}

// The accessor and the status document must reach the same verdict as the pure
// function: a node whose command surface and whose document disagree about a
// stranded set is a node an operator cannot act on.
func TestThePublishedFindingReachesTheAccessorAndTheDocument(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetStrandedScaleSets([]StrandedScaleSetMetric{strandedSet()}); err != nil {
		t.Fatal(err)
	}

	if result := health.Ingest(); result.OK || len(result.Reasons) != 1 {
		t.Fatalf("the accessor must fail on the finding: %#v", result)
	}
	ok := HealthResult{OK: true}
	document := statusEnvelope(health.Snapshot(), "v1", "authority", ok, ok, ok, ok, ok, ok, ok, ok, ok)
	rows := document.Data.StrandedScaleSets
	if len(rows) != 1 || rows[0].ScaleSetID != 17 || rows[0].Profile != "linux-4x8" ||
		!strings.Contains(rows[0].Reason, "recreate the set") {
		t.Fatalf("the document must carry the finding: %#v", rows)
	}
	if document.Data.IngestCheck == nil || document.Data.IngestCheck.OK {
		t.Fatalf("the document's ingest check must fail with it: %#v", document.Data.IngestCheck)
	}
}

// The per-set instance count is what makes a per-set verdict possible, and it
// is published as a whole set: a malformed row is refused, a later publication
// replaces the previous one, and nil is an absence rather than an empty fleet.
func TestThePerSetInstanceCountIsPublishedAsAWholeSet(t *testing.T) {
	health, _ := newTestHealth(t)

	if err := health.SetScaleSetInstances([]ScaleSetInstanceMetric{{Scope: "budgie", Count: 1}}); err == nil {
		t.Fatal("a row naming no scale set must be refused")
	}
	if err := health.SetScaleSetInstances([]ScaleSetInstanceMetric{
		{Scope: "budgie", ScaleSetID: 17, Profile: "linux-4x8", Count: -1}}); err == nil {
		t.Fatal("a negative instance count must be refused")
	}
	rows := []ScaleSetInstanceMetric{
		{Scope: "budgie", ScaleSetID: 17, Profile: "linux-4x8", Count: 0},
		{Scope: "fleet", ScaleSetID: 12, Profile: "linux-4x8", Count: 1}}
	if err := health.SetScaleSetInstances(rows); err != nil {
		t.Fatal(err)
	}
	published := health.Snapshot().ScaleSetInstances
	if len(published) != 2 || published[0].ScaleSetID != 17 || published[0].Count != 0 || published[1].Count != 1 {
		t.Fatalf("both sets are published, including the empty one: %#v", published)
	}

	// A tick that could not observe the inventory publishes nothing, which the
	// detector reads as "not observed" and never as "no instance".
	if err := health.SetScaleSetInstances(nil); err != nil {
		t.Fatal(err)
	}
	if len(health.Snapshot().ScaleSetInstances) != 0 {
		t.Fatal("an unobserved inventory must leave no per-set count standing")
	}
}
