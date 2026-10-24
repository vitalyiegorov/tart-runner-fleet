package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// TestDoctorDegradesOnToolDrift is issue #414: the macOS image carried Maestro
// 2.6.1 while the consumers pinned 2.10.0, so every shard downloaded 315 MB over
// the home uplink and every check read healthy. A declared gap is exit 5.
func TestDoctorDegradesOnToolDrift(t *testing.T) {
	const reason = `macOS base image "macos-tartelet-base" does not carry maestro@2.10.0, pinned by scale sets ` +
		`maestro; every such job downloads it over the node's uplink`
	status := healthyStatus()
	status.Data.ImageTools = []adminapi.ImageTools{
		{Platform: "macOS", VM: "macos-tartelet-base", Declared: []string{"maestro@2.6.1"},
			Missing: []string{"maestro@2.10.0"}, Drift: true, Reason: reason},
	}
	status.Data.ImageToolCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "image tools", "does not carry maestro@2.10.0")
}

// TestDoctorNamesTheCarriedToolsWhenItPasses keeps what each image carries
// readable without opening a guest, the same terms the runner-version row has.
func TestDoctorNamesTheCarriedToolsWhenItPasses(t *testing.T) {
	status := healthyStatus()
	status.Data.ImageTools = []adminapi.ImageTools{
		{Platform: "linux", VM: "trf-runner", Declared: []string{"node@22.22.2", "node@24.22.1"}},
		{Platform: "macOS", VM: "macos-tartelet-base", Declared: []string{}},
	}
	status.Data.ImageToolCheck = &adminapi.Check{OK: true, Reasons: []string{}}
	out := doctorTable(t, status, exitSuccess)
	for _, want := range []string{"image tools", "linux node@22.22.2 node@24.22.1", "macOS none declared"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor must name the carried tools, missing %q:\n%s", want, out)
		}
	}
	status.Data.ImageTools = nil
	if out := doctorTable(t, status, exitSuccess); !strings.Contains(out, "no base image is booted") {
		t.Fatalf("a published empty set must say why it is empty:\n%s", out)
	}
}

// TestDoctorSaysWhenToolDriftWasNotReported is the handoff half: a daemon that
// never published is not a covered fleet, and doctor must not imply one.
func TestDoctorSaysWhenToolDriftWasNotReported(t *testing.T) {
	status := healthyStatus()
	status.Data.ImageTools, status.Data.ImageToolCheck = nil, nil
	if out := doctorTable(t, status, exitSuccess); !strings.Contains(out, "image tools") ||
		!strings.Contains(out, "not reported by this daemon") {
		t.Fatalf("doctor must say the check was not reported:\n%s", out)
	}
}

func doctorTable(t *testing.T, status adminapi.StatusEnvelope, want int) string {
	t.Helper()
	client := &fakeClient{status: status, metrics: "fleet_up 1"}
	var stdout, stderr bytes.Buffer
	if code := runDoctor(context.Background(), client, "", &stdout, &stderr); code != want {
		t.Fatalf("doctor exit = %d, want %d:\n%s", code, want, stdout.String())
	}
	return stdout.String()
}
