package hostprocess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/hostlauncher"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const (
	name     = "trf-host-sim-2x4-1"
	template = "/Users/ci-runner/actions-runner-template"
)

var ownership = operations.Ownership{ControllerID: "ctrl", ResourceID: "res", OperationID: "op-1"}

type exitError int

func (e exitError) Error() string { return "exit status" }
func (e exitError) ExitCode() int { return int(e) }

// fakeLauncher answers launcher verbs from a script keyed by verb, and records
// every vector. It is the process boundary and nothing else.
type fakeLauncher struct {
	calls   [][]string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeLauncher) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return []byte(f.outputs[args[0]]), f.errs[args[0]]
}

func (f *fakeLauncher) verbs() []string {
	verbs := make([]string, 0, len(f.calls))
	for _, call := range f.calls {
		verbs = append(verbs, call[0])
	}
	return verbs
}

type registry map[string]operations.Ownership

func (r registry) PutOwnership(_ context.Context, key string, value operations.Ownership) error {
	if current, ok := r[key]; ok && current != value {
		return operations.ErrConflict
	}
	r[key] = value
	return nil
}

func (r registry) Ownership(_ context.Context, key string) (operations.Ownership, error) {
	value, ok := r[key]
	if !ok {
		return operations.Ownership{}, operations.ErrNotFound
	}
	return value, nil
}

type confirmations struct {
	safe  bool
	err   error
	calls int
}

func (c *confirmations) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	c.calls++
	return operations.DeletionConfirmation{Fresh: c.safe, RunnerInactive: c.safe, JobsInactive: c.safe,
		ObservedAt: time.Now().UTC()}, c.err
}

func listing(power hostlauncher.Power) string {
	return `[{"name":"` + name + `","image":"` + template + `","operation":"op-1","power":"` + string(power) + `"}]`
}

func newAdapter(launcher *fakeLauncher) (*Adapter, *confirmations) {
	confirm := &confirmations{safe: true}
	return &Adapter{Runner: launcher, Ownership: registry{name: ownership}, Confirmation: confirm}, confirm
}

