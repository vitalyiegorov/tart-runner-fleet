package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func renderCommand(output io.Writer, command string, status adminapi.StatusEnvelope) {
	switch command {
	case "queues":
		renderQueues(output, status.Data.Queues)
		renderScopeQueues(output, status.Data.ScopeQueues)
	case "instances":
		renderInstances(output, status.Data.Instances)
	case "operations":
		renderOperations(output, status.Data.Operations)
	case "observations":
		renderObservations(output, status.Data.Observations)
	default:
		renderStatus(output, status)
	}
}

func renderStatus(output io.Writer, status adminapi.StatusEnvelope) {
	queueSLO := status.Data.EffectiveQueueSLO()
	state := "READY"
	if !status.Data.Ready.OK {
		state = "NOT READY"
	} else if !queueSLO.OK {
		state = "DEGRADED"
	}
	fmt.Fprintf(output, "TART RUNNER FLEET — %s\n", state)
	fmt.Fprintf(output, "controller %s  mode %s  host %s  revision %d\n", status.Data.ControllerVersion,
		status.Data.ControllerMode, status.Data.HostMode, status.Revision)
	if !status.Data.Ready.OK {
		fmt.Fprintf(output, "blocked: %s\n", joinReasons(status.Data.Ready))
	}
	if !queueSLO.OK {
		fmt.Fprintf(output, "queue SLO: %s\n", joinReasons(queueSLO))
	}
	fmt.Fprintln(output, "\nHOST PRESSURE")
	pressure := status.Data.HostPressure
	fmt.Fprintf(output, "disk %d GiB  memory %d MiB  swap %d MiB (%s)  cpu idle %.1f%%  load %.2f  admission %s (%s)\n",
		pressure.FreeDiskGiB, pressure.AvailableMemoryMiB, pressure.SwapUsedMiB, pagingState(pressure),
		pressure.CPUIdlePercent, pressure.LoadAverage, admissionState(pressure.AdmissionAllowed), pressure.AdmissionReason)
	fmt.Fprintln(output, "\nQUEUES")
	renderQueues(output, status.Data.Queues)
	fmt.Fprintln(output, "\nINSTANCES")
	renderInstances(output, status.Data.Instances)
	fmt.Fprintln(output, "\nOPERATIONS")
	fmt.Fprintf(output, "retrying %d  dead %d\n", status.Data.Operations.Retrying, status.Data.Operations.Dead)
	for _, failure := range status.Data.Operations.Failures {
		fmt.Fprintf(output, "stuck: %s %s  count %d  attempts %d\n", failure.Kind, failure.Code, failure.Count, failure.Attempts)
	}
	for _, letter := range status.Data.Operations.DeadLetters {
		fmt.Fprintf(output, "parked %t: %s on %s  %s  attempts %d\n", letter.Parked, letter.OperationID,
			letter.ResourceID, letter.Code, letter.Attempts)
	}
	fmt.Fprintln(output, "\nOBSERVATIONS")
	renderObservations(output, status.Data.Observations)
}

func admissionState(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "deferred"
}

// pagingState renders the swap guardrail's deciding signal beside the level it
// qualifies. The level alone is a high-water mark -- macOS does not eagerly
// reclaim swap -- so admission is refused only when the host is also paging out
// (ADR 0018). Without this, a host far over its ceiling and a host at zero swap
// print the same verdict with no fact that reconciles them, which is how a
// correct decision on node C read as an ignored guardrail.
//
// An unmeasured rate is named, never printed as 0/s: it is the fail-closed case
// where the level blocks on its own, and reading it as a quiet host is the one
// mistake this line exists to prevent.
func pagingState(pressure adminapi.HostPressure) string {
	if !pressure.SwapOutRateObserved {
		return "paging unmeasured"
	}
	return fmt.Sprintf("paging %s/s", strconv.FormatFloat(pressure.SwapOutRatePerSecond, 'f', -1, 64))
}

