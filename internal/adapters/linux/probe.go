// Package linux measures the machine a Linux node runs on.
//
// It is the `executor.HostProbe` twin of internal/adapters/macos, and it exists
// because ADR 0034's second node has no `vm_stat`, no `sysctl -n hw.memsize`
// and no `top -l 1`. What it does have is `/proc`, which reports the same six
// admission facts the scheduler has always decided on — available memory, free
// disk, swap pressure, load, idle CPU, and the machine's physical totals — as
// files rather than as command output.
//
// The split between hard and advisory signals is the macOS probe's, deliberately:
//
//   - `/proc/meminfo`, `/proc/vmstat` and free disk are capacity facts. Without
//     them admission cannot be judged, so their absence degrades the whole
//     observation to stale (with a prior reading) or unavailable (without one).
//   - Load and idle CPU are soft throttles. A single unreadable file must never
//     fail-close the fleet, so each falls back to its last good reading and then
//     to a permissive default.
//
// Free disk is the one measurement `/proc` does not carry: the kernel exposes
// per-filesystem free space only through `statfs(2)`, whose struct differs by
// platform. `df -P -k` is asked for it instead, through the same injected
// argument-vector runner every other adapter uses, so this package needs no
// build tags and no per-platform syscall shim, and the whole probe stays
// testable on any machine.
package linux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// The procfs files this probe reads, relative to the root of the injected
// filesystem so that os.DirFS("/") and a test fixture address them alike.
const (
	memInfoPath      = "proc/meminfo"
	vmStatPath       = "proc/vmstat"
	statPath         = "proc/stat"
	loadAveragePath  = "proc/loadavg"
	kibibytesPerMB   = 1024
	kibibytesPerGB   = 1024 * 1024
	permissiveLoad   = 0
	permissiveIdle   = 100
	defaultDiskPath  = "/"
	defaultProbeWait = 3 * time.Second
)

// DiskRunner is `df`, bound to its binary. It is an executor.CommandRunner
// because the probe shells out to exactly one program, which is what that port
// is: one argument vector of one command line, never a shell string.
type DiskRunner struct {
	Binary string
}

func (r DiskRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	binary := r.Binary
	if binary == "" {
		binary = "df"
	}
	// #nosec G204 -- the binary is fixed configuration and the arguments are an
	// argument vector, never shell text.
	command := exec.CommandContext(ctx, binary, args...)
	// parseFreeDiskGB locates the free-space column by its header name, and `df`
	// translates that header. A node whose operator's locale is not English would
	// otherwise report an unreadable disk, which fails the whole host observation
	// closed and leaves the daemon permanently unready — a machine-specific
	// failure that no amount of testing on an English host would ever surface.
	//
	// All three variables are needed and all three are appended, because
	// exec.Cmd keeps the last occurrence of a duplicated key: LC_ALL outranks
	// LANG in POSIX, and GNU gettext's LANGUAGE outranks both, so clearing it is
	// what actually stops the translation.
	command.Env = append(os.Environ(), "LANG=C", "LC_ALL=C", "LANGUAGE=")
	return command.CombinedOutput()
}

// Probe reads one Linux machine. The zero value reads the real `/proc` and the
// real `df`, so production wiring supplies only the timeout it wants.
type Probe struct {
	// FS is the filesystem `/proc` is read from. Nil means the real root.
	FS fs.FS
	// Disk answers `df -P -k <path>`. Nil means the real `df`.
	Disk executor.CommandRunner
	// Timeout bounds the one command this probe runs.
	Timeout time.Duration
	// DiskPath is the mount point whose free space guards admission.
	DiskPath string
	Now      func() time.Time

	mu sync.Mutex
	// last is the previous fresh snapshot, which a degraded read is reported
	// against and which the advisory signals fall back to.
	last executor.HostSnapshot
	// lastProcessor is the previous raw CPU time accounting. Idle percentage is
	// a rate, so it exists only between two samples and cannot live in the
	// snapshot, which carries the derived figure.
	lastProcessor processorTime
}

func (p *Probe) Snapshot(ctx context.Context) executor.HostSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()

	memory, err := p.memory()
	if err != nil {
		return p.degraded(now, err)
	}
	pageOuts, err := p.pageOuts()
	if err != nil {
		return p.degraded(now, err)
	}
	freeDiskGB, err := p.freeDiskGB(ctx)
	if err != nil {
		return p.degraded(now, err)
	}

	hasPrior := !p.last.ObservedAt.IsZero()
	rate, rateObserved := p.pageOutRate(now, pageOuts, hasPrior)
	idle, processors := p.processorPressure(hasPrior)
	snapshot := executor.HostSnapshot{
		Freshness:            executor.Fresh,
		ObservedAt:           now,
		AvailableMemoryMB:    memory.availableMB,
		FreeDiskGB:           freeDiskGB,
		SwapUsedMB:           memory.swapUsedMB,
		SwapOuts:             pageOuts,
		SwapOutRatePerSecond: rate,
		SwapOutRateObserved:  rateObserved,
		CPUidlePercent:       idle,
		LoadAverage:          p.loadAverage(hasPrior),
		PhysicalCPU:          processors,
		PhysicalMemoryMB:     memory.totalMB,
	}
	p.last = snapshot
	return snapshot
}

