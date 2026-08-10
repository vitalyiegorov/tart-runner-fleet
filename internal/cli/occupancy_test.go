package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// starvingOccupancy is the 2026-08-09 incident (issue #223) as the CLI receives
// it: one instance holding 6 CPU and 12288 MiB for seventy-five minutes against
// a forty-five minute ceiling, with queued work that would fit the vector.
func starvingOccupancy() adminapi.Occupancy {
	return adminapi.Occupancy{Instance: "trf-xl-05bbe1c83f21fcd6", Profile: "xl",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMiB: 12_288, AgeSeconds: 4500, BudgetSeconds: 2700,
		Warned: true, OverBudget: true, StarvesQueuedDemand: true}
}

func occupancyStatus(rows []adminapi.Occupancy, check *adminapi.Check) adminapi.StatusEnvelope {
	status := healthyStatus()
	status.Data.Occupancy = rows
	status.Data.OccupancyCheck = check
	return status
}

// TestOccupancyTableNamesTheHoldAndItsCeiling proves the table carries the whole
// judgement. Sixty percent of a Mac mini held for seventy-five minutes is not
// legible as a fault until the ceiling is printed beside it, and STATE is the
// column that says whether an operator should act now.
func TestOccupancyTableNamesTheHoldAndItsCeiling(t *testing.T) {
	var buffer bytes.Buffer
	renderOccupancy(&buffer, []adminapi.Occupancy{starvingOccupancy()})
	out := buffer.String()
	for _, want := range []string{"INSTANCE", "PROFILE", "MEMORY MiB", "HELD", "BUDGET", "STATE",
		"trf-xl-05bbe1c83f21fcd6", "xl", "6", "12288", "1h15m0s", "45m0s", "STARVING"} {
		if !strings.Contains(out, want) {
			t.Fatalf("occupancy table missing %q:\n%s", want, out)
		}
	}
}

// TestOccupancyStateSeparatesASlowJobFromAnIncident pins all four readings. Only
// STARVING is worth acting on immediately; over and warn are advance notice, and
// ok is the ordinary long job that must never cry wolf.
func TestOccupancyStateSeparatesASlowJobFromAnIncident(t *testing.T) {
	overBudgetOnly, warned, healthy := starvingOccupancy(), starvingOccupancy(), starvingOccupancy()
	overBudgetOnly.StarvesQueuedDemand = false
	warned.OverBudget, warned.StarvesQueuedDemand = false, false
	healthy.Warned, healthy.OverBudget, healthy.StarvesQueuedDemand = false, false, false
	for want, row := range map[string]adminapi.Occupancy{
		"STARVING": starvingOccupancy(),
		"over":     overBudgetOnly,
		"warn":     warned,
		"ok":       healthy,
	} {
		t.Run(want, func(t *testing.T) {
			if got := occupancyState(row); got != want {
				t.Fatalf("occupancyState(%#v) = %q, want %q", row, got, want)
			}
		})
	}
	// A hold past its ceiling with nothing waiting is still not the incident, so
	// STARVING must not be reachable from the queue alone.
	starvingButYoung := healthy
	starvingButYoung.StarvesQueuedDemand = true
	if got := occupancyState(starvingButYoung); got != "ok" {
		t.Fatalf("a deep queue behind a young job rendered as %q", got)
	}
}

// TestUnboundedBudgetRendersAsADashNotZero is the reason renderBudget exists.
// "0s" in the BUDGET column reads as a ceiling of zero — the opposite of a
// profile that has no ceiling at all — and would send an operator after every
// instance of an unbounded profile.
func TestUnboundedBudgetRendersAsADashNotZero(t *testing.T) {
	if got := renderBudget(0); got != "-" {
		t.Fatalf("renderBudget(0) = %q, want a dash", got)
	}
	if got := renderBudget(-1); got != "-" {
		t.Fatalf("renderBudget(-1) = %q, want a dash", got)
	}
	if got := renderBudget(2700); got != "45m0s" {
		t.Fatalf("renderBudget(2700) = %q", got)
	}
	if got := renderSeconds(4500); got != "1h15m0s" {
		t.Fatalf("renderSeconds(4500) = %q", got)
	}
	unbounded := starvingOccupancy()
	unbounded.BudgetSeconds, unbounded.Warned, unbounded.OverBudget = 0, false, false
	var buffer bytes.Buffer
	renderOccupancy(&buffer, []adminapi.Occupancy{unbounded})
	// The dash has to be in the BUDGET column itself, not merely somewhere on the
	// line: the held duration prints beside it and would satisfy a looser check.
	row := strings.Fields(strings.Split(strings.TrimSpace(buffer.String()), "\n")[1])
	if budget := row[len(row)-2]; budget != "-" {
		t.Fatalf("an unbounded profile rendered a ceiling of %q:\n%s", budget, buffer.String())
	}
}

