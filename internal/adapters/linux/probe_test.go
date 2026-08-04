package linux

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// A machine with 32 GiB, 24 logical processors, 2 GiB of swap of which 512 MiB
// is in use, and a terabyte-class NVMe root: the GEEKOM node of ADR 0034.
const (
	memInfoBody = `MemTotal:       32874120 kB
MemFree:         1284932 kB
MemAvailable:   28471296 kB
Buffers:          229384 kB
Cached:         24193472 kB
SwapCached:         1024 kB
SwapTotal:       2097152 kB
SwapFree:        1572864 kB
`
	vmStatBody = `nr_free_pages 321233
pgpgin 918273
pswpin 12
pswpout 4096
pgfault 88123123
`
	statBody = `cpu  100 20 50 700 30 0 0 0 0 0
cpu0 50 10 25 350 15 0 0 0 0 0
cpu1 50 10 25 350 15 0 0 0 0 0
intr 12345
ctxt 998877
`
	loadAverageBody = `1.75 2.10 2.35 3/1024 88123
`
	dfBody = `Filesystem     1024-blocks      Used  Available Capacity Mounted on
/dev/nvme0n1p2   982940334 120000000  810000000      13% /
`
)

func fixture() fstest.MapFS {
	return fstest.MapFS{
		memInfoPath:     {Data: []byte(memInfoBody)},
		vmStatPath:      {Data: []byte(vmStatBody)},
		statPath:        {Data: []byte(statBody)},
		loadAveragePath: {Data: []byte(loadAverageBody)},
	}
}

type diskStub struct {
	body string
	err  error
	args []string
}

func (d *diskStub) Run(_ context.Context, args ...string) ([]byte, error) {
	d.args = args
	if d.err != nil {
		return nil, d.err
	}
	return []byte(d.body), nil
}

func fixedClock(times ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		value := times[min(index, len(times)-1)]
		index++
		return value
	}
}

func newProbe(files fstest.MapFS, disk executor.CommandRunner, now func() time.Time) *Probe {
	return &Probe{FS: files, Disk: disk, Now: now}
}

// TestFirstObservationReportsTheWholeMachine is the acceptance criterion of
// Part A of docs/MULTI_NODE_PLAN.md: the host probe must report plausible CPU,
// memory, disk, load, and swap from /proc.
func TestFirstObservationReportsTheWholeMachine(t *testing.T) {
	start := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	disk := &diskStub{body: dfBody}
	snapshot := newProbe(fixture(), disk, fixedClock(start)).Snapshot(context.Background())

	if snapshot.Freshness != executor.Fresh || !snapshot.ObservedAt.Equal(start) {
		t.Fatalf("freshness=%q observedAt=%v", snapshot.Freshness, snapshot.ObservedAt)
	}
	if snapshot.AvailableMemoryMB != 27804 || snapshot.PhysicalMemoryMB != 32103 {
		t.Errorf("memory available=%d physical=%d", snapshot.AvailableMemoryMB, snapshot.PhysicalMemoryMB)
	}
	if snapshot.SwapUsedMB != 512 || snapshot.SwapOuts != 4096 {
		t.Errorf("swap used=%d pageOuts=%d", snapshot.SwapUsedMB, snapshot.SwapOuts)
	}
	if snapshot.FreeDiskGB != 772 {
		t.Errorf("free disk = %d GB", snapshot.FreeDiskGB)
	}
	if snapshot.LoadAverage != 1.75 {
		t.Errorf("load = %v", snapshot.LoadAverage)
	}
	if snapshot.PhysicalCPU != 2 {
		t.Errorf("processors = %d", snapshot.PhysicalCPU)
	}
	if want := []string{"-P", "-k", "/"}; len(disk.args) != 3 || disk.args[0] != want[0] || disk.args[2] != want[2] {
		t.Errorf("df arguments = %v, want %v", disk.args, want)
	}
}

// TestFirstObservationCannotDenyAdmissionOnUnmeasurableRates pins the two
// signals that need a prior sample. An idle percentage the probe has not been
// running long enough to measure must be permissive, and an unmeasured page-out
// rate must be reported as unmeasured so the guardrail fails closed on the level
// rather than reading zero as a quiet host.
func TestFirstObservationCannotDenyAdmissionOnUnmeasurableRates(t *testing.T) {
	snapshot := newProbe(fixture(), &diskStub{body: dfBody}, nil).Snapshot(context.Background())
	if snapshot.CPUidlePercent != permissiveIdle {
		t.Errorf("first idle = %v, want the permissive default", snapshot.CPUidlePercent)
	}
	if snapshot.SwapOutRateObserved || snapshot.SwapOutRatePerSecond != 0 {
		t.Errorf("first page-out rate = %v observed=%v", snapshot.SwapOutRatePerSecond, snapshot.SwapOutRateObserved)
	}
}

