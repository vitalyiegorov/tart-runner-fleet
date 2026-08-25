package integration

import (
	"context"
	"errors"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/podman"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the only test in the suite that starts a real container. It is
// opt-in, because rootless Podman is a property of node B and not of every
// machine that runs `make unit`; `scripts/podman-smoke.sh` is what turns it on,
// and the Geekom bring-up checklist in docs/MULTI_NODE_PLAN.md requires that
// script to PASS rather than SKIP before the node is promoted to authority.
//
// Everything the adapter's unit tests assert against a fake command runner is
// asserted here against the real one: the argument vectors podman actually
// accepts, the JSON podman actually prints, the states it actually reports, and
// the two verbs no fake can prove — that `podman exec -i` can carry a secret
// into the container on stdin, and that `--device /dev/kvm` is a grant the
// runtime honours.

const (
	liveEnvironment = "TRF_PODMAN_LIVE"
	liveImageEnv    = "TRF_PODMAN_IMAGE"
	liveKVMEnv      = "TRF_PODMAN_KVM"
	defaultLiveImg  = "docker.io/library/alpine:3.20"
	liveInstance    = "trf-livesmoke-0000000000000000"
	liveKVMInstance = "trf-livekvm-0000000000000000"
	// liveBootstrapInstance is the container the guest helper is exercised in. It
	// is created with `podman run` rather than through the adapter because it
	// needs two bind mounts — a guest home and the helper binary — and the
	// executor port has no volumes: an image supplies both on a real node.
	liveBootstrapInstance = "trf-livebootstrap-0000000000000000"
	// liveHelperPath is where every image must install the helper, and where
	// internal/lifecycle invokes it.
	liveHelperPath = "/usr/local/libexec/tart-runner-fleet-bootstrap"
)

func liveImage() string {
	if image := os.Getenv(liveImageEnv); image != "" {
		return image
	}
	return defaultLiveImg
}

// liveAdapter is the production adapter over the real podman command line, with
// a real SQLite ownership registry underneath it: the only thing this test
// substitutes is the deletion confirmation, which is GitHub's answer and not
// podman's.
// liveOnly is the opt-in gate. Every test in this file is skipped on a machine
// that has not asked for a real container runtime.
func liveOnly(t *testing.T) {
	t.Helper()
	if os.Getenv(liveEnvironment) == "" {
		t.Skipf("%s is unset; run scripts/podman-smoke.sh to exercise a real rootless podman", liveEnvironment)
	}
}

func liveAdapter(t *testing.T) *podman.Adapter {
	t.Helper()
	liveOnly(t)
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/fleet.db")
	if err != nil {
		t.Fatalf("open the durable ownership registry: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &podman.Adapter{Ownership: store, Confirmation: liveConfirmation{}, Image: liveImage(),
		// An explicit finite hold command rather than the adapter's `sleep
		// infinity` default: BusyBox's sleep does not accept `infinity` on every
		// image, and a smoke test that leaks a container forever is worse than one
		// that leaks it for ten minutes.
		HoldCommand:         []string{"sleep", "600"},
		KVMInstancePrefixes: []string{"trf-livekvm-"},
		CommandTimeout:      2 * time.Minute, StopTimeout: 5 * time.Second}
}

type liveConfirmation struct{}

func (liveConfirmation) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	return operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true,
		ObservedAt: time.Now().UTC()}, nil
}

func liveOwnership(instance string) operations.Ownership {
	return operations.Ownership{ControllerID: "podman-smoke", ResourceID: instance, OperationID: "op-live-1"}
}

// removeLeftovers is the guard that keeps a failed run from poisoning the next
// one. It bypasses the adapter on purpose: it is cleanup, not a verb under test.
func removeLeftovers(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		_ = exec.Command("podman", "rm", "--force", "--ignore", name).Run()
	}
}