// renderOperations keeps the terse counts a healthy fleet shows and appends the
// bounded failure aggregate when operations are not progressing, so the cause of
// a long retry is visible from the runbook's own inspection command.
func renderOperations(output io.Writer, summary adminapi.OperationSummary) {
	fmt.Fprintf(output, "STATUS\tCOUNT\nretrying\t%d\ndead\t%d\n", summary.Retrying, summary.Dead)
	if len(summary.Failures) == 0 {
		return
	}
	fmt.Fprint(output, "\nKIND\tCODE\tCOUNT\tATTEMPTS\n")
	for _, failure := range summary.Failures {
		fmt.Fprintf(output, "%s\t%s\t%d\t%d\n", failure.Kind, failure.Code, failure.Count, failure.Attempts)
	}
	renderDeadLetters(output, summary.DeadLetters)
}

// renderDeadLetters names each parked operation with the identity
// `fleet operations discharge` needs. Without it an operator can see that
// something is wedged but has nothing to act on.
func renderDeadLetters(output io.Writer, letters []adminapi.DeadLetter) {
	if len(letters) == 0 {
		return
	}
	fmt.Fprint(output, "\nOPERATION\tINSTANCE\tCODE\tATTEMPTS\tPARKED\n")
	for _, letter := range letters {
		fmt.Fprintf(output, "%s\t%s\t%s\t%d\t%t\n", letter.OperationID, letter.ResourceID,
			letter.Code, letter.Attempts, letter.Parked)
	}
}

func renderQueues(output io.Writer, queues []adminapi.Queue) {
	table := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "PROFILE\tJOBS\tOLDEST")
	for _, queue := range queues {
		fmt.Fprintf(table, "%s\t%d\t%s\n", queue.Profile, queue.Jobs, formatAge(queue.OldestAgeSeconds))
	}
	_ = table.Flush()
}

// renderScopeQueues prints the per-scope breakdown. It is a separate table
// because the aggregate above answers "what is queued" while this answers "whose
// demand is it", and an incident needs the second question answered first.
func renderScopeQueues(output io.Writer, rows []adminapi.ScopeQueue) {
	if len(rows) == 0 {
		return
	}
	table := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "SCOPE\tPROFILE\tSCALE SET\tJOBS\tOLDEST")
	for _, row := range rows {
		fmt.Fprintf(table, "%s\t%s\t%d\t%d\t%s\n", row.Scope, row.Profile, row.ScaleSetID, row.Jobs,
			formatAge(row.OldestAgeSeconds))
	}
	_ = table.Flush()
}

func renderInstances(output io.Writer, instances []adminapi.Instance) {
	table := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "PROFILE\tCOUNT\tCPU\tMEMORY MiB")
	for _, instance := range instances {
		fmt.Fprintf(table, "%s\t%d\t%d\t%d\n", instance.Profile, instance.Count, instance.CPU, instance.MemoryMiB)
	}
	_ = table.Flush()
}

func renderObservations(output io.Writer, observations []adminapi.Observation) {
	table := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "SOURCE\tFRESHNESS\tAGE")
	for _, observation := range observations {
		fmt.Fprintf(table, "%s\t%s\t%s\n", observation.Name, observation.Freshness, formatAge(observation.AgeSeconds))
	}
	_ = table.Flush()
}

func renderCheck(output io.Writer, name string, check adminapi.Check) {
	state := "PASS"
	if !check.OK {
		state = "FAIL"
	}
	fmt.Fprintf(output, "%-5s  %-8s  %s\n", state, name, joinReasons(check))
}

func renderDoctor(output io.Writer, checks []doctorCheck) {
	allOK := true
	for _, check := range checks {
		state := "PASS"
		if !check.OK {
			state, allOK = "FAIL", false
		}
		fmt.Fprintf(output, "%-5s  %-16s  %s\n", state, check.Name, check.Detail)
	}
	result := "PASS"
	if !allOK {
		result = "FAIL"
	}
	fmt.Fprintf(output, "RESULT %s\n", result)
}

func joinReasons(check adminapi.Check) string {
	if check.OK && len(check.Reasons) == 0 {
		return "ok"
	}
	if len(check.Reasons) == 0 {
		return "unspecified"
	}
	return strings.Join(check.Reasons, ",")
}

func formatAge(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}
