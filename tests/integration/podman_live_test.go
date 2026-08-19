package integration

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/podman"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/sqlite"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
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
func liveAdapter(t *testing.T) *podman.Adapter {
	t.Helper()
	if os.Getenv(liveEnvironment) == "" {
		t.Skipf("%s is unset; run scripts/podman-smoke.sh to exercise a real rootless podman", liveEnvironment)
	}
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
