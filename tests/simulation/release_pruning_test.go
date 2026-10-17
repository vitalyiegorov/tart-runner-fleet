package simulation_test

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
)

// Issue #352 is a supervisor/update state interleaving, outside the scheduler's
// VM world. Replay the actual pure deletion policy with independent cached and
// committed boot identities, then model a daemon exit and relaunch.
func TestACommittedUpdateCannotPruneTheCachedRelaunchBinary(t *testing.T) {
	for _, loaded := range []string{"v0.1.575", "v0.1.609", "v0.1.610"} {
		names := []string{"v0.1.575", "v0.1.608", "v0.1.609", "v0.1.610", "v0.1.612", "v0.1.613"}
		exists := map[string]bool{}
		for _, name := range names {
			exists[name] = true
		}
		for _, gone := range autoupdate.PrunableReleases(names, "v0.1.612", "v0.1.612", 2, loaded) {
			delete(exists, gone)
		}
		if !exists[loaded] {
			t.Fatalf("update committed but next relaunch cannot execute %s", loaded)
		}
		if exists["v0.1.608"] {
			t.Fatal("unreferenced superseded release leaked")
		}
	}
}
