package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		LogPath:     filepath.Join(workDir, ".tart-runner-fleet", "runner.log"),
		MaxJITBytes: 1 << 20,
	}, nil
}

type runBootstrap func(context.Context, io.Reader, guestbootstrap.Config) error

// withCapabilities applies the one optional argument this helper accepts. The
// value form `--require-capabilities=a,b` is the whole grammar: a single token
// keeps the parser trivial and keeps the daemon's argument vector fixed-length.
func withCapabilities(base guestbootstrap.Config, args []string) (guestbootstrap.Config, error) {
	if len(args) == 0 {
		return base, nil
	}
	if len(args) > 1 {
		return base, guestbootstrap.ErrInput
	}
	value, found := strings.CutPrefix(args[0], guestbootstrap.CapabilityFlag+"=")
	if !found {
		return base, guestbootstrap.ErrInput
	}
	capabilities, err := guestbootstrap.ParseCapabilityList(value)
	if err != nil {
		return base, err
	}
	base.RequiredCapabilities = capabilities
	return base, nil
}

func execute(args []string, stdin io.Reader, stderr io.Writer, run runBootstrap) int {
	config, err := withCapabilities(guestConfig, args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "usage: tart-runner-fleet-bootstrap ["+guestbootstrap.CapabilityFlag+"=name[,name...]]")
		return 2
	}
	if run == nil {
		_, _ = fmt.Fprintln(stderr, "runner bootstrap failed")
		return 1
	}
	runErr := run(context.Background(), stdin, config)
	if runErr == nil {
		return 0
	}
	// The one failure this helper may describe. A capability check is decided
	// from the operator's own flag and the image's own manifest before standard
	// input is read at all, so its message provably cannot carry the JIT value;
	// the two statuses distinguish the two opposite repairs for the daemon.
	var capability *guestbootstrap.CapabilityError
	if errors.As(runErr, &capability) {
		_, _ = fmt.Fprintln(stderr, capability.Error())
		if capability.Unverifiable() {
			return guestbootstrap.ExitCapabilityUnverifiable
		}
		return guestbootstrap.ExitCapabilityMissing
	}
	// Never reflect any other child error: it may contain the JIT value supplied
	// on stdin even when the production launcher itself is careful.
	_, _ = fmt.Fprintln(stderr, "runner bootstrap failed")
	return 1
}

func main() {
	os.Exit(execute(os.Args[1:], os.Stdin, os.Stderr, (guestbootstrap.Bootstrap{Launcher: guestbootstrap.ExecLauncher{}}).Run))
}