// degraded reports an unreadable capacity fact without ever inventing a
// measurement: the first failure is unavailable, a later one republishes the
// last good reading as stale, carrying the cause either way.
func (p *Probe) degraded(now time.Time, cause error) executor.HostSnapshot {
	if p.last.ObservedAt.IsZero() {
		return executor.HostSnapshot{Freshness: executor.Unavailable, ObservedAt: now, Cause: cause}
	}
	previous := p.last
	previous.Freshness = executor.Stale
	previous.Cause = cause
	return previous
}

// pageOutRate derives the page-out rate from consecutive observations, and
// reports ok=false whenever it cannot be established honestly: no prior sample,
// a non-advancing clock, or a counter that went backwards across a reboot. A
// negative delta must never be published as a low rate.
func (p *Probe) pageOutRate(now time.Time, pageOuts int64, hasPrior bool) (float64, bool) {
	if !hasPrior {
		return 0, false
	}
	elapsed := now.Sub(p.last.ObservedAt).Seconds()
	delta := pageOuts - p.last.SwapOuts
	if elapsed <= 0 || delta < 0 {
		return 0, false
	}
	return float64(delta) / elapsed, true
}

// processorPressure reports measured idle percentage and the machine's logical
// processor count. `/proc/stat` carries cumulative jiffies, so idle is a rate
// between two samples: the first observation after a start has no prior and
// takes the permissive default, which by itself can never deny admission.
func (p *Probe) processorPressure(hasPrior bool) (float64, int64) {
	current, err := p.processorTime()
	if err != nil {
		if hasPrior {
			return p.last.CPUidlePercent, p.last.PhysicalCPU
		}
		return permissiveIdle, 0
	}
	previous := p.lastProcessor
	p.lastProcessor = current
	idle, ok := current.idlePercentSince(previous)
	if ok {
		return idle, current.processors
	}
	if hasPrior {
		return p.last.CPUidlePercent, current.processors
	}
	return permissiveIdle, current.processors
}

// loadAverage reads the one-minute load, degrading to the last good reading and
// then to a permissive zero. It never fails the observation.
func (p *Probe) loadAverage(hasPrior bool) float64 {
	body, err := p.read(loadAveragePath)
	if err == nil {
		if load, parseErr := parseLoadAverage(string(body)); parseErr == nil {
			return load
		}
	}
	if hasPrior {
		return p.last.LoadAverage
	}
	return permissiveLoad
}

type memoryFacts struct {
	availableMB, totalMB, swapUsedMB int64
}

func (p *Probe) memory() (memoryFacts, error) {
	body, err := p.read(memInfoPath)
	if err != nil {
		return memoryFacts{}, fmt.Errorf("meminfo probe: %w", err)
	}
	return parseMemInfo(string(body))
}

func (p *Probe) pageOuts() (int64, error) {
	body, err := p.read(vmStatPath)
	if err != nil {
		return 0, fmt.Errorf("vmstat probe: %w", err)
	}
	return parsePageOuts(string(body))
}

func (p *Probe) processorTime() (processorTime, error) {
	body, err := p.read(statPath)
	if err != nil {
		return processorTime{}, err
	}
	return parseProcessorTime(string(body))
}

func (p *Probe) freeDiskGB(ctx context.Context) (int64, error) {
	deadline := p.Timeout
	if deadline <= 0 {
		deadline = defaultProbeWait
	}
	commandCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	output, err := p.disk().Run(commandCtx, "-P", "-k", p.diskPath())
	if err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return 0, fmt.Errorf("df timeout: %w", context.DeadlineExceeded)
		}
		return 0, fmt.Errorf("df probe: %w", err)
	}
	return parseFreeDiskGB(string(output))
}

func (p *Probe) read(name string) ([]byte, error) {
	source := p.FS
	if source == nil {
		source = os.DirFS("/")
	}
	return fs.ReadFile(source, name)
}

func (p *Probe) disk() executor.CommandRunner {
	if p.Disk == nil {
		return DiskRunner{}
	}
	return p.Disk
}

func (p *Probe) diskPath() string {
	if p.DiskPath == "" {
		return defaultDiskPath
	}
	return p.DiskPath
}

func (p *Probe) clock() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now().UTC()
}

