//go:build linux

package guestbootstrap

func defaultSystemdRunPath() string { return "/usr/bin/systemd-run" }
