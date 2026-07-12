package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

var version = "dev"
var exit = os.Exit
var run = runDaemon

type options struct {
	ConfigPath    string
	DatabasePath  string
	HealthAddress string
	Mode          reconcile.Mode
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
	mode := flags.String("mode", string(reconcile.Observe), "observe or shadow")
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "usage: fleetd version | run [--config path --database path --mode observe|shadow]")
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return 2
	}
	opts := options{ConfigPath: *configPath, DatabasePath: *databasePath, HealthAddress: *healthAddress, Mode: reconcile.Mode(*mode)}
	if err := run(ctx, opts); err != nil {
		fmt.Fprintf(stderr, "fleetd failed: %v\n", err)
		return 1
	}
	return 0
}
