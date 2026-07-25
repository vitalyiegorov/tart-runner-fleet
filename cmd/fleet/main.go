// Command fleet is the single control-plane executable. It carries both the
// daemon and the operator interface so that the two can never be different
// builds, which ADR 0019 records as the reason the previous `fleetd` and
// `fleetctl` binaries were merged.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/cli"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/daemon"
)

// version is injected at release build time and is the one build identity the
// daemon, the operator interface, GitHub, and telemetry all report.
var version = "dev"

var exit = os.Exit

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	exit(execute(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// execute routes `run` to the daemon and every other verb to the operator
// interface. Dispatch is by explicit subcommand rather than by argv[0], so the
// executable's name on disk never changes its behaviour.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "run" {
		return daemon.Execute(ctx, args, stdout, stderr, version)
	}
	return cli.Execute(ctx, args, stdout, stderr, version)
}
