package podman

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// hostCPUInfo is four processor blocks in the shape `/proc/cpuinfo` really has:
// blank-line separated, numbered from zero, with fields a tool actually reads.
const hostCPUInfo = `processor	: 0
model name	: AMD Ryzen AI 9 HX 470
flags		: fpu vme de

processor	: 1
model name	: AMD Ryzen AI 9 HX 470
flags		: fpu vme de

processor	: 2
model name	: AMD Ryzen AI 9 HX 470
flags		: fpu vme de

processor	: 3
model name	: AMD Ryzen AI 9 HX 470
flags		: fpu vme de

`

// hostStat carries the aggregate line, four per-CPU lines, and the non-CPU rows
// that every reader of this file also depends on.
const hostStat = `cpu  100 0 200 300 0 0 0 0 0 0
cpu0 25 0 50 75 0 0 0 0 0 0
cpu1 25 0 50 75 0 0 0 0 0 0
cpu2 25 0 50 75 0 0 0 0 0 0
cpu3 25 0 50 75 0 0 0 0 0 0
intr 12345 0 0
ctxt 987654
btime 1756000000
processes 4242
procs_running 2
procs_blocked 0
`

func fakeProc(cpuinfo, stat string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch path {
		case procCPUInfo:
			return []byte(cpuinfo), nil
		case procStat:
			return []byte(stat), nil
		}
		return nil, errors.New("unexpected path")
	}
}

func viewAdapter(t *testing.T, cpuinfo, stat string) *Adapter {
	t.Helper()
	return &Adapter{VectorViewDir: t.TempDir(), VectorViewSource: fakeProc(cpuinfo, stat)}
}

// TestTheContainerCountsItsOwnCPUsAndNotTheHosts is issue #291.
//
// Three of four Playwright shards timed out in cascade because Playwright sizes
// its worker pool from `os.cpus().length`, and inside a container charged two
// CPUs that counted the geekom's twenty-four host threads. libuv derives that
// count from the `cpuN` lines of `/proc/stat`, so narrowing that file is what
// turns twelve browser workers into one.
func TestTheContainerCountsItsOwnCPUsAndNotTheHosts(t *testing.T) {
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 2})

	if len(mounts) != 4 {
		t.Fatalf("both files are mounted or neither is: %v", mounts)
	}
	stat := readMountSource(t, mounts, procStat)
	if got := strings.Count(stat, "\ncpu0 ") + strings.Count(stat, "\ncpu1 "); got != 2 {
		t.Fatalf("the vector's own CPUs must survive: %q", stat)
	}
	if strings.Contains(stat, "cpu2 ") || strings.Contains(stat, "cpu3 ") {
		t.Fatalf("a CPU the container was not charged must not be counted: %q", stat)
	}
	cpuinfo := readMountSource(t, mounts, procCPUInfo)
	if strings.Count(cpuinfo, "processor") != 2 {
		t.Fatalf("cpuinfo must agree with stat: %q", cpuinfo)
	}
}

// Everything in `/proc/stat` that is not a processor is something else's fact —
// boot time, context switches, the process count — and dropping any of it would
// break a reader for no gain.
func TestNarrowingStatKeepsEveryRowThatIsNotAProcessor(t *testing.T) {
	narrowed, ok := narrowStat([]byte(hostStat), 2)
	if !ok {
		t.Fatal("narrowStat refused a host with enough CPUs")
	}
	for _, want := range []string{"cpu  100 0 200", "intr 12345", "ctxt 987654",
		"btime 1756000000", "processes 4242", "procs_running 2", "procs_blocked 0"} {
		if !strings.Contains(string(narrowed), want) {
			t.Fatalf("narrowed stat dropped %q: %s", want, narrowed)
		}
	}
}

