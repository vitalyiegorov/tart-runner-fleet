package daemon

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

var run = runDaemon

type options struct {
	ConfigPath    string
	DatabasePath  string
	HealthAddress string
	AdminSocket   string
	Mode          reconcile.Mode
	CanaryScope   string
	CanaryProfile string
	// Version is the single build identity, injected by the fleet entry point
	// and reported to GitHub and telemetry. It is never a package-local default.
	Version string
}

// Execute runs the daemon command surface. The caller owns process exit and
// signal handling; version is injected by the single fleet entry point so the
// daemon and the operator CLI can never report different builds.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	return execute(ctx, args, stdout, stderr, version)
}

func execute(ctx context.Context, args []string, _ io.Writer, stderr io.Writer, version string) int {
	flags := flag.NewFlagSet("fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "fleet.json", "configuration path")
	databasePath := flags.String("database", "fleet.db", "SQLite state path")
	healthAddress := flags.String("health-address", "127.0.0.1:9876", "local health listener")
	adminSocket := flags.String("admin-socket", adminapi.DefaultSocketPath(), "private operator Unix socket")
	mode := flags.String("mode", string(reconcile.Observe), "observe, shadow, canary, or authority")
	canaryScope := flags.String("canary-scope", "", "exact GitHub scope selected for canary mutation")
	canaryProfile := flags.String("canary-profile", "", "exact runner profile selected for canary mutation")
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "usage: fleet run [--config path --database path --mode observe|shadow|canary|authority]")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return 2
	}
	opts := options{ConfigPath: *configPath, DatabasePath: *databasePath, HealthAddress: *healthAddress, AdminSocket: *adminSocket,
		Mode: reconcile.Mode(*mode), CanaryScope: *canaryScope, CanaryProfile: *canaryProfile, Version: version}
	if opts.Mode == reconcile.Canary && (opts.CanaryScope == "" || opts.CanaryProfile == "") {
		fmt.Fprintln(stderr, "canary requires both --canary-scope and --canary-profile")
		return 2
	}
	if err := run(ctx, opts); err != nil {
		fmt.Fprintf(stderr, "fleet daemon failed: %v\n", err)
		return 1
	}
	return 0
}
