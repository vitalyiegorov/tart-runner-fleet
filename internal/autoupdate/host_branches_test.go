package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalHostConstructionAndCurrentFailClosed(t *testing.T) {
	valid := LocalHostConfig{RootDir: "/root", StateDir: "/state", LaunchAgentsDir: "/agents", Domain: "gui/1",
		Repository: "owner/repo", ReadyAttempts: 1, ReadyDelay: time.Second}
	tests := []LocalHostConfig{
		{},
		{RootDir: "relative", StateDir: "/state", LaunchAgentsDir: "/agents", Domain: "gui/1", Repository: "owner/repo", ReadyAttempts: 1},
		{RootDir: "/root", StateDir: "/state", LaunchAgentsDir: "/agents", Domain: "gui/1", Repository: "bad", ReadyAttempts: 1},
		{RootDir: "/root", StateDir: "/state", LaunchAgentsDir: "/agents", Domain: "gui/1", Repository: "owner/repo", ReadyAttempts: 0},
	}
	for _, cfg := range tests {
		if _, err := NewLocalHost(cfg, &fakeCommand{}); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("cfg=%+v error=%v", cfg, err)
		}
	}
	valid.UpdateInterval = time.Second
	if _, err := NewLocalHost(valid, &fakeCommand{}); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("short interval error=%v", err)
	}
	if _, err := NewLocalHost(valid, nil); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("nil command error=%v", err)
	}

	host, _, _, _, _ := hostFixture(t)
	installed := filepath.Join(host.stateDir, InstalledGenerationFile)
	if err := os.Remove(installed); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Current(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing current error=%v", err)
	}
	for _, body := range [][]byte{[]byte(`{"unknown":true}`), []byte(`{`), []byte(`{}`)} {
		if err := os.WriteFile(installed, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := host.Current(context.Background()); err == nil {
			t.Fatalf("invalid installed generation accepted: %s", body)
		}
	}
}

