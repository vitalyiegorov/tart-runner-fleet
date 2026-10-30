package contract_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/hostprocess"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/hostlauncher"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// inProcessLauncher stands where `sudo -n -u <runnerUser> -- <launcher>` stands
// in production: it runs the real launcher's verbs against a temporary
// instance root. A supervisor runs in a goroutine instead of a new session, and
// a process-group signal is delivered by closing that supervisor's FIFO, which
// ends it exactly as SIGTERM would end a supervisor still waiting for its JIT.
type inProcessLauncher struct {
	t      *testing.T
	config hostlauncher.Config
}

type launcherExit int

func (e launcherExit) Error() string { return "launcher exit status" }
func (e launcherExit) ExitCode() int { return int(e) }

func (l *inProcessLauncher) launcher(stdout *bytes.Buffer) *hostlauncher.Launcher {
	launcher := &hostlauncher.Launcher{Config: l.config, Stdin: strings.NewReader(""), Stdout: stdout,
		Stderr: &bytes.Buffer{}, Getuid: os.Getuid, Getenv: func(string) string { return "" },
		Kill: l.kill, PollInterval: time.Millisecond, StartTimeout: 2 * time.Second, KillWait: 2 * time.Second}
	launcher.Spawn = func(args []string) error {
		go launcher.Main(args)
		return nil
	}
	return launcher
}

func (l *inProcessLauncher) Run(_ context.Context, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if code := l.launcher(&stdout).Main(args); code != hostlauncher.ExitOK {
		return nil, launcherExit(code)
	}
	return stdout.Bytes(), nil
}

func (l *inProcessLauncher) kill(int, syscall.Signal) error {
	fifos, _ := filepath.Glob(filepath.Join(l.config.InstanceRoot, "*", "control", "jit.fifo"))
	for _, fifo := range fifos {
		if file, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = file.Close()
		}
	}
	return nil
}

func hostProcessBackend(t *testing.T, registry *portOwnership) conformingBackend {
	t.Helper()
	base := t.TempDir()
	template := filepath.Join(base, "template")
	root := filepath.Join(base, "instances")
	for _, dir := range []string{template, root} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(template, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	launcher := &inProcessLauncher{t: t, config: hostlauncher.Config{RunnerUser: "ci-runner", RunnerUID: os.Getuid(),
		RunnerHome: base, InstanceRoot: root, RunnerTemplate: template, CloneCommand: []string{"/bin/cp", "-R"},
		CapabilityManifest: filepath.Join(base, "capabilities.json")}}
	return conformingBackend{name: "hostprocess", image: template, backend: &hostprocess.Adapter{
		Runner: launcher, Ownership: registry, Confirmation: portControl{}}}
}

// TestAHostProcessInstanceIsBootstrappedThroughTheLauncher closes the one gap
// the shared harness leaves: it bootstraps with a stub. Here the lifecycle's own
// StdinBootstrapper vector reaches the real launcher's `exec -i` verb, and the
// runner the launcher starts sees the configuration in its environment.
func TestAHostProcessInstanceIsBootstrappedThroughTheLauncher(t *testing.T) {
	registry := &portOwnership{records: map[string]operations.Ownership{}}
	subject := hostProcessBackend(t, registry)
	adapter := subject.backend.(*hostprocess.Adapter)
	launcher := adapter.Runner.(*inProcessLauncher)
	script := "#!/bin/sh\nprintf '%s' \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" > \"$TMPDIR/seen\"\n"
	if err := os.WriteFile(filepath.Join(subject.image, "run.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	ownership := operations.Ownership{ControllerID: "contract", ResourceID: "r", OperationID: "op-1"}
	ctx := context.Background()
	if err := adapter.Create(ctx, executorSpec(subject.image, ownership)); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := adapter.Start(ctx, "trf-small-1", ownership); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	var stdout bytes.Buffer
	bootstrap := launcher.launcher(&stdout)
	bootstrap.Stdin = strings.NewReader("jit-from-github\n")
	code := bootstrap.Main([]string{"exec", "-i", "trf-small-1", "/usr/local/libexec/tart-runner-fleet-bootstrap"})
	if code != hostlauncher.ExitOK {
		t.Fatalf("bootstrap exited %d", code)
	}
	seen := filepath.Join(launcher.config.InstanceRoot, "trf-small-1", "tmp", "seen")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if body, _ := os.ReadFile(seen); string(body) == "jit-from-github" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the runner never saw its configuration")
		}
		time.Sleep(time.Millisecond)
	}
}

func executorSpec(image string, ownership operations.Ownership) executor.InstanceSpec {
	return executor.InstanceSpec{Name: "trf-small-1", Image: image, CPU: 1, MemoryMB: 2048, Ownership: ownership}
}
