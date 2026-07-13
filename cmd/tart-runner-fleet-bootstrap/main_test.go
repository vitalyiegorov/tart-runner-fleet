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
	run = func(context.Context, io.Reader, guestbootstrap.Config) error { return errors.New("safe bootstrap failure") }
	if code := execute(nil, strings.NewReader("jit"), &stderr, run); code != 1 || stderr.String() != "runner bootstrap failed\n" {
		t.Fatalf("failure code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := execute([]string{"unexpected"}, strings.NewReader("jit"), &stderr, run); code != 2 || !strings.Contains(stderr.String(), "takes no arguments") {
		t.Fatalf("usage code=%d stderr=%q", code, stderr.String())
	}
}