func TestListMapsLauncherPowerAndNeverReadsUnknownAsStopped(t *testing.T) {
	launcher := &fakeLauncher{outputs: map[string]string{"list": `[
		{"name":"a","image":"/t","power":"running"},
		{"name":"b","image":"/t","power":"stopped"},
		{"name":"c","image":"","power":"unknown"}]`}}
	adapter, _ := newAdapter(launcher)
	got, err := adapter.List(context.Background())
	want := []executor.Instance{
		{Name: "a", Power: domain.InstancePowerRunning, Source: "/t"},
		{Name: "b", Power: domain.InstancePowerStopped, Source: "/t"},
		{Name: "c", Power: domain.InstancePowerUnknown, Unreadable: domain.PowerReadFailure{Reason: domain.PowerReadOther}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, %v", got, err)
	}
	for _, power := range []domain.InstancePower{domain.InstancePowerUnknown} {
		if got, _ := adapter.Power(context.Background(), "c"); got != power {
			t.Fatalf("Power(c) = %q", got)
		}
	}
	if got, err := adapter.Power(context.Background(), "z"); err != nil || got != domain.InstancePowerAbsent {
		t.Fatalf("Power(absent) = %q, %v", got, err)
	}
}

func TestAnUnreadableListingIsUncertainNotEmpty(t *testing.T) {
	for name, launcher := range map[string]*fakeLauncher{
		"failed":  {errs: map[string]error{"list": exitError(1)}},
		"garbled": {outputs: map[string]string{"list": "not json"}},
	} {
		adapter, _ := newAdapter(launcher)
		if got, err := adapter.List(context.Background()); !errors.Is(err, operations.ErrUncertain) || got != nil {
			t.Errorf("%s: List() = %#v, %v", name, got, err)
		}
		if power, err := adapter.Power(context.Background(), "x"); err == nil || power != domain.InstancePowerUnknown {
			t.Errorf("%s: Power() = %q, %v", name, power, err)
		}
	}
}

func TestCreateRecordsOwnershipBeforeTheInstanceExists(t *testing.T) {
	launcher := &fakeLauncher{}
	adapter, _ := newAdapter(launcher)
	adapter.Ownership = registry{}
	spec := executor.InstanceSpec{Name: name, Image: template, CPU: 2, MemoryMB: 4096, Ownership: ownership}
	if err := adapter.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	want := [][]string{{"create", name, template, "ctrl", "res", "op-1"}}
	if !reflect.DeepEqual(launcher.calls, want) {
		t.Fatalf("calls = %v", launcher.calls)
	}
	if recorded, _ := adapter.Ownership.Ownership(context.Background(), name); recorded != ownership {
		t.Fatalf("ownership = %#v", recorded)
	}
	for label, bad := range map[string]executor.InstanceSpec{
		"bad name":       {Name: "../x", Image: template, CPU: 1, MemoryMB: 1, Ownership: ownership},
		"relative image": {Name: name, Image: "runner", CPU: 1, MemoryMB: 1, Ownership: ownership},
		"unclean image":  {Name: name, Image: "/a/../b", CPU: 1, MemoryMB: 1, Ownership: ownership},
		"no ownership":   {Name: name, Image: template, CPU: 1, MemoryMB: 1},
		"no vector":      {Name: name, Image: template, Ownership: ownership},
	} {
		if err := adapter.Create(context.Background(), bad); !errors.Is(err, operations.ErrInvalid) {
			t.Errorf("%s: Create() = %v", label, err)
		}
	}
	adapter.Ownership = registry{name: {ControllerID: "other", ResourceID: "r", OperationID: "o"}}
	if err := adapter.Create(context.Background(), spec); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("Create() over another owner = %v", err)
	}
}

func TestLauncherExitStatusesMapOntoTheFleetVocabulary(t *testing.T) {
	for code, want := range map[int]error{
		hostlauncher.ExitNotFound: operations.ErrNotFound,
		hostlauncher.ExitConflict: operations.ErrConflict,
		hostlauncher.ExitRunning:  operations.ErrConflict,
		hostlauncher.ExitUsage:    operations.ErrInvalid,
	} {
		launcher := &fakeLauncher{errs: map[string]error{"start": exitError(code)}}
		adapter, _ := newAdapter(launcher)
		err := adapter.Start(context.Background(), name, ownership)
		if !errors.Is(err, want) {
			t.Errorf("exit %d: Start() = %v, want %v", code, err, want)
		}
		var status interface{ ExitCode() int }
		if !errors.As(err, &status) || status.ExitCode() != code {
			t.Errorf("exit %d: the launcher status was lost: %v", code, err)
		}
	}
	plain := errors.New("sudo: a password is required")
	launcher := &fakeLauncher{errs: map[string]error{"start": plain}}
	adapter, _ := newAdapter(launcher)
	if err := adapter.Start(context.Background(), name, ownership); !errors.Is(err, plain) {
		t.Fatalf("Start() = %v", err)
	}
}

func TestMutationsRequireTheDurableOwner(t *testing.T) {
	launcher := &fakeLauncher{}
	adapter, _ := newAdapter(launcher)
	other := operations.Ownership{ControllerID: "ctrl", ResourceID: "res", OperationID: "op-2"}
	ctx := context.Background()
	for verb, call := range map[string]func() error{
		"start":     func() error { return adapter.Start(ctx, name, other) },
		"stop":      func() error { return adapter.Stop(ctx, name, other) },
		"terminate": func() error { return adapter.Terminate(ctx, name, other) },
		"destroy":   func() error { return adapter.Destroy(ctx, name, other) },
		"delete":    func() error { return adapter.Delete(ctx, name, other) },
		"reap":      func() error { return adapter.Reap(ctx, name, other) },
	} {
		if err := call(); !errors.Is(err, operations.ErrConflict) {
			t.Errorf("%s by another operation = %v", verb, err)
		}
	}
	if err := adapter.Start(ctx, "bad name", ownership); !errors.Is(err, operations.ErrInvalid) {
		t.Errorf("Start(bad name) = %v", err)
	}
	if len(launcher.calls) != 0 {
		t.Fatalf("the launcher was asked to act for a non-owner: %v", launcher.calls)
	}
}