// The bare aggregate `cpu ` line is a total, not a processor. Reading it as
// `cpuN` would drop it and break every reader of the file.
func TestTheAggregateCPULineIsNotAProcessor(t *testing.T) {
	if _, isCPU := statCPUIndex("cpu  100 0 200 300"); isCPU {
		t.Fatal("the aggregate line must never be treated as a processor")
	}
	index, isCPU := statCPUIndex("cpu7 1 2 3")
	if !isCPU || index != 7 {
		t.Fatalf("statCPUIndex(cpu7) = %d, %v", index, isCPU)
	}
	if _, isCPU := statCPUIndex("ctxt 987654"); isCPU {
		t.Fatal("a non-CPU row must not be read as one")
	}
}

// A host with fewer CPUs than the vector asks for is refused rather than padded.
// Inventing a core is a lie a tool would act on.
func TestAHostTooSmallForTheVectorNarrowsNothing(t *testing.T) {
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	if mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-xl-a", CPU: 8}); mounts != nil {
		t.Fatalf("a host of four CPUs cannot show eight: %v", mounts)
	}
}

// Both files or neither. A container told two CPUs by one file and four by the
// other is a worse place to debug from than one told four twice.
func TestOneUnreadableFileNarrowsNeither(t *testing.T) {
	adapter := &Adapter{VectorViewDir: t.TempDir(), VectorViewSource: func(path string) ([]byte, error) {
		if path == procStat {
			return nil, errors.New("unreadable")
		}
		return []byte(hostCPUInfo), nil
	}}

	if mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 2}); mounts != nil {
		t.Fatalf("a half-narrowed view must not be mounted: %v", mounts)
	}
}

// Every failure is silent and narrows nothing. A container that sees the host is
// a performance defect the fleet lived with for its whole life; a container that
// will not start is a failed job.
func TestNarrowingIsSkippedRatherThanFailed(t *testing.T) {
	// A regular file where the directory has to go: the node's state directory
	// is operator-managed, so a path that cannot hold a directory is reachable
	// and must not take a job down with it.
	blocked := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	for name, adapter := range map[string]*Adapter{
		"no directory configured": {VectorViewSource: fakeProc(hostCPUInfo, hostStat)},
		"unreadable host proc": {VectorViewDir: t.TempDir(),
			VectorViewSource: func(string) ([]byte, error) { return nil, errors.New("denied") }},
		"undirectorable path": {VectorViewDir: blocked,
			VectorViewSource: fakeProc(hostCPUInfo, hostStat)},
	} {
		if mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 2}); mounts != nil {
			t.Fatalf("%s: narrowing must be skipped, got %v", name, mounts)
		}
	}
}

// A vector of zero or less is not a vector. It reaches here only from a spec no
// scheduler produces, and it must not divide the host by nothing.
func TestAnAbsentVectorNarrowsNothing(t *testing.T) {
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	if mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 0}); mounts != nil {
		t.Fatalf("no vector, no narrowing: %v", mounts)
	}
}

// The pair is keyed by width alone, because its content depends on nothing else.
// Two containers of one profile share a pair and a restarted controller rewrites
// what it already wrote, so nothing here is ever cleaned up per instance.
func TestTwoContainersOfOneWidthShareOnePair(t *testing.T) {
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	first := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 2})
	second := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-b", CPU: 2})

	if len(first) == 0 || strings.Join(first, " ") != strings.Join(second, " ") {
		t.Fatalf("one width is one pair: %v vs %v", first, second)
	}
	wider := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-large-a", CPU: 4})
	if strings.Join(wider, " ") == strings.Join(first, " ") {
		t.Fatal("a different width must not share the narrower view")
	}
}

// The created container is told the narrowed view, read-only. A writable mount
// over /proc would let a job rewrite what the next one is shown.
func TestCreateArgsMountTheNarrowedViewReadOnly(t *testing.T) {
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	args := strings.Join(adapter.createArgs(executor.InstanceSpec{Name: "trf-small-a", CPU: 2, MemoryMB: 4096}), " ")

	for _, want := range []string{":" + procCPUInfo + ":ro", ":" + procStat + ":ro"} {
		if !strings.Contains(args, want) {
			t.Fatalf("create args omit %q: %s", want, args)
		}
	}
}

