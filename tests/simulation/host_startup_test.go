package simulation_test

import (
	"fmt"
	"io/fs"
	"math/rand"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

// The tick generator previously assumed startup always succeeded and had no
// host filesystem. Replay #161 with seeded startup evidence before allowing the
// existing fleet world to boot. This is a fleet defect, not an oracle defect:
// configuration copied from another Mac names a directory this Mac lacks.
func TestHostShareStartupBeforeFleetSimulation(t *testing.T) {
	for seed := int64(1); seed <= 24; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			order := rng.Perm(5)
			for _, state := range order {
				cfg := config.Default()
				cfg.MacOS.SharedDirectoryPath = "/Users/other/ci-shared"
				files := fstest.MapFS{}
				switch state {
				case 1:
					files["Users/other/ci-shared"] = &fstest.MapFile{}
				case 2:
					files["Users/other/ci-shared"] = &fstest.MapFile{Mode: fs.ModeDir}
				}
				stat := func(path string) (fs.FileInfo, error) {
					if state == 3 {
						return nil, fs.ErrPermission
					}
					if state == 4 {
						return nil, fs.ErrInvalid
					}
					return files.Stat(strings.TrimPrefix(path, "/"))
				}
				err := app.ValidateLocalHost(cfg, "darwin", stat)
				if state != 2 {
					if err == nil {
						t.Fatalf("unusable host share state %d allowed fleet startup", state)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				wc := macOSOnlyNodeWorld()
				w := newWorld(t, wc, generateTrace(seed, 8, wc))
				findings := w.run()
				w.close()
				if len(findings) > 0 {
					t.Fatalf("valid host startup: %+v", findings)
				}
			}
		})
	}
}
