//go:build !darwin

package app

import "errors"

// HostProductVersion has no answer off Darwin. ValidateLocalHost only asks it
// on a Darwin node, so a production call here is a wiring defect, and it fails
// closed like every other unreadable host fact.
func HostProductVersion() (string, error) {
	return "", errors.New("host macOS version is only readable on darwin")
}
