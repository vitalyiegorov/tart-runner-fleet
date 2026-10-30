package hostlauncher

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

const (
	testName      = "trf-host-sim-2x4-1"
	testOperation = "op-1"
)

// harness is one launcher over a temporary host: a runner template, an
// instance root, and a root-owned configuration whose owner is whoever runs the
// suite. Supervisors run in goroutines and process-group signals are recorded
// rather than sent, so no test can signal its own process group.
type harness struct {
	t        *testing.T
	root     string
	template string
	config   Config
	kills    []string
	mu       sync.Mutex
	spawned  chan error
	held     []*os.File
}

func newHarness(t *testing.T, runnerScript string) *harness {
	t.Helper()
	base := t.TempDir()
	template := filepath.Join(base, "template")
	if err := os.MkdirAll(template, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(template, "run.sh"), []byte(runnerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "instances")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(base, "capabilities.json")
	if err := os.WriteFile(manifest, []byte(`{"schemaVersion":1,"image":"host","sealedAt":"now","capabilities":["xcode","maestro-cli"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: root, template: template, spawned: make(chan error, 8)}
	h.config = Config{RunnerUser: "ci-runner", RunnerUID: os.Getuid(), RunnerHome: base,
		InstanceRoot: root, RunnerTemplate: template, CloneCommand: []string{"/bin/cp", "-R"},
		CapabilityManifest: manifest}
	t.Cleanup(func() {
		for _, file := range h.held {
			_ = file.Close()
		}
	})
	return h
}

func (h *harness) launcher(stdin string) (*Launcher, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	l := &Launcher{Config: h.config, Stdin: strings.NewReader(stdin), Stdout: stdout, Stderr: stderr,
		Getuid: os.Getuid, Getenv: func(string) string { return "" },
		Kill: func(pid int, signal syscall.Signal) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.kills = append(h.kills, signal.String())
			return nil
		},
		PollInterval: time.Millisecond, StartTimeout: 2 * time.Second, KillWait: 50 * time.Millisecond}
	l.Spawn = func(args []string) error {
		go func() {
			supervisor := *l
			supervisor.Stdin = strings.NewReader("")
			supervisor.Stdout, supervisor.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
			if code := supervisor.Main(args); code != 0 {
				h.spawned <- errors.New("supervisor exited " + string(rune('0'+code)))
				return
			}
			h.spawned <- nil
		}()
		return nil
	}
	return l, stdout, stderr
}

func (h *harness) run(stdin string, args ...string) (int, string, string) {
	h.t.Helper()
	l, stdout, stderr := h.launcher(stdin)
	return l.Main(args), stdout.String(), stderr.String()
}

func (h *harness) mustRun(stdin string, args ...string) string {
	h.t.Helper()
	code, stdout, stderr := h.run(stdin, args...)
	if code != ExitOK {
		h.t.Fatalf("%v exited %d: %s", args, code, stderr)
	}
	return stdout
}

func (h *harness) create() {
	h.t.Helper()
	h.mustRun("", "create", testName, h.template, "ctrl", "res", testOperation)
}

func (h *harness) list() []Entry {
	h.t.Helper()
	var entries []Entry
	if err := json.Unmarshal([]byte(h.mustRun("", "list")), &entries); err != nil {
		h.t.Fatalf("list is not JSON: %v", err)
	}
	return entries
}

// hold takes the instance's alive lock the way a supervisor and its runner do,
// so a test can stage a running instance without a process.
func (h *harness) hold(name string) {
	h.t.Helper()
	control := filepath.Join(h.root, name, "control")
	file, err := os.OpenFile(filepath.Join(control, "alive.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(control, "pgid"), []byte("4242\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.held = append(h.held, file)
}

func TestCreateClonesTheTemplateAndListsTheInstanceStopped(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	if _, err := os.Stat(filepath.Join(h.root, testName, "runner", "run.sh")); err != nil {
		t.Fatalf("the runner template was not cloned: %v", err)
	}
	info, err := os.Stat(filepath.Join(h.root, testName))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("instance directory mode = %v (err=%v), want 0700", info.Mode().Perm(), err)
	}
	entries := h.list()
	want := Entry{Name: testName, Image: h.template, Operation: testOperation, Power: PowerStopped}
	if len(entries) != 1 || entries[0] != want {
		t.Fatalf("list = %#v, want %#v", entries, want)
	}
	// A repeated create by the same operation is the lost-response retry.
	h.create()
	if code, _, _ := h.run("", "create", testName, h.template, "ctrl", "res", "op-2"); code != ExitConflict {
		t.Fatalf("another operation's create exited %d, want conflict", code)
	}
}

func TestCreateRefusesWhatTheRootConfigurationDoesNotName(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	for name, args := range map[string][]string{
		"foreign template": {"create", testName, "/tmp", "c", "r", "o"},
		"bad name":         {"create", "../escape", h.template, "c", "r", "o"},
		"bad ownership":    {"create", testName, h.template, "c", "r", "has space"},
		"short vector":     {"create", testName, h.template},
	} {
		if code, _, _ := h.run("", args...); code == ExitOK {
			t.Errorf("%s: create succeeded", name)
		}
	}
	if entries := h.list(); len(entries) != 0 {
		t.Fatalf("a refused create left %#v", entries)
	}
}

func TestCreateClearsAStagingDirectoryACrashLeftBehind(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(h.root, ".staging-"+testName, "runner", "junk"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.create()
	if _, err := os.Stat(filepath.Join(h.root, testName, "runner", "junk")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staging residue was adopted: %v", err)
	}
}

func TestCreateFailsWhenTheCloneFails(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.config.CloneCommand = []string{"/bin/false"}
	if code, _, _ := h.run("", "create", testName, h.template, "c", "r", "o"); code != ExitFailure {
		t.Fatalf("create with a failing clone exited %d", code)
	}
	if _, err := os.Stat(filepath.Join(h.root, testName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed clone published an instance: %v", err)
	}
}

// TestAWholeJobRunsThroughTheLauncher is the lifecycle the daemon drives:
// create, start (a supervisor holds the instance), the readiness probe, the JIT
// bootstrap over the FIFO, the runner's exit, and the delete.
func TestAWholeJobRunsThroughTheLauncher(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nprintf '%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" > \"$TMPDIR/seen\"\nprintf '%s' \"$HOME\" > \"$TMPDIR/home\"\n")
	h.create()
	if code, _, _ := h.run("", "exec", testName, "true"); code == ExitOK {
		t.Fatal("a created but unstarted instance answered the readiness probe")
	}
	h.mustRun("", "start", testName, testOperation)
	h.mustRun("", "exec", testName, "true")
	if got := h.list()[0].Power; got != PowerRunning {
		t.Fatalf("power after start = %q", got)
	}
	// Starting a running instance is idempotent.
	h.mustRun("", "start", testName, testOperation)
	h.mustRun("jit-secret\n", "exec", "-i", testName, guestbootstrap.HelperPath,
		guestbootstrap.CapabilityFlag+"=xcode")
	if err := <-h.spawned; err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(h.root, testName, "tmp")
	if seen, _ := os.ReadFile(filepath.Join(tmp, "seen")); string(seen) != "jit-secret" {
		t.Fatalf("runner saw JIT %q", seen)
	}
	if home, _ := os.ReadFile(filepath.Join(tmp, "home")); string(home) != h.config.RunnerHome {
		t.Fatalf("runner HOME = %q", home)
	}
	if _, err := os.Stat(filepath.Join(h.root, testName, "control", "jit.fifo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the JIT FIFO outlived its read: %v", err)
	}
	if got := h.list()[0].Power; got != PowerStopped {
		t.Fatalf("power after the runner exited = %q", got)
	}
	// ADR 0010: an ephemeral instance never runs a second job.
	if code, _, _ := h.run("", "start", testName, testOperation); code != ExitConflict {
		t.Fatalf("restarting a finished instance exited %d, want conflict", code)
	}
	h.mustRun("", "stop", testName, testOperation, "5")
	h.mustRun("", "delete", testName, testOperation)
	if entries := h.list(); len(entries) != 0 {
		t.Fatalf("delete left %#v", entries)
	}
	h.mustRun("", "delete", testName, testOperation)
}

func TestBootstrapRefusesAMissingCapabilityBeforeReadingTheSecret(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	h.hold(testName)
	stdin := &countingReader{}
	l, _, _ := h.launcher("")
	l.Stdin = stdin
	code := l.Main([]string{"exec", "-i", testName, guestbootstrap.HelperPath, guestbootstrap.CapabilityFlag + "=android"})
	if code != guestbootstrap.ExitCapabilityMissing || stdin.reads != 0 {
		t.Fatalf("exit=%d reads=%d, want a capability refusal before stdin", code, stdin.reads)
	}
	h.config.CapabilityManifest = filepath.Join(h.root, "absent.json")
	code, _, _ = h.run("", "exec", "-i", testName, guestbootstrap.HelperPath, guestbootstrap.CapabilityFlag+"=xcode")
	if code != guestbootstrap.ExitCapabilityUnverifiable {
		t.Fatalf("exit=%d, want unverifiable", code)
	}
}

type countingReader struct{ reads int }

func (r *countingReader) Read([]byte) (int, error) { r.reads++; return 0, errors.New("read") }

func TestBootstrapRefusesWhatIsNotAWaitingSupervisor(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	helper := guestbootstrap.HelperPath
	if code, _, _ := h.run("jit\n", "exec", "-i", testName, helper); code != ExitFailure {
		t.Fatalf("bootstrap of a stopped instance exited %d", code)
	}
	h.hold(testName)
	if code, _, _ := h.run("jit\n", "exec", "-i", testName, helper); code != ExitFailure {
		t.Fatalf("bootstrap with no FIFO exited %d", code)
	}
	// A regular file planted where the FIFO belongs must never receive the JIT.
	planted := filepath.Join(h.root, testName, "control", "jit.fifo")
	if err := os.WriteFile(planted, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run("jit\n", "exec", "-i", testName, helper); code != ExitFailure {
		t.Fatalf("bootstrap into a regular file exited %d", code)
	}
	if body, _ := os.ReadFile(planted); len(body) != 0 {
		t.Fatalf("the JIT reached a planted regular file: %q", body)
	}
	if code, _, _ := h.run("", "exec", "-i", testName, helper); code != ExitFailure {
		t.Fatal("an empty JIT was accepted")
	}
	for _, args := range [][]string{
		{"exec", "-i", testName, "/bin/sh"},
		{"exec", "-i", testName, helper, "--container"},
		{"exec", "-i", testName, helper, guestbootstrap.CapabilityFlag + "=BAD"},
		{"exec", testName, "id"},
	} {
		if code, _, _ := h.run("jit\n", args...); code != ExitUsage {
			t.Errorf("%v exited %d, want usage", args, code)
		}
	}
}

func TestStopSignalsTheProcessGroupAndEscalates(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	h.hold(testName)
	if code, _, _ := h.run("", "stop", testName, testOperation, "0"); code != ExitRunning {
		t.Fatalf("stop of a group that ignores SIGKILL exited %d, want running", code)
	}
	if strings.Join(h.kills, ",") != syscall.SIGKILL.String() {
		t.Fatalf("terminate sent %v, want SIGKILL alone", h.kills)
	}
	h.kills = nil
	if code, _, _ := h.run("", "stop", testName, testOperation, "1"); code != ExitRunning {
		t.Fatalf("stop exited %d", code)
	}
	if strings.Join(h.kills, ",") != syscall.SIGTERM.String()+","+syscall.SIGKILL.String() {
		t.Fatalf("stop sent %v, want SIGTERM then SIGKILL", h.kills)
	}
	if code, _, _ := h.run("", "delete", testName, testOperation); code != ExitRunning {
		t.Fatalf("delete of a running instance exited %d, want running", code)
	}
	h.held[0].Close()
	h.held = nil
	h.mustRun("", "stop", testName, testOperation, "1")
	h.mustRun("", "delete", testName, testOperation)
}

func TestStopRefusesAProcessGroupItCannotName(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	h.hold(testName)
	if err := os.WriteFile(filepath.Join(h.root, testName, "control", "pgid"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run("", "stop", testName, testOperation, "1"); code != ExitFailure {
		t.Fatalf("stop with pgid 1 exited %d", code)
	}
	if len(h.kills) != 0 {
		t.Fatalf("a signal was sent to an unnamed group: %v", h.kills)
	}
}

func TestMutatingVerbsRequireTheOwningOperation(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	for _, args := range [][]string{{"start", testName, testOperation}} {
		if code, _, _ := h.run("", args...); code != ExitNotFound {
			t.Errorf("%v on an absent instance exited %d", args, code)
		}
	}
	h.mustRun("", "stop", testName, testOperation, "1")
	h.create()
	for _, args := range [][]string{{"start", testName, "op-x"}, {"stop", testName, "op-x", "1"}, {"delete", testName, "op-x"}} {
		if code, _, _ := h.run("", args...); code != ExitConflict {
			t.Errorf("%v exited %d, want conflict", args, code)
		}
	}
	if code, _, _ := h.run("", "stop", testName, testOperation, "x"); code != ExitUsage {
		t.Errorf("a non-numeric grace exited %d", code)
	}
}

func TestPowerThatCannotBeReadIsUnknown(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	lock := filepath.Join(h.root, testName, "control", "alive.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := h.list()[0].Power; got != PowerUnknown {
		t.Fatalf("power over an unreadable lock = %q, want unknown", got)
	}
	for _, args := range [][]string{{"stop", testName, testOperation, "1"}, {"delete", testName, testOperation},
		{"start", testName, testOperation}} {
		if code, _, _ := h.run("", args...); code != ExitFailure {
			t.Errorf("%v with unknown power exited %d, want failure", args, code)
		}
	}
}

func TestListSkipsWhatIsNotAnInstanceAndReportsAnUnreadableMarker(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	if err := os.Mkdir(filepath.Join(h.root, ".staging-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.root, "trf-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.template, filepath.Join(h.root, "trf-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(h.root, "trf-unmarked"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries := h.list()
	if len(entries) != 2 || entries[0].Name != testName || entries[1].Name != "trf-unmarked" ||
		entries[1].Image != "" || entries[1].Power != PowerStopped {
		t.Fatalf("list = %#v", entries)
	}
	if code, _, _ := h.run("", "delete", "trf-unmarked", testOperation); code != ExitConflict {
		t.Fatalf("deleting an unmarked directory exited %d, want conflict", code)
	}
}

func TestDeleteRemovesWhatTheJobMadeUnwritable(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	locked := filepath.Join(h.root, testName, "runner", "_work", "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	h.mustRun("", "delete", testName, testOperation)
	if _, err := os.Stat(filepath.Join(h.root, testName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete left the instance: %v", err)
	}
}

func TestTheLauncherRefusesToRunAsAnyoneButTheRunnerUser(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	for name, mutate := range map[string]func(*Launcher){
		"another uid": func(l *Launcher) { l.Getuid = func() int { return os.Getuid() + 1 } },
		"root":        func(l *Launcher) { l.Getuid = func() int { return 0 }; l.Config.RunnerUID = 0 },
		"invoked by the runner user itself through sudo": func(l *Launcher) {
			l.Getenv = func(key string) string {
				if key == "SUDO_UID" {
					return strconv.Itoa(os.Getuid())
				}
				return ""
			}
		},
	} {
		l, _, _ := h.launcher("")
		mutate(l)
		if code := l.Main([]string{"list"}); code != ExitFailure {
			t.Errorf("%s: exit %d, want refusal", name, code)
		}
	}
	for _, args := range [][]string{nil, {"reboot"}, {"list", "extra"}, {"supervise"}} {
		if code, _, _ := h.run("", args...); code != ExitUsage {
			t.Errorf("%v exited %d, want usage", args, code)
		}
	}
}

func TestSupervisorRefusesASecondBoot(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	if err := os.WriteFile(filepath.Join(h.root, testName, "control", "booted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run("", "supervise", testName); code != ExitConflict {
		t.Fatalf("a second supervisor exited %d, want conflict", code)
	}
	h.hold(testName)
	l, _, _ := h.launcher("")
	l.StartTimeout = 5 * time.Millisecond
	if code := l.Main([]string{"supervise", testName}); code != ExitFailure {
		t.Fatalf("a supervisor that cannot take the lock exited %d", code)
	}
}

func TestStartFailsWhenTheSupervisorNeverComesUp(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	l, _, _ := h.launcher("")
	l.Spawn = func([]string) error { return nil }
	l.StartTimeout = 5 * time.Millisecond
	if code := l.Main([]string{"start", testName, testOperation}); code != ExitFailure {
		t.Fatalf("start with a silent supervisor exited %d", code)
	}
	l.Spawn = func([]string) error { return errors.New("fork") }
	if code := l.Main([]string{"start", testName, testOperation}); code != ExitFailure {
		t.Fatalf("start with a failed spawn exited %d", code)
	}
}

func TestLoadConfigRequiresARootOwnedFileTheRunnerCannotRewrite(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	path := filepath.Join(t.TempDir(), "host-launcher.json")
	body, _ := json.Marshal(h.config)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	owner := uint32(os.Getuid())
	if cfg, err := loadConfig(path, owner); err != nil || cfg.InstanceRoot != h.config.InstanceRoot {
		t.Fatalf("loadConfig() = %#v, %v", cfg, err)
	}
	if _, err := loadConfig(path, owner+1); err == nil {
		t.Fatal("a file not owned by root was accepted")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, owner); err == nil {
		t.Fatal("a world-writable file was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"relative root":     func(c *Config) { c.InstanceRoot = "instances" },
		"no clone command":  func(c *Config) { c.CloneCommand = nil },
		"relative clone":    func(c *Config) { c.CloneCommand = []string{"cp"} },
		"root runner":       func(c *Config) { c.RunnerUID = 0 },
		"no runner user":    func(c *Config) { c.RunnerUser = "" },
		"relative template": func(c *Config) { c.RunnerTemplate = "t" },
		"relative home":     func(c *Config) { c.RunnerHome = "h" },
		"no manifest":       func(c *Config) { c.CapabilityManifest = "" },
	} {
		cfg := h.config
		mutate(&cfg)
		body, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path, owner); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path, owner); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent"), owner); err == nil {
		t.Fatal("an absent file was accepted")
	}
}

func TestRunnerEnvironmentIsBuiltNotInherited(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "leak")
	env := strings.Join(h.config.runnerEnvironment("jit", "/i/tmp"), "\n")
	if strings.Contains(env, "leak") {
		t.Fatal("the launcher's own environment reached the runner")
	}
	for _, want := range []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=jit", "TMPDIR=/i/tmp", "USER=ci-runner",
		"HOME=" + h.config.RunnerHome, "PATH=" + defaultRunnerPath} {
		if !strings.Contains(env, want) {
			t.Errorf("runner environment lacks %q", want)
		}
	}
}

func TestProductionWiringIsComplete(t *testing.T) {
	l := Production()
	if l.Getuid == nil || l.Getenv == nil || l.Kill == nil || l.Spawn == nil || l.Stdin == nil {
		t.Fatalf("production launcher is missing a dependency: %#v", l)
	}
	if code := l.Main([]string{"list"}); code != ExitFailure {
		t.Fatalf("a launcher with no root configuration exited %d", code)
	}
}

func TestSpawnDetachedStartsItselfInANewSession(t *testing.T) {
	t.Cleanup(func() { executable = os.Executable })
	executable = func() (string, error) { return "/bin/true", nil }
	if err := spawnDetached([]string{"supervise", testName}); err != nil {
		t.Fatalf("spawnDetached() = %v", err)
	}
	executable = func() (string, error) { return "/nonexistent/launcher", nil }
	if err := spawnDetached(nil); err == nil {
		t.Fatal("spawning a missing binary succeeded")
	}
	executable = func() (string, error) { return "", errors.New("no self") }
	if err := spawnDetached(nil); err == nil {
		t.Fatal("spawning with no executable path succeeded")
	}
}

func TestCreateThatLosesThePublishLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	t.Cleanup(func() { rename = os.Rename })
	rename = func(string, string) error { return errors.New("raced") }
	if code, _, _ := h.run("", "create", testName, h.template, "c", "r", "o"); code != ExitFailure {
		t.Fatalf("a lost publish exited %d", code)
	}
	if residue, _ := os.ReadDir(h.root); len(residue) != 0 {
		t.Fatalf("a lost publish left %v", residue)
	}
}

func TestCreateFailsOnARootItCannotWrite(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(h.root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.root, 0o700) })
	if code, _, _ := h.run("", "create", testName, h.template, "c", "r", "o"); code != ExitFailure {
		t.Fatalf("create on a read-only root exited %d", code)
	}
}

func TestListFailsWhenTheRootIsUnreadable(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.config.InstanceRoot = filepath.Join(h.root, "absent")
	if code, stdout, _ := h.run("", "list"); code != ExitFailure || stdout != "" {
		t.Fatalf("list of an absent root = %d %q, want a failure and no rows", code, stdout)
	}
}

func TestAnInstanceThatIsNotADirectoryCannotBeActedOn(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(h.root, testName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run("", "delete", testName, testOperation); code != ExitFailure {
		t.Fatalf("delete of a file exited %d", code)
	}
}

func TestASignalFailureIsReportedButDoesNotDecide(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	h.hold(testName)
	l, _, stderr := h.launcher("")
	l.Kill = func(int, syscall.Signal) error { return syscall.EPERM }
	if code := l.Main([]string{"stop", testName, testOperation, "0"}); code != ExitRunning {
		t.Fatalf("stop exited %d", code)
	}
	if !strings.Contains(stderr.String(), "signal") {
		t.Fatalf("a refused signal was not reported: %q", stderr.String())
	}
}

// startSupervised creates and starts an instance whose supervisor runs in a
// goroutine, and returns once it waits on its FIFO.
func (h *harness) startSupervised() {
	h.t.Helper()
	h.create()
	h.mustRun("", "start", testName, testOperation)
}

func TestASupervisorGivenNoConfigurationNeverStartsTheRunner(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\ntouch \"$TMPDIR/ran\"\n")
	h.startSupervised()
	fifo, err := os.OpenFile(filepath.Join(h.root, testName, "control", "jit.fifo"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = fifo.Close()
	if err := <-h.spawned; err == nil {
		t.Fatal("a supervisor with an empty configuration succeeded")
	}
	if _, err := os.Stat(filepath.Join(h.root, testName, "tmp", "ran")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the runner ran without a configuration")
	}
}

func TestBootstrapFailsWhenTheRunnerCannotStart(t *testing.T) {
	for name, sabotage := range map[string]func(dir string) error{
		"no runner": func(dir string) error { return os.Remove(filepath.Join(dir, "runner", "run.sh")) },
		"a planted log": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "control", "runner.log"), nil, 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "#!/bin/sh\nexit 0\n")
			h.startSupervised()
			if err := sabotage(filepath.Join(h.root, testName)); err != nil {
				t.Fatal(err)
			}
			l, _, _ := h.launcher("jit\n")
			l.StartTimeout = 200 * time.Millisecond
			if code := l.Main([]string{"exec", "-i", testName, guestbootstrap.HelperPath}); code != ExitFailure {
				t.Fatalf("bootstrap exited %d", code)
			}
			if err := <-h.spawned; err == nil {
				t.Fatal("the supervisor reported success")
			}
		})
	}
}

func TestASupervisorRefusesAPlantedFIFOPath(t *testing.T) {
	h := newHarness(t, "#!/bin/sh\nexit 0\n")
	h.create()
	if err := os.WriteFile(filepath.Join(h.root, testName, "control", "jit.fifo"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := h.run("", "supervise", testName); code != ExitFailure {
		t.Fatalf("supervise over a planted file exited %d", code)
	}
	if code, _, _ := h.run("", "supervise", "trf-absent"); code != ExitFailure {
		t.Fatalf("supervise of an absent instance exited %d", code)
	}
}

func TestStopReturnsAsSoonAsTheGroupHasExited(t *testing.T) {
	for _, grace := range []string{"5", "0"} {
		h := newHarness(t, "#!/bin/sh\nexit 0\n")
		h.create()
		h.hold(testName)
		l, _, _ := h.launcher("")
		var sent []string
		l.Kill = func(_ int, signal syscall.Signal) error {
			sent = append(sent, signal.String())
			return h.held[0].Close()
		}
		if code := l.Main([]string{"stop", testName, testOperation, grace}); code != ExitOK {
			t.Fatalf("grace %s: stop exited %d", grace, code)
		}
		if len(sent) != 1 {
			t.Fatalf("grace %s: sent %v, want one signal", grace, sent)
		}
		h.held = nil
	}
}
