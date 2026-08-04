package podman

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

var testOwnership = operations.Ownership{ControllerID: "node-b", ResourceID: "trf-small-1", OperationID: "op-7"}

const rootlessInfo = `{"host":{"security":{"rootless":true}}}`

// fakePodman is a container host with no containers in it: it answers the six
// verbs this adapter shells, remembers every argument vector it was given, and
// can be told to fail one verb, to succeed at one without changing anything (the
// silent no-op that a real runtime produces under load), or to become unreadable
// the moment a verb fails (the lost response).
type fakePodman struct {
	mu         sync.Mutex
	containers map[string]string
	images     map[string]string
	labels     map[string]map[string]string
	argv       [][]string
	fail       map[string]error
	inert      map[string]bool
	psOutput   []byte
	psError    error
	onFailure  func(*fakePodman)
	info       string
	infoError  error
}

func newFakePodman() *fakePodman {
	return &fakePodman{containers: map[string]string{}, images: map[string]string{},
		labels: map[string]map[string]string{}, fail: map[string]error{}, inert: map[string]bool{}, info: rootlessInfo}
}

// with puts one container on the host in the given podman state, labelled as
// though this test's operation had created it.
func (f *fakePodman) with(name, state string) *fakePodman {
	f.foreign(name, state)
	f.labels[name] = map[string]string{"trf.operation": testOwnership.OperationID}
	return f
}

// foreign puts a container on the host that the fleet did not create: same name,
// no ownership label. It is the collision Create must never adopt.
func (f *fakePodman) foreign(name, state string) *fakePodman {
	f.containers[name] = state
	f.images[name] = "ghcr.io/example/runner:1"
	f.labels[name] = map[string]string{}
	return f
}

func (f *fakePodman) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.argv = append(f.argv, append([]string(nil), args...))
	verb := args[0]
	if verb == "info" {
		return []byte(f.info), f.infoError
	}
	if verb == "ps" {
		if f.psError != nil {
			return nil, f.psError
		}
		if f.psOutput != nil {
			return f.psOutput, nil
		}
		return f.listing(), nil
	}
	if err := f.fail[verb]; err != nil {
		if f.onFailure != nil {
			f.onFailure(f)
		}
		return []byte(err.Error()), err
	}
	if !f.inert[verb] {
		f.apply(verb, args)
	}
	return nil, nil
}

// apply is the whole state machine of a container host: create makes one, start
// and stop move it, rm removes it.
func (f *fakePodman) apply(verb string, args []string) {
	name := args[len(args)-1]
	switch verb {
	case "create":
		name = args[2]
		f.containers[name] = "created"
		f.images[name] = args[len(args)-len(defaultHoldCommand)-1]
		f.labels[name] = map[string]string{}
		for index, arg := range args {
			if arg == "--label" {
				key, value, _ := strings.Cut(args[index+1], "=")
				f.labels[name][key] = value
			}
		}
	case "start":
		f.containers[name] = "running"
	case "stop":
		f.containers[name] = "exited"
	case "rm":
		delete(f.containers, name)
		delete(f.labels, name)
	}
}

func (f *fakePodman) listing() []byte {
	names := make([]string, 0, len(f.containers))
	for name := range f.containers {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]container, 0, len(names))
	for _, name := range names {
		rows = append(rows, container{Names: []string{name}, Image: f.images[name],
			State: f.containers[name], Labels: f.labels[name]})
	}
	encoded, _ := json.Marshal(rows)
	return encoded
}

func (f *fakePodman) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := make([]string, 0, len(f.argv))
	for _, args := range f.argv {
		lines = append(lines, strings.Join(args, " "))
	}
	return lines
}

func (f *fakePodman) state(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[name]
}

// memoryOwnership is the durable ownership registry, in memory.
type memoryOwnership struct {
	records map[string]operations.Ownership
	putErr  error
	getErr  error
}

func ownedBy(name string, ownership operations.Ownership) *memoryOwnership {
	return &memoryOwnership{records: map[string]operations.Ownership{name: ownership}}
}

// PutOwnership mirrors sqlite.Store: an existing record for another owner is a
// conflict, never an overwrite.
func (m *memoryOwnership) PutOwnership(_ context.Context, name string, ownership operations.Ownership) error {
	if m.putErr != nil {
		return m.putErr
	}
	if current, exists := m.records[name]; exists && current != ownership {
		return operations.ErrConflict
	}
	m.records[name] = ownership
	return nil
}

func (m *memoryOwnership) Ownership(_ context.Context, name string) (operations.Ownership, error) {
	if m.getErr != nil {
		return operations.Ownership{}, m.getErr
	}
	record, ok := m.records[name]
	if !ok {
		return operations.Ownership{}, operations.ErrNotFound
	}
	return record, nil
}

type fakeConfirmation struct {
	answers []operations.DeletionConfirmation
	errs    []error
	calls   int
}

func (f *fakeConfirmation) ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error) {
	index := f.calls
	f.calls++
	if index < len(f.errs) && f.errs[index] != nil {
		return operations.DeletionConfirmation{}, f.errs[index]
	}
	if index >= len(f.answers) {
		index = len(f.answers) - 1
	}
	return f.answers[index], nil
}

