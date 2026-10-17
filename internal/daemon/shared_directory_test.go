package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

// Replay #161: a Mac inherited another user's absent share. Refuse before
// opening durable state, even in observe, rather than failing every VM at ready.
func TestStartupRejectsAbsentMacOSShareBeforeOpeningState(t *testing.T) {
	for _, mode := range []reconcile.Mode{reconcile.Observe, reconcile.Shadow, reconcile.Canary, reconcile.Authority} {
		t.Run(string(mode), func(t *testing.T) {
			path := writeConfig(t, true)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			share := filepath.Join(t.TempDir(), "other-user", "ci-shared")
			body = []byte(strings.Replace(string(body), `"macosBurst":{`, `"macosBurst":{"sharedDirectoryPath":"`+share+`",`, 1))
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			d := newDependencies("darwin")
			d.openStore = func(context.Context, string) (runtimeStore, error) {
				t.Fatal("opened durable state before rejecting absent local share")
				return nil, nil
			}
			err = runWithDependencies(context.Background(), options{Mode: mode, ConfigPath: path, CanaryScope: "owner", CanaryProfile: "builder"}, d)
			if err == nil || !strings.Contains(err.Error(), "sharedDirectoryPath") || !strings.Contains(err.Error(), share) {
				t.Fatalf("startup error = %v", err)
			}
		})
	}
}
