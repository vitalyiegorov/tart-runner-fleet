//go:build !darwin

package app

import "testing"

func TestHostProductVersionIsNotReadOffDarwin(t *testing.T) {
	if version, err := HostProductVersion(); err == nil || version != "" {
		t.Fatalf("version=%q err=%v", version, err)
	}
}
