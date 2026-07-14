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
	calls      []string
	ready      string
	current    string
	readyErr   error
	currentErr error
	bootstrap  error
	fail       map[string]error
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
		if strings.Contains(call, "/v1/fleetctl") && c.current != "" {
			return []byte(c.current), c.currentErr
		}
		return []byte(c.ready), c.readyErr
	}
	if strings.Contains(call, "launchctl bootstrap") && c.bootstrap != nil {
		return nil, c.bootstrap
	}
	return nil, nil
}

func makeRelease(t *testing.T, root, version string) string {
	t.Helper()
	dir := filepath.Join(root, "releases", version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	template := `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.vitalyiegorov.tart-runner-fleet.authority</string><key>ProgramArguments</key><array><string>__RELEASE_DIR__/fleetd</string><string>--mode=authority</string><string>--config=__STATE_DIR__/fleet.json</string></array></dict></plist>`
	files := map[string][]byte{
		"RELEASE_VERSION": []byte(version + "\n"), "fleetd": []byte("daemon-" + version),
		"fleetctl": []byte("ctl-" + version), "com.vitalyiegorov.tart-runner-fleet.authority.plist": []byte(template),
	}
	var sums strings.Builder
	for _, name := range []string{"RELEASE_VERSION", "fleetd", "fleetctl", "com.vitalyiegorov.tart-runner-fleet.authority.plist"} {
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
	v1 := Generation{Version: "v1", Mode: "authority", ReleaseDir: makeRelease(t, root, "v1"), ConfigPath: configPath, Endpoint: "unix:///state/fleetd.sock"}
	v2 := Generation{Version: "v2", Mode: "authority", ReleaseDir: makeRelease(t, root, "v2"), ConfigPath: configPath, Endpoint: "unix:///state/fleetd.sock"}
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
	if _, err := os.Stat(filepath.Join(host.stateDir, UpdateJournalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal survived commit: %v", err)
	}
	updater, err := os.ReadFile(filepath.Join(host.launchAgentsDir, UpdaterPlist))
	if err != nil || !strings.Contains(string(updater), candidate.ReleaseDir+"/fleetctl") || !strings.Contains(string(updater), "<integer>300</integer>") {
		t.Fatalf("updater plist=%q err=%v", updater, err)
	}
	joined := strings.Join(command.calls, "\n")
	for _, want := range []string{"fleetctl config validate", "launchctl bootout", "launchctl bootstrap", "status --require-ready"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in calls:\n%s", want, joined)
		}
	}
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
}

func TestLocalHostRejectsTamperedOrMismatchedReleaseBeforeLaunchd(t *testing.T) {
	host, command, _, candidate, _ := hostFixture(t)
	if err := os.WriteFile(filepath.Join(candidate.ReleaseDir, "fleetd"), []byte("tampered"), 0o700); err != nil {
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