func safeConfirmations() *fakeConfirmation {
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: fixedNow()}
	return &fakeConfirmation{answers: []operations.DeletionConfirmation{safe, safe}}
}

func fixedNow() time.Time { return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC) }

// adapterFor builds a production-shaped adapter over a fake host. Every test
// starts here so a default that changes is a test that changes with it.
func adapterFor(host *fakePodman, registry *memoryOwnership) *Adapter {
	return &Adapter{Runner: host, Ownership: registry, Confirmation: safeConfirmations(),
		Image: "ghcr.io/example/runner:1", Now: fixedNow}
}

func spec(name string) executor.InstanceSpec {
	return executor.InstanceSpec{Name: name, Image: "ghcr.io/example/runner:1", CPU: 2, MemoryMB: 4096,
		DiskGB: 50, Ownership: testOwnership}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestHealthyRefusesAContainerRuntimeThatCannotBeTrusted(t *testing.T) {
	commandFailure := errors.New("podman: command not found")
	for _, testCase := range []struct {
		name      string
		info      string
		infoError error
		wantKind  ErrorKind
		wantOK    bool
	}{
		{name: "rootless podman is healthy", info: rootlessInfo, wantOK: true},
		{name: "absent podman is unhealthy", infoError: commandFailure, wantKind: ErrorUnhealthy},
		{name: "unreadable info is unhealthy", info: "not json", wantKind: ErrorUnhealthy},
		{name: "rootful podman is refused", info: `{"host":{"security":{"rootless":false}}}`, wantKind: ErrorUnhealthy},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakePodman()
			host.info, host.infoError = testCase.info, testCase.infoError
			err := adapterFor(host, &memoryOwnership{records: map[string]operations.Ownership{}}).Healthy(context.Background())
			if testCase.wantOK {
				if err != nil {
					t.Fatalf("Healthy() = %v, want nil", err)
				}
				return
			}
			var adapterError *Error
			if !errors.As(err, &adapterError) || adapterError.Kind != testCase.wantKind {
				t.Fatalf("Healthy() = %v, want kind %s", err, testCase.wantKind)
			}
		})
	}
}

// TestHealthyProbesOnceAndRetriesAfterAFailure pins the caching rule: a healthy
// runtime is asked once for the life of the daemon, and an unhealthy one is
// asked again, because podman that comes back must be picked up without a
// restart.
func TestHealthyProbesOnceAndRetriesAfterAFailure(t *testing.T) {
	host := newFakePodman()
	host.infoError = errors.New("no such file")
	adapter := adapterFor(host, &memoryOwnership{records: map[string]operations.Ownership{}})
	if err := adapter.Healthy(context.Background()); err == nil {
		t.Fatal("an absent podman reported healthy")
	}
	if err := adapter.Healthy(context.Background()); err == nil {
		t.Fatal("a failed probe was cached")
	}
	host.infoError = nil
	for range 3 {
		if err := adapter.Healthy(context.Background()); err != nil {
			t.Fatalf("Healthy() = %v", err)
		}
	}
	if got := len(host.commands()); got != 3 {
		t.Fatalf("podman info ran %d times, want 3 (two failures and one cached success)", got)
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// TestCreateBuildsTheWholeContainerSpecificationInOneVector is the audit: every
// capability a job's container has is on this line, so a grant that appears
// without a decision record fails a test.
func TestCreateBuildsTheWholeContainerSpecificationInOneVector(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		instance string
		kvm      []string
		hold     []string
		want     string
	}{
		{
			name: "an ordinary profile gets no device", instance: "trf-small-abc",
			kvm: []string{"trf-android-"},
			want: "create --name trf-small-abc --cpus 2 --memory 4096m --restart no --init " +
				"--label trf.controller=node-b --label trf.resource=trf-small-1 --label trf.operation=op-7 " +
				"ghcr.io/example/runner:1 sleep infinity",
		},
		{
			name: "the android profile gets /dev/kvm", instance: "trf-android-abc",
			kvm: []string{"", "trf-android-"},
			want: "create --name trf-android-abc --cpus 2 --memory 4096m --restart no --init " +
				"--label trf.controller=node-b --label trf.resource=trf-small-1 --label trf.operation=op-7 " +
				"--device /dev/kvm ghcr.io/example/runner:1 sleep infinity",
		},
		{
			name: "a configured hold command replaces the default", instance: "trf-small-abc",
			hold: []string{"/usr/bin/tail", "-f", "/dev/null"},
			want: "create --name trf-small-abc --cpus 2 --memory 4096m --restart no --init " +
				"--label trf.controller=node-b --label trf.resource=trf-small-1 --label trf.operation=op-7 " +
				"ghcr.io/example/runner:1 /usr/bin/tail -f /dev/null",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakePodman()
			adapter := adapterFor(host, &memoryOwnership{records: map[string]operations.Ownership{}})
			adapter.KVMInstancePrefixes, adapter.HoldCommand = testCase.kvm, testCase.hold
			if err := adapter.Create(context.Background(), spec(testCase.instance)); err != nil {
				t.Fatalf("Create() = %v", err)
			}
			commands := host.commands()
			if got := commands[len(commands)-1]; got != testCase.want {
				t.Fatalf("create vector =\n%s\nwant\n%s", got, testCase.want)
			}
			if host.state(testCase.instance) != "created" {
				t.Fatalf("container state = %q, want created", host.state(testCase.instance))
			}
		})
	}
}

