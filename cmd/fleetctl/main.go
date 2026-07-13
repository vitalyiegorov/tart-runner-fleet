package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

const (
	exitSuccess     = 0
	exitFailure     = 1
	exitUsage       = 2
	exitNotFound    = 3
	exitUnavailable = 4
	exitDegraded    = 5
	exitUnsafe      = 6
)

var version = "dev"
var exit = os.Exit

type apiClient interface {
	Status(context.Context) (adminapi.StatusEnvelope, error)
	Probe(context.Context, bool) (adminapi.Check, error)
	Metrics(context.Context) (string, error)
}

type dependencies struct {
	newClient func(string, time.Duration) (apiClient, error)
}

func defaultDependencies() dependencies {
	return dependencies{newClient: func(endpoint string, timeout time.Duration) (apiClient, error) {
		return adminapi.NewClient(endpoint, timeout)
	}}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exit(executeWith(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDependencies()))
}

func execute(args []string, stdout, stderr io.Writer) int {
	return executeWith(context.Background(), args, stdout, stderr, defaultDependencies())
}

func executeWith(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) == 0 {
		writeHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeHelp(stdout)
		return exitSuccess
	case "version":
		return runVersion(args[1:], stdout, stderr)
	case "api-version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: fleetctl api-version")
			return exitUsage
		}
		fmt.Fprintln(stdout, adminapi.APIVersion)
		return exitSuccess
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "validate-config":
		return runConfig(append([]string{"validate"}, args[1:]...), stdout, stderr)
	case "status", "queues", "instances", "operations", "observations", "health", "doctor", "metrics":
		return runRemote(ctx, args[0], args[1:], stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		writeHelp(stderr)
		return exitUsage
	}
}

type remoteOptions struct {
	endpoint     string
	timeout      time.Duration
	output       string
	requireReady bool
}

func parseRemote(command string, args []string, stderr io.Writer) (remoteOptions, int) {
	flags := flag.NewFlagSet("fleetctl "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	opts := remoteOptions{}
	flags.StringVar(&opts.endpoint, "endpoint", adminapi.DefaultEndpoint(), "local unix:// or loopback http:// endpoint")
	flags.DurationVar(&opts.timeout, "timeout", 5*time.Second, "request timeout (maximum 30s)")
	flags.StringVar(&opts.output, "output", "table", "output format: table or json")
	flags.StringVar(&opts.output, "o", "table", "output format: table or json")
	flags.BoolVar(&opts.requireReady, "require-ready", false, "exit 5 unless the controller is ready")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return remoteOptions{}, exitUsage
	}
	if opts.output != "table" && opts.output != "json" {
		fmt.Fprintf(stderr, "invalid output %q: use table or json\n", opts.output)
		return remoteOptions{}, exitUsage
	}
	if opts.timeout <= 0 || opts.timeout > 30*time.Second {
		fmt.Fprintln(stderr, "invalid timeout: use a duration between 0 and 30s")
		return remoteOptions{}, exitUsage
	}
	return opts, exitSuccess
}

func runRemote(ctx context.Context, command string, args []string, stdout, stderr io.Writer, deps dependencies) int {
	opts, code := parseRemote(command, args, stderr)
	if code != exitSuccess {
		return code
	}
	client, err := deps.newClient(opts.endpoint, opts.timeout)
	if err != nil {
		fmt.Fprintf(stderr, "connect: %v\n", err)
		return exitUnavailable
	}
	switch command {
	case "metrics":
		metrics, err := client.Metrics(ctx)
		if err != nil {
			return remoteError(stderr, err)
		}
		fmt.Fprint(stdout, metrics)
		return exitSuccess
	case "health":
		return runHealth(ctx, client, opts.output, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, client, opts.output, stdout, stderr)
	default:
		status, err := client.Status(ctx)
		if err != nil {
			return remoteError(stderr, err)
		}
		if opts.output == "json" {
			if err := writeJSON(stdout, viewFor(command, status)); err != nil {
				return remoteError(stderr, err)
			}
		} else {
			renderCommand(stdout, command, status)
		}
		if opts.requireReady && !status.Data.Ready.OK {
			return exitDegraded
		}
		return exitSuccess
	}
}

func viewFor(command string, status adminapi.StatusEnvelope) any {
	switch command {
	case "queues":
		return status.Data.Queues
	case "instances":
		return status.Data.Instances
	case "operations":
		return status.Data.Operations
	case "observations":
		return status.Data.Observations
	default:
		return status
	}
}

