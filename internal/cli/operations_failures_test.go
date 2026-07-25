package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// `fleet operations` is the runbook's inspection command, so it is where the
// 2026-07-25 triage failed: 397 attempts on one wedged drain were published as
// "retrying 1" with no cause. The failure aggregate must render with the closed
// code and the worst attempt count, and a healthy fleet must keep the terse
// counts-only output it has today.
func TestOperationsRenderIncludesFailureCodesAndAttempts(t *testing.T) {
	status := healthyStatus()
	status.Data.Operations.Failures = []adminapi.OperationFailure{
		{Kind: "deregister", Code: "deregister:runner_busy", Count: 1, Attempts: 397},
		{Kind: "clone", Code: "acquire_jit", Count: 2, Attempts: 3},
	}
	var output bytes.Buffer

	renderCommand(&output, "operations", status)

	rendered := output.String()
	for _, want := range []string{"retrying", "dead", "KIND", "CODE", "COUNT", "ATTEMPTS",
		"deregister\tderegister:runner_busy\t1\t397", "clone\tacquire_jit\t2\t3"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("operations output missing %q:\n%s", want, rendered)
		}
	}

	var healthy bytes.Buffer
	renderCommand(&healthy, "operations", healthyStatus())
	if strings.Contains(healthy.String(), "KIND") {
		t.Fatalf("a healthy fleet must not print an empty failure table:\n%s", healthy.String())
	}
}

// The same aggregate must appear in the full status view, where an operator
// looking at one screen decides whether the fleet needs attention.
func TestStatusRenderSurfacesStuckOperations(t *testing.T) {
	status := healthyStatus()
	status.Data.Operations.Failures = []adminapi.OperationFailure{
		{Kind: "deregister", Code: "deregister:runner_forbidden", Count: 1, Attempts: 42},
	}
	var output bytes.Buffer

	renderCommand(&output, "status", status)

	if !strings.Contains(output.String(), "deregister:runner_forbidden") || !strings.Contains(output.String(), "42") {
		t.Fatalf("status output hides the stuck operation:\n%s", output.String())
	}
}