func TestCreateRefusesARequestItCannotHonour(t *testing.T) {
	for _, testCase := range []struct {
		name string
		spec executor.InstanceSpec
	}{
		{name: "invalid instance name", spec: executor.InstanceSpec{Name: "has space", Image: "img", CPU: 1, MemoryMB: 1, Ownership: testOwnership}},
		{name: "empty image", spec: executor.InstanceSpec{Name: "trf-small-1", CPU: 1, MemoryMB: 1, Ownership: testOwnership}},
		{name: "image that reads as an option", spec: executor.InstanceSpec{Name: "trf-small-1", Image: "--rm", CPU: 1, MemoryMB: 1, Ownership: testOwnership}},
		{name: "unowned request", spec: executor.InstanceSpec{Name: "trf-small-1", Image: "img", CPU: 1, MemoryMB: 1}},
		{name: "no cpu", spec: executor.InstanceSpec{Name: "trf-small-1", Image: "img", MemoryMB: 1, Ownership: testOwnership}},
		{name: "no memory", spec: executor.InstanceSpec{Name: "trf-small-1", Image: "img", CPU: 1, Ownership: testOwnership}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakePodman()
			err := adapterFor(host, &memoryOwnership{records: map[string]operations.Ownership{}}).Create(context.Background(), testCase.spec)
			if !errors.Is(err, operations.ErrInvalid) {
				t.Fatalf("Create() = %v, want ErrInvalid", err)
			}
			if len(host.commands()) != 0 {
				t.Fatalf("an invalid request reached podman: %v", host.commands())
			}
		})
	}
}

func TestCreateSurvivesEveryWayOneCanFail(t *testing.T) {
	createFailed := errors.New("Error: creating container storage: name is already in use")
	unreadable := errors.New("Error: cannot connect to podman")
	for _, testCase := range []struct {
		name      string
		host      func() *fakePodman
		registry  func() *memoryOwnership
		wantErr   bool
		wantKind  ErrorKind
		wantState string
	}{
		{
			name: "an unhealthy runtime never creates anything",
			host: func() *fakePodman {
				host := newFakePodman()
				host.infoError = errors.New("no such file or directory")
				return host
			},
			wantErr: true, wantKind: ErrorUnhealthy,
		},
		{
			name: "a durable ownership write that fails stops the create",
			host: newFakePodman,
			registry: func() *memoryOwnership {
				return &memoryOwnership{records: map[string]operations.Ownership{}, putErr: errors.New("disk full")}
			},
			wantErr: true,
		},
		{
			name: "an unreadable host before the create is not a create",
			host: func() *fakePodman {
				host := newFakePodman()
				host.psError = unreadable
				return host
			},
			registry: func() *memoryOwnership { return ownedBy("trf-small-abc", testOwnership) },
			wantErr:  true,
		},
		{
			name:      "an owned container that already exists is success and is not recreated",
			host:      func() *fakePodman { return newFakePodman().with("trf-small-abc", "created") },
			registry:  func() *memoryOwnership { return ownedBy("trf-small-abc", testOwnership) },
			wantState: "created",
		},
		{
			name: "a lost response over a container that did appear is success",
			host: func() *fakePodman {
				host := newFakePodman()
				host.fail["create"] = createFailed
				host.onFailure = func(f *fakePodman) { f.with("trf-small-abc", "created") }
				return host
			},
			wantState: "created",
		},
		{
			name: "a create that failed and left nothing behind is the command failure",
			host: func() *fakePodman {
				host := newFakePodman()
				host.fail["create"] = createFailed
				return host
			},
			wantErr: true,
		},
		{
			name: "a create that failed onto an unreadable host is uncertain",
			host: func() *fakePodman {
				host := newFakePodman()
				host.fail["create"] = createFailed
				host.onFailure = func(f *fakePodman) { f.psError = unreadable }
				return host
			},
			wantErr: true, wantKind: ErrorUncertain,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := &memoryOwnership{records: map[string]operations.Ownership{}}
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			host := testCase.host()
			err := adapterFor(host, registry).Create(context.Background(), spec("trf-small-abc"))
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Create() = %v, wantErr %v", err, testCase.wantErr)
			}
			var adapterError *Error
			if testCase.wantKind != "" && (!errors.As(err, &adapterError) || adapterError.Kind != testCase.wantKind) {
				t.Fatalf("Create() = %v, want kind %s", err, testCase.wantKind)
			}
			if got := host.state("trf-small-abc"); got != testCase.wantState {
				t.Fatalf("container state = %q, want %q", got, testCase.wantState)
			}
		})
	}
}

