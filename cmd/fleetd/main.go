package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

var version = "dev"
var exit = os.Exit
var run = runDaemon

type options struct {
	ConfigPath    string
	DatabasePath  string
	HealthAddress string
	AdminSocket   string
	Mode          reconcile.Mode
	CanaryScope   string
	CanaryProfile string
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	flags := flag.NewFlagSet("fleetd", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "fleet.json", "configuration path")
	databasePath := flags.String("database", "fleet.db", "SQLite state path")
	healthAddress := flags.String("health-address", "127.0.0.1:9876", "local health listener")
	adminSocket := flags.String("admin-socket", adminapi.DefaultSocketPath(), "private fleetctl Unix socket")
	mode := flags.String("mode", string(reconcile.Observe), "observe, shadow, canary, or authority")
	canaryScope := flags.String("canary-scope", "", "exact GitHub scope selected for canary mutation")
	canaryProfile := flags.String("canary-profile", "", "exact runner profile selected for canary mutation")
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "usage: fleetd version | run [--config path --database path --mode observe|shadow|canary|authority]")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return 2
	}
	opts := options{ConfigPath: *configPath, DatabasePath: *databasePath, HealthAddress: *healthAddress, AdminSocket: *adminSocket,
		Mode: reconcile.Mode(*mode), CanaryScope: *canaryScope, CanaryProfile: *canaryProfile}
	if opts.Mode == reconcile.Canary && (opts.CanaryScope == "" || opts.CanaryProfile == "") {
		fmt.Fprintln(stderr, "canary requires both --canary-scope and --canary-profile")
		return 2
	}
	if err := run(ctx, opts); err != nil {
		fmt.Fprintf(stderr, "fleetd failed: %v\n", err)
		return 1
	}
	return 0
}
