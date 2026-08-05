package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodeConfigJSON is one node's whole configuration, parameterised by the two
// facts a cross-node comparison is about: what its Linux image declares and what
// its own scale set requires behind the shared `linux-small` label.
func nodeConfigJSON(scaleSet, declares, requires string) string {
	return `{"baseVm":"linux","vmPrefix":"gha","pollSeconds":20,"maxLinuxWhenMacosIdle":1,` +
		`"maxLinuxCpu":2,"maxLinuxMemoryMb":4096,"linuxReservationAgeSeconds":300,"minFreeDiskGb":1` +
		declares +
		`,"linuxProfiles":[{"id":"small","label":"linux-small","cpu":1,"memoryMb":2048}],` +
		`"macosBurst":{"enabled":false},"github":{"scaleSets":[{"profile":"small","name":"` + scaleSet +
		`","id":1,"maxCapacity":1,"labels":["self-hosted","linux-small"]` + requires + `}]},` +
		`"targets":[{"type":"repo","slug":"owner/repo","maxActive":1}]}`
}

func writeNode(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestConfigValidateComparesSeveralNodes covers the one behaviour a second path
// adds: the rules of ADR 0034 that are knowable only when every node is in hand.
// One path must keep behaving exactly as it did, which the case below asserts by
// leaving a node that would fail the cross-node rule valid on its own.
func TestConfigValidateComparesSeveralNodes(t *testing.T) {
	dir := t.TempDir()
	declaring := writeNode(t, dir, "mac-mini.json",
		nodeConfigJSON("fleet-repo-small-mini", `,"baseImageCapabilities":["redroid-android"]`,
			`,"requiresCapabilities":["redroid-android"]`))
	lean := writeNode(t, dir, "mac-studio.json", nodeConfigJSON("fleet-repo-small-studio", "", ""))
	equal := writeNode(t, dir, "geekom.json",
		nodeConfigJSON("fleet-repo-small-geekom", `,"baseImageCapabilities":["redroid-android"]`, ""))
	collides := writeNode(t, dir, "clone.json",
		nodeConfigJSON("fleet-repo-small-mini", `,"baseImageCapabilities":["redroid-android"]`, ""))
	deps := defaultDependencies()

	// Each node passes on its own, including the lean one: nothing in its own
	// file is wrong, which is exactly why the incident was invisible per node.
	for _, path := range []string{declaring, lean, equal, collides} {
		var stdout, stderr bytes.Buffer
		if code := executeWith(context.Background(), []string{"config", "validate", path}, &stdout, &stderr, deps); code != exitSuccess {
			t.Fatalf("single path %s: code=%d stderr=%q", path, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "configuration is valid: "+path) {
			t.Fatalf("single-path output changed: %q", stdout.String())
		}
	}

	var pairStdout, pairStderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "validate", declaring, equal},
		&pairStdout, &pairStderr, deps); code != exitSuccess {
		t.Fatalf("matched pair: code=%d stderr=%q", code, pairStderr.String())
	}

	var leanStdout, leanStderr bytes.Buffer
	code := executeWith(context.Background(), []string{"config", "validate", declaring, lean}, &leanStdout, &leanStderr, deps)
	if code != exitFailure {
		t.Fatalf("unequal pair: code=%d stdout=%q", code, leanStdout.String())
	}
	for _, fragment := range []string{"linux-small", "redroid-android", "mac-studio.json", "mac-mini.json"} {
		if !strings.Contains(leanStderr.String(), fragment) {
			t.Errorf("cross-node failure = %q, missing %q", leanStderr.String(), fragment)
		}
	}

	var ownStdout, ownStderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "validate", declaring, collides},
		&ownStdout, &ownStderr, deps); code != exitFailure ||
		!strings.Contains(ownStderr.String(), "exactly one node") {
		t.Fatalf("duplicate ownership: code=%d stderr=%q", code, ownStderr.String())
	}

	var jsonStdout, jsonStderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "validate", "--output", "json", declaring, equal},
		&jsonStdout, &jsonStderr, deps); code != exitSuccess {
		t.Fatalf("json pair: code=%d stderr=%q", code, jsonStderr.String())
	}
	for _, fragment := range []string{`"valid": true`, `"paths"`, declaring, equal} {
		if !strings.Contains(jsonStdout.String(), fragment) {
			t.Errorf("json output = %q, missing %q", jsonStdout.String(), fragment)
		}
	}

	var missingStdout, missingStderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"config", "validate", declaring, filepath.Join(dir, "absent.json")},
		&missingStdout, &missingStderr, deps); code != exitFailure {
		t.Fatalf("absent second path: code=%d", code)
	}
}