// TestStatusReportsAnOccupancyIncidentAsDegraded proves the top line changes.
// A fleet whose scheduler is ready but whose vector is pinned by a dead job is
// not READY in any sense an operator cares about, and the reason has to appear
// on the same screen rather than only in `fleet doctor`.
func TestStatusReportsAnOccupancyIncidentAsDegraded(t *testing.T) {
	reason := "instance trf-xl-05bbe1c83f21fcd6 of profile xl has held 6 cpu / 12288 MiB for 1h15m0s"
	status := occupancyStatus([]adminapi.Occupancy{starvingOccupancy()},
		&adminapi.Check{Reasons: []string{reason}})
	var buffer bytes.Buffer
	renderStatus(&buffer, status)
	out := buffer.String()
	for _, want := range []string{"TART RUNNER FLEET — DEGRADED", "occupancy: " + reason, "\nOCCUPANCY\n", "STARVING"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
}

// TestStatusOmitsOccupancyWhenNothingIsHeld proves the section is additive: an
// older daemon that measures no occupancy, and a fleet holding nothing, both
// render exactly the screen they always did — no empty header implying data the
// fleet does not have.
func TestStatusOmitsOccupancyWhenNothingIsHeld(t *testing.T) {
	var buffer bytes.Buffer
	renderStatus(&buffer, healthyStatus())
	out := buffer.String()
	if strings.Contains(out, "OCCUPANCY") || strings.Contains(out, "occupancy:") {
		t.Fatalf("a fleet holding nothing rendered an occupancy section:\n%s", out)
	}
	if !strings.Contains(out, "TART RUNNER FLEET — READY") {
		t.Fatalf("status is no longer READY:\n%s", out)
	}
	// Rows without a failing check are still worth showing: the table is the
	// evidence, the check is the verdict, and they are independent.
	buffer.Reset()
	healthy := starvingOccupancy()
	healthy.Warned, healthy.OverBudget, healthy.StarvesQueuedDemand = false, false, false
	renderStatus(&buffer, occupancyStatus([]adminapi.Occupancy{healthy}, &adminapi.Check{OK: true}))
	if out := buffer.String(); !strings.Contains(out, "OCCUPANCY") || strings.Contains(out, "occupancy:") ||
		!strings.Contains(out, "TART RUNNER FLEET — READY") {
		t.Fatalf("a healthy hold was rendered as an incident:\n%s", out)
	}
}

// TestDoctorFailsOnAStarvedVector proves the check an operator runs first names
// the condition. Before ADR 0036 `fleet doctor` passed cleanly through the whole
// seventy-five minutes of the incident, which is why it was found by an owner
// asking about a slow release instead.
func TestDoctorFailsOnAStarvedVector(t *testing.T) {
	const reason = "instance trf-xl-05bbe1c83f21fcd6 of profile xl has held 6 cpu / 12288 MiB for 1h15m0s"
	assertDoctorNames(t, occupancyStatus([]adminapi.Occupancy{starvingOccupancy()},
		&adminapi.Check{Reasons: []string{reason}}), "occupancy", reason)
}

// TestDoctorPassesWhenTheDaemonNeverMeasuredOccupancy is the handoff half. An
// older daemon omits the check entirely, and `fleet doctor` must not start
// failing against every host that has not been upgraded yet.
func TestDoctorPassesWhenTheDaemonNeverMeasuredOccupancy(t *testing.T) {
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		status := healthyStatus()
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	var stdout, stderr bytes.Buffer
	if got := executeWith(context.Background(), []string{"doctor"}, &stdout, &stderr, deps); got != exitSuccess {
		t.Fatalf("code=%d, want exitSuccess; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "PASS   occupancy") || !strings.Contains(stdout.String(), "RESULT PASS") {
		t.Fatalf("doctor did not pass an unmeasured occupancy check:\n%s", stdout.String())
	}
}

// assertDoctorNames drives `fleet doctor` in both output modes against a status
// document and requires the named check to FAIL carrying the given reason. Both
// modes matter: an operator reads the table and an alert reads the JSON, and a
// check that names a condition in one and not the other is half a check.
func assertDoctorNames(t *testing.T, status adminapi.StatusEnvelope, check, reason string) {
	t.Helper()
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready, metrics: "fleet_mode 1\n"}, nil
	}}
	for name, testCase := range map[string][]string{
		"table": {"FAIL   " + check, reason, "RESULT FAIL"},
		"json":  {`"name": "` + check + `"`, `"ok": false`, reason},
	} {
		args := []string{"doctor"}
		if name == "json" {
			args = append(args, "--output", "json")
		}
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := executeWith(context.Background(), args, &stdout, &stderr, deps); got != exitDegraded {
				t.Fatalf("code=%d, want exitDegraded; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
			}
			for _, want := range testCase {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("doctor %s missing %q:\n%s", name, want, stdout.String())
				}
			}
		})
	}
}