// parseMemInfo reads the four capacity fields admission needs. Every value in
// `/proc/meminfo` is in kibibytes. MemAvailable is the kernel's own estimate of
// what a new workload can have without swapping, which is exactly the question
// the guardrail asks, and it is required rather than reconstructed from free
// pages: a probe that silently substituted MemFree would understate the machine
// by the whole page cache.
func parseMemInfo(body string) (memoryFacts, error) {
	fields := map[string]int64{}
	wanted := []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"}
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found || !slices.Contains(wanted, name) {
			continue
		}
		kibibytes, err := strconv.ParseInt(strings.Fields(value)[0], 10, 64)
		if err != nil || kibibytes < 0 {
			return memoryFacts{}, fmt.Errorf("meminfo field %s is not a size", name)
		}
		fields[name] = kibibytes
	}
	for _, name := range wanted {
		if _, present := fields[name]; !present {
			return memoryFacts{}, fmt.Errorf("meminfo field %s is missing", name)
		}
	}
	swapUsed := fields["SwapTotal"] - fields["SwapFree"]
	if swapUsed < 0 {
		return memoryFacts{}, errors.New("meminfo reports more free swap than total swap")
	}
	return memoryFacts{
		availableMB: fields["MemAvailable"] / kibibytesPerMB,
		totalMB:     fields["MemTotal"] / kibibytesPerMB,
		swapUsedMB:  swapUsed / kibibytesPerMB,
	}, nil
}

// parsePageOuts reads the cumulative count of pages written to swap. It is the
// Linux `pswpout` counter, the exact analogue of the `Swapouts` figure the macOS
// probe takes from `vm_stat`, and it is what separates a host paging right now
// from one carrying residue from an earlier burst.
func parsePageOuts(body string) (int64, error) {
	for _, line := range strings.Split(body, "\n") {
		name, value, found := strings.Cut(line, " ")
		if !found || name != "pswpout" {
			continue
		}
		pages, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || pages < 0 {
			return 0, errors.New("vmstat pswpout is not a counter")
		}
		return pages, nil
	}
	return 0, errors.New("vmstat pswpout is missing")
}

// processorTime is one sample of the machine's aggregate CPU time accounting.
type processorTime struct {
	idle, total int64
	processors  int64
}

// idlePercentSince converts two cumulative samples into the idle share of the
// interval between them. It reports ok=false when there is no interval to
// measure or the counters moved backwards, so a caller never reads an
// unmeasured stretch as a busy or an idle machine.
//
// The zero sample is "never measured", not "measured at boot". Differencing
// against it would publish the machine's whole since-boot average as its current
// pressure, which on a host that has been idle for a week reads as an idle host
// during the burst that is saturating it right now.
func (t processorTime) idlePercentSince(previous processorTime) (float64, bool) {
	if previous.total <= 0 {
		return 0, false
	}
	total := t.total - previous.total
	idle := t.idle - previous.idle
	if total <= 0 || idle < 0 || idle > total {
		return 0, false
	}
	return float64(idle) * 100 / float64(total), true
}

// parseProcessorTime reads the aggregate `cpu` line and counts the per-processor
// `cpuN` lines. Idle is idle plus iowait: a processor blocked on I/O is not
// executing anything, which is what the admission guardrail is asking about.
func parseProcessorTime(body string) (processorTime, error) {
	sample := processorTime{}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			sample.processors++
			continue
		}
		// user nice system idle iowait irq softirq steal ...
		if len(fields) < 6 {
			return processorTime{}, errors.New("stat cpu line is truncated")
		}
		for index, field := range fields[1:] {
			jiffies, err := strconv.ParseInt(field, 10, 64)
			if err != nil || jiffies < 0 {
				return processorTime{}, errors.New("stat cpu line carries a non-counter")
			}
			sample.total += jiffies
			if index == 3 || index == 4 {
				sample.idle += jiffies
			}
		}
		found = true
	}
	if !found {
		return processorTime{}, errors.New("stat aggregate cpu line is missing")
	}
	return sample, nil
}

// parseLoadAverage reads the one-minute figure, the same window the macOS probe
// takes from `vm.loadavg`.
func parseLoadAverage(body string) (float64, error) {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return 0, errors.New("loadavg is empty")
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || load < 0 {
		return 0, errors.New("loadavg one-minute figure is not a load")
	}
	return load, nil
}

// parseFreeDiskGB reads POSIX `df` output. The header names the columns and the
// last row carries the filesystem asked about, so the available column is
// located by name rather than by position: `df` implementations disagree about
// how many columns precede it, and reading the wrong one would report a mount's
// used space as its free space.
func parseFreeDiskGB(body string) (int64, error) {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return 0, errors.New("df output has no data row")
	}
	column := slices.Index(strings.Fields(lines[0]), "Available")
	if column < 0 {
		return 0, errors.New("df output has no available column")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if column >= len(fields) {
		return 0, errors.New("df data row has no available blocks")
	}
	blocks, err := strconv.ParseInt(fields[column], 10, 64)
	if err != nil || blocks < 0 {
		return 0, errors.New("df available blocks is not a size")
	}
	return blocks / kibibytesPerGB, nil
}