// TestPodmanBackendAgainstARealRootlessPodman runs the whole lifecycle of one
// ephemeral runner container: health, create, start, readiness exec, a secret
// piped in over stdin, stop, and delete. It is the evidence that the fake-driven
// unit tests describe the real command line.
func TestPodmanBackendAgainstARealRootlessPodman(t *testing.T) {
	adapter := liveAdapter(t)
	ctx := context.Background()
	removeLeftovers(t, liveInstance)
	t.Cleanup(func() { removeLeftovers(t, liveInstance) })

	if err := adapter.Healthy(ctx); err != nil {
		t.Fatalf("this machine's podman is not a rootless runtime the fleet may use: %v", err)
	}

	ownership := liveOwnership(liveInstance)
	spec := executor.InstanceSpec{Name: liveInstance, Image: liveImage(), CPU: 1, MemoryMB: 512,
		DiskGB: 50, Ownership: ownership}
	if err := adapter.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power != domain.InstancePowerStopped {
		t.Fatalf("a created container reported power=%v (err=%v); Create must leave it stopped", power, err)
	}
	// Create is idempotent for the operation that made the container, and only
	// for that operation.
	if err := adapter.Create(ctx, spec); err != nil {
		t.Fatalf("Create is not idempotent for its own container: %v", err)
	}
	other := spec
	other.Ownership.OperationID = "op-live-2"
	if err := adapter.Create(ctx, other); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("a second operation adopted an existing container: %v", err)
	}

	if err := adapter.Start(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power != domain.InstancePowerRunning {
		t.Fatalf("Start returned but the container is not running (power=%v err=%v)", power, err)
	}
	listed, err := adapter.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, instance := range listed {
		if instance.Name == liveInstance {
			found = true
			if instance.Power != domain.InstancePowerRunning || instance.Source == "" {
				t.Errorf("List reported %#v", instance)
			}
		}
	}
	if !found {
		t.Fatalf("List did not report the container it just started: %#v", listed)
	}

	// The two primitives the lifecycle drives through this backend without
	// naming it: `podman exec <name> true` is the readiness probe, and
	// `podman exec -i <name> <helper>` is how the JIT configuration reaches the
	// runner without ever appearing in argv or in the environment.
	if _, err := (podman.ExecRunner{}).Run(ctx, "exec", liveInstance, "true"); err != nil {
		t.Fatalf("the readiness probe cannot reach a running container: %v", err)
	}
	secret := "jit-configuration-that-must-not-appear-in-argv"
	piped := exec.CommandContext(ctx, "podman", "exec", "-i", liveInstance, "cat")
	piped.Stdin = strings.NewReader(secret + "\n")
	echoed, err := piped.Output()
	if err != nil || strings.TrimSpace(string(echoed)) != secret {
		t.Fatalf("stdin bootstrap carried %q (err=%v), want the secret back", echoed, err)
	}

	if err := adapter.Stop(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power == domain.InstancePowerRunning {
		t.Fatalf("Stop returned but the container is still running (power=%v err=%v)", power, err)
	}
	if err := adapter.Delete(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power == domain.InstancePowerRunning {
		t.Fatalf("a deleted container is not running (power=%v err=%v)", power, err)
	}
	// Every verb whose durable cleanup depends on retry treats an absent
	// container as success.
	if err := adapter.Stop(ctx, liveInstance, ownership); err != nil {
		t.Errorf("Stop of an absent container: %v", err)
	}
	if err := adapter.Delete(ctx, liveInstance, ownership); err != nil {
		t.Errorf("Delete of an absent container: %v", err)
	}
	if err := adapter.Reap(ctx, liveInstance, ownership); err != nil {
		t.Errorf("Reap of an absent container: %v", err)
	}
}

// TestPodmanReapRefusesRunningWorkAndRemovesStoppedWork is the operator-authority
// verb against a real runtime. Reap must never stop a container, because a
// running one may be executing a real job.
func TestPodmanReapRefusesRunningWorkAndRemovesStoppedWork(t *testing.T) {
	adapter := liveAdapter(t)
	ctx := context.Background()
	removeLeftovers(t, liveInstance)
	t.Cleanup(func() { removeLeftovers(t, liveInstance) })

	ownership := liveOwnership(liveInstance)
	if err := adapter.Create(ctx, executor.InstanceSpec{Name: liveInstance, Image: liveImage(), CPU: 1,
		MemoryMB: 512, Ownership: ownership}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := adapter.Start(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := adapter.Reap(ctx, liveInstance, ownership); err == nil {
		t.Fatal("Reap removed a running container")
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power != domain.InstancePowerRunning {
		t.Fatalf("a refused Reap stopped the container anyway (power=%v err=%v)", power, err)
	}
	if err := adapter.Stop(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := adapter.Reap(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Reap of a stopped owned container: %v", err)
	}
	if power, err := adapter.Power(ctx, liveInstance); err != nil || power == domain.InstancePowerRunning {
		t.Fatalf("the container survived its Reap (power=%v err=%v)", power, err)
	}
}

// TestPodmanSizesSharedMemoryFromTheVector is the /dev/shm rule against a real
// runtime, and the assertion no fake can make: that podman accepts the flag and
// that the tmpfs inside the container is actually the size the vector implies.
// Podman's unsized default is 64 MB, where the Chromium renderers of issue #284
// die; a quarter of this spec's 2048 MiB is 512 MiB, which /proc/mounts reports
// in kibibytes.
func TestPodmanSizesSharedMemoryFromTheVector(t *testing.T) {
	adapter := liveAdapter(t)
	ctx := context.Background()
	removeLeftovers(t, liveInstance)
	t.Cleanup(func() { removeLeftovers(t, liveInstance) })

	ownership := liveOwnership(liveInstance)
	if err := adapter.Create(ctx, executor.InstanceSpec{Name: liveInstance, Image: liveImage(), CPU: 1,
		MemoryMB: 2048, DiskGB: 50, Ownership: ownership}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := adapter.Start(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Start: %v", err)
	}
	mounted, err := exec.CommandContext(ctx, "podman", "exec", liveInstance,
		"sh", "-c", "grep ' /dev/shm ' /proc/mounts").Output()
	if err != nil {
		t.Fatalf("read /dev/shm from inside the container: %v", err)
	}
	if want := "size=524288k"; !strings.Contains(string(mounted), want) {
		t.Fatalf("/dev/shm mounted as %q, want a tmpfs with %s — a 2048 MiB profile gets a 512 MiB /dev/shm",
			strings.TrimSpace(string(mounted)), want)
	}
}

// TestPodmanGrantsKvmOnlyToTheNamedProfile is the ADR 0034 device rule against a
// real runtime, and the one assertion no fake can make: that podman accepts the
// grant and that the device is actually inside the container. It runs only where
// /dev/kvm exists, which the Geekom has and a nested CI guest may not.
func TestPodmanGrantsKvmOnlyToTheNamedProfile(t *testing.T) {
	adapter := liveAdapter(t)
	if os.Getenv(liveKVMEnv) == "" {
		t.Skipf("%s is unset; this machine has no /dev/kvm to grant", liveKVMEnv)
	}
	ctx := context.Background()
	removeLeftovers(t, liveKVMInstance, liveInstance)
	t.Cleanup(func() { removeLeftovers(t, liveKVMInstance, liveInstance) })

	for _, testCase := range []struct {
		instance string
		wantKVM  bool
	}{
		{instance: liveKVMInstance, wantKVM: true},
		{instance: liveInstance, wantKVM: false},
	} {
		ownership := liveOwnership(testCase.instance)
		if err := adapter.Create(ctx, executor.InstanceSpec{Name: testCase.instance, Image: liveImage(),
			CPU: 1, MemoryMB: 512, Ownership: ownership}); err != nil {
			t.Fatalf("Create %s: %v", testCase.instance, err)
		}
		if err := adapter.Start(ctx, testCase.instance, ownership); err != nil {
			t.Fatalf("Start %s: %v", testCase.instance, err)
		}
		err := exec.CommandContext(ctx, "podman", "exec", testCase.instance, "test", "-c", "/dev/kvm").Run()
		if (err == nil) != testCase.wantKVM {
			t.Errorf("%s: /dev/kvm present = %v, want %v (err=%v)", testCase.instance, err == nil, testCase.wantKVM, err)
		}
	}
}

// TestPodmanReadinessProbeIsTheOneTheDaemonWires closes the loop between the
// live runtime and the neutral port: the daemon's readiness probe and JIT
// bootstrapper are typed on executor.CommandRunner and lifecycle.StdinRunner,
// and both must work against podman without a line of backend-specific code.
func TestPodmanReadinessProbeIsTheOneTheDaemonWires(t *testing.T) {
	adapter := liveAdapter(t)
	ctx := context.Background()
	removeLeftovers(t, liveInstance)
	t.Cleanup(func() { removeLeftovers(t, liveInstance) })

	ownership := liveOwnership(liveInstance)
	if err := adapter.Create(ctx, executor.InstanceSpec{Name: liveInstance, Image: liveImage(), CPU: 1,
		MemoryMB: 512, Ownership: ownership}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := adapter.Start(ctx, liveInstance, ownership); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var runner executor.CommandRunner = podman.ExecRunner{}
	if _, err := runner.Run(ctx, "exec", liveInstance, "true"); err != nil {
		t.Fatalf("the neutral command runner cannot exec into a container: %v", err)
	}
	var stdin lifecycle.StdinRunner = lifecycle.ExecStdinRunner{Binary: podman.DefaultBinary}
	if err := stdin.Run(ctx, strings.NewReader("payload\n"), "exec", "-i", liveInstance, "cat"); err != nil {
		t.Fatalf("the neutral stdin runner cannot bootstrap a container: %v", err)
	}
}

// TestPodmanBootstrapsARunnerInAContainerWithNoInitSystem is issue #273
// against the runtime that found it, and it closes this file's one remaining
// gap: every assertion above proves the daemon can reach into a container, and
// none of them proves the guest helper can start a runner once it is there —
// which is exactly why the geekom node smoke-tested clean and then failed on its
// first real job.
//
// The image is the plainest container there is: no systemd, no `sudo`, no
// `shutdown`. The two invocations differ by one flag and nothing else, so the
// flag is provably what makes the difference, and the runner really runs.
func TestPodmanBootstrapsARunnerInAContainerWithNoInitSystem(t *testing.T) {
	liveOnly(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	helper := buildBootstrapHelper(t)
	home := t.TempDir()
	work := filepath.Join(home, "actions-runner")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in for `run.sh`: an ephemeral listener that records that it ran
	// and exits, which is the whole of what the launcher is responsible for.
	if err := os.WriteFile(filepath.Join(work, "run.sh"),
		[]byte("#!/bin/sh\nprintf started > \"$(dirname \"$0\")/runner-started\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The guest helper resolves its work dir from os.UserHomeDir(), which reads
	// $HOME inside the container exactly as podman set it for the image's own
	// default user — /root for alpine, but /home/runner for the real runner
	// image (USER runner, ENV HOME). Mounting the guest home anywhere else
	// proves nothing about the image under test; it proves the test agrees
	// with itself (issue #278).
	guestHome, guestUID, guestGID := liveGuestIdentity(ctx, t, liveImage())
	guestStarted := guestHome + "/actions-runner/runner-started"
	// Rootless podman maps a non-root container user to a subordinate host UID
	// that owns nothing this test just created on the host side, so that user
	// cannot write into the bind-mounted home until it owns it. `podman
	// unshare` performs the chown inside the same user namespace the container
	// itself will run in, which is the one place host and container UIDs agree
	// on what "the image's own user" means. That reassignment also strips this
	// process's own access to the tree (alpine's root maps back to this host
	// user; the runner image's non-root user does not), so cleanup and every
	// later read of the guest's files go through podman, not the os package.
	if output, err := exec.CommandContext(ctx, "podman", "unshare", "chown", "--recursive",
		guestUID+":"+guestGID, home).CombinedOutput(); err != nil {
		t.Fatalf("give the image's own user ownership of the guest home before mounting it: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("podman", "unshare", "rm", "-rf", home).CombinedOutput(); err != nil {
			t.Errorf("clean up the guest home this test reassigned to the image's user: %v: %s", err, output)
		}
	})

	removeLeftovers(t, liveBootstrapInstance)
	t.Cleanup(func() { removeLeftovers(t, liveBootstrapInstance) })
	create := exec.CommandContext(ctx, "podman", "run", "--detach", "--name", liveBootstrapInstance,
		"--volume", home+":"+guestHome, "--volume", helper+":"+liveHelperPath+":ro",
		liveImage(), "sleep", "600")
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("start an idling container to bootstrap into: %v: %s", err, output)
	}

	// A guest nobody told is a virtual machine, and a virtual machine's launcher
	// cannot start here: there is no `sudo` and no `shutdown` to power off a
	// machine that does not exist. It must also leave the runner unstarted.
	if err := execBootstrap(ctx); err == nil {
		t.Error("a VM-mode bootstrap claimed success in a container with no init system")
	}
	if err := exec.CommandContext(ctx, "podman", "exec", liveBootstrapInstance, "test", "-e",
		guestStarted).Run(); err == nil {
		t.Error("a refused bootstrap started the runner anyway")
	}

	if err := execBootstrap(ctx, guestbootstrap.ContainerFlag); err != nil {
		t.Fatalf("container-mode bootstrap: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		output, err := exec.CommandContext(ctx, "podman", "exec", liveBootstrapInstance, "cat",
			guestStarted).Output()
		if err == nil && string(output) == "started" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatal("the container-mode supervisor never ran the runner")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// execBootstrap is the daemon's own bootstrap call, argument for argument, with
// a JIT configuration that is syntactically acceptable and useless: the runner
// stand-in never contacts GitHub, and a real one would fail long after the
// launch this test is about.
func execBootstrap(ctx context.Context, args ...string) error {
	return lifecycle.ExecStdinRunner{Binary: podman.DefaultBinary}.Run(ctx,
		strings.NewReader("not-a-real-jit-configuration\n"),
		append([]string{"exec", "-i", liveBootstrapInstance, liveHelperPath}, args...)...)
}

// liveGuestIdentity asks the image itself which user it runs as and where
// that user's home is, the same way the container this test is about to
// build from it will resolve them: podman derives a fresh container's $HOME
// and default user from the image's own USER and ENV directives, not from
// anything this test could assume. A throwaway container from the same image
// answers with podman's own resolution rather than this test re-implementing
// the USER/ENV/passwd merge. A probe that cannot answer (no shell, pull
// failure, unparseable output) defaults to the alpine-shaped identity, root
// at /root, which is the one every image in this file used before issue #278.
func liveGuestIdentity(ctx context.Context, t *testing.T, image string) (home, uid, gid string) {
	t.Helper()
	output, err := exec.CommandContext(ctx, "podman", "run", "--rm", "--entrypoint", "", image,
		"sh", "-c", "echo \"$HOME\" && id -u && id -g").Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		if len(lines) == 3 && filepath.IsAbs(lines[0]) && filepath.Clean(lines[0]) == lines[0] &&
			isDecimal(lines[1]) && isDecimal(lines[2]) {
			return lines[0], lines[1], lines[2]
		}
	}
	return "/root", "0", "0"
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// buildBootstrapHelper builds the guest helper from this working tree, so the
// binary under test is the one this commit produces rather than whatever an
// image happens to carry.
func buildBootstrapHelper(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "tart-runner-fleet-bootstrap")
	command := exec.Command(filepath.Join(build.Default.GOROOT, "bin", "go"), "build", "-trimpath",
		"-o", binary, "github.com/vitalyiegorov/tart-runner-fleet/cmd/tart-runner-fleet-bootstrap")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build the guest bootstrap helper: %v: %s", err, output)
	}
	return binary
}