// TestSecondObservationMeasuresRates proves both rates come from the interval
// between two samples, not from a single reading.
func TestSecondObservationMeasuresRates(t *testing.T) {
	start := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	files := fixture()
	probe := newProbe(files, &diskStub{body: dfBody}, fixedClock(start, start.Add(10*time.Second)))
	probe.Snapshot(context.Background())

	files[vmStatPath] = &fstest.MapFile{Data: []byte("pswpout 4196\n")}
	files[statPath] = &fstest.MapFile{Data: []byte("cpu  200 20 50 1400 30 0 0 0 0 0\ncpu0 1 1 1 1 1\n")}
	snapshot := probe.Snapshot(context.Background())

	if !snapshot.SwapOutRateObserved || snapshot.SwapOutRatePerSecond != 10 {
		t.Errorf("page-out rate = %v observed=%v", snapshot.SwapOutRatePerSecond, snapshot.SwapOutRateObserved)
	}
	// 700 idle jiffies of the 800 that elapsed.
	if snapshot.CPUidlePercent != 87.5 {
		t.Errorf("idle = %v", snapshot.CPUidlePercent)
	}
}

// TestRatesAreNeverInventedAcrossAReboot covers a counter that went backwards
// and a clock that did not advance. A negative delta must never be published as
// a low rate.
func TestRatesAreNeverInventedAcrossAReboot(t *testing.T) {
	start := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	files := fixture()
	probe := newProbe(files, &diskStub{body: dfBody}, fixedClock(start, start.Add(time.Second), start.Add(time.Second)))
	probe.Snapshot(context.Background())

	files[vmStatPath] = &fstest.MapFile{Data: []byte("pswpout 1\n")}
	files[statPath] = &fstest.MapFile{Data: []byte("cpu  1 0 0 1 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0\n")}
	rebooted := probe.Snapshot(context.Background())
	if rebooted.SwapOutRateObserved {
		t.Error("a page-out counter that went backwards must not yield a rate")
	}
	if rebooted.CPUidlePercent != permissiveIdle {
		t.Errorf("idle across a reboot = %v, want the permissive default", rebooted.CPUidlePercent)
	}

	stopped := probe.Snapshot(context.Background())
	if stopped.SwapOutRateObserved {
		t.Error("a clock that did not advance must not yield a rate")
	}
}

// TestUnreadableCapacityFactsDegradeTheObservation walks every hard signal. The
// first failure has no prior reading and must be unavailable; a later one must
// republish the last good reading as stale. Neither may report a machine with no
// memory, no disk, and no swap.
func TestUnreadableCapacityFactsDegradeTheObservation(t *testing.T) {
	broken := map[string]func(fstest.MapFS, *diskStub){
		"meminfo": func(files fstest.MapFS, _ *diskStub) { delete(files, memInfoPath) },
		"vmstat":  func(files fstest.MapFS, _ *diskStub) { delete(files, vmStatPath) },
		"df":      func(_ fstest.MapFS, disk *diskStub) { disk.err = errors.New("df: no such mount") },
	}
	for name, breakIt := range broken {
		t.Run("unavailable without a prior reading/"+name, func(t *testing.T) {
			files, disk := fixture(), &diskStub{body: dfBody}
			breakIt(files, disk)
			snapshot := newProbe(files, disk, nil).Snapshot(context.Background())
			if snapshot.Freshness != executor.Unavailable || snapshot.Cause == nil {
				t.Fatalf("freshness=%q cause=%v", snapshot.Freshness, snapshot.Cause)
			}
			if snapshot.AvailableMemoryMB != 0 || snapshot.PhysicalCPU != 0 {
				t.Fatalf("an unavailable observation carried measurements: %#v", snapshot)
			}
		})
		t.Run("stale against the last good reading/"+name, func(t *testing.T) {
			files, disk := fixture(), &diskStub{body: dfBody}
			probe := newProbe(files, disk, nil)
			good := probe.Snapshot(context.Background())
			breakIt(files, disk)
			snapshot := probe.Snapshot(context.Background())
			if snapshot.Freshness != executor.Stale || snapshot.Cause == nil {
				t.Fatalf("freshness=%q cause=%v", snapshot.Freshness, snapshot.Cause)
			}
			if snapshot.AvailableMemoryMB != good.AvailableMemoryMB || snapshot.FreeDiskGB != good.FreeDiskGB {
				t.Fatalf("stale reading is not the last good one: %#v", snapshot)
			}
		})
	}
}

