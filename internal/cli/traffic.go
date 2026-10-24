package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// runTraffic is `fleet traffic`: the bytes each repository's workflow moved
// on each profile in a window, from the guests' own end-of-job counters
// (ADR 0080). It reads the daemon's durable samples through the admin socket.
func runTraffic(ctx context.Context, connectionArgs, args []string, stdout, stderr io.Writer, deps dependencies) int {
	flags := flag.NewFlagSet("fleet traffic", flag.ContinueOnError)
	flags.SetOutput(stderr)
	since := flags.Duration("since", 24*time.Hour, "window to report, at most 720h")
	opts, code := parseRemoteInto(flags, append(connectionArgs, args...), stderr)
	if code != exitSuccess {
		return code
	}
	if *since <= 0 || *since > adminapi.MaxTrafficWindow {
		fmt.Fprintln(stderr, "invalid --since: use a duration between 0 and 720h")
		return exitUsage
	}
	client, err := deps.newClient(opts.endpoint, opts.timeout)
	if err != nil {
		fmt.Fprintf(stderr, "connect: %v\n", err)
		return exitUnavailable
	}
	traffic, err := client.Traffic(ctx, *since)
	if err != nil {
		return remoteError(stderr, err)
	}
	if opts.output == "json" {
		if err := writeJSON(stdout, traffic); err != nil {
			return remoteError(stderr, err)
		}
		return exitSuccess
	}
	renderTraffic(stdout, traffic.Data)
	return exitSuccess
}

func renderTraffic(output io.Writer, traffic adminapi.Traffic) {
	if traffic.Truncated {
		fmt.Fprintf(output, "truncated: only the newest %d jobs are summed\n", adminapi.MaxTrafficJobs)
	}
	if len(traffic.Totals) == 0 {
		fmt.Fprintln(output, "no samples in this window")
		return
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "REPO\tWORKFLOW\tPROFILE\tJOBS\tRECEIVED\tSENT")
	for _, total := range traffic.Totals {
		jobs := fmt.Sprintf("%d", total.Jobs)
		if total.UnmeasuredJobs > 0 {
			jobs = fmt.Sprintf("%d (%d unmeasured)", total.Jobs, total.UnmeasuredJobs)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", total.Repo, total.Workflow, total.Profile, jobs,
			formatBytes(total.ReceiveBytes), formatBytes(total.TransmitBytes))
	}
	_ = table.Flush()
}

// formatBytes renders decimal units, the ones network rates are quoted in.
func formatBytes(value int64) string {
	if value < 1000 {
		return fmt.Sprintf("%d B", value)
	}
	scaled := float64(value)
	for _, unit := range []string{"kB", "MB", "GB"} {
		scaled /= 1000
		if scaled < 1000 {
			return fmt.Sprintf("%.2f %s", scaled, unit)
		}
	}
	return fmt.Sprintf("%.2f TB", scaled/1000)
}
