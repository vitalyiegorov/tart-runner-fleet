package contract_test

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the executor port's contract (issue #137). It exists so the
// container backend of issue #139 has a target that cannot move underneath it:
// the verb set, the caller-side ports one backend must satisfy, the name grammar
// both backends share, and the architectural rule that no layer above the port
// may name a backend.
//
// It is deliberately typed against nothing Tart-specific except the one
// assertion that today's only backend implements the port.

// tartAdapterIsABackend is the whole point of the extraction: the production
// adapter satisfies the neutral port, so a second implementation is a wiring
// change in internal/daemon and nothing else.
var _ executor.Backend = (*tart.Adapter)(nil)

// One backend serves every caller. These assignments fail to compile the moment
// a caller's port asks for something executor.Backend does not promise, which is
// what makes the port the single thing #139 has to implement.
var (
	_ lifecycle.VMControl    = executor.Backend(nil)
	_ app.ExecutorInventory  = executor.Backend(nil)
	_ discharge.VM           = executor.Backend(nil)
	_ executor.CommandRunner = tart.ExecRunner{}
	_ executor.Backend       = (*memoryBackend)(nil)
)

// TestExecutorPortSurfaceIsExactlyTheAgreedVerbs pins the port's shape by
// reflection. A new verb is not a detail: it is a demand on every future node,
// so it must arrive with a decision record rather than with a commit that widens
// an interface. A removed or re-signed verb breaks the adapter #139 writes
// against this list.
func TestExecutorPortSurfaceIsExactlyTheAgreedVerbs(t *testing.T) {
	backend := reflect.TypeOf((*executor.Backend)(nil)).Elem()
	want := map[string]string{
		"Create":  "func(context.Context, executor.InstanceSpec) error",
		"Start":   "func(context.Context, string, operations.Ownership) error",
		"Stop":    "func(context.Context, string, operations.Ownership) error",
		"Delete":  "func(context.Context, string, operations.Ownership) error",
		"Running": "func(context.Context, string) (bool, error)",
		"Reap":    "func(context.Context, string, operations.Ownership) error",
		"List":    "func(context.Context) ([]executor.Instance, error)",
	}
	got := map[string]string{}
	for index := range backend.NumMethod() {
		method := backend.Method(index)
		got[method.Name] = "func" + strings.TrimPrefix(method.Type.String(), "func")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("executor.Backend surface drifted:\ngot  %v\nwant %v", sortedPairs(got), sortedPairs(want))
	}
}

