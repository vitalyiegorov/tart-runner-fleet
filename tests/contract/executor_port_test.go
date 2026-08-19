package contract_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/macos"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/podman"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/tart"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/discharge"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// This file is the executor port's contract (issues #137 and #139). It exists so
// that a backend has a target that cannot move underneath it: the verb set, the
// caller-side ports one backend must satisfy, the name grammar every backend
// shares, and the architectural rule that no layer above the port may name a
// backend.
//
// It is deliberately typed against nothing backend-specific except the
// assertions that the production adapters implement the port, and the
// conformance harness at the bottom, which drives the real lifecycle state
// machines through every one of them in turn.

// The production adapters satisfy the neutral port. This is the whole point of
// the extraction: adding a node type is a wiring change in internal/daemon and
// nothing else.
var (
	_ executor.Backend = (*tart.Adapter)(nil)
	_ executor.Backend = (*podman.Adapter)(nil)
)

// One backend serves every caller. These assignments fail to compile the moment
// a caller's port asks for something executor.Backend does not promise, which is
// what makes the port the single thing #139 has to implement.
var (
	_ lifecycle.VMControl    = executor.Backend(nil)
	_ app.ExecutorInventory  = executor.Backend(nil)
	_ discharge.VM           = executor.Backend(nil)
	_ executor.CommandRunner = tart.ExecRunner{}
	_ executor.CommandRunner = podman.ExecRunner{}
	_ executor.HostProbe     = (*macos.Probe)(nil)
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
		"Create":    "func(context.Context, executor.InstanceSpec) error",
		"Start":     "func(context.Context, string, operations.Ownership) error",
		"Stop":      "func(context.Context, string, operations.Ownership) error",
		"Terminate": "func(context.Context, string, operations.Ownership) error",
		"Destroy":   "func(context.Context, string, operations.Ownership) error",
		"Delete":    "func(context.Context, string, operations.Ownership) error",
		"Power":     "func(context.Context, string) (domain.InstancePower, error)",
		"Reap":      "func(context.Context, string, operations.Ownership) error",
		"List":      "func(context.Context) ([]executor.Instance, error)",
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
	// Power replaced Running in issue #252: a bool could not carry the answer a
	// backend gives when it could not read the machine, and a backend that
	// answers "off" for a reading it never took is the premise a destructive
	// recovery rests on. Unreadable carries WHY, for a trigger nobody has
	// reproduced yet.
	wantInstance := []string{"Name", "Power", "Source", "Unreadable"}
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
// tart.Request, internal/app read a tart.VM, and the whole scheduler's host
// facts were a macOS type. Only internal/daemon, which exists to choose
// implementations, may name a node's execution or host adapter.
func TestNoLayerAboveThePortNamesABackend(t *testing.T) {
	root := repositoryRoot(t)
	for _, pkg := range []string{"internal/lifecycle", "internal/app", "internal/scheduler",
		"internal/reconcile", "internal/operations", "internal/telemetry", "internal/discharge",
		"internal/domain", "internal/executor", "internal/cli", "internal/adminapi"} {
		for _, file := range packageImports(t, filepath.Join(root, pkg)) {
			for _, path := range file.imports {
				for _, backend := range []string{"internal/adapters/tart", "internal/adapters/macos",
					"internal/adapters/linux", "internal/adapters/podman", "internal/adapters/noexecutor"} {
					if strings.Contains(path, backend) {
						t.Errorf("%s imports %s; the executor port exists so it does not have to", file.name, path)
					}
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
// no process. It is the floor of the conformance harness below — a backend with
// nothing in it but the port's own semantics — so a harness assertion that only
// this implementation passes is an assertion about the port and not about a
// runtime.
type memoryBackend struct {
	instances map[string]executor.Instance
	owners    map[string]operations.Ownership
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{instances: map[string]executor.Instance{}, owners: map[string]operations.Ownership{}}
}

func (b *memoryBackend) Create(_ context.Context, spec executor.InstanceSpec) error {
	if domain.ValidateInstanceName(spec.Name) != nil || domain.ValidateInstanceName(spec.Image) != nil ||
		!spec.Ownership.Valid() || spec.CPU <= 0 || spec.MemoryMB <= 0 {
		return operations.ErrInvalid
	}
	b.owners[spec.Name] = spec.Ownership
	b.instances[spec.Name] = executor.Instance{Name: spec.Name, Source: spec.Image}
	return nil
}

func (b *memoryBackend) Start(_ context.Context, name string, ownership operations.Ownership) error {
	instance, err := b.owned(name, ownership)
	if err != nil {
		return err
	}
	instance.Power = domain.InstancePowerRunning
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
	instance.Power = domain.InstancePowerStopped
	b.instances[name] = instance
	return nil
}

// Terminate and Destroy are the stop ladder's forceful rungs (ADR 0039). In a
// backend with no guest there is nothing to be forceful about, so Terminate is
// Stop and Destroy is Delete — which is exactly the semantic difference the port
// promises: the same effect, reached without waiting for a guest to agree.
func (b *memoryBackend) Terminate(ctx context.Context, name string, ownership operations.Ownership) error {
	return b.Stop(ctx, name, ownership)
}

func (b *memoryBackend) Destroy(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := b.Terminate(ctx, name, ownership); err != nil {
		return err
	}
	return b.Delete(ctx, name, ownership)
}

func (b *memoryBackend) Delete(_ context.Context, name string, ownership operations.Ownership) error {
	if _, err := b.owned(name, ownership); err != nil {
		if errors.Is(err, operations.ErrNotFound) {
			return nil
		}
		return err
	}
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
	if instance.Power != domain.InstancePowerStopped {
		return operations.ErrConflict
	}
	delete(b.instances, name)
	return nil
}

func (b *memoryBackend) Power(_ context.Context, name string) (domain.InstancePower, error) {
	instance, ok := b.instances[name]
	if !ok {
		return domain.InstancePowerAbsent, nil
	}
	return instance.Power, nil
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

// conformingBackend is one implementation under the harness below, with the
// image its Create verb is given: a Tart base VM name on node A, an OCI
// reference on node B.
type conformingBackend struct {
	name    string
	image   string
	backend executor.Backend
}

// conformingBackends is every executor.Backend the fleet ships, plus the
// in-memory floor. Adding a node type means adding a line here, and a backend
// that cannot run the whole lifecycle fails this file rather than the node.
//
// The Podman adapter is driven over a scripted podman command line rather than a
// real one, because a container runtime is not installable on every machine that
// runs this suite. `scripts/podman-smoke.sh` is the same lifecycle against a
// real rootless podman, and the Geekom bring-up checklist requires it.
func conformingBackends(t *testing.T) []conformingBackend {
	t.Helper()
	registry := &portOwnership{records: map[string]operations.Ownership{}}
	return []conformingBackend{
		{name: "memory", image: "linux-base", backend: newMemoryBackend()},
		{name: "podman", image: "ghcr.io/example/trf-runner-amd64:1", backend: &podman.Adapter{
			Runner: newScriptedPodman(), Ownership: registry, Confirmation: portControl{},
			Image: "ghcr.io/example/trf-runner-amd64:1"}},
	}
}

// TestABackendOfThisShapeRunsAWholeRunner drives the production lifecycle state
// machines through every conforming backend. Nothing backend-specific
// participates, so a backend that satisfies the port is sufficient to provision
// and to drain — and the Podman adapter is proved sufficient by the same test
// that proved the port was, rather than by a copy of it.
func TestABackendOfThisShapeRunsAWholeRunner(t *testing.T) {
	for _, subject := range conformingBackends(t) {
		t.Run(subject.name, func(t *testing.T) {
			backend := &recordingBackend{Backend: subject.backend}
			ownership := operations.Ownership{ControllerID: "contract", ResourceID: "trf-small-1", OperationID: "op-1"}
			instance := operations.Instance{ID: "trf-small-1", Repo: "a/repo", Platform: domain.PlatformLinux,
				Profile: "small", Route: "linux-small", Resources: domain.Resources{CPU: 1, MemoryMB: 2048, Slots: 1},
				Demand:    domain.DemandKey{Repo: "a/repo", RunID: 1, Attempt: 1, JobID: 7},
				Ownership: ownership, State: operations.StatePlanned}
			state := &portState{instance: instance}

			provision := lifecycle.ProvisionExecutor{State: state, VM: backend, Ready: portReady{},
				Registration: &portRegistration{}, Bootstrap: portBootstrap{},
				Bases:   map[domain.Platform]string{domain.PlatformLinux: subject.image},
				DiskGiB: map[domain.ProfileID]int{"small": 50}}
			if err := provision.Execute(context.Background(), operations.Operation{
				Kind: lifecycle.OperationProvision, ResourceID: instance.ID}); err != nil {
				t.Fatalf("provision through the port: %v", err)
			}
			if state.instance.State != operations.StateAssigned {
				t.Fatalf("state=%s, want %s", state.instance.State, operations.StateAssigned)
			}
			if power, err := backend.Power(context.Background(), instance.ID); err != nil ||
				power != domain.InstancePowerRunning {
				t.Fatalf("the port provisioned an instance whose power reads %q (err=%v)", power, err)
			}
			// The image is the one dimension of InstanceSpec whose meaning differs
			// per backend, so it is asserted through the port's own report rather
			// than through a command line.
			listed, err := backend.List(context.Background())
			if err != nil || len(listed) != 1 || listed[0].Name != instance.ID || listed[0].Source != subject.image {
				t.Fatalf("List() = %#v (err=%v), want one %s made from %s", listed, err, instance.ID, subject.image)
			}

			state.instance.State = operations.StateDraining
			state.instance.DrainPhase = operations.DrainPhaseStoppedRecovery
			drain := lifecycle.DrainExecutor{State: state, VM: backend, Control: portControl{}}
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
			listed, err = backend.List(context.Background())
			if err != nil || len(listed) != 0 {
				t.Fatalf("the port left %d instances behind (err=%v)", len(listed), err)
			}
			want := []string{"Create:trf-small-1", "Start:trf-small-1", "Stop:trf-small-1",
				"Stop:trf-small-1", "Delete:trf-small-1"}
			if !reflect.DeepEqual(backend.mutations, want) {
				t.Fatalf("port verbs=%v, want %v", backend.mutations, want)
			}
		})
	}
}

// recordingBackend counts the port's mutating verbs rather than a backend's own
// commands, so one expected sequence describes every conforming implementation.
// A backend is free to skip a command it can prove is unnecessary — the Podman
// adapter runs no `podman stop` for a container it has just observed stopped —
// and the harness still holds it to the same lifecycle.
type recordingBackend struct {
	executor.Backend
	mutations []string
}

func (b *recordingBackend) Create(ctx context.Context, spec executor.InstanceSpec) error {
	b.mutations = append(b.mutations, "Create:"+spec.Name)
	return b.Backend.Create(ctx, spec)
}

func (b *recordingBackend) Start(ctx context.Context, name string, ownership operations.Ownership) error {
	b.mutations = append(b.mutations, "Start:"+name)
	return b.Backend.Start(ctx, name, ownership)
}

func (b *recordingBackend) Stop(ctx context.Context, name string, ownership operations.Ownership) error {
	b.mutations = append(b.mutations, "Stop:"+name)
	return b.Backend.Stop(ctx, name, ownership)
}

func (b *recordingBackend) Delete(ctx context.Context, name string, ownership operations.Ownership) error {
	b.mutations = append(b.mutations, "Delete:"+name)
	return b.Backend.Delete(ctx, name, ownership)
}

// portOwnership is the durable ownership registry a CLI-shelling backend needs,
// with the same conflict-on-mismatch rule sqlite.Store has.
type portOwnership struct {
	records map[string]operations.Ownership
}

func (o *portOwnership) PutOwnership(_ context.Context, name string, ownership operations.Ownership) error {
	if current, exists := o.records[name]; exists && current != ownership {
		return operations.ErrConflict
	}
	o.records[name] = ownership
	return nil
}

func (o *portOwnership) Ownership(_ context.Context, name string) (operations.Ownership, error) {
	record, exists := o.records[name]
	if !exists {
		return operations.Ownership{}, operations.ErrNotFound
	}
	return record, nil
}

// scriptedPodman is a rootless container host with no containers on it. It
// implements exactly the six podman invocations the adapter makes, which is the
// point: the harness above drives the production adapter's real argument vectors
// and real output parsing, and only the process boundary is simulated.
type scriptedPodman struct {
	containers map[string]scriptedContainer
}

type scriptedContainer struct {
	image  string
	state  string
	labels map[string]string
}

func newScriptedPodman() *scriptedPodman {
	return &scriptedPodman{containers: map[string]scriptedContainer{}}
}

func (p *scriptedPodman) Run(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] {
	case "info":
		return []byte(`{"host":{"security":{"rootless":true}}}`), nil
	case "ps":
		return p.listing()
	case "create":
		p.containers[args[2]] = scriptedContainer{image: p.imageOf(args), state: "created", labels: labelsOf(args)}
	case "start":
		p.move(args[len(args)-1], "running")
	case "stop":
		p.move(args[len(args)-1], "exited")
	case "rm":
		delete(p.containers, args[len(args)-1])
	default:
		return nil, fmt.Errorf("the adapter ran a podman verb this host does not implement: %v", args)
	}
	return nil, nil
}

// imageOf reads the image out of a create vector: it is the argument after the
// last option, followed only by the container's hold command.
func (p *scriptedPodman) imageOf(args []string) string {
	for index := len(args) - 1; index > 0; index-- {
		if strings.HasPrefix(args[index], "--") {
			return args[index+2]
		}
	}
	return ""
}

func labelsOf(args []string) map[string]string {
	labels := map[string]string{}
	for index, arg := range args {
		if arg == "--label" {
			key, value, _ := strings.Cut(args[index+1], "=")
			labels[key] = value
		}
	}
	return labels
}

func (p *scriptedPodman) move(name, state string) {
	if current, exists := p.containers[name]; exists {
		current.state = state
		p.containers[name] = current
	}
}

func (p *scriptedPodman) listing() ([]byte, error) {
	names := make([]string, 0, len(p.containers))
	for name := range p.containers {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]map[string]any, 0, len(names))
	for _, name := range names {
		current := p.containers[name]
		rows = append(rows, map[string]any{"Names": []string{name}, "Image": current.image,
			"State": current.state, "Labels": current.labels})
	}
	return json.Marshal(rows)
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

func (portBootstrap) Bootstrap(context.Context, string, *githubscaleset.JITSecret, []string) error {
	return nil
}

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
