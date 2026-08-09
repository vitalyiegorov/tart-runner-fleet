package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// starvingHold is the 2026-08-09 incident (issue #223) as telemetry sees it:
// one instance holding 6 CPU and 12288 MiB for seventy-five minutes against a
// forty-five minute ceiling, with queued work that would fit the vector it is
// sitting on.
func starvingHold() OccupancyMetric {
	return OccupancyMetric{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "builder",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMiB: 12_288,
		Age: 75 * time.Minute, Budget: 45 * time.Minute,
		Warned: true, OverBudget: true, StarvesQueuedDemand: true}
}

// healthyHold is the same shape well inside its ceiling: the long-but-legitimate
// job that must never be reported as an incident.
func healthyHold() OccupancyMetric {
	return OccupancyMetric{Instance: "trf-small-9a1c", Profile: "linux-small", Repo: "sudoku-repo/builder",
		CPU: 2, MemoryMiB: 4096, Age: 3 * time.Minute, Budget: 45 * time.Minute}
}

func occupancyServer(t *testing.T, health *Health) *Server {
	t.Helper()
	server, err := NewServer(health, ServerConfig{ControllerVersion: "v1", ControllerMode: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func occupancyMetricsBody(t *testing.T, health *Health) string {
	t.Helper()
	response := request(t, occupancyServer(t, health), http.MethodGet, adminapi.MetricsPath)
	defer func() { _ = response.Body.Close() }()
	return readAllString(t, response)
}

// TestSetOccupancyReplacesTheWholeSet proves a released vector disappears from
// the document. A merged update could not express release, so an instance that
// finished its job would keep reporting a hold that has ended — and a hold that
// has ended is exactly the reading that would send an operator after a leak that
// is no longer there.
func TestSetOccupancyReplacesTheWholeSet(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetOccupancy([]OccupancyMetric{starvingHold(), healthyHold()}); err != nil {
		t.Fatalf("SetOccupancy() = %v", err)
	}
	if got := health.Snapshot().Occupancy; len(got) != 2 {
		t.Fatalf("published occupancy = %#v, want both holds", got)
	}
	if err := health.SetOccupancy([]OccupancyMetric{healthyHold()}); err != nil {
		t.Fatalf("SetOccupancy() = %v", err)
	}
	got := health.Snapshot().Occupancy
	if len(got) != 1 || got[0].Instance != "trf-small-9a1c" {
		t.Fatalf("a released instance still holds a vector: %#v", got)
	}
	if !health.Occupancy().OK {
		t.Fatalf("the released hold still reads as an incident: %#v", health.Occupancy())
	}
}

// TestSetOccupancyRejectsRatherThanTruncates is rule 4 in the shape this metric
// makes dangerous. A truncated or partially stored observation reads as
// "nothing is holding anything for too long", which is the one sentence this
// metric exists to stop the fleet from saying falsely. Every ungrammatical row
// is refused outright and the previous coherent set survives intact.
func TestSetOccupancyRejectsRatherThanTruncates(t *testing.T) {
	negativeAge, negativeBudget := starvingHold(), starvingHold()
	negativeAge.Age, negativeBudget.Budget = -time.Second, -time.Second
	negativeCPU, negativeMemory := starvingHold(), starvingHold()
	negativeCPU.CPU, negativeMemory.MemoryMiB = -1, -1
	upstreamText, unknownProfile := starvingHold(), starvingHold()
	upstreamText.Instance = "Runner trf-xl is currently running a job"
	unknownProfile.Profile = "xl"

	for name, testCase := range map[string]struct {
		row  OccupancyMetric
		want error
	}{
		"negative age":            {row: negativeAge, want: errInvalidMetric},
		"negative budget":         {row: negativeBudget, want: errInvalidMetric},
		"negative cpu":            {row: negativeCPU, want: errInvalidMetric},
		"negative memory":         {row: negativeMemory, want: errInvalidMetric},
		"upstream text as an id":  {row: upstreamText, want: errInvalidMetric},
		"a profile nobody serves": {row: unknownProfile, want: errUnknownProfile},
	} {
		t.Run(name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			if err := health.SetOccupancy([]OccupancyMetric{healthyHold()}); err != nil {
				t.Fatal(err)
			}
			if err := health.SetOccupancy([]OccupancyMetric{starvingHold(), testCase.row}); !errors.Is(err, testCase.want) {
				t.Fatalf("SetOccupancy(%s) = %v, want %v", name, err, testCase.want)
			}
			got := health.Snapshot().Occupancy
			if len(got) != 1 || got[0].Instance != "trf-small-9a1c" {
				t.Fatalf("a refused observation replaced the published set: %#v", got)
			}
		})
	}
}

// TestSetOccupancyRejectsAnUnboundedSet keeps published cardinality inside the
// fleet's physical envelope. No node this fleet runs on can hold thirty-three
// instances at once, so an over-long set is a producer fault, and truncating it
// would silently drop the very hold an operator is looking for.
func TestSetOccupancyRejectsAnUnboundedSet(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetOccupancy([]OccupancyMetric{starvingHold()}); err != nil {
		t.Fatal(err)
	}
	oversized := make([]OccupancyMetric, maxOccupancy+1)
	for index := range oversized {
		oversized[index] = healthyHold()
	}
	if err := health.SetOccupancy(oversized); !errors.Is(err, errInvalidMetric) {
		t.Fatalf("SetOccupancy(%d rows) = %v, want errInvalidMetric", len(oversized), err)
	}
	if got := health.Snapshot().Occupancy; len(got) != 1 || got[0].Instance != "trf-xl-05bbe1c83f21fcd6" {
		t.Fatalf("a refused oversized set replaced the published one: %#v", got)
	}
	// The bound itself must admit a fleet exactly at the ceiling.
	if err := health.SetOccupancy(oversized[:maxOccupancy]); err != nil {
		t.Fatalf("SetOccupancy(%d rows) = %v, want the bound to be inclusive", maxOccupancy, err)
	}
}

// TestOccupancyReportsOnlyTheConjunction is the judgement ADR 0036 asks the
// fleet to make. A job is allowed to be long and a queue is allowed to be deep;
// neither on its own is worth waking anyone for. Only an over-budget hold with
// work waiting that the held vector would fit is the incident, so each half is
// asserted alone before the pair is asserted together.
func TestOccupancyReportsOnlyTheConjunction(t *testing.T) {
	overBudgetOnly, starvingOnly := starvingHold(), starvingHold()
	overBudgetOnly.StarvesQueuedDemand = false
	starvingOnly.OverBudget, starvingOnly.Warned = false, false

	for name, row := range map[string]OccupancyMetric{
		"a long job with nothing waiting": overBudgetOnly,
		"a deep queue behind a young job": starvingOnly,
		"a job inside its budget":         healthyHold(),
	} {
		t.Run(name, func(t *testing.T) {
			health, _ := newTestHealth(t)
			if err := health.SetOccupancy([]OccupancyMetric{row}); err != nil {
				t.Fatal(err)
			}
			if result := health.Occupancy(); !result.OK || len(result.Reasons) != 0 {
				t.Fatalf("%s reported as an incident: %#v", name, result)
			}
		})
	}

	health, _ := newTestHealth(t)
	if err := health.SetOccupancy([]OccupancyMetric{healthyHold(), starvingHold()}); err != nil {
		t.Fatal(err)
	}
	result := health.Occupancy()
	if result.OK || len(result.Reasons) != 1 {
		t.Fatalf("the conjunction was not reported: %#v", result)
	}
	// The reason has to be actionable on its own: which instance, which vector,
	// how long it has held it, and what the ceiling was.
	for _, fragment := range []string{"trf-xl-05bbe1c83f21fcd6", "builder", "6 cpu", "12288 MiB",
		"1h15m0s", "45m0s", "queued work fits it"} {
		if !strings.Contains(result.Reasons[0], fragment) {
			t.Fatalf("occupancy reason missing %q: %q", fragment, result.Reasons[0])
		}
	}
}

// TestStatusAndMetricsPublishEveryHold proves the per-instance projection
// survives into both published surfaces with every field intact, and that the
// alertable gauge is 1 for the conjunction alone. The fault is ONE instance
// holding ONE vector too long, which is why nothing here is aggregated by
// profile: an average over a healthy neighbour hides it.
func TestStatusAndMetricsPublishEveryHold(t *testing.T) {
	overBudgetOnly := starvingHold()
	overBudgetOnly.Instance, overBudgetOnly.StarvesQueuedDemand = "trf-xl-slowbuild", false
	health, _ := newTestHealth(t)
	if err := health.SetOccupancy([]OccupancyMetric{starvingHold(), overBudgetOnly}); err != nil {
		t.Fatal(err)
	}

	response := request(t, occupancyServer(t, health), http.MethodGet, adminapi.StatusPath)
	defer func() { _ = response.Body.Close() }()
	var envelope adminapi.StatusEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	rows := envelope.Data.Occupancy
	if len(rows) != 2 {
		t.Fatalf("status occupancy = %#v, want both instances", rows)
	}
	want := adminapi.Occupancy{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "builder",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMiB: 12_288, AgeSeconds: 4500, BudgetSeconds: 2700,
		Warned: true, OverBudget: true, StarvesQueuedDemand: true}
	if rows[0] != want {
		t.Fatalf("projected row = %#v, want %#v", rows[0], want)
	}
	if rows[1].StarvesQueuedDemand || !rows[1].OverBudget {
		t.Fatalf("the slow-but-unblocking hold was mislabelled: %#v", rows[1])
	}
	check := envelope.Data.EffectiveOccupancy()
	if check.OK || len(check.Reasons) != 1 {
		t.Fatalf("status occupancy check = %#v, want the conjunction named once", check)
	}

	rendered := occupancyMetricsBody(t, health)
	for _, line := range []string{
		`fleet_instance_occupancy_seconds{profile="builder",instance="trf-xl-05bbe1c83f21fcd6"} 4500` + "\n",
		`fleet_instance_occupancy_seconds{profile="builder",instance="trf-xl-slowbuild"} 4500` + "\n",
		`fleet_instance_occupancy_budget_seconds{profile="builder",instance="trf-xl-05bbe1c83f21fcd6"} 2700` + "\n",
		`fleet_instance_occupancy_budget_seconds{profile="builder",instance="trf-xl-slowbuild"} 2700` + "\n",
		`fleet_instance_occupancy_starving{profile="builder",instance="trf-xl-05bbe1c83f21fcd6"} 1` + "\n",
		`fleet_instance_occupancy_starving{profile="builder",instance="trf-xl-slowbuild"} 0` + "\n",
		"# TYPE fleet_instance_occupancy_seconds gauge\n",
		"# TYPE fleet_instance_occupancy_budget_seconds gauge\n",
		"# TYPE fleet_instance_occupancy_starving gauge\n",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}

// TestUnboundedProfilePublishesAZeroBudgetNotAZeroCeiling proves the sentinel
// travels as documented: a profile with no ceiling publishes budget 0 and can
// never be over budget, so an alert on the starving gauge cannot fire on a
// profile nobody bounded.
func TestUnboundedProfilePublishesAZeroBudgetNotAZeroCeiling(t *testing.T) {
	unbounded := starvingHold()
	unbounded.Budget, unbounded.Warned, unbounded.OverBudget = 0, false, false
	health, _ := newTestHealth(t)
	if err := health.SetOccupancy([]OccupancyMetric{unbounded}); err != nil {
		t.Fatal(err)
	}
	if !health.Occupancy().OK {
		t.Fatalf("an unbounded hold was reported as past a bound: %#v", health.Occupancy())
	}
	rendered := occupancyMetricsBody(t, health)
	for _, line := range []string{
		`fleet_instance_occupancy_budget_seconds{profile="builder",instance="trf-xl-05bbe1c83f21fcd6"} 0` + "\n",
		`fleet_instance_occupancy_starving{profile="builder",instance="trf-xl-05bbe1c83f21fcd6"} 0` + "\n",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}

// TestFleetHoldingNothingPublishesNoOccupancyAtAll keeps the addition additive.
// A fleet with no live instance must emit exactly the status document and
// exactly the metrics text an older client already saw: no empty array, and none
// of the three families with no samples under them.
func TestFleetHoldingNothingPublishesNoOccupancyAtAll(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetOccupancy(nil); err != nil {
		t.Fatal(err)
	}
	if rows := occupancyRows(health.Snapshot()); rows != nil {
		t.Fatalf("occupancyRows(empty) = %#v, want nil", rows)
	}
	response := request(t, occupancyServer(t, health), http.MethodGet, adminapi.StatusPath)
	defer func() { _ = response.Body.Close() }()
	if body := readAllString(t, response); strings.Contains(body, `"occupancy"`) {
		t.Fatalf("an empty fleet named occupancy in its status document: %s", body)
	}
	if rendered := occupancyMetricsBody(t, health); strings.Contains(rendered, "fleet_instance_occupancy") {
		t.Fatalf("an empty fleet emitted an occupancy metric family:\n%s", rendered)
	}
}
