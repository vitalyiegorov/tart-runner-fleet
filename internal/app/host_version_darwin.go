//go:build darwin

package app

import "syscall"

// HostProductVersion is the host's marketing macOS version, for example
// "26.5" or "27.0.1", read from the kernel rather than by running sw_vers.
func HostProductVersion() (string, error) {
	return syscall.Sysctl("kern.osproductversion")
}