func TestAdoptRequiresMatchingHealthyBootAndNoTransaction(t *testing.T) {
	host, _, current, _, canonical := hostFixture(t)
	if err := os.WriteFile(canonical, []byte(current.ReleaseDir+"/fleetd --mode=authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.Adopt(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(host.launchAgentsDir, UpdaterPlist)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.stateDir, UpdateJournalFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.Adopt(context.Background(), current); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("transaction adoption error=%v", err)
	}
	if err := os.Remove(filepath.Join(host.stateDir, UpdateJournalFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := host.Adopt(context.Background(), current); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("mismatched boot error=%v", err)
	}
	host, command, current, _, canonical := hostFixture(t)
	if err := os.WriteFile(canonical, []byte(current.ReleaseDir+"/fleetd --mode=authority"), 0o600); err != nil {
		t.Fatal(err)
	}
	command.currentErr = errors.New("offline")
	if err := host.Adopt(context.Background(), current); err == nil {
		t.Fatal("unhealthy generation adopted")
	}
	current.ReleaseDir = "relative"
	if err := host.Adopt(context.Background(), current); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("invalid adoption error=%v", err)
	}
}

func TestValidationAndPreparationRejectEveryUntrustedBoundary(t *testing.T) {
	t.Run("invalid generation", func(t *testing.T) {
		host, _, _, _, _ := hostFixture(t)
		if err := host.Validate(context.Background(), Generation{}); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("canary", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		candidate.Mode = "canary"
		if err := host.Validate(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("release path", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		candidate.ReleaseDir = filepath.Dir(candidate.ReleaseDir)
		if err := host.Validate(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("identity", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, "RELEASE_VERSION"), []byte("wrong"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Validate(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("config", func(t *testing.T) {
		host, command, _, candidate, _ := hostFixture(t)
		command.fail = map[string]error{"config validate": errors.New("schema")}
		if err := host.Validate(context.Background(), candidate); err == nil {
			t.Fatal("candidate config failure ignored")
		}
	})
	t.Run("template placeholder", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		path := filepath.Join(candidate.ReleaseDir, "com.vitalyiegorov.tart-runner-fleet.authority.plist")
		if err := os.WriteFile(path, []byte("__UNKNOWN__"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("plist lint", func(t *testing.T) {
		host, command, current, candidate, _ := hostFixture(t)
		command.fail = map[string]error{"plutil -lint": errors.New("bad plist")}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("plist failure ignored")
		}
	})
}

func TestActivationReadinessCommitAndRollbackFaults(t *testing.T) {
	t.Run("missing journal", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		if err := host.Activate(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatal(err)
		}
	})
	t.Run("bootstrap", func(t *testing.T) {
		host, command, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		command.bootstrap = errors.New("bootstrap")
		if err := host.Activate(context.Background(), candidate); err == nil {
			t.Fatal("bootstrap failure ignored")
		}
	})
	t.Run("readiness identity and retry", func(t *testing.T) {
		host, command, _, candidate, _ := hostFixture(t)
		host.readyAttempts = 2
		host.readyDelay = time.Millisecond
		command.ready = `{"data":{"controllerVersion":"wrong","controllerMode":"authority","ready":{"ok":true}}}`
		if err := host.Ready(context.Background(), candidate); err == nil {
			t.Fatal("wrong controller accepted")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		command.readyErr = errors.New("offline")
		if err := host.Ready(ctx, candidate); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled readiness=%v", err)
		}
	})
	t.Run("updater bootstrap", func(t *testing.T) {
		host, command, _, candidate, _ := hostFixture(t)
		command.fail = map[string]error{"launchctl print": errors.New("missing"), "launchctl bootstrap": errors.New("denied")}
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("updater bootstrap failure ignored")
		}
	})
	t.Run("restore updater", func(t *testing.T) {
		host, command, current, candidate, canonical := hostFixture(t)
		updater := filepath.Join(host.launchAgentsDir, UpdaterPlist)
		if err := os.WriteFile(updater, []byte("old-updater"), 0o600); err != nil {
			t.Fatal(err)
		}
		command.readyErr = errors.New("offline")
		if err := (Controller{Host: host}).Apply(context.Background(), candidate); err == nil {
			t.Fatal("readiness failure accepted")
		}
		body, _ := os.ReadFile(updater)
		boot, _ := os.ReadFile(canonical)
		installed, _ := host.Current(context.Background())
		if string(body) != "old-updater" || string(boot) != "old-v1-plist" || installed != current {
			t.Fatalf("updater=%q boot=%q installed=%+v", body, boot, installed)
		}
	})
}

func TestQuiescenceRejectsInvalidIdentityAndOperations(t *testing.T) {
	for _, status := range []string{
		`not-json`,
		`{"data":{"controllerVersion":"wrong","controllerMode":"authority"}}`,
		`{"data":{"controllerVersion":"v1","controllerMode":"authority","operations":{"retrying":1}}}`,
		`{"data":{"controllerVersion":"v1","controllerMode":"authority","operations":{"dead":1}}}`,
		`{"data":{"controllerVersion":"v1","controllerMode":"authority","instances":[{"count":1}]}}`,
	} {
		host, command, current, _, _ := hostFixture(t)
		command.current = status
		if err := host.ensureQuiescent(context.Background(), current); err == nil {
			t.Fatalf("unsafe status accepted: %s", status)
		}
	}
	host, command, current, _, _ := hostFixture(t)
	command.currentErr = errors.New("offline")
	if err := host.ensureQuiescent(context.Background(), current); err == nil {
		t.Fatal("offline controller accepted")
	}
}

func TestJournalAndServiceHelpers(t *testing.T) {
	host, _, _, _, _ := hostFixture(t)
	if _, err := host.readJournal(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if serviceLabel("observe") != "com.vitalyiegorov.tart-runner-fleet" || !strings.HasSuffix(serviceLabel("authority"), ".authority") {
		t.Fatal("service labels changed")
	}
	journal, _ := json.Marshal(updateJournal{})
	if err := os.WriteFile(filepath.Join(host.stateDir, UpdateJournalFile), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := host.readJournal(); err != nil {
		t.Fatal(err)
	}
}

func TestChecksumVerifierRejectsMissingMalformedAndUnreadableEntries(t *testing.T) {
	dir := t.TempDir()
	if err := verifyChecksums(dir); err == nil {
		t.Fatal("missing checksum file accepted")
	}
	for _, sums := range []string{
		"malformed\n",
		strings.Repeat("x", 2<<20),
		strings.Repeat("0", 64) + "  ../fleetd\n",
		strings.Repeat("0", 64) + "  fleetd\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChecksums(dir); err == nil {
			t.Fatalf("unsafe sums accepted: %.20q", sums)
		}
	}
	body := []byte("fleetd")
	digest := sha256.Sum256(body)
	if err := os.WriteFile(filepath.Join(dir, "fleetd"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(digest[:])+"  fleetd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(dir); !errors.Is(err, ErrChecksum) {
		t.Fatalf("incomplete sums error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyChecksums(dir); !errors.Is(err, ErrChecksum) {
		t.Fatalf("untracked sums error=%v", err)
	}
}

func TestPrepareActivateCommitAndRollbackFilesystemFailures(t *testing.T) {
	t.Run("rollback missing journal", func(t *testing.T) {
		host, _, current, _, _ := hostFixture(t)
		if err := host.Rollback(context.Background(), current); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("template missing", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := os.Remove(filepath.Join(candidate.ReleaseDir, "com.vitalyiegorov.tart-runner-fleet.authority.plist")); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("missing template accepted")
		}
	})
	t.Run("canonical missing", func(t *testing.T) {
		host, _, current, candidate, canonical := hostFixture(t)
		if err := os.Remove(canonical); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("missing current boot plist accepted")
		}
	})
	t.Run("generation directory", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := os.WriteFile(filepath.Join(host.rootDir, "launchd"), []byte("blocked"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("unwritable generation directory accepted")
		}
	})
	t.Run("updater unreadable", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := os.Mkdir(filepath.Join(host.launchAgentsDir, UpdaterPlist), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("unreadable updater accepted")
		}
	})
	t.Run("journal path", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		journal := filepath.Join(host.stateDir, UpdateJournalFile)
		if err := os.Mkdir(journal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("unwritable journal accepted")
		}
	})
	t.Run("prepared missing", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		journal, _ := host.readJournal()
		if err := os.Remove(journal.PreparedPlist); err != nil {
			t.Fatal(err)
		}
		if err := host.Activate(context.Background(), candidate); err == nil {
			t.Fatal("missing prepared plist accepted")
		}
	})
	t.Run("kickstart", func(t *testing.T) {
		host, command, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		command.fail = map[string]error{"launchctl kickstart": errors.New("kickstart")}
		if err := host.Activate(context.Background(), candidate); err == nil {
			t.Fatal("kickstart failure ignored")
		}
	})
	t.Run("installed manifest", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		path := filepath.Join(host.stateDir, InstalledGenerationFile)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("unwritable manifest ignored")
		}
	})
	t.Run("updater install and initial bootstrap", func(t *testing.T) {
		host, command, _, candidate, _ := hostFixture(t)
		command.fail = map[string]error{"launchctl print": errors.New("missing")}
		if err := host.Commit(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(command.calls, "\n"), "launchctl bootstrap") {
			t.Fatal("initial updater was not bootstrapped")
		}
	})
	t.Run("updater reload bootout", func(t *testing.T) {
		host, command, _, candidate, _ := hostFixture(t)
		command.fail = map[string]error{"launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater": errors.New("denied")}
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("loaded updater bootout failure ignored")
		}
	})
	t.Run("commit cleanup", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		journal := filepath.Join(host.stateDir, UpdateJournalFile)
		replacePathWithNonEmptyDirectory(t, journal)
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("cleanup failure ignored")
		}
	})
	t.Run("rollback boundaries", func(t *testing.T) {
		for _, needle := range []string{"launchctl bootstrap", "launchctl kickstart"} {
			host, command, current, candidate, _ := hostFixture(t)
			if err := host.Prepare(context.Background(), current, candidate); err != nil {
				t.Fatal(err)
			}
			command.fail = map[string]error{needle: errors.New("denied")}
			if err := host.Rollback(context.Background(), current); err == nil {
				t.Fatalf("%s failure ignored", needle)
			}
		}
		host, _, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		journal, _ := host.readJournal()
		if err := os.Remove(journal.BackupPlist); err != nil {
			t.Fatal(err)
		}
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("missing rollback plist ignored")
		}
	})
	t.Run("rollback canonical", func(t *testing.T) {
		host, _, current, candidate, canonical := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		replaceFileWithDirectory(t, canonical)
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("canonical rollback failure ignored")
		}
	})
	t.Run("rollback updater backup", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		updater := filepath.Join(host.launchAgentsDir, UpdaterPlist)
		if err := os.WriteFile(updater, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		journal, _ := host.readJournal()
		if err := os.Remove(journal.BackupUpdater); err != nil {
			t.Fatal(err)
		}
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("missing updater backup ignored")
		}
	})
	t.Run("rollback removes new updater", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		updater := filepath.Join(host.launchAgentsDir, UpdaterPlist)
		if err := os.Mkdir(updater, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(updater, "child"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("updater removal failure ignored")
		}
	})
}