func TestStopAndTerminateCarryTheGrace(t *testing.T) {
	launcher := &fakeLauncher{}
	adapter, _ := newAdapter(launcher)
	adapter.StopTimeout = 2500 * time.Millisecond
	ctx := context.Background()
	if err := adapter.Stop(ctx, name, ownership); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Terminate(ctx, name, ownership); err != nil {
		t.Fatal(err)
	}
	adapter.StopTimeout = 0
	if err := adapter.Stop(ctx, name, ownership); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"stop", name, "op-1", "2"}, {"stop", name, "op-1", "0"}, {"stop", name, "op-1", "10"}}
	if !reflect.DeepEqual(launcher.calls, want) {
		t.Fatalf("calls = %v", launcher.calls)
	}
}

func TestDeleteConfirmsBeforeAndAfterTheStop(t *testing.T) {
	launcher := &fakeLauncher{outputs: map[string]string{"list": listing(hostlauncher.PowerRunning)}}
	adapter, confirm := newAdapter(launcher)
	if err := adapter.Delete(context.Background(), name, ownership); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if got := launcher.verbs(); strings.Join(got, ",") != "list,stop,delete" || confirm.calls != 2 {
		t.Fatalf("verbs = %v confirmations = %d", got, confirm.calls)
	}
}

func TestDeleteRefusesWithoutFreshSafeConfirmation(t *testing.T) {
	for label, mutate := range map[string]func(*Adapter, *confirmations){
		"unsafe":      func(_ *Adapter, c *confirmations) { c.safe = false },
		"unreadable":  func(_ *Adapter, c *confirmations) { c.err = errors.New("github down") },
		"no provider": func(a *Adapter, _ *confirmations) { a.Confirmation = nil },
	} {
		launcher := &fakeLauncher{outputs: map[string]string{"list": listing(hostlauncher.PowerStopped)}}
		adapter, confirm := newAdapter(launcher)
		mutate(adapter, confirm)
		if err := adapter.Delete(context.Background(), name, ownership); err == nil {
			t.Errorf("%s: Delete() succeeded", label)
		}
		if strings.Contains(strings.Join(launcher.verbs(), ","), "delete") {
			t.Errorf("%s: the launcher deleted without confirmation", label)
		}
	}
}

func TestDeleteOfAnAbsentInstanceAsksNothingOfGitHub(t *testing.T) {
	launcher := &fakeLauncher{outputs: map[string]string{"list": "[]"}}
	adapter, confirm := newAdapter(launcher)
	if err := adapter.Delete(context.Background(), name, ownership); err != nil || confirm.calls != 0 {
		t.Fatalf("Delete(absent) = %v after %d confirmations", err, confirm.calls)
	}
	launcher.errs = map[string]error{"list": exitError(1)}
	if err := adapter.Delete(context.Background(), name, ownership); err == nil {
		t.Fatal("Delete with an unreadable listing succeeded")
	}
}

func TestDestroyTerminatesThenDeletes(t *testing.T) {
	launcher := &fakeLauncher{outputs: map[string]string{"list": listing(hostlauncher.PowerStopped)}}
	adapter, _ := newAdapter(launcher)
	if err := adapter.Destroy(context.Background(), name, ownership); err != nil {
		t.Fatalf("Destroy() = %v", err)
	}
	if got := strings.Join(launcher.verbs(), ","); got != "stop,list,stop,delete" {
		t.Fatalf("verbs = %s", got)
	}
}

