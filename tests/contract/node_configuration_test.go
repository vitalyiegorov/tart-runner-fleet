package contract_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// loadNodeConfigs decodes every configuration in one directory. Decoding runs
// Config.Validate, and BuildBindings is what fleetd runs at startup, so a file
// that survives both is a file a node could actually boot on.
func loadNodeConfigs(t *testing.T, dir string) []config.NodeConfig {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	nodes := make([]config.NodeConfig, 0, len(names))
	for _, name := range names {
		file, err := os.Open(filepath.Join(dir, name)) // #nosec G304 -- fixed repository fixture.
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		cfg, decodeErr := config.Decode(file)
		_ = file.Close()
		if decodeErr != nil {
			t.Fatalf("%s does not decode and validate: %v", name, decodeErr)
		}
		if err := app.ValidateBindings(cfg); err != nil {
			t.Fatalf("%s cannot build the bindings the daemon builds at startup: %v", name, err)
		}
		nodes = append(nodes, config.NodeConfig{Node: name, Config: cfg})
	}
	return nodes
}

// TestNodeConfigurationsAgreeAcrossTheFleet is the gate ADR 0034's amendment
// asks for, run over whatever `config/nodes/` holds.
//
// It is written to go live on its own. Today the directory holds two hand-written
// examples; when §5's render step lands and writes real node files there, this
// test starts guarding them on the same commit, with no edit here. That is
// deliberate: the enforcement must not depend on someone remembering to widen a
// list after the artifact it guards appears.
func TestNodeConfigurationsAgreeAcrossTheFleet(t *testing.T) {
	nodes := loadNodeConfigs(t, filepath.Join(documentationRoot(t), "config", "nodes"))
	if len(nodes) == 0 {
		t.Skip("config/nodes holds no node configurations yet")
	}
	if err := config.CheckFleet(nodes).Err(); err != nil {
		t.Fatalf("config/nodes violates the cross-node rules of ADR 0034:\n%v", err)
	}
}

// TestSharedLabelWithUnequalCapabilitiesIsRejected replays the 2026-08-04
// incident as configuration.
//
// Node A's Linux image carries a prewarmed Redroid container and its scale set
// says so; node C's image, built the same day from docs/LINUX_BASE_IMAGE.md and
// correct against every other contract this codebase imposes, does not. Both
// advertise `linux-xl`. Nothing in either file is wrong on its own — the fixture
// asserts that too — and the job that needs Redroid therefore failed
// deterministically on node C and passed deterministically on node A, which
// presents as flakiness in a repository this fleet does not own.
//
// The fixture lives in testdata rather than in config/nodes because it must stay
// broken: it is the evidence that the gate catches this, not a node anyone runs.
func TestSharedLabelWithUnequalCapabilitiesIsRejected(t *testing.T) {
	nodes := loadNodeConfigs(t, filepath.Join(documentationRoot(t), "tests", "contract", "testdata",
		"capability-parity-regression"))
	if len(nodes) != 2 {
		t.Fatalf("the regression fixture is two nodes, got %d", len(nodes))
	}
	err := config.CheckFleet(nodes).Err()
	if err == nil {
		t.Fatal("two nodes advertising linux-xl with unequal images passed the parity rule")
	}
	for _, fragment := range []string{"linux-xl", "redroid-android", "node-c.json", "node-a.json"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("parity failure = %q, missing %q", err, fragment)
		}
	}
}