// TestInstanceSpecCarriesNoBackendSpecificField pins the request shape. Tart's
// nested-virtualization flag, its root-disk options, and its shared-directory
// path are all real, and all of them stay inside the Tart adapter's own
// configuration: a field here is a field every backend must answer for.
func TestInstanceSpecCarriesNoBackendSpecificField(t *testing.T) {
	spec := reflect.TypeOf(executor.InstanceSpec{})
	want := []string{"CPU", "DiskGB", "Image", "MemoryMB", "Name", "Ownership"}
	got := make([]string, 0, spec.NumField())
	for index := range spec.NumField() {
		got = append(got, spec.Field(index).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("executor.InstanceSpec fields=%v, want %v", got, want)
	}
	instance := reflect.TypeOf(executor.Instance{})
	wantInstance := []string{"Name", "Running", "Source"}
	gotInstance := make([]string, 0, instance.NumField())
	for index := range instance.NumField() {
		gotInstance = append(gotInstance, instance.Field(index).Name)
	}
	sort.Strings(gotInstance)
	if !reflect.DeepEqual(gotInstance, wantInstance) {
		t.Fatalf("executor.Instance fields=%v, want %v", gotInstance, wantInstance)
	}
}

// ociContainerName is Podman's container-name grammar. docs/MULTI_NODE_PLAN.md
// carries the fleet's instance grammar onto a container node unchanged, and this
// is the assertion that claim rests on: every name the fleet will ever generate
// is a name the container backend can accept, so #139 needs no second identity
// scheme and no translation table.
var ociContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestInstanceNameGrammarIsLegalOnEveryBackend(t *testing.T) {
	valid := []string{"a", "trf-small-1", "trf-macos-builder-096ffcb3a52d8624", "A.b_c-9",
		strings.Repeat("z", 128)}
	for _, name := range valid {
		if err := domain.ValidateInstanceName(name); err != nil {
			t.Fatalf("domain rejected %q: %v", name, err)
		}
		if !ociContainerName.MatchString(name) {
			t.Fatalf("%q is a fleet instance name that no container backend can accept", name)
		}
	}
	for _, name := range []string{"", ".", "..", "-leading", "_leading", "has space", "has/slash",
		"has:colon", strings.Repeat("z", 129)} {
		if err := domain.ValidateInstanceName(name); err == nil {
			t.Fatalf("domain accepted %q", name)
		}
	}
}

// TestNoLayerAboveThePortNamesABackend is the architectural half of the
// contract. The port is worth nothing if a caller can still reach around it, and
// the leak this issue closed was exactly that: internal/lifecycle took a
// tart.Request and internal/app read a tart.VM. Only internal/daemon, which
// exists to choose implementations, may name a backend adapter.
func TestNoLayerAboveThePortNamesABackend(t *testing.T) {
	root := repositoryRoot(t)
	for _, pkg := range []string{"internal/lifecycle", "internal/app", "internal/scheduler",
		"internal/reconcile", "internal/operations", "internal/telemetry", "internal/discharge",
		"internal/domain", "internal/executor", "internal/cli", "internal/adminapi"} {
		for _, file := range packageImports(t, filepath.Join(root, pkg)) {
			for _, path := range file.imports {
				if strings.Contains(path, "internal/adapters/tart") {
					t.Errorf("%s imports the Tart adapter; the executor port exists so it does not have to", file.name)
				}
			}
		}
	}
}

type parsedFile struct {
	name    string
	imports []string
}

// packageImports reads every non-test Go file's import paths. Test files are
// excluded on purpose: a package's own tests may construct the production
// adapter, and the rule under test is about production dependencies.
func packageImports(t *testing.T, dir string) []parsedFile {
	t.Helper()
	set := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []parsedFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, parseErr := parser.ParseFile(set, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}
		parsed := parsedFile{name: path}
		for _, spec := range file.Imports {
			parsed.imports = append(parsed.imports, strings.Trim(spec.Path.Value, `"`))
		}
		files = append(files, parsed)
	}
	if len(files) == 0 {
		t.Fatalf("no production Go files under %s", dir)
	}
	return files
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func sortedPairs(m map[string]string) []string {
	pairs := make([]string, 0, len(m))
	for key, value := range m {
		pairs = append(pairs, key+" "+value)
	}
	sort.Strings(pairs)
	return pairs
}

// ---------------------------------------------------------------------------
// Sufficiency: a backend of this shape and nothing else runs a whole runner.
// ---------------------------------------------------------------------------

// memoryBackend is the smallest possible executor.Backend: no VM, no container,
// no process. It is what issue #139's Podman adapter has to look like from the
// lifecycle's point of view, and driving the real ProvisionExecutor and
// DrainExecutor through it proves the port carries every fact those state
// machines need.
type memoryBackend struct {
	instances map[string]executor.Instance
	owners    map[string]operations.Ownership
	calls     []string
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{instances: map[string]executor.Instance{}, owners: map[string]operations.Ownership{}}
}

func (b *memoryBackend) Create(_ context.Context, spec executor.InstanceSpec) error {
	if domain.ValidateInstanceName(spec.Name) != nil || domain.ValidateInstanceName(spec.Image) != nil ||
		!spec.Ownership.Valid() || spec.CPU <= 0 || spec.MemoryMB <= 0 {
		return operations.ErrInvalid
	}
	b.calls = append(b.calls, "create:"+spec.Image+":"+spec.Name)
	b.owners[spec.Name] = spec.Ownership
	b.instances[spec.Name] = executor.Instance{Name: spec.Name, Source: spec.Image}
	return nil
}

func (b *memoryBackend) Start(_ context.Context, name string, ownership operations.Ownership) error {
	instance, err := b.owned(name, ownership)
	if err != nil {
		return err
	}
	b.calls = append(b.calls, "start:"+name)
	instance.Running = true
	b.instances[name] = instance
	return nil
}

func (b *memoryBackend) Stop(_ context.Context, name string, ownership operations.Ownership) error {
	instance, err := b.owned(name, ownership)
	if err != nil {
		if errors.Is(err, operations.ErrNotFound) {
			return nil
		}
		return err
	}
	b.calls = append(b.calls, "stop:"+name)
	instance.Running = false
	b.instances[name] = instance
	return nil
}

func (b *memoryBackend) Delete(_ context.Context, name string, ownership operations.Ownership) error {
	if _, err := b.owned(name, ownership); err != nil {
		if errors.Is(err, operations.ErrNotFound) {
			return nil
		}
		return err
	}
	b.calls = append(b.calls, "delete:"+name)
	delete(b.instances, name)
	return nil
}

func (b *memoryBackend) Reap(_ context.Context, name string, ownership operations.Ownership) error {
	instance, err := b.owned(name, ownership)
	if err != nil {
		if errors.Is(err, operations.ErrNotFound) {
			return nil
		}
		return err
	}
	if instance.Running {
		return operations.ErrConflict
	}
	b.calls = append(b.calls, "reap:"+name)
	delete(b.instances, name)
	return nil
}

func (b *memoryBackend) Running(_ context.Context, name string) (bool, error) {
	return b.instances[name].Running, nil
}

func (b *memoryBackend) List(context.Context) ([]executor.Instance, error) {
	names := make([]string, 0, len(b.instances))
	for name := range b.instances {
		names = append(names, name)
	}
	sort.Strings(names)
	listed := make([]executor.Instance, 0, len(names))
	for _, name := range names {
		listed = append(listed, b.instances[name])
	}
	return listed, nil
}

func (b *memoryBackend) owned(name string, ownership operations.Ownership) (executor.Instance, error) {
	if domain.ValidateInstanceName(name) != nil {
		return executor.Instance{}, operations.ErrInvalid
	}
	instance, exists := b.instances[name]
	if !exists {
		return executor.Instance{}, operations.ErrNotFound
	}
	if b.owners[name] != ownership {
		return executor.Instance{}, operations.ErrConflict
	}
	return instance, nil
}

// TestABackendOfThisShapeRunsAWholeRunner drives the production lifecycle state
// machines against memoryBackend. Nothing Tart-specific participates, so a
// backend that satisfies the port is sufficient to provision and to drain.
func TestABackendOfThisShapeRunsAWholeRunner(t *testing.T) {
	backend := newMemoryBackend()
	ownership := operations.Ownership{ControllerID: "contract", ResourceID: "trf-small-1", OperationID: "op-1"}
	instance := operations.Instance{ID: "trf-small-1", Repo: "a/repo", Platform: domain.PlatformLinux,
		Profile: "small", Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
		Demand:    domain.DemandKey{Repo: "a/repo", RunID: 1, Attempt: 1, JobID: 7},
		Ownership: ownership, State: operations.StatePlanned}
	state := &portState{instance: instance}
	control := &portControl{}

	provision := lifecycle.ProvisionExecutor{State: state, VM: backend, Ready: portReady{},
		Registration: &portRegistration{}, Bootstrap: portBootstrap{},
		Bases:   map[domain.Platform]string{domain.PlatformLinux: "linux-base"},
		DiskGiB: map[domain.ProfileID]int{"small": 50}}
	if err := provision.Execute(context.Background(), operations.Operation{
		Kind: lifecycle.OperationProvision, ResourceID: instance.ID}); err != nil {
		t.Fatalf("provision through the port: %v", err)
	}
	if state.instance.State != operations.StateAssigned {
		t.Fatalf("state=%s, want %s", state.instance.State, operations.StateAssigned)
	}
	if running, _ := backend.Running(context.Background(), instance.ID); !running {
		t.Fatal("the port provisioned an instance that is not running")
	}

	state.instance.State = operations.StateDraining
	state.instance.DrainPhase = operations.DrainPhaseStoppedRecovery
	drain := lifecycle.DrainExecutor{State: state, VM: backend, Control: control}
	// A stopped-recovery drain re-verifies power through the port before it
	// deregisters, so power the instance down exactly as the recovery premise
	// says it already is.
	if err := backend.Stop(context.Background(), instance.ID, ownership); err != nil {
		t.Fatalf("stop through the port: %v", err)
	}
	if err := drain.Execute(context.Background(), operations.Operation{
		Kind: lifecycle.OperationDrain, ResourceID: instance.ID}); err != nil {
		t.Fatalf("drain through the port: %v", err)
	}
	if state.instance.State != operations.StateDeleted {
		t.Fatalf("state=%s, want %s", state.instance.State, operations.StateDeleted)
	}
	listed, err := backend.List(context.Background())
	if err != nil || len(listed) != 0 {
		t.Fatalf("the port left %d instances behind (err=%v)", len(listed), err)
	}
	want := []string{"create:linux-base:trf-small-1", "start:trf-small-1", "stop:trf-small-1",
		"stop:trf-small-1", "delete:trf-small-1"}
	if !reflect.DeepEqual(backend.calls, want) {
		t.Fatalf("backend calls=%v, want %v", backend.calls, want)
	}
}

type portState struct{ instance operations.Instance }

func (s *portState) Instance(context.Context, string) (operations.Instance, error) {
	return s.instance, nil
}

func (s *portState) Advance(_ context.Context, change lifecycle.StateChange) (operations.Instance, error) {
	if !change.ExpectedState.CanTransitionTo(change.NextState) {
		return operations.Instance{}, operations.ErrConflict
	}
	s.instance.State = change.NextState
	s.instance.Version++
	return s.instance, nil
}

type portReady struct{}

func (portReady) Wait(context.Context, operations.Instance) error { return nil }

type portRegistration struct{ registered bool }

func (r *portRegistration) Registered(context.Context, string) (bool, error) {
	current := r.registered
	r.registered = true
	return current, nil
}
func (r *portRegistration) ResetRegistration(context.Context, string) error { return nil }
func (r *portRegistration) AcquireAndGenerateJIT(context.Context, int64, string, string) (*githubscaleset.JITSecret, error) {
	return githubscaleset.NewJITSecret("jit"), nil
}

type portBootstrap struct{}

func (portBootstrap) Bootstrap(context.Context, string, *githubscaleset.JITSecret) error { return nil }

type portControl struct{}

func (portControl) SafeToDeregister(context.Context, operations.Instance) (bool, error) {
	return true, nil
}
func (portControl) Deregister(context.Context, operations.Instance) error { return nil }
func (portControl) ConfirmDeletion(_ context.Context, _ string) (operations.DeletionConfirmation, error) {
	return operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true,
		ObservedAt: time.Now().UTC()}, nil
}
func (portControl) RunnerRegistered(context.Context, operations.Instance) (bool, error) {
	return false, nil
}
func (portControl) JobStarted(context.Context, operations.Instance) (bool, error) { return false, nil }
func (portControl) JobActive(context.Context, operations.Instance) (bool, error)  { return false, nil }
func (portControl) RunnerBusy(context.Context, operations.Instance) (bool, error) { return false, nil }