func TestReapRemovesOnlyWhatIsProvenStopped(t *testing.T) {
	for power, wantDelete := range map[hostlauncher.Power]bool{
		hostlauncher.PowerStopped: true, hostlauncher.PowerRunning: false, hostlauncher.PowerUnknown: false,
	} {
		launcher := &fakeLauncher{outputs: map[string]string{"list": listing(power)}}
		adapter, confirm := newAdapter(launcher)
		err := adapter.Reap(context.Background(), name, ownership)
		deleted := strings.Contains(strings.Join(launcher.verbs(), ","), "delete")
		if deleted != wantDelete || (err == nil) != wantDelete || confirm.calls != 0 {
			t.Errorf("%s: Reap() = %v, deleted=%v, confirmations=%d", power, err, deleted, confirm.calls)
		}
	}
	launcher := &fakeLauncher{outputs: map[string]string{"list": "[]"}}
	adapter, _ := newAdapter(launcher)
	if err := adapter.Reap(context.Background(), name, ownership); err != nil {
		t.Fatalf("Reap(absent) = %v", err)
	}
}

func TestHealthyRefusesRootAndProvesTheSudoRule(t *testing.T) {
	t.Cleanup(func() { geteuid = os.Geteuid })
	launcher := &fakeLauncher{outputs: map[string]string{"list": "[]"}}
	adapter, _ := newAdapter(launcher)
	geteuid = func() int { return 501 }
	if err := adapter.Healthy(context.Background()); err != nil {
		t.Fatalf("Healthy() = %v", err)
	}
	geteuid = func() int { return 0 }
	if err := adapter.Healthy(context.Background()); err == nil {
		t.Fatal("a root daemon was healthy")
	}
}

// TestRunnerInvokesTheLauncherThroughSudoAsArguments pins the one vector the
// sudoers rule admits, and the JIT path: stdin in, nothing in argv.
func TestRunnerInvokesTheLauncherThroughSudoAsArguments(t *testing.T) {
	dir := t.TempDir()
	sudo := filepath.Join(dir, "sudo")
	record := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\ncat >> " + record + "\nprintf out\n"
	if err := os.WriteFile(sudo, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := Runner{User: "ci-runner", Launcher: "/usr/local/libexec/l", Sudo: sudo}
	output, err := runner.Run(context.Background(), "list")
	if err != nil || string(output) != "out" {
		t.Fatalf("Run() = %q, %v", output, err)
	}
	if body, _ := os.ReadFile(record); string(body) != "-n\n-u\nci-runner\n--\n/usr/local/libexec/l\nlist\n" {
		t.Fatalf("argv = %q", body)
	}
	if err := (StdinRunner{Runner: runner}).Run(context.Background(), strings.NewReader("jit-value\n"),
		"exec", "-i", name, "/helper"); err != nil {
		t.Fatalf("StdinRunner.Run() = %v", err)
	}
	if body, _ := os.ReadFile(record); string(body) != "-n\n-u\nci-runner\n--\n/usr/local/libexec/l\nexec\n-i\n"+name+"\n/helper\njit-value\n" {
		t.Fatalf("argv+stdin = %q", body)
	}
	if (Runner{User: "u", Launcher: "/l"}).command(context.Background(), nil).Path != SudoPath {
		t.Fatal("the production runner does not use the fixed sudo")
	}
}

func TestAStaleConfirmationIsNotEvidence(t *testing.T) {
	launcher := &fakeLauncher{outputs: map[string]string{"list": listing(hostlauncher.PowerStopped)}}
	adapter, _ := newAdapter(launcher)
	adapter.ConfirmationMaxAge = time.Minute
	adapter.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if err := adapter.Delete(context.Background(), name, ownership); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("Delete() on an hour-old confirmation = %v", err)
	}
}
