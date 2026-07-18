package main

import (
	"fmt"
	"io"
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
	case "instances":
		renderInstances(output, status.Data.Instances)
	case "operations":
		fmt.Fprintf(output, "STATUS\tCOUNT\nretrying\t%d\ndead\t%d\n", status.Data.Operations.Retrying, status.Data.Operations.Dead)
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
	fmt.Fprintf(output, "disk %d GiB  memory %d MiB  swap %d MiB  cpu idle %.1f%%  load %.2f  admission %s (%s)\n",
		pressure.FreeDiskGiB, pressure.AvailableMemoryMiB, pressure.SwapUsedMiB, pressure.CPUIdlePercent,
		pressure.LoadAverage, admissionState(pressure.AdmissionAllowed), pressure.AdmissionReason)
	fmt.Fprintln(output, "\nQUEUES")
	renderQueues(output, status.Data.Queues)
	fmt.Fprintln(output, "\nINSTANCES")
	renderInstances(output, status.Data.Instances)
	fmt.Fprintln(output, "\nOPERATIONS")
	fmt.Fprintf(output, "retrying %d  dead %d\n", status.Data.Operations.Retrying, status.Data.Operations.Dead)
	fmt.Fprintln(output, "\nOBSERVATIONS")
	renderObservations(output, status.Data.Observations)
}

func admissionState(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "deferred"
}

func renderQueues(output io.Writer, queues []adminapi.Queue) {
	table := tabwriter.NewWriter(output, 0, 2, 2, ' ', 0)
	fmt.Fprintln(table, "PROFILE\tJOBS\tOLDEST")
	for _, queue := range queues {
		fmt.Fprintf(table, "%s\t%d\t%s\n", queue.Profile, queue.Jobs, formatAge(queue.OldestAgeSeconds))
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