// A node that never configured a directory produces exactly the argument vector
// it produced before this existed.
func TestCreateArgsAreUnchangedWithoutADirectory(t *testing.T) {
	spec := executor.InstanceSpec{Name: "trf-small-a", CPU: 2, MemoryMB: 4096}

	args := strings.Join((&Adapter{}).createArgs(spec), " ")

	if strings.Contains(args, procStat) || strings.Contains(args, procCPUInfo) {
		t.Fatalf("an unconfigured node must mount nothing: %s", args)
	}
}

func readMountSource(t *testing.T, mounts []string, target string) string {
	t.Helper()
	for _, mount := range mounts {
		source, rest, found := strings.Cut(mount, ":")
		if !found || !strings.HasPrefix(rest, target+":") {
			continue
		}
		content, err := os.ReadFile(source) // #nosec G304 -- a path this test just wrote
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		return string(content)
	}
	t.Fatalf("no mount for %s in %v", target, mounts)
	return ""
}

// The default source is the host's real `/proc`. A width of one is asked for
// because every machine that can run this test has at least one CPU.
func TestTheDefaultSourceIsTheHostsOwnProc(t *testing.T) {
	adapter := &Adapter{VectorViewDir: t.TempDir()}

	mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 1})

	if len(mounts) != 4 {
		t.Fatalf("the host's own /proc must be readable without injection: %v", mounts)
	}
	if got := strings.Count(readMountSource(t, mounts, procCPUInfo), "processor"); got == 0 {
		t.Fatal("the narrowed cpuinfo carries no processor block")
	}
}

// A CPU index that is not a number, or not a whole one, is not a processor row.
func TestAMalformedCPURowIsNotAProcessor(t *testing.T) {
	for _, line := range []string{"cpu-1 1 2 3", "cpuX 1 2 3", "cpufreq 1 2"} {
		if _, isCPU := statCPUIndex(line); isCPU {
			t.Fatalf("%q must not be read as a processor row", line)
		}
	}
}

// Both halves of the atomic write report failure rather than leaving a partial
// file where a container would mount it.
func TestAtomicWriteReportsEveryFailure(t *testing.T) {
	if writeFileAtomic(filepath.Join(t.TempDir(), "absent", "cpuinfo"), []byte("x")) {
		t.Fatal("a write into a directory that does not exist must fail")
	}
	occupied := filepath.Join(t.TempDir(), "cpuinfo")
	if err := os.Mkdir(occupied, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if writeFileAtomic(occupied, []byte("x")) {
		t.Fatal("a rename onto a directory must fail")
	}
	if _, err := os.Stat(occupied + ".staging"); !os.IsNotExist(err) {
		t.Fatal("a failed rename must not leave its staging file behind")
	}
}

// A directory that cannot be written narrows nothing, and says so by mounting
// nothing rather than by failing the container.
func TestAnUnwritableTargetNarrowsNothing(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "2", "cpuinfo"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	adapter := &Adapter{VectorViewDir: directory, VectorViewSource: fakeProc(hostCPUInfo, hostStat)}

	if mounts := adapter.vectorViewMounts(executor.InstanceSpec{Name: "trf-small-a", CPU: 2}); mounts != nil {
		t.Fatalf("an unwritable target must narrow nothing: %v", mounts)
	}
}

// The authority runs under UMask=0077, which filters WriteFile's 0644 down to
// 0600 on disk. The container's mapped user then reads the bind-mounted file as
// "other" and gets EACCES, libuv cannot open /proc/stat, and os.cpus() inside
// the guest reports zero. The write must state its mode in a way the umask
// cannot filter.
func TestTheViewIsReadableByTheContainersMappedUser(t *testing.T) {
	oldMask := syscall.Umask(0o077)
	defer syscall.Umask(oldMask)
	adapter := viewAdapter(t, hostCPUInfo, hostStat)

	cpuinfo, stat, ok := adapter.writeVectorView(2)
	if !ok {
		t.Fatal("writeVectorView failed")
	}
	for _, path := range []string{cpuinfo, stat} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Fatalf("%s must be world-readable for the mapped container user, got %04o", path, mode)
		}
	}
}
