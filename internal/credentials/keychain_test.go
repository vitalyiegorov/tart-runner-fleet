package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	args []string
	out  []byte
	err  error
}

func (f *fakeRunner) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	f.args = append([]string{binary}, args...)
	return f.out, f.err
}

func TestKeychainLoadAndSecretRedaction(t *testing.T) {
	runner := &fakeRunner{out: []byte("private-key\n")}
	keychain := Keychain{Runner: runner, Timeout: time.Second}
	secret, err := keychain.Load(context.Background(), "fleet-app", "controller")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/security", "find-generic-password", "-w", "-s", "fleet-app", "-a", "controller"}
	if fmt.Sprint(runner.args) != fmt.Sprint(want) {
		t.Fatalf("argv = %q", runner.args)
	}
	if secret.Reveal() != "private-key" || secret.String() != redactedSecret || secret.GoString() != redactedSecret {
		t.Fatalf("secret behavior = %q %s %#v", secret.Reveal(), secret, secret)
	}
	if secret.LogValue().Kind() != slog.KindString || secret.LogValue().String() != redactedSecret {
		t.Fatal("slog value not redacted")
	}
	for _, marshal := range []func() error{
		func() error { _, err := secret.MarshalJSON(); return err },
		func() error { _, err := secret.MarshalText(); return err },
		func() error { _, err := secret.MarshalBinary(); return err },
	} {
		if err := marshal(); err == nil {
			t.Fatal("secret serialization succeeded")
		}
	}
	secret.Destroy()
	if secret.Reveal() != "" {
		t.Fatal("Destroy did not clear secret")
	}
}

func TestKeychainValidationAndFailures(t *testing.T) {
	tests := []struct {
		name, service, account string
		runner                 *fakeRunner
		want                   string
	}{
		{name: "service", account: "a", runner: &fakeRunner{}, want: "required"},
		{name: "account", service: "s", runner: &fakeRunner{}, want: "required"},
		{name: "command", service: "s", account: "a", runner: &fakeRunner{err: errors.New("boom")}, want: "read keychain"},
		{name: "empty", service: "s", account: "a", runner: &fakeRunner{out: []byte(" \n")}, want: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (Keychain{Runner: tt.runner}).Load(context.Background(), tt.service, tt.account)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestNilSecretAndDefaults(t *testing.T) {
	var secret *Secret
	if secret.Reveal() != "" || secret.String() != redactedSecret || secret.GoString() != redactedSecret {
		t.Fatal("nil secret behavior changed")
	}
	secret.Destroy()
	if _, err := secret.MarshalJSON(); err == nil {
		t.Fatal("nil secret serialized")
	}
	if _, err := secret.MarshalText(); err == nil {
		t.Fatal("nil secret text serialized")
	}
	if _, err := secret.MarshalBinary(); err == nil {
		t.Fatal("nil secret binary serialized")
	}
	if secret.LogValue().Kind() != slog.KindString || secret.LogValue().String() != redactedSecret {
		t.Fatal("nil slog value")
	}
	runner := &fakeRunner{out: []byte("x")}
	if _, err := (Keychain{Runner: runner}).Load(context.Background(), "s", "a"); err != nil {
		t.Fatal(err)
	}
}

func TestExecRunnerUsesArgumentVector(t *testing.T) {
	output, err := (ExecRunner{}).Run(context.Background(), "/usr/bin/printf", "%s", "safe value; $(ignored)")
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "safe value; $(ignored)" {
		t.Fatalf("output = %q", output)
	}
}

func TestKeychainDefaultRunnerFailureIsSanitized(t *testing.T) {
	_, err := (Keychain{Timeout: time.Millisecond}).Load(context.Background(), "tart-runner-fleet-test-does-not-exist", "nobody")
	if err == nil || !strings.Contains(err.Error(), "read keychain credential") {
		t.Fatalf("Load() error = %v", err)
	}
}
