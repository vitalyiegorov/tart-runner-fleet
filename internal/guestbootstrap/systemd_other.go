//go:build !linux

package guestbootstrap

func defaultSystemdRunPath() string { return "" }
