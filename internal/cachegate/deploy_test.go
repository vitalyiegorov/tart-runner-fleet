package cachegate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func deployFile(t *testing.T, parts ...string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "deploy"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func uncommented(text string) string {
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// TestTheDropInIsTheShippedGateCommandAndOneFlag: a gate that drifted here would run
// old flags on the one node with the npm route.
func TestTheDropInIsTheShippedGateCommandAndOneFlag(t *testing.T) {
	execStart := regexp.MustCompile(`(?m)^ExecStart=(.+)$`)
	shipped := execStart.FindStringSubmatch(deployFile(t, "actions-cache", "tart-runner-fleet-cache-gate.service"))
	dropIn := deployFile(t, "npm-cache", "tart-runner-fleet-cache-gate.service.d", "npm-cache.conf")
	found := execStart.FindAllStringSubmatch(dropIn, -1)
	if shipped == nil || len(found) != 1 {
		t.Fatalf("shipped = %q, drop-in = %q", shipped, found)
	}
	if want := shipped[1] + " --npm-cache %h/.local/share/tart-runner-fleet-npm-cache/data"; found[0][1] != want {
		t.Fatalf("drop-in = %q\nwant      %q", found[0][1], want)
	}
	if !regexp.MustCompile(`(?m)^ExecStart=$`).MatchString(dropIn) {
		t.Fatal("the drop-in does not clear the shipped ExecStart")
	}
}
