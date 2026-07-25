package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// dischargeArgs is the complete, well-formed operator command for the
// 2026-07-25 phantom.
func dischargeArgs(extra ...string) []string {
	return append([]string{"operations", "discharge",
		"--operation", "op-ea9b705d234ad29f14e79b6d",
		"--instance", "trf-maestro-096ffcb3a52d8624",
		"--confirm", adminapi.DischargeConfirmation,
		"--reason", "GitHub 422 runner_busy leak; no owner run exists"}, extra...)
}

func runCLI(args []string, client fakeClient) (int, string, string, adminapi.DischargeRequest) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	sent := adminapi.DischargeRequest{}
	client.discharged = &sent
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) { return client, nil }}
	code := executeWith(context.Background(), args, stdout, stderr, deps)
	return code, stdout.String(), stderr.String(), sent
}

func TestDischargeCommandSendsTheGuardedMutation(t *testing.T) {
	client := fakeClient{discharge: adminapi.DischargeResult{APIVersion: adminapi.APIVersion,
		Kind: adminapi.DischargeKind, OperationID: "op-ea9b705d234ad29f14e79b6d",
		InstanceID: "trf-maestro-096ffcb3a52d8624", OperationDischarged: true, InstanceReaped: true, VMDeleted: true}}
	code, stdout, stderr, sent := runCLI(dischargeArgs("--reap-instance"), client)
	if code != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if sent.OperationID != "op-ea9b705d234ad29f14e79b6d" || sent.InstanceID != "trf-maestro-096ffcb3a52d8624" ||
		!sent.ReapInstance || sent.Confirm != adminapi.DischargeConfirmation || sent.Reason == "" {
		t.Fatalf("sent=%#v", sent)
	}
	for _, fragment := range []string{"op-ea9b705d234ad29f14e79b6d", "operation discharged true",
		"instance reaped true", "vm deleted true"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("output %q omits %q", stdout, fragment)
		}
	}
}

// Without --reap-instance the mutation is the smaller one: no VM removal is
// requested, so a mistyped command cannot delete a guest.
func TestDischargeDefaultsToNotReapingTheInstance(t *testing.T) {
	client := fakeClient{discharge: adminapi.DischargeResult{APIVersion: adminapi.APIVersion,
		Kind: adminapi.DischargeKind, OperationDischarged: true}}
	code, _, stderr, sent := runCLI(dischargeArgs(), client)
	if code != exitSuccess || sent.ReapInstance {
		t.Fatalf("exit=%d reap=%t stderr=%s", code, sent.ReapInstance, stderr)
	}
}

func TestDischargeCommandEmitsJSON(t *testing.T) {
	client := fakeClient{discharge: adminapi.DischargeResult{APIVersion: adminapi.APIVersion,
		Kind: adminapi.DischargeKind, OperationID: "op-1", OperationDischarged: true}}
	code, stdout, stderr, _ := runCLI(dischargeArgs("--output", "json"), client)
	if code != exitSuccess || !strings.Contains(stdout, `"operationDischarged": true`) {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

// The guards the operator sees before anything is sent. A refused command must
// never reach the daemon, and must never exit zero.
func TestDischargeRefusesUnguardedInvocations(t *testing.T) {
	for name, testCase := range map[string]struct {
		args []string
		code int
	}{
		"no identities": {args: []string{"operations", "discharge", "--confirm",
			adminapi.DischargeConfirmation, "--reason", "leak"}, code: exitUsage},
		"no instance": {args: []string{"operations", "discharge", "--operation", "op-1",
			"--confirm", adminapi.DischargeConfirmation, "--reason", "leak"}, code: exitUsage},
		"unparseable flags": {args: []string{"operations", "discharge", "--nonsense"}, code: exitUsage},
		"positional argument": {args: []string{"operations", "discharge", "--operation", "op-1", "--instance", "trf-1",
			"--confirm", adminapi.DischargeConfirmation, "--reason", "leak", "extra"}, code: exitUsage},
		"invalid output": {args: dischargeArgs("--output", "yaml"), code: exitUsage},
		"wrong confirmation": {args: []string{"operations", "discharge", "--operation", "op-1", "--instance", "trf-1",
			"--confirm", "yes", "--reason", "leak"}, code: exitUnsafe},
		"no confirmation": {args: []string{"operations", "discharge", "--operation", "op-1", "--instance", "trf-1",
			"--reason", "leak"}, code: exitUnsafe},
		"blank reason": {args: []string{"operations", "discharge", "--operation", "op-1", "--instance", "trf-1",
			"--confirm", adminapi.DischargeConfirmation, "--reason", "   "}, code: exitUnsafe},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, _, sent := runCLI(testCase.args, fakeClient{})
			if code != testCase.code {
				t.Fatalf("exit=%d want %d", code, testCase.code)
			}
			if sent.OperationID != "" {
				t.Fatalf("refused command still contacted the daemon: %#v", sent)
			}
		})
	}
}