// TestAdvisorySignalsNeverFailTheObservation is the other half of the split:
// load and idle CPU are throttles, so an unreadable one degrades to the last
// good reading and then to a permissive default, and the host stays admissible.
func TestAdvisorySignalsNeverFailTheObservation(t *testing.T) {
	files := fixture()
	delete(files, loadAveragePath)
	delete(files, statPath)
	first := newProbe(files, &diskStub{body: dfBody}, nil).Snapshot(context.Background())
	if first.Freshness != executor.Fresh {
		t.Fatalf("an unreadable throttle failed the whole observation: %q", first.Freshness)
	}
	if first.LoadAverage != permissiveLoad || first.CPUidlePercent != permissiveIdle || first.PhysicalCPU != 0 {
		t.Fatalf("permissive defaults not applied: %#v", first)
	}

	probe := newProbe(fixture(), &diskStub{body: dfBody}, nil)
	good := probe.Snapshot(context.Background())
	probe.FS = files
	degraded := probe.Snapshot(context.Background())
	if degraded.LoadAverage != good.LoadAverage || degraded.CPUidlePercent != good.CPUidlePercent ||
		degraded.PhysicalCPU != good.PhysicalCPU {
		t.Fatalf("advisory fallback did not reuse the last good reading: %#v", degraded)
	}
}

// TestMalformedAdvisoryContentFallsBackLikeAnUnreadableFile covers the parse
// arm rather than the read arm of both throttles.
func TestMalformedAdvisoryContentFallsBackLikeAnUnreadableFile(t *testing.T) {
	files := fixture()
	files[loadAveragePath] = &fstest.MapFile{Data: []byte("-1.5 1 2\n")}
	files[statPath] = &fstest.MapFile{Data: []byte("intr 1\n")}
	snapshot := newProbe(files, &diskStub{body: dfBody}, nil).Snapshot(context.Background())
	if snapshot.Freshness != executor.Fresh {
		t.Fatalf("freshness = %q", snapshot.Freshness)
	}
	if snapshot.LoadAverage != permissiveLoad || snapshot.CPUidlePercent != permissiveIdle {
		t.Fatalf("malformed throttles were not treated as unmeasured: %#v", snapshot)
	}
}

func TestParseMemInfoRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"missing MemAvailable":    "MemTotal: 100 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"non-numeric field":       "MemTotal: many kB\nMemAvailable: 1 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"negative field":          "MemTotal: -1 kB\nMemAvailable: 1 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"free swap exceeds total": "MemTotal: 100 kB\nMemAvailable: 1 kB\nSwapTotal: 0 kB\nSwapFree: 8 kB\n",
	} {
		if _, err := parseMemInfo(body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestParsePageOutsRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"missing counter": "pgpgin 1\n",
		"non-numeric":     "pswpout many\n",
		"negative":        "pswpout -3\n",
	} {
		if _, err := parsePageOuts(body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestParseProcessorTimeRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"no aggregate line": "cpu0 1 2 3 4 5 6\n",
		"truncated":         "cpu 1 2 3\n",
		"non-counter":       "cpu 1 2 3 four 5 6\n",
		"negative counter":  "cpu 1 2 3 -4 5 6\n",
	} {
		if _, err := parseProcessorTime(body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestIdlePercentRejectsImpossibleIntervals proves the derived rate refuses an
// interval in which idle time exceeds total time, which no honest pair of
// samples produces.
func TestIdlePercentRejectsImpossibleIntervals(t *testing.T) {
	current := processorTime{idle: 100, total: 50}
	if _, ok := current.idlePercentSince(processorTime{}); ok {
		t.Fatal("idle time above total time was accepted")
	}
}

func TestParseLoadAverageRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"empty":       "\n  \n",
		"non-numeric": "idle 1 2\n",
		"negative":    "-0.5 1 2\n",
	} {
		if _, err := parseLoadAverage(body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestParseFreeDiskRejectsUnusableContent(t *testing.T) {
	for name, body := range map[string]string{
		"header only":         "Filesystem 1024-blocks Used Available Capacity Mounted on\n",
		"no available column": "Filesystem Blocks Used Free Capacity Mounted on\n/dev/sda1 1 2 3 4% /\n",
		"short data row":      "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1 2\n",
		"non-numeric":         "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1 2 lots 4% /\n",
		"negative":            "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1 2 -9 4% /\n",
	} {
		if _, err := parseFreeDiskGB(body); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestAvailableColumnIsFoundByNameNotPosition is why the header is parsed at
// all: a `df` that prints an extra column before the free-space one would
// otherwise have its used space reported as its free space.
func TestAvailableColumnIsFoundByNameNotPosition(t *testing.T) {
	body := "Filesystem Type 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 ext4 100 1048576 2097152 1% /\n"
	free, err := parseFreeDiskGB(body)
	if err != nil || free != 2 {
		t.Fatalf("free = %d GB, %v", free, err)
	}
}

// TestDiskProbeTimeoutIsReportedAsATimeout keeps a wedged `df` distinguishable
// from a `df` that answered with an error, so an operator reading the cause can
// tell an overloaded machine from a misconfigured mount point.
func TestDiskProbeTimeoutIsReportedAsATimeout(t *testing.T) {
	probe := &Probe{FS: fixture(), Timeout: time.Millisecond, DiskPath: "/var",
		Disk: &slowDisk{delay: 40 * time.Millisecond}}
	snapshot := probe.Snapshot(context.Background())
	if snapshot.Freshness != executor.Unavailable || snapshot.Cause == nil {
		t.Fatalf("freshness=%q cause=%v", snapshot.Freshness, snapshot.Cause)
	}
	if !errors.Is(snapshot.Cause, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want a deadline", snapshot.Cause)
	}
}

type slowDisk struct {
	delay time.Duration
	args  []string
}

func (d *slowDisk) Run(ctx context.Context, args ...string) ([]byte, error) {
	d.args = args
	select {
	case <-ctx.Done():
	case <-time.After(d.delay):
	}
	return nil, errors.New("signal: killed")
}

// TestTheRealDiskRunnerIsUsedWhenNoneIsInjected covers the production wiring of
// the one command this probe shells out to.
func TestTheRealDiskRunnerIsUsedWhenNoneIsInjected(t *testing.T) {
	snapshot := (&Probe{FS: fixture()}).Snapshot(context.Background())
	if snapshot.Freshness != executor.Fresh || snapshot.FreeDiskGB < 0 {
		t.Fatalf("real df did not answer: freshness=%q cause=%v", snapshot.Freshness, snapshot.Cause)
	}
}

// TestDefaultsReadTheRealMachine is the Linux smoke test: the zero-value probe
// with no injected filesystem and no injected runner must read the real /proc
// and the real df and report a plausible machine. It is the evidence behind
// Part A's "verify the host probe reports plausible CPU, memory, disk, load and
// swap from /proc", and it runs on every Linux CI job.
//
// Off Linux there is no /proc, and the assertion is the one AGENTS.md §4 cares
// about: the probe reports an unavailable observation rather than a machine with
// no memory and no processors.
func TestDefaultsReadTheRealMachine(t *testing.T) {
	probe := &Probe{}
	snapshot := probe.Snapshot(context.Background())
	if runtime.GOOS != "linux" {
		if snapshot.Freshness != executor.Unavailable || snapshot.Cause == nil {
			t.Fatalf("%s reported freshness=%q instead of an unavailable observation", runtime.GOOS, snapshot.Freshness)
		}
		return
	}
	if snapshot.Freshness != executor.Fresh {
		t.Fatalf("freshness=%q cause=%v", snapshot.Freshness, snapshot.Cause)
	}
	if snapshot.AvailableMemoryMB <= 0 || snapshot.PhysicalMemoryMB <= 0 {
		t.Errorf("implausible memory: available=%d physical=%d", snapshot.AvailableMemoryMB, snapshot.PhysicalMemoryMB)
	}
	if snapshot.PhysicalCPU <= 0 || int(snapshot.PhysicalCPU) < runtime.NumCPU() {
		t.Errorf("processors = %d, runtime reports %d", snapshot.PhysicalCPU, runtime.NumCPU())
	}
	if snapshot.FreeDiskGB < 0 || snapshot.SwapUsedMB < 0 || snapshot.LoadAverage < 0 {
		t.Errorf("implausible machine: %#v", snapshot)
	}
	second := probe.Snapshot(context.Background())
	if second.CPUidlePercent < 0 || second.CPUidlePercent > 100 {
		t.Errorf("measured idle = %v%%", second.CPUidlePercent)
	}
}

// TestDiskRunnerExecutesTheRealBinary covers the production runner without
// depending on `df` being installed: the binary is injectable, so a fixed
// harmless one proves the argument vector reaches a real process.
func TestDiskRunnerExecutesTheRealBinary(t *testing.T) {
	output, err := DiskRunner{Binary: "/bin/echo"}.Run(context.Background(), "-P", "-k", "/")
	if err != nil || string(output) != "-P -k /\n" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if _, err := (DiskRunner{}).Run(context.Background(), "--tart-runner-fleet-not-a-flag"); err == nil {
		t.Fatal("the default binary accepted an invalid argument")
	}
}