func runHealth(ctx context.Context, client apiClient, output string, stdout, stderr io.Writer) int {
	live, err := client.Probe(ctx, false)
	if err != nil {
		return remoteError(stderr, err)
	}
	ready, err := client.Probe(ctx, true)
	if err != nil {
		return remoteError(stderr, err)
	}
	result := struct {
		APIVersion string         `json:"apiVersion"`
		Live       adminapi.Check `json:"live"`
		Ready      adminapi.Check `json:"ready"`
	}{adminapi.APIVersion, live, ready}
	if output == "json" {
		_ = writeJSON(stdout, result)
	} else {
		renderCheck(stdout, "live", live)
		renderCheck(stdout, "ready", ready)
	}
	if !live.OK || !ready.OK {
		return exitDegraded
	}
	return exitSuccess
}

func runDoctor(ctx context.Context, client apiClient, output string, stdout, stderr io.Writer) int {
	status, err := client.Status(ctx)
	if err != nil {
		return remoteError(stderr, err)
	}
	metrics, err := client.Metrics(ctx)
	if err != nil {
		return remoteError(stderr, err)
	}
	checks := []doctorCheck{
		{Name: "admin API", OK: status.APIVersion == adminapi.APIVersion, Detail: status.APIVersion},
		{Name: "daemon live", OK: status.Data.Live.OK, Detail: joinReasons(status.Data.Live)},
		{Name: "scheduler ready", OK: status.Data.Ready.OK, Detail: joinReasons(status.Data.Ready)},
		{Name: "metrics", OK: metrics != "", Detail: "bounded endpoint responds"},
	}
	if output == "json" {
		_ = writeJSON(stdout, struct {
			APIVersion string        `json:"apiVersion"`
			Checks     []doctorCheck `json:"checks"`
		}{adminapi.APIVersion, checks})
	} else {
		renderDoctor(stdout, checks)
	}
	for _, check := range checks {
		if !check.OK {
			return exitDegraded
		}
	}
	return exitSuccess
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fleetctl version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "table", "output format: table or json")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*output != "table" && *output != "json") {
		if *output != "table" && *output != "json" {
			fmt.Fprintln(stderr, "invalid output: use table or json")
		}
		return exitUsage
	}
	if *output == "json" {
		_ = writeJSON(stdout, struct {
			Version    string `json:"version"`
			APIVersion string `json:"apiVersion"`
		}{version, adminapi.APIVersion})
	} else {
		fmt.Fprintln(stdout, version)
	}
	return exitSuccess
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, "usage: fleetctl config validate [--output table|json] <path>")
		return exitUsage
	}
	flags := flag.NewFlagSet("fleetctl config validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "table", "output format: table or json")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 1 || (*output != "table" && *output != "json") {
		return exitUsage
	}
	path := flags.Arg(0)
	// #nosec G304 -- the operator explicitly selects the configuration file to validate.
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "open config: %v\n", err)
		return exitFailure
	}
	defer file.Close()
	if _, err := config.Decode(file); err != nil {
		fmt.Fprintf(stderr, "invalid config: %v\n", err)
		return exitFailure
	}
	if *output == "json" {
		_ = writeJSON(stdout, struct {
			Valid bool   `json:"valid"`
			Path  string `json:"path"`
		}{true, path})
	} else {
		fmt.Fprintf(stdout, "configuration is valid: %s\n", path)
	}
	return exitSuccess
}

func remoteError(stderr io.Writer, err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "request canceled")
	} else {
		fmt.Fprintf(stderr, "fleet unavailable: %v\n", err)
	}
	return exitUnavailable
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeHelp(output io.Writer) {
	fmt.Fprint(output, `fleetctl — safe operator interface for Tart Runner Fleet

READ-ONLY COMMANDS (observe/shadow safe)
  fleetctl status [--output table|json] [--require-ready]
  fleetctl queues|instances|operations|observations [--output table|json]
  fleetctl health|doctor [--output table|json]
  fleetctl metrics
  fleetctl config validate <path>
  fleetctl version | api-version

CONNECTION
  --endpoint unix:///path/to/fleetd.sock   private local socket (default)
  --timeout 5s                            bounded request deadline

EXIT CODES
  0 success  1 failure  2 usage  3 not-found  4 unavailable  5 degraded  6 unsafe

Mutation commands are intentionally absent while authority mode is disabled.
`)
}