func TestRemainingGenerationFilesystemBoundaries(t *testing.T) {
	t.Run("observe template", func(t *testing.T) {
		host, command, current, candidate, _ := hostFixture(t)
		current.Mode, candidate.Mode = "observe", "observe"
		command.current = `{"data":{"controllerVersion":"v1","controllerMode":"observe","queues":[],"instances":[],"operations":{}}}`
		if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, CanonicalPlist), []byte("__RELEASE_DIR__/fleetd __STATE_DIR__/fleet.json --mode=observe"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("prepared path", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		path := filepath.Join(host.rootDir, "launchd", candidate.Version, candidate.Mode+".plist")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err == nil {
			t.Fatal("unwritable prepared plist accepted")
		}
	})
	t.Run("backup paths", func(t *testing.T) {
		for _, updater := range []bool{false, true} {
			host, _, current, candidate, _ := hostFixture(t)
			blocked := updateBackupFile
			if updater {
				blocked = updateBackupUpdaterFile
				if err := os.WriteFile(filepath.Join(host.launchAgentsDir, UpdaterPlist), []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(host.stateDir, blocked), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := host.Prepare(context.Background(), current, candidate); err == nil {
				t.Fatalf("unwritable backup accepted: %s", blocked)
			}
		}
	})
	t.Run("activate canonical", func(t *testing.T) {
		host, _, current, candidate, canonical := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		replaceFileWithDirectory(t, canonical)
		if err := host.Activate(context.Background(), candidate); err == nil {
			t.Fatal("unwritable canonical accepted")
		}
	})
	t.Run("commit updater", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		if err := os.Mkdir(filepath.Join(host.launchAgentsDir, UpdaterPlist), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("unwritable updater accepted")
		}
	})
	t.Run("commit current generation link", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		current := filepath.Join(host.rootDir, CurrentGenerationLink)
		replacePathWithNonEmptyDirectory(t, current)
		if err := host.Commit(context.Background(), candidate); err == nil {
			t.Fatal("unwritable current generation link accepted")
		}
	})
	t.Run("rollback updater write", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		updater := filepath.Join(host.launchAgentsDir, UpdaterPlist)
		if err := os.WriteFile(updater, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(updater); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(updater, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("unwritable updater restore accepted")
		}
	})
	t.Run("rollback installed generation", func(t *testing.T) {
		host, _, current, candidate, _ := hostFixture(t)
		if err := host.Prepare(context.Background(), current, candidate); err != nil {
			t.Fatal(err)
		}
		replaceFileWithDirectory(t, filepath.Join(host.stateDir, InstalledGenerationFile))
		if err := host.Rollback(context.Background(), current); err == nil {
			t.Fatal("unwritable installed generation accepted")
		}
	})
	t.Run("invalid journal", func(t *testing.T) {
		host, _, _, _, _ := hostFixture(t)
		if err := os.WriteFile(filepath.Join(host.stateDir, UpdateJournalFile), []byte(`{`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := host.readJournal(); err == nil {
			t.Fatal("invalid journal accepted")
		}
	})
}

func replaceFileWithDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func replacePathWithNonEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWriteRejectsInvalidParents(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "file")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(parent, "child"), []byte("x"), 0o600); err == nil {
		t.Fatal("invalid parent accepted")
	}
}

type failingAtomicFile struct {
	stage string
}

func (f *failingAtomicFile) Name() string { return "/tmp/atomic" }
func (f *failingAtomicFile) Write(body []byte) (int, error) {
	if f.stage == "write" {
		return 0, errors.New("write")
	}
	return len(body), nil
}
func (f *failingAtomicFile) Chmod(os.FileMode) error {
	if f.stage == "chmod" {
		return errors.New("chmod")
	}
	return nil
}
func (f *failingAtomicFile) Sync() error {
	if f.stage == "sync" || f.stage == "dir sync" {
		return errors.New("sync")
	}
	return nil
}
func (f *failingAtomicFile) Close() error {
	if f.stage == "close" || f.stage == "dir close" {
		return errors.New("close")
	}
	return nil
}

func TestAtomicWriteReportsEveryDurabilityFailure(t *testing.T) {
	for _, stage := range []string{"mkdir", "create", "chmod", "write", "sync", "close", "rename", "open dir", "dir sync", "dir close"} {
		t.Run(stage, func(t *testing.T) {
			file := &failingAtomicFile{stage: stage}
			if strings.HasPrefix(stage, "dir ") {
				file = &failingAtomicFile{}
			}
			ops := atomicWriteOps{
				mkdirAll: func(string, os.FileMode) error {
					if stage == "mkdir" {
						return errors.New(stage)
					}
					return nil
				},
				createTemp: func(string, string) (atomicWriteFile, error) {
					if stage == "create" {
						return nil, errors.New(stage)
					}
					return file, nil
				},
				remove: func(string) error { return nil },
				rename: func(string, string) error {
					if stage == "rename" {
						return errors.New(stage)
					}
					return nil
				},
				openDirectory: func(string) (atomicSyncCloser, error) {
					if stage == "open dir" {
						return nil, errors.New(stage)
					}
					return &failingAtomicFile{stage: stage}, nil
				},
			}
			if err := atomicWriteWith("/target/file", []byte("body"), 0o600, ops); err == nil {
				t.Fatalf("%s failure ignored", stage)
			}
		})
	}
	file := &failingAtomicFile{}
	ops := atomicWriteOps{mkdirAll: func(string, os.FileMode) error { return nil },
		createTemp: func(string, string) (atomicWriteFile, error) { return file, nil }, remove: func(string) error { return nil },
		rename: func(string, string) error { return nil }, openDirectory: func(string) (atomicSyncCloser, error) { return file, nil }}
	if err := atomicWriteWith("/target/file", []byte("body"), 0o600, ops); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicSymlinkReportsEveryDurabilityFailure(t *testing.T) {
	for _, stage := range []string{"create", "close", "remove", "symlink", "rename", "open dir", "dir sync", "dir close"} {
		t.Run(stage, func(t *testing.T) {
			file := &failingAtomicFile{stage: stage}
			if strings.HasPrefix(stage, "dir ") {
				file = &failingAtomicFile{}
			}
			removeCalls := 0
			ops := atomicSymlinkOps{
				createTemp: func(string, string) (atomicWriteFile, error) {
					if stage == "create" {
						return nil, errors.New(stage)
					}
					return file, nil
				},
				remove: func(string) error {
					removeCalls++
					if stage == "remove" && removeCalls == 1 {
						return errors.New(stage)
					}
					return nil
				},
				symlink: func(string, string) error {
					if stage == "symlink" {
						return errors.New(stage)
					}
					return nil
				},
				rename: func(string, string) error {
					if stage == "rename" {
						return errors.New(stage)
					}
					return nil
				},
				openDirectory: func(string) (atomicSyncCloser, error) {
					if stage == "open dir" {
						return nil, errors.New(stage)
					}
					return &failingAtomicFile{stage: stage}, nil
				},
			}
			if err := atomicSymlinkWith("/releases/v2", "/root/current", ops); err == nil {
				t.Fatalf("%s failure ignored", stage)
			}
		})
	}
	file := &failingAtomicFile{}
	ops := atomicSymlinkOps{
		createTemp:    func(string, string) (atomicWriteFile, error) { return file, nil },
		remove:        func(string) error { return nil },
		symlink:       func(string, string) error { return nil },
		rename:        func(string, string) error { return nil },
		openDirectory: func(string) (atomicSyncCloser, error) { return file, nil },
	}
	if err := atomicSymlinkWith("/releases/v2", "/root/current", ops); err != nil {
		t.Fatal(err)
	}
}
