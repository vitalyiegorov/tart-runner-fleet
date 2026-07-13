package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

var guestConfig = defaultGuestConfig()

func defaultGuestConfig() guestbootstrap.Config {
	home, _ := os.UserHomeDir()
	config, _ := guestConfigForHome(home)
	return config
}

func guestConfigForHome(home string) (guestbootstrap.Config, error) {
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) != home {
		return guestbootstrap.Config{}, guestbootstrap.ErrFilesystem
	}
	workDir := filepath.Join(home, "actions-runner")
	return guestbootstrap.Config{
		RunnerPath:  filepath.Join(workDir, "run.sh"),
		WorkDir:     workDir,
		LogPath:     filepath.Join(workDir, "_diag", "tart-runner-fleet.log"),
		MaxJITBytes: 1 << 20,
	}, nil
}

type runBootstrap func(context.Context, io.Reader, guestbootstrap.Config) error

func execute(args []string, stdin io.Reader, stderr io.Writer, run runBootstrap) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintln(stderr, "runner bootstrap takes no arguments")
		return 2
	}
	if run == nil || run(context.Background(), stdin, guestConfig) != nil {
		// Never reflect a child error: it may contain the JIT value supplied on
		// stdin even when the production launcher itself is careful.
		_, _ = fmt.Fprintln(stderr, "runner bootstrap failed")
		return 1
	}
	return 0
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stderr, (guestbootstrap.Bootstrap{Launcher: guestbootstrap.ExecLauncher{}}).Run))
}
