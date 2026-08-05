package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// exitError is a child process that failed with a status, without executing one.
type exitError struct{ code int }

func (e exitError) Error() string { return "exit status" }
func (e exitError) ExitCode() int { return e.code }
func (e exitError) Unwrap() error { return nil }
func failing(code int) error      { return exitError{code: code} }

func jitSecret() *githubscaleset.JITSecret { return githubscaleset.NewJITSecret("jit-super-secret") }

// TestBootstrapArgumentVectorIsUnchangedWithoutCapabilities is the no-op half of
// the backstop: an image that predates the feature must be invoked with exactly
// the arguments it always was, or every guest in the fleet fails at once.
func TestBootstrapArgumentVectorIsUnchangedWithoutCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []string
		wantArgs     []string
		wantErr      error
	}{
		{name: "none", wantArgs: []string{"exec", "-i", "trf-small-1", bootstrapHelper}},
		{name: "one", capabilities: []string{"redroid-android"},
			wantArgs: []string{"exec", "-i", "trf-small-1", bootstrapHelper,
				guestbootstrap.CapabilityFlag + "=redroid-android"}},
		{name: "several", capabilities: []string{"container-runtime", "redroid-android"},
			wantArgs: []string{"exec", "-i", "trf-small-1", bootstrapHelper,
				guestbootstrap.CapabilityFlag + "=container-runtime,redroid-android"}},
		{name: "outside the vocabulary", capabilities: []string{"Redroid_Android"}, wantErr: operations.ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &captureStdin{}
			err := StdinBootstrapper{Runner: runner}.Bootstrap(context.Background(), "trf-small-1",
				jitSecret(), test.capabilities)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Bootstrap() = %v, want %v", err, test.wantErr)
				}
				if len(runner.args) != 0 {
					t.Fatalf("a refused capability still executed %v", runner.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bootstrap() = %v", err)
			}
			if strings.Join(runner.args, " ") != strings.Join(test.wantArgs, " ") {
				t.Fatalf("argv = %q, want %q", runner.args, test.wantArgs)
			}
		})
	}
}

// TestGuestCapabilityFailuresCarryAClosedReason is the ADR 0020 half: the guest
// answered, the answer was no, and the durable operation says which of the two
// opposite repairs the operator needs — without echoing a byte the child wrote.
func TestGuestCapabilityFailuresCarryAClosedReason(t *testing.T) {
	tests := []struct {
		name       string
		failure    error
		wantReason string
		wantCode   string
	}{
		{name: "missing capability", failure: failing(guestbootstrap.ExitCapabilityMissing),
			wantReason: ReasonGuestCapabilityMissing, wantCode: "bootstrap:guest_capability_missing"},
		{name: "unverifiable manifest", failure: failing(guestbootstrap.ExitCapabilityUnverifiable),
			wantReason: ReasonGuestCapabilityUnverifiable, wantCode: "bootstrap:guest_capability_unverifiable"},
		{name: "any other exit status stays a bare stage", failure: failing(1), wantCode: "bootstrap"},
		{name: "a failure that is not a child exit at all", failure: errors.New("ghs_do-not-leak"), wantCode: "bootstrap"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bootstrapErr := StdinBootstrapper{Runner: &captureStdin{err: test.failure}}.Bootstrap(
				context.Background(), "trf-small-1", jitSecret(), []string{"redroid-android"})
			if bootstrapErr == nil {
				t.Fatal("Bootstrap() succeeded")
			}
			persisted := bootstrapFailure(bootstrapErr)
			if got := FailureCode(persisted.Error()); got != test.wantCode {
				t.Fatalf("FailureCode(%q) = %q, want %q", persisted, got, test.wantCode)
			}
			var detail interface{ FailureReason() string }
			if !errors.As(persisted, &detail) {
				t.Fatal("a bootstrap failure must expose its bounded reason programmatically")
			}
			if detail.FailureReason() != test.wantReason {
				t.Fatalf("FailureReason() = %q, want %q", detail.FailureReason(), test.wantReason)
			}
			if strings.Contains(persisted.Error(), "ghs_do-not-leak") {
				t.Fatal("upstream text reached the durable failure")
			}
		})
	}
}

// TestProvisionThreadsTheProfilesCapabilitiesToTheGuest closes the loop: the
// daemon knows what the assigned scale sets require, and the guest is the only
// thing that can answer for the image.
func TestProvisionThreadsTheProfilesCapabilitiesToTheGuest(t *testing.T) {
	executor, state, _, _, _, bootstrap := provisionFixture(operations.StateReachable)
	var observed []string
	bootstrap.capabilities = &observed
	executor.Bootstrap = bootstrap
	executor.Capabilities = map[domain.ProfileID][]string{
		state.instance.Profile: {"container-runtime", "redroid-android"},
		"other":                {"never-asked-for"},
	}
	if err := executor.Execute(context.Background(), operations.Operation{
		Kind: OperationProvision, ResourceID: state.instance.ID}); err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if strings.Join(observed, ",") != "container-runtime,redroid-android" {
		t.Fatalf("guest was told %v", observed)
	}
}

func TestBootstrapFailureVocabularyIsClosed(t *testing.T) {
	for _, reason := range BootstrapFailureReasons() {
		if !ValidBootstrapFailureReason(reason) {
			t.Errorf("%q is listed but not valid", reason)
		}
	}
	if ValidBootstrapFailureReason("guest_capability_whatever") {
		t.Error("an unlisted reason was accepted")
	}
	if got := (stageError{stage: StageBootstrap, reason: "ghs_do-not-leak"}).FailureReason(); got != "" {
		t.Errorf("FailureReason() = %q, want it withheld", got)
	}
}
