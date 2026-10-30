// Command tart-runner-fleet-host-launcher is the one program a host-process
// node's daemon may run as the runner user (ADR 0090). It is installed
// root-owned at /usr/local/libexec/tart-runner-fleet-host-launcher by
// deploy/macos-host-runner/setup.sh and reached only through the sudoers rule
// that script installs. Every verb is in internal/hostlauncher.
package main

import (
	"os"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/hostlauncher"
)

func main() { os.Exit(hostlauncher.Production().Main(os.Args[1:])) }