// The daemon's own refusal code is what the operator needs to see; the exit code
// must make a refusal impossible to mistake for success.
func TestDischargeReportsTheDaemonsRefusalCode(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		code int
		text string
	}{
		"vm running": {err: adminapi.Refusal{Code: adminapi.RefusalVMRunning}, code: exitUnsafe, text: "vm_running"},
		"not authority": {err: adminapi.Refusal{Code: adminapi.RefusalNotAuthority}, code: exitUnsafe,
			text: "not_authority"},
		"unknown operation": {err: adminapi.Refusal{Code: adminapi.RefusalUnknownOperation}, code: exitNotFound,
			text: "unknown_operation"},
		"vm delete failed": {err: adminapi.Refusal{Code: adminapi.RefusalVMDeleteFailed}, code: exitUnsafe,
			text: "vm_delete_failed"},
		"daemon unavailable": {err: adminapi.ErrResponse, code: exitUnavailable, text: "fleet unavailable"},
		"canceled":           {err: context.Canceled, code: exitUnavailable, text: "request canceled"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr, _ := runCLI(dischargeArgs(), fakeClient{dischargeErr: testCase.err})
			if code != testCase.code || !strings.Contains(stderr, testCase.text) {
				t.Fatalf("exit=%d stderr=%q want %d containing %q", code, stderr, testCase.code, testCase.text)
			}
		})
	}
}

func TestDischargeReportsAnUnreachableEndpoint(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) { return nil, adminapi.ErrInvalidEndpoint }}
	if code := executeWith(context.Background(), dischargeArgs(), stdout, stderr, deps); code != exitUnavailable {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr.String(), "connect") {
		t.Fatalf("stderr=%s", stderr)
	}
}

// `operations` must remain the read-only view. Only the explicit subcommand
// mutates, and the connection flags work in either position for both.
func TestOperationsStaysAReadOnlyViewAndAcceptsLeadingConnectionFlags(t *testing.T) {
	client := fakeClient{status: adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status"}}
	code, _, stderr, sent := runCLI([]string{"operations"}, client)
	if code != exitSuccess || sent.OperationID != "" {
		t.Fatalf("exit=%d sent=%#v stderr=%s", code, sent, stderr)
	}
	code, _, stderr, sent = runCLI([]string{"--timeout", "3s", "operations"}, client)
	if code != exitSuccess || sent.OperationID != "" {
		t.Fatalf("exit=%d sent=%#v stderr=%s", code, sent, stderr)
	}
	mutating := fakeClient{discharge: adminapi.DischargeResult{APIVersion: adminapi.APIVersion,
		Kind: adminapi.DischargeKind, OperationDischarged: true}}
	code, _, stderr, sent = runCLI(append([]string{"--timeout", "3s"}, dischargeArgs()...), mutating)
	if code != exitSuccess || sent.OperationID == "" {
		t.Fatalf("exit=%d sent=%#v stderr=%s", code, sent, stderr)
	}
}

// The parked operations must be visible in the same command an operator already
// runs; otherwise the identity the mutation needs is only in the logs.
func TestOperationsRendersTheParkedDeadLetters(t *testing.T) {
	status := adminapi.StatusEnvelope{APIVersion: adminapi.APIVersion, Kind: "Status"}
	status.Data.Operations = adminapi.OperationSummary{Retrying: 0, Dead: 1,
		Failures: []adminapi.OperationFailure{{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: 835}},
		DeadLetters: []adminapi.DeadLetter{{OperationID: "op-ea9b705d234ad29f14e79b6d", Kind: "deregister",
			Code: "deregister:runner_busy", ResourceID: "trf-maestro-096ffcb3a52d8624", Attempts: 835, Parked: true}}}
	code, stdout, stderr, _ := runCLI([]string{"operations"}, fakeClient{status: status})
	if code != exitSuccess {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	for _, fragment := range []string{"OPERATION", "op-ea9b705d234ad29f14e79b6d", "trf-maestro-096ffcb3a52d8624", "true"} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("operations output %q omits %q", stdout, fragment)
		}
	}
	code, stdout, _, _ = runCLI([]string{"status"}, fakeClient{status: status})
	if code != exitSuccess || !strings.Contains(stdout, "parked true: op-ea9b705d234ad29f14e79b6d") {
		t.Fatalf("status output %q hides the parked operation", stdout)
	}
}

// The help text must name the remedy. An operator working the incident reads
// `fleet --help` first, and the old text promised no mutations existed at all.
func TestHelpDocumentsTheGuardedDischarge(t *testing.T) {
	stdout := &bytes.Buffer{}
	writeHelp(stdout)
	for _, fragment := range []string{"operations discharge", adminapi.DischargeConfirmation, "--reap-instance",
		"docs/OPERATIONS.md"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Fatalf("help omits %q", fragment)
		}
	}
}
