package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

func TestExecuteIsQuietOnSuccessAndSanitizesFailure(t *testing.T) {
	var stderr bytes.Buffer
	run := func(context.Context, io.Reader, guestbootstrap.Config) error { return nil }
	if code := execute(nil, strings.NewReader("jit"), &stderr, run); code != 0 || stderr.Len() != 0 {
		t.Fatalf("success code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	run = func(context.Context, io.Reader, guestbootstrap.Config) error {
		return errors.New("safe bootstrap failure")
	}
	if code := execute(nil, strings.NewReader("jit"), &stderr, run); code != 1 || stderr.String() != "runner bootstrap failed\n" {
		t.Fatalf("failure code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := execute([]string{"unexpected"}, strings.NewReader("jit"), &stderr, run); code != 2 ||
		!strings.Contains(stderr.String(), "usage: tart-runner-fleet-bootstrap ["+guestbootstrap.CapabilityFlag+"=") {
		t.Fatalf("usage code=%d stderr=%q", code, stderr.String())
	}
}

func TestGuestConfigUsesTheExecutingGuestUsersRunner(t *testing.T) {
	config, err := guestConfigForHome("/home/admin")
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkDir != "/home/admin/actions-runner" ||
		config.RunnerPath != "/home/admin/actions-runner/run.sh" ||
		config.LogPath != "/home/admin/actions-runner/.tart-runner-fleet/runner.log" {
		t.Fatalf("guest config = %#v", config)
	}
	if _, err := guestConfigForHome(""); err == nil {
		t.Fatal("empty guest home accepted")
	}
}