// TestCreateNeverAdoptsAContainerTheFleetCannotProveIsItsOwn is the rule that
// separates this backend from the Tart one: a name collision with an unowned
// container is a conflict, because a container is made per job and never reused.
func TestCreateNeverAdoptsAContainerTheFleetCannotProveIsItsOwn(t *testing.T) {
	previous := operations.Ownership{ControllerID: "node-b", ResourceID: "trf-small-1", OperationID: "op-6"}
	for _, testCase := range []struct {
		name     string
		host     func() *fakePodman
		registry func() *memoryOwnership
	}{
		{
			name: "a container the fleet never labelled holds the name",
			host: func() *fakePodman { return newFakePodman().foreign("trf-small-abc", "running") },
		},
		{
			name: "a container a previous operation left behind holds the name",
			host: func() *fakePodman {
				host := newFakePodman().foreign("trf-small-abc", "exited")
				host.labels["trf-small-abc"]["trf.operation"] = previous.OperationID
				return host
			},
		},
		{
			name:     "a durable record from a previous operation refuses the write",
			host:     func() *fakePodman { return newFakePodman() },
			registry: func() *memoryOwnership { return ownedBy("trf-small-abc", previous) },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := &memoryOwnership{records: map[string]operations.Ownership{}}
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			host := testCase.host()
			err := adapterFor(host, registry).Create(context.Background(), spec("trf-small-abc"))
			if !errors.Is(err, operations.ErrConflict) {
				t.Fatalf("Create() = %v, want ErrConflict", err)
			}
			for _, command := range host.commands() {
				if strings.HasPrefix(command, "create") {
					t.Fatalf("the adapter created over a container it does not own: %s", command)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestStart(t *testing.T) {
	startFailed := errors.New("Error: unable to start container")
	for _, testCase := range []struct {
		name     string
		host     func() *fakePodman
		registry func() *memoryOwnership
		instance string
		wantErr  bool
		wantKind ErrorKind
		wantRun  bool
		wantRuns int
	}{
		{
			name:    "a created container starts and is observed running",
			host:    func() *fakePodman { return newFakePodman().with("trf-small-abc", "created") },
			wantRun: true, wantRuns: 1,
		},
		{
			name:    "an already running container is not started twice",
			host:    func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			wantRun: true, wantRuns: 0,
		},
		{
			name:     "an invalid name never reaches podman",
			host:     newFakePodman,
			instance: "has space", wantErr: true,
		},
		{
			name:     "an absent container cannot be started",
			host:     newFakePodman,
			registry: func() *memoryOwnership { return ownedBy("trf-small-abc", testOwnership) },
			wantErr:  true,
		},
		{
			name: "a start that failed but did start the container is success",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "created")
				host.fail["start"] = startFailed
				host.onFailure = func(f *fakePodman) { f.containers["trf-small-abc"] = "running" }
				return host
			},
			wantRun: true, wantRuns: 1,
		},
		{
			name: "a start that failed and left the container down is the command failure",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "created")
				host.fail["start"] = startFailed
				return host
			},
			wantErr: true, wantRuns: 1,
		},
		{
			name: "a start that succeeded without effect is uncertain, never assumed",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "created")
				host.inert["start"] = true
				return host
			},
			wantErr: true, wantKind: ErrorUncertain, wantRuns: 1,
		},
		{
			name: "a start onto an unreadable host is uncertain",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "created")
				host.fail["start"] = startFailed
				host.onFailure = func(f *fakePodman) { f.psError = errors.New("cannot connect to podman") }
				return host
			},
			wantErr: true, wantKind: ErrorUncertain, wantRuns: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := testCase.host()
			registry := ownedBy("trf-small-abc", testOwnership)
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			instance := testCase.instance
			if instance == "" {
				instance = "trf-small-abc"
			}
			err := adapterFor(host, registry).Start(context.Background(), instance, testOwnership)
			assertFailure(t, "Start", err, testCase.wantErr, testCase.wantKind)
			if got := host.state("trf-small-abc") == "running"; got != testCase.wantRun {
				t.Errorf("running = %v, want %v", got, testCase.wantRun)
			}
			if got := countVerb(host, "start"); got != testCase.wantRuns {
				t.Errorf("podman start ran %d times, want %d", got, testCase.wantRuns)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

func TestStop(t *testing.T) {
	stopFailed := errors.New("Error: given PID did not die within timeout")
	for _, testCase := range []struct {
		name     string
		host     func() *fakePodman
		registry func() *memoryOwnership
		instance string
		wantErr  bool
		wantKind ErrorKind
		wantStop int
	}{
		{
			name:     "a running container is stopped",
			host:     func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			wantStop: 1,
		},
		{
			name:     "a paused container still holds resources and is stopped",
			host:     func() *fakePodman { return newFakePodman().with("trf-small-abc", "paused") },
			wantStop: 1,
		},
		{
			name: "an already stopped container is left alone",
			host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
		},
		{
			name: "an absent container is success, because cleanup retries",
			host: newFakePodman,
		},
		{
			name:     "an invalid name never reaches podman",
			host:     newFakePodman,
			instance: "has/slash", wantErr: true,
		},
		{
			name: "an unreadable ownership record is a failure, not an absence",
			host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			registry: func() *memoryOwnership {
				return &memoryOwnership{records: map[string]operations.Ownership{}, getErr: errors.New("database is locked")}
			},
			wantErr: true,
		},
		{
			name: "a stop that failed onto a vanished container is success",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "running")
				host.fail["stop"] = stopFailed
				host.onFailure = func(f *fakePodman) { delete(f.containers, "trf-small-abc") }
				return host
			},
			wantStop: 1,
		},
		{
			name: "a stop that failed after the container went down is success",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "running")
				host.fail["stop"] = stopFailed
				host.onFailure = func(f *fakePodman) { f.containers["trf-small-abc"] = "exited" }
				return host
			},
			wantStop: 1,
		},
		{
			name: "a stop that failed with the container still up is the command failure",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "running")
				host.fail["stop"] = stopFailed
				return host
			},
			wantErr: true, wantStop: 1,
		},
		{
			name: "a stop onto an unreadable host is uncertain",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "running")
				host.fail["stop"] = stopFailed
				host.onFailure = func(f *fakePodman) { f.psError = errors.New("cannot connect to podman") }
				return host
			},
			wantErr: true, wantKind: ErrorUncertain, wantStop: 1,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := testCase.host()
			registry := ownedBy("trf-small-abc", testOwnership)
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			instance := testCase.instance
			if instance == "" {
				instance = "trf-small-abc"
			}
			err := adapterFor(host, registry).Stop(context.Background(), instance, testOwnership)
			assertFailure(t, "Stop", err, testCase.wantErr, testCase.wantKind)
			if got := countVerb(host, "stop"); got != testCase.wantStop {
				t.Errorf("podman stop ran %d times, want %d", got, testCase.wantStop)
			}
		})
	}
}

