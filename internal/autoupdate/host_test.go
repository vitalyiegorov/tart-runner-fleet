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

type fakeCommand struct {
	calls             []string
	ready             string
	current           string
	readyErr          error
	currentErr        error
	bootstrap         error
	bootstrapFailures int
	launchPrint       string
	printFailures     int
	fail              map[string]error
}

func (c *fakeCommand) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	for needle, err := range c.fail {
		if strings.Contains(call, needle) {
			return nil, err
		}
	}
	if strings.Contains(call, "status --require-ready") {
		if strings.Contains(call, "/v1/fleet") && c.current != "" {
			return []byte(c.current), c.currentErr
		}
		return []byte(c.ready), c.readyErr
	}
	if strings.Contains(call, "launchctl print") {
		if c.printFailures > 0 {
			c.printFailures--
			return nil, errors.New("launchd job not loaded")
		}
		return []byte(c.launchPrint), nil
	}
	if strings.Contains(call, "launchctl bootstrap") && c.bootstrap != nil {
		return nil, c.bootstrap
	}
	if strings.Contains(call, "launchctl bootstrap") && c.bootstrapFailures > 0 {
		c.bootstrapFailures--
		return nil, errors.New("transient launchd bootstrap failure")
	}
	return nil, nil
}

func makeRelease(t *testing.T, root, version string) string {
	t.Helper()
	dir := filepath.Join(root, "releases", version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	template := `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.vitalyiegorov.tart-runner-fleet.authority</string><key>ProgramArguments</key><array><string>__RELEASE_DIR__/fleet</string><string>--mode=authority</string><string>--config=__STATE_DIR__/fleet.json</string></array></dict></plist>`
	files := map[string][]byte{
		"RELEASE_VERSION": []byte(version + "\n"), "fleet": []byte("control-plane-" + version),
		"com.vitalyiegorov.tart-runner-fleet.authority.plist": []byte(template),
	}
	var sums strings.Builder
	for _, name := range []string{"RELEASE_VERSION", "fleet", "com.vitalyiegorov.tart-runner-fleet.authority.plist"} {
		body := files[name]
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(digest[:]) + "  " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func hostFixture(t *testing.T) (*LocalHost, *fakeCommand, Generation, Generation, string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	agents := filepath.Join(root, "LaunchAgents")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agents, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(state, "fleet.json")
	if err := os.WriteFile(configPath, []byte(`{"valid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	v1 := Generation{Version: "v1", Mode: "authority", ReleaseDir: makeRelease(t, root, "v1"), ConfigPath: configPath, Endpoint: "unix:///state/fleet.sock"}
	v2 := Generation{Version: "v2", Mode: "authority", ReleaseDir: makeRelease(t, root, "v2"), ConfigPath: configPath, Endpoint: "unix:///state/fleet.sock"}
	installed, _ := json.Marshal(v1)
	if err := os.WriteFile(filepath.Join(state, InstalledGenerationFile), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(agents, CanonicalPlist)
	if err := os.WriteFile(canonical, []byte("old-v1-plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := &fakeCommand{
		ready:   `{"data":{"controllerVersion":"v2","controllerMode":"authority","ready":{"ok":true}}}`,
		current: `{"data":{"controllerVersion":"v1","controllerMode":"authority","ready":{"ok":true},"queues":[],"instances":[],"operations":{"retrying":0,"dead":0}}}`,
	}
	host, err := NewLocalHost(LocalHostConfig{RootDir: root, StateDir: state, LaunchAgentsDir: agents,
		Domain: "gui/501", Repository: "owner/repo", ReadyAttempts: 1, ReadyDelay: time.Millisecond}, command)
	if err != nil {
		t.Fatal(err)
	}
	return host, command, v1, v2, canonical
}

func TestLocalHostAtomicallyPersistsTheBootGeneration(t *testing.T) {
	host, command, _, candidate, canonical := hostFixture(t)
	if err := (Controller{Host: host}).Apply(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(canonical)
	if err != nil || !strings.Contains(string(body), candidate.ReleaseDir) || strings.Contains(string(body), "__RELEASE_DIR__") {
		t.Fatalf("canonical plist=%q err=%v", body, err)
	}
	current, err := host.Current(context.Background())
	if err != nil || current != candidate {
		t.Fatalf("installed=%+v err=%v", current, err)
	}
	currentLink, err := os.Readlink(filepath.Join(host.rootDir, "current"))
	if err != nil || currentLink != candidate.ReleaseDir {
		t.Fatalf("current link=%q want=%q err=%v", currentLink, candidate.ReleaseDir, err)
	}
	if _, err := os.Stat(filepath.Join(host.stateDir, UpdateJournalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived commit: %v", err)
	}
	updater, err := os.ReadFile(filepath.Join(host.launchAgentsDir, UpdaterPlist))
	if err != nil || !strings.Contains(string(updater), candidate.ReleaseDir+"/fleet") || !strings.Contains(string(updater), "<integer>300</integer>") ||
		!strings.Contains(string(updater), "<key>RunAtLoad</key><true/>") {
		t.Fatalf("updater plist=%q err=%v", updater, err)
	}
	if !strings.Contains(string(updater), "<string>--config</string>") || !strings.Contains(string(updater), "<string>"+candidate.ConfigPath+"</string>") {
		t.Fatalf("updater did not preserve config path: %s", updater)
	}
	for _, want := range []string{
		"<key>EnvironmentVariables</key>",
		"<key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
	} {
		if !strings.Contains(string(updater), want) {
			t.Fatalf("updater plist does not provide launchd dependency PATH %q: %s", want, updater)
		}
	}
	joined := strings.Join(command.calls, "\n")
	for _, want := range []string{"fleet config validate --mode authority", "launchctl bootout", "launchctl bootstrap", "status --require-ready"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in calls:\n%s", want, joined)
		}
	}
}

func TestLocalHostConfigOnlyRolloutRendersCandidateConfigIntoDaemonArguments(t *testing.T) {
	host, _, current, _, _ := hostFixture(t)
	candidate := current
	candidate.ConfigPath = filepath.Join(host.stateDir, "profiles", "maestro-4x4.json")
	if err := os.MkdirAll(filepath.Dir(candidate.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate.ConfigPath, []byte(`{"valid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	journal, err := host.readJournal()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(journal.PreparedPlist)
	if err != nil {
		t.Fatal(err)
	}
	want := "--config=" + candidate.ConfigPath
	if !strings.Contains(string(prepared), want) {
		t.Fatalf("prepared daemon arguments do not contain candidate config %q: %s", want, prepared)
	}
	if strings.Contains(string(prepared), "--config="+filepath.Join(host.stateDir, "fleet.json")) {
		t.Fatalf("prepared daemon arguments retained default config: %s", prepared)
	}
}

func TestLocalHostHandsALoadedUpdaterReloadToAnIndependentLaunchdJob(t *testing.T) {
	host, command, _, candidate, _ := hostFixture(t)

	if err := host.Commit(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}

	handoffPath := filepath.Join(host.launchAgentsDir, "com.vitalyiegorov.tart-runner-fleet.updater-handoff.plist")
	want := []string{
		"launchctl print gui/501/com.vitalyiegorov.tart-runner-fleet.updater",
		"launchctl bootstrap gui/501 " + handoffPath,
	}
	joined := strings.Join(command.calls, "\n")
	for _, call := range want {
		if !strings.Contains(joined, call) {
			t.Fatalf("loaded updater reload was not delegated; missing %q in calls:\n%s", call, joined)
		}
	}
	for _, forbidden := range []string{
		"launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater",
		"launchctl bootstrap gui/501 " + filepath.Join(host.launchAgentsDir, UpdaterPlist),
	} {
		for _, call := range command.calls {
			if call == forbidden {
				t.Fatalf("updater attempted to terminate or replace itself via %q:\n%s", forbidden, joined)
			}
		}
	}
	handoff, err := os.ReadFile(handoffPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{candidate.ReleaseDir + "/fleet", "finish-updater-handoff", "automatic-updater-handoff"} {
		if !strings.Contains(string(handoff), value) {
			t.Fatalf("handoff plist missing %q: %s", value, handoff)
		}
	}
}

func TestLocalHostUpdaterHandoffWaitsForTheDurableCommit(t *testing.T) {
	host, command, current, candidate, _ := hostFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}

	if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrBusy) {
		t.Fatalf("handoff raced an open transaction: %v", err)
	}
	if strings.Contains(strings.Join(command.calls, "\n"), "launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater") {
		t.Fatalf("handoff touched the updater before commit: %v", command.calls)
	}
}

func TestLocalHostUpdaterHandoffReloadsAndVerifiesTheExactGeneration(t *testing.T) {
	host, command, _, candidate, _ := hostFixture(t)
	installed, _ := json.Marshal(candidate)
	if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	updaterPath := filepath.Join(host.launchAgentsDir, UpdaterPlist)
	if err := os.WriteFile(updaterPath, host.renderUpdater(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	command.launchPrint = "program = " + candidate.ReleaseDir + "/fleet"

	if err := host.FinishUpdaterHandoff(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(command.calls, "\n")
	for _, want := range []string{
		"launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater",
		"launchctl bootstrap gui/501 " + updaterPath,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("handoff missing %q:\n%s", want, joined)
		}
	}
	if count := strings.Count(joined, "launchctl print gui/501/com.vitalyiegorov.tart-runner-fleet.updater"); count != 2 {
		t.Fatalf("handoff did not verify the replacement, print count=%d:\n%s", count, joined)
	}
}

func TestLocalHostUpdaterHandoffRejectsGenerationOrPlistDrift(t *testing.T) {
	for _, test := range []struct {
		name          string
		writeCurrent  bool
		writeExpected bool
	}{
		{name: "installed generation", writeExpected: true},
		{name: "updater plist", writeCurrent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, command, _, candidate, _ := hostFixture(t)
			if test.writeCurrent {
				body, _ := json.Marshal(candidate)
				if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.writeExpected {
				if err := os.WriteFile(filepath.Join(host.launchAgentsDir, UpdaterPlist), host.renderUpdater(candidate), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
				t.Fatalf("drift accepted: %v", err)
			}
			if strings.Contains(strings.Join(command.calls, "\n"), "launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater") {
				t.Fatal("drifted handoff touched updater")
			}
		})
	}
}

func TestLocalHostUpdaterHandoffRecoversAnUnloadedUpdater(t *testing.T) {
	host, command, _, candidate, _ := committedHandoffFixture(t)
	command.printFailures = 1
	command.launchPrint = "program = " + candidate.ReleaseDir + "/fleet"

	if err := host.FinishUpdaterHandoff(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(command.calls, "\n"), "launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater") {
		t.Fatal("unloaded updater was booted out")
	}
}

func TestLocalHostUpdaterHandoffFailsClosedAtLaunchdBoundaries(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*fakeCommand)
	}{
		{name: "bootout", configure: func(command *fakeCommand) {
			command.launchPrint = "program = /old/fleet"
			command.fail = map[string]error{"launchctl bootout gui/501/com.vitalyiegorov.tart-runner-fleet.updater": errors.New("denied")}
		}},
		{name: "bootstrap", configure: func(command *fakeCommand) {
			command.launchPrint = "program = /old/fleet"
			command.bootstrap = errors.New("denied")
		}},
		{name: "verification", configure: func(command *fakeCommand) {
			command.launchPrint = "program = /stale/fleet"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, command, _, candidate, _ := committedHandoffFixture(t)
			test.configure(command)
			if err := host.FinishUpdaterHandoff(context.Background(), candidate); err == nil {
				t.Fatal("launchd handoff failure accepted")
			}
		})
	}
}

func TestLocalHostUpdaterHandoffRejectsInvalidCandidateAndJournalState(t *testing.T) {
	t.Run("candidate", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		candidate.ReleaseDir = filepath.Join(host.rootDir, "elsewhere", candidate.Version)
		if err := host.FinishUpdaterHandoff(context.Background(), candidate); !errors.Is(err, ErrInvalidGeneration) {
			t.Fatalf("invalid candidate=%v", err)
		}
	})
	t.Run("journal stat", func(t *testing.T) {
		host, _, _, candidate, _ := hostFixture(t)
		journal := filepath.Join(host.stateDir, UpdateJournalFile)
		if err := os.Symlink(journal, journal); err != nil {
			t.Fatal(err)
		}
		if err := host.FinishUpdaterHandoff(context.Background(), candidate); err == nil {
			t.Fatal("unreadable journal state accepted")
		}
	})
}

func committedHandoffFixture(t *testing.T) (*LocalHost, *fakeCommand, Generation, Generation, string) {
	t.Helper()
	host, command, current, candidate, canonical := hostFixture(t)
	installed, _ := json.Marshal(candidate)
	if err := os.WriteFile(filepath.Join(host.stateDir, InstalledGenerationFile), installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.launchAgentsDir, UpdaterPlist), host.renderUpdater(candidate), 0o600); err != nil {
		t.Fatal(err)
	}
	return host, command, current, candidate, canonical
}

func TestLocalHostRestoresTheOldBootGenerationOnFailure(t *testing.T) {
	host, command, current, candidate, canonical := hostFixture(t)
	command.readyErr = errors.New("offline")
	err := (Controller{Host: host}).Apply(context.Background(), candidate)
	if err == nil {
		t.Fatal("readiness failure accepted")
	}
	body, readErr := os.ReadFile(canonical)
	if readErr != nil || string(body) != "old-v1-plist" {
		t.Fatalf("rollback plist=%q err=%v", body, readErr)
	}
	installed, readErr := host.Current(context.Background())
	if readErr != nil || installed != current {
		t.Fatalf("installed=%+v err=%v", installed, readErr)
	}
	currentLink, readErr := os.Readlink(filepath.Join(host.rootDir, "current"))
	if readErr != nil || currentLink != current.ReleaseDir {
		t.Fatalf("rollback current link=%q want=%q err=%v", currentLink, current.ReleaseDir, readErr)
	}
}

func TestLocalHostRetriesTransientLaunchdBootstrapDuringRollback(t *testing.T) {
	host, command, current, candidate, _ := hostFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	command.bootstrapFailures = 1
	if err := host.Rollback(context.Background(), current); err != nil {
		t.Fatalf("transient launchd rollback failure was not recovered: %v", err)
	}
	requireBootstrapAttempts(t, command, 2)
}

func TestLocalHostRetriesTransientLaunchdBootstrapDuringActivation(t *testing.T) {
	host, command, current, candidate, _ := hostFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	command.bootstrapFailures = 1
	if err := host.Activate(context.Background(), candidate); err != nil {
		t.Fatalf("transient launchd activation failure was not recovered: %v", err)
	}
	requireBootstrapAttempts(t, command, 2)
}

func TestLocalHostToleratesProlongedLaunchdBootstrapTransition(t *testing.T) {
	host, command, current, candidate, _ := hostFixture(t)
	host.readyAttempts = 6
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	command.bootstrapFailures = 5
	if err := host.Rollback(context.Background(), current); err != nil {
		t.Fatalf("prolonged launchd rollback transition was not recovered: %v", err)
	}
	requireBootstrapAttempts(t, command, 6)
}

func requireBootstrapAttempts(t *testing.T, command *fakeCommand, want int) {
	t.Helper()
	var bootstraps int
	for _, call := range command.calls {
		if strings.Contains(call, "launchctl bootstrap") {
			bootstraps++
		}
	}
	if bootstraps != want {
		t.Fatalf("launchd bootstrap attempts=%d want=%d\ncalls:\n%s", bootstraps, want, strings.Join(command.calls, "\n"))
	}
}

func TestLocalHostStopsBootstrapRecoveryWhenContextIsCanceled(t *testing.T) {
	host, command, current, candidate, _ := hostFixture(t)
	if err := host.Prepare(context.Background(), current, candidate); err != nil {
		t.Fatal(err)
	}
	command.bootstrapFailures = minimumLaunchdBootstrapAttempts
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := host.Activate(ctx, candidate); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled launchd recovery error=%v", err)
	}
}

func TestLocalHostRejectsTamperedOrMismatchedReleaseBeforeLaunchd(t *testing.T) {
	host, command, _, candidate, _ := hostFixture(t)
	if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, "fleet"), []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (Controller{Host: host}).Apply(context.Background(), candidate); !errors.Is(err, ErrChecksum) {
		t.Fatalf("tampered release error=%v", err)
	}
	for _, call := range command.calls {
		if strings.Contains(call, "launchctl") {
			t.Fatalf("tampered release touched launchd: %s", call)
		}
	}
}

func TestLocalHostDefersUpdatesUntilAllCapacityIsIdle(t *testing.T) {
	host, command, _, candidate, canonical := hostFixture(t)
	command.current = `{"data":{"controllerVersion":"v1","controllerMode":"authority","queues":[{"jobs":1}],"instances":[{"count":2}],"operations":{"retrying":0,"dead":0}}}`
	err := (Controller{Host: host}).Apply(context.Background(), candidate)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("busy fleet error=%v", err)
	}
	body, readErr := os.ReadFile(canonical)
	if readErr != nil || string(body) != "old-v1-plist" {
		t.Fatalf("busy update changed boot plist=%q err=%v", body, readErr)
	}
	for _, call := range command.calls {
		if strings.Contains(call, "launchctl") {
			t.Fatalf("busy update touched launchd: %s", call)
		}
	}
}
