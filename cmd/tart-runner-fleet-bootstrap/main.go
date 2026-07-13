package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

var guestConfig = guestbootstrap.Config{
	RunnerPath:  "/opt/actions-runner/run.sh",
	WorkDir:     "/opt/actions-runner",
	LogPath:     "/opt/actions-runner/_diag/tart-runner-fleet.log",
	MaxJITBytes: 1 << 20,
}

type runBootstrap func(context.Context, io.Reader, guestbootstrap.Config) error

func execute(args []string, stdin io.Reader, stderr io.Writer, run runBootstrap) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "runner bootstrap takes no arguments")
		return 2
	}
	if run == nil || run(context.Background(), stdin, guestConfig) != nil {
		// Never reflect a child error: it may contain the JIT value supplied on
		// stdin even when the production launcher itself is careful.
		fmt.Fprintln(stderr, "runner bootstrap failed")
		return 1
	}
	return 0
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stderr, (guestbootstrap.Bootstrap{Launcher: guestbootstrap.ExecLauncher{}}).Run))
}