// TestStopUsesTheConfiguredGracePeriod pins the one argument a stop carries.
func TestStopUsesTheConfiguredGracePeriod(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{name: "unset is ten seconds", want: "stop --time 10 trf-small-abc"},
		{name: "configured whole seconds", timeout: 45 * time.Second, want: "stop --time 45 trf-small-abc"},
		{name: "a sub-second grace is one second, never zero", timeout: 200 * time.Millisecond, want: "stop --time 1 trf-small-abc"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakePodman().with("trf-small-abc", "running")
			adapter := adapterFor(host, ownedBy("trf-small-abc", testOwnership))
			adapter.StopTimeout = testCase.timeout
			if err := adapter.Stop(context.Background(), "trf-small-abc", testOwnership); err != nil {
				t.Fatalf("Stop() = %v", err)
			}
			commands := host.commands()
			if got := commands[len(commands)-1]; got != testCase.want {
				t.Fatalf("stop vector = %q, want %q", got, testCase.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDelete(t *testing.T) {
	stale := operations.DeletionConfirmation{Fresh: false, RunnerInactive: true, JobsInactive: true, ObservedAt: fixedNow()}
	safe := operations.DeletionConfirmation{Fresh: true, RunnerInactive: true, JobsInactive: true, ObservedAt: fixedNow()}
	removeFailed := errors.New("Error: removing container: container is in use")
	for _, testCase := range []struct {
		name         string
		host         func() *fakePodman
		registry     func() *memoryOwnership
		confirmation *fakeConfirmation
		instance     string
		wantErr      bool
		wantKind     ErrorKind
		wantGone     bool
		wantVector   string
	}{
		{
			name:       "a stopped confirmed container is removed by force",
			host:       func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			wantGone:   true,
			wantVector: "rm --force trf-small-abc",
		},
		{
			name:       "a running container is stopped, re-confirmed, then removed",
			host:       func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			wantGone:   true,
			wantVector: "rm --force trf-small-abc",
		},
		{
			name:     "an absent container is success",
			host:     newFakePodman,
			wantGone: true,
		},
		{
			name:     "an invalid name never reaches podman",
			host:     newFakePodman,
			instance: "-leading", wantErr: true, wantGone: true,
		},
		{
			name: "an unowned container is a conflict",
			host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			registry: func() *memoryOwnership {
				return ownedBy("trf-small-abc", operations.Ownership{ControllerID: "x", ResourceID: "y", OperationID: "z"})
			},
			wantErr: true,
		},
		{
			name:         "no confirmation provider is uncertain, never a delete",
			host:         func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			confirmation: nil, wantErr: true, wantKind: ErrorUncertain,
		},
		{
			name:         "a confirmation that cannot be taken stops the delete",
			host:         func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			confirmation: &fakeConfirmation{errs: []error{errors.New("github unreachable")}, answers: []operations.DeletionConfirmation{safe}},
			wantErr:      true,
		},
		{
			name:         "a stale confirmation stops the delete",
			host:         func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			confirmation: &fakeConfirmation{answers: []operations.DeletionConfirmation{stale}},
			wantErr:      true, wantKind: ErrorUncertain,
		},
		{
			name:         "a job that started during the stop stops the delete",
			host:         func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			confirmation: &fakeConfirmation{answers: []operations.DeletionConfirmation{safe, stale}},
			wantErr:      true, wantKind: ErrorUncertain,
		},
		{
			name:         "a re-confirmation that cannot be taken stops the delete",
			host:         func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			confirmation: &fakeConfirmation{answers: []operations.DeletionConfirmation{safe}, errs: []error{nil, errors.New("github unreachable")}},
			wantErr:      true, wantKind: ErrorUncertain,
		},
		{
			name: "a stop that cannot be completed stops the delete",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "running")
				host.fail["stop"] = errors.New("Error: given PID did not die within timeout")
				return host
			},
			wantErr: true,
		},
		{
			name: "a removal that failed onto a vanished container is success",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = removeFailed
				host.onFailure = func(f *fakePodman) { delete(f.containers, "trf-small-abc") }
				return host
			},
			wantGone: true,
		},
		{
			name: "a removal that failed with the container still there is the command failure",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = removeFailed
				return host
			},
			wantErr: true,
		},
		{
			name: "a removal onto an unreadable host is uncertain",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = removeFailed
				host.onFailure = func(f *fakePodman) { f.psError = errors.New("cannot connect to podman") }
				return host
			},
			wantErr: true, wantKind: ErrorUncertain,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := testCase.host()
			registry := ownedBy("trf-small-abc", testOwnership)
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			adapter := adapterFor(host, registry)
			if testCase.confirmation != nil {
				adapter.Confirmation = testCase.confirmation
			} else if strings.Contains(testCase.name, "no confirmation provider") {
				adapter.Confirmation = nil
			}
			instance := testCase.instance
			if instance == "" {
				instance = "trf-small-abc"
			}
			err := adapter.Delete(context.Background(), instance, testOwnership)
			assertFailure(t, "Delete", err, testCase.wantErr, testCase.wantKind)
			if gone := host.state("trf-small-abc") == ""; gone != testCase.wantGone {
				t.Errorf("container gone = %v, want %v", gone, testCase.wantGone)
			}
			if testCase.wantVector != "" {
				commands := host.commands()
				if got := commands[len(commands)-1]; got != testCase.wantVector {
					t.Errorf("removal vector = %q, want %q", got, testCase.wantVector)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Reap
// ---------------------------------------------------------------------------

func TestReap(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		host       func() *fakePodman
		registry   func() *memoryOwnership
		instance   string
		wantErr    bool
		wantKind   ErrorKind
		wantGone   bool
		wantVector string
	}{
		{
			name:     "a stopped owned container is removed without GitHub evidence",
			host:     func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			wantGone: true, wantVector: "rm trf-small-abc",
		},
		{
			name:    "a running container is refused, because it may hold live work",
			host:    func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") },
			wantErr: true, wantKind: ErrorUncertain,
		},
		{
			name:     "an already absent container is success",
			host:     newFakePodman,
			wantGone: true,
		},
		{
			name:     "an invalid name never reaches podman",
			host:     newFakePodman,
			instance: "..", wantErr: true, wantGone: true,
		},
		{
			name: "an unowned container is a conflict",
			host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") },
			registry: func() *memoryOwnership {
				return &memoryOwnership{records: map[string]operations.Ownership{}, getErr: errors.New("database is locked")}
			},
			wantErr: true,
		},
		{
			name: "a removal that failed with the container still there is the command failure",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = errors.New("Error: removing container")
				return host
			},
			wantErr: true,
		},
		{
			name: "a removal onto an unreadable host is uncertain",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = errors.New("Error: removing container")
				host.onFailure = func(f *fakePodman) { f.psError = errors.New("cannot connect to podman") }
				return host
			},
			wantErr: true, wantKind: ErrorUncertain,
		},
		{
			name: "a removal that failed onto a vanished container is success",
			host: func() *fakePodman {
				host := newFakePodman().with("trf-small-abc", "exited")
				host.fail["rm"] = errors.New("Error: removing container")
				host.onFailure = func(f *fakePodman) { delete(f.containers, "trf-small-abc") }
				return host
			},
			wantGone: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := testCase.host()
			registry := ownedBy("trf-small-abc", testOwnership)
			if testCase.registry != nil {
				registry = testCase.registry()
			}
			instance := testCase.instance
			if instance == "" {
				instance = "trf-small-abc"
			}
			err := adapterFor(host, registry).Reap(context.Background(), instance, testOwnership)
			assertFailure(t, "Reap", err, testCase.wantErr, testCase.wantKind)
			if gone := host.state("trf-small-abc") == ""; gone != testCase.wantGone {
				t.Errorf("container gone = %v, want %v", gone, testCase.wantGone)
			}
			if testCase.wantVector != "" {
				commands := host.commands()
				if got := commands[len(commands)-1]; got != testCase.wantVector {
					t.Errorf("removal vector = %q, want %q", got, testCase.wantVector)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// List and Running
// ---------------------------------------------------------------------------

func TestListReportsEveryContainerTheHostCanSee(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		output  string
		listErr error
		want    []executor.Instance
		wantErr ErrorKind
	}{
		{
			name:   "running, stopped, and untracked containers alike",
			output: `[{"Names":["trf-small-abc"],"Image":"runner:1","State":"running"},{"Names":["someone-elses"],"Image":"other:2","State":"exited"}]`,
			want: []executor.Instance{{Name: "trf-small-abc", Running: true, Source: "runner:1"},
				{Name: "someone-elses", Running: false, Source: "other:2"}},
		},
		{
			name:   "an empty host is an empty measurement, never nil",
			output: `[]`, want: []executor.Instance{},
		},
		{
			name:   "podman's null for an empty host is the same measurement",
			output: `null`, want: []executor.Instance{},
		},
		{
			name:   "every alias of a multiply named container is reported",
			output: `[{"Names":["first","second"],"Image":"runner:1","State":"created"}]`,
			want:   []executor.Instance{{Name: "first", Source: "runner:1"}, {Name: "second", Source: "runner:1"}},
		},
		{
			name:   "unreadable JSON is uncertain, never an empty host",
			output: `{"not":"a list"}`, wantErr: ErrorUncertain,
		},
		{name: "a failed listing is the command failure", listErr: errors.New("cannot connect to podman")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			host := newFakePodman()
			host.psOutput, host.psError = []byte(testCase.output), testCase.listErr
			instances, err := adapterFor(host, &memoryOwnership{}).List(context.Background())
			if testCase.want == nil {
				if err == nil {
					t.Fatalf("List() = %v, want an error", instances)
				}
				var adapterError *Error
				if testCase.wantErr != "" && (!errors.As(err, &adapterError) || adapterError.Kind != testCase.wantErr) {
					t.Fatalf("List() error = %v, want kind %s", err, testCase.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("List() = %v", err)
			}
			if !reflect.DeepEqual(instances, testCase.want) {
				t.Fatalf("List() = %#v, want %#v", instances, testCase.want)
			}
		})
	}
}

func TestRunning(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		host    func() *fakePodman
		want    bool
		wantErr bool
	}{
		{name: "a running container", host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "running") }, want: true},
		{name: "a stopped container", host: func() *fakePodman { return newFakePodman().with("trf-small-abc", "exited") }},
		{name: "an absent container is not running", host: newFakePodman},
		{
			name: "an unreadable host fails rather than reporting stopped",
			host: func() *fakePodman {
				host := newFakePodman()
				host.psError = errors.New("cannot connect to podman")
				return host
			},
			wantErr: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			running, err := adapterFor(testCase.host(), &memoryOwnership{}).Running(context.Background(), "trf-small-abc")
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Running() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if running != testCase.want {
				t.Fatalf("Running() = %v, want %v", running, testCase.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Error classification and the production command runner
// ---------------------------------------------------------------------------

func TestClassifyNamesTheFailureModeAnOperatorMustActuponon(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		output     string
		err        error
		contextErr error
		want       ErrorKind
	}{
		{name: "a deadline outranks every stderr fragment", output: "no such container", contextErr: context.DeadlineExceeded, want: ErrorTimeout},
		{name: "a cancellation is a timeout", contextErr: context.Canceled, want: ErrorTimeout},
		{name: "an absent binary is an unhealthy runtime", err: exec.ErrNotFound, want: ErrorUnhealthy},
		{name: "no such container", output: "Error: no such container trf-small-1", want: ErrorNotFound},
		{name: "no container with that name", output: "Error: no container with name or ID trf-small-1 found", want: ErrorNotFound},
		{name: "no such object", output: "Error: no such object: trf-small-1", want: ErrorNotFound},
		{name: "a name collision", output: `Error: creating container: the container name "x" is already in use`, want: ErrorAlreadyExist},
		{name: "an existing container", output: "Error: container already exists", want: ErrorAlreadyExist},
		{name: "a denied permission", output: "Error: permission denied", want: ErrorPermission},
		{name: "an operation that is not permitted", output: "Error: operation not permitted", want: ErrorPermission},
		{name: "a broken user namespace is unhealthy", output: "Error: cannot find newuidmap", want: ErrorUnhealthy},
		{name: "an unreachable service is unhealthy", output: "Error: cannot connect to podman", want: ErrorUnhealthy},
		{name: "a missing executable is unhealthy", output: `exec: "podman": executable file not found in $PATH`, want: ErrorUnhealthy},
		{name: "anything else is a command failure", output: "Error: something novel", want: ErrorCommand},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.err
			if err == nil {
				err = errors.New(testCase.output)
			}
			classified := classify([]string{"ps", "--all"}, []byte(testCase.output), err, testCase.contextErr)
			var adapterError *Error
			if !errors.As(classified, &adapterError) {
				t.Fatalf("classify() = %T", classified)
			}
			if adapterError.Kind != testCase.want {
				t.Fatalf("kind = %s, want %s", adapterError.Kind, testCase.want)
			}
			if adapterError.Op != "ps --all" || adapterError.ExitCode != -1 || !errors.Is(classified, err) {
				t.Fatalf("classified = %#v", adapterError)
			}
			if !strings.Contains(adapterError.Error(), "podman ps --all") {
				t.Fatalf("Error() = %q", adapterError.Error())
			}
		})
	}
}

// TestExecRunnerShellsThePodmanBinaryDirectly covers the one place a real
// process is started. It uses `/bin/echo` and `/usr/bin/false` rather than
// podman, which is not installed on every machine that runs this suite.
func TestExecRunnerShellsThePodmanBinaryDirectly(t *testing.T) {
	output, err := ExecRunner{Binary: "/bin/echo"}.Run(context.Background(), "ps", "--all")
	if err != nil || strings.TrimSpace(string(output)) != "ps --all" {
		t.Fatalf("Run() = %q, %v", output, err)
	}

	_, err = ExecRunner{Binary: "/usr/bin/false"}.Run(context.Background(), "ps")
	var adapterError *Error
	if !errors.As(err, &adapterError) || adapterError.Kind != ErrorCommand || adapterError.ExitCode != 1 {
		t.Fatalf("a failing command classified as %#v", err)
	}

	_, err = ExecRunner{Binary: "definitely-not-a-real-podman"}.Run(context.Background(), "info")
	if !errors.As(err, &adapterError) || adapterError.Kind != ErrorUnhealthy {
		t.Fatalf("an absent binary classified as %#v", err)
	}

	// An unset binary resolves the bare name through PATH, which is the
	// production configuration; podman is absent here, so the observable is the
	// unhealthy classification rather than a successful command.
	if _, err := (ExecRunner{}).Run(context.Background(), "info"); err == nil {
		t.Skip("this machine has podman on PATH; the default-binary path is covered by the live smoke test")
	}
}

// ---------------------------------------------------------------------------
// Defaults and pure helpers
// ---------------------------------------------------------------------------

func TestUnconfiguredAdapterDefaults(t *testing.T) {
	adapter := &Adapter{}
	if adapter.timeout() != 30*time.Second || adapter.confirmationMaxAge() != 30*time.Second {
		t.Errorf("timeouts = %v, %v", adapter.timeout(), adapter.confirmationMaxAge())
	}
	if !reflect.DeepEqual(adapter.holdCommand(), defaultHoldCommand) {
		t.Errorf("hold command = %v", adapter.holdCommand())
	}
	if _, ok := adapter.runner().(ExecRunner); !ok {
		t.Errorf("runner = %T", adapter.runner())
	}
	if adapter.now().Location() != time.UTC || adapter.now().IsZero() {
		t.Errorf("now = %v", adapter.now())
	}
	configured := &Adapter{CommandTimeout: time.Minute, ConfirmationMaxAge: time.Minute, Now: fixedNow}
	if configured.timeout() != time.Minute || configured.confirmationMaxAge() != time.Minute ||
		!configured.now().Equal(fixedNow()) {
		t.Error("configured values were not honoured")
	}
}

func TestValidateImageRefusesAReferenceThatCannotSafelyBeAnArgument(t *testing.T) {
	for _, image := range []string{"docker.io/library/ubuntu:24.04", "ghcr.io/o/r@sha256:abc", "localhost/runner"} {
		if err := ValidateImage(image); err != nil {
			t.Errorf("ValidateImage(%q) = %v", image, err)
		}
	}
	for _, image := range []string{"", "   ", " padded ", "-rm", "two words", "line\nbreak", "null\x00byte"} {
		if err := ValidateImage(image); err == nil {
			t.Errorf("ValidateImage(%q) accepted", image)
		}
	}
}

func TestContainerStateReadsAsRunningOnlyWhenResourcesAreHeld(t *testing.T) {
	for state, want := range map[string]bool{"running": true, "paused": true, "Running": true, " running ": true,
		"created": false, "exited": false, "stopped": false, "removing": false, "dead": false, "": false} {
		if got := (container{State: state}).running(); got != want {
			t.Errorf("state %q running = %v, want %v", state, got, want)
		}
	}
}

func TestDefaultBinaryIsResolvedThroughPath(t *testing.T) {
	if DefaultBinary != "podman" {
		t.Fatalf("DefaultBinary = %q", DefaultBinary)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertFailure(t *testing.T, verb string, err error, wantErr bool, wantKind ErrorKind) {
	t.Helper()
	if (err != nil) != wantErr {
		t.Fatalf("%s() = %v, wantErr %v", verb, err, wantErr)
	}
	var adapterError *Error
	if wantKind != "" && (!errors.As(err, &adapterError) || adapterError.Kind != wantKind) {
		t.Fatalf("%s() = %v, want kind %s", verb, err, wantKind)
	}
}

func countVerb(host *fakePodman, verb string) int {
	count := 0
	for _, command := range host.commands() {
		if strings.HasPrefix(command, verb+" ") || command == verb {
			count++
		}
	}
	return count
}
