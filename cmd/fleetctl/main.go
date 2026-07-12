package main

import (
	"fmt"
	"io"
	"os"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

var version = "dev"
var exit = os.Exit

func main() {
	exit(execute(os.Args[1:], os.Stdout, os.Stderr))
}

func execute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if len(args) == 2 && args[0] == "validate-config" {
		file, err := os.Open(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "open config: %v\n", err)
			return 1
		}
		defer file.Close()
		if _, err := config.Decode(file); err != nil {
			fmt.Fprintf(stderr, "invalid config: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "configuration is valid")
		return 0
	}
	fmt.Fprintln(stderr, "usage: fleetctl version | validate-config <path>")
	return 2
}
