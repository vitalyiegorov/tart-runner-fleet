//go:build !darwin && !linux

package guestbootstrap

import (
	"errors"
	"os"
)

func openLogNoFollow(string) (*os.File, error) {
	return nil, errors.New("secure runner log is unsupported on this platform")
}
