package macos

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Freshness string

const (
	Fresh       Freshness = "fresh"
	Stale       Freshness = "stale"
	Unavailable Freshness = "unavailable"
)

type Snapshot struct {
	Freshness         Freshness
	ObservedAt        time.Time
	AvailableMemoryMB int64
	FreeDiskGB        int64
	SwapUsedMB        int64
	SwapOuts          int64
	CPUidlePercent    float64
	LoadAverage       float64
	Cause             error
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, binary string, args ...string) ([]byte, error) {
	// #nosec G204 -- the executable is injected by trusted controller configuration and tests.
	return exec.CommandContext(ctx, binary, args...).CombinedOutput()
}

type Probe struct {
	Runner   CommandRunner
	Timeout  time.Duration
	DiskPath string
	Now      func() time.Time
	mu       sync.Mutex
	last     Snapshot
}

// Permissive advisory defaults cannot by themselves deny admission: full CPU
// idle, zero load, and zero swap all pass the guardrail pressure checks. They
// apply only when an advisory probe fails and no prior reading exists.
const (
	permissiveCPUidlePercent = 100
	permissiveLoadAverage    = 0
	permissiveSwapUsedMB     = 0
)

func (p *Probe) Snapshot(ctx context.Context) Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()()
	hasPrior := !p.last.ObservedAt.IsZero()

	// Memory and disk are hard capacity signals. Without them admission cannot
	// be judged safely, so their absence degrades the whole observation.
	vmStat, err := p.run(ctx, "vm_stat")
	if err != nil {
		return p.degraded(now, err)
	}
	availableMemoryMB, swapouts, err := parseVMStat(string(vmStat))
	if err != nil {
		return p.degraded(now, err)
	}
	disk, err := p.run(ctx, "df", "-k", p.diskPath())
	if err != nil {
		return p.degraded(now, err)
	}
	freeDiskGB, err := parseDisk(string(disk))
	if err != nil {
		return p.degraded(now, err)
	}

	// Swap, CPU idle, and load are advisory throttles. A single flaky heavy
	// probe (notably `top`) must never fail the whole host observation and
	// fail-close admission fleet-wide, so each falls back to its last good
	// reading and then to a permissive default. This is the difference between
	// throttling on unmeasurable soft pressure and bricking the scheduler.
	snapshot := Snapshot{
		Freshness:         Fresh,
		ObservedAt:        now,
		AvailableMemoryMB: availableMemoryMB,
		FreeDiskGB:        freeDiskGB,
		SwapOuts:          swapouts,
		SwapUsedMB:        int64(p.advisory(ctx, hasPrior, float64(p.last.SwapUsedMB), permissiveSwapUsedMB, parseSwapFloat, "sysctl", "-n", "vm.swapusage")),
		CPUidlePercent:    p.advisory(ctx, hasPrior, p.last.CPUidlePercent, permissiveCPUidlePercent, parseCPU, "top", "-l", "1", "-n", "0"),
		LoadAverage:       p.advisory(ctx, hasPrior, p.last.LoadAverage, permissiveLoadAverage, parseLoad, "sysctl", "-n", "vm.loadavg"),
	}
	p.last = snapshot
	return snapshot
}

// advisory reads one soft-pressure metric, degrading gracefully: a probe or
// parse failure yields the last good reading when one exists, otherwise the
// permissive default. It never fails the observation.
func (p *Probe) advisory(ctx context.Context, hasPrior bool, lastKnown, permissive float64, parse func(string) (float64, error), binary string, args ...string) float64 {
	if output, err := p.run(ctx, binary, args...); err == nil {
		if value, parseErr := parse(string(output)); parseErr == nil {
			return value
		}
	}
	if hasPrior {
		return lastKnown
	}
	return permissive
}

func parseSwapFloat(value string) (float64, error) {
	swapUsedMB, err := parseSwap(value)
	return float64(swapUsedMB), err
}

func (p *Probe) degraded(now time.Time, cause error) Snapshot {
	if p.last.ObservedAt.IsZero() {
		return Snapshot{Freshness: Unavailable, ObservedAt: now, Cause: cause}
	}
	stale := p.last
	stale.Freshness = Stale
	stale.Cause = cause
	return stale
}

func (p *Probe) run(ctx context.Context, binary string, args ...string) ([]byte, error) {
	deadline := p.Timeout
	if deadline <= 0 {
		deadline = 3 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	output, err := p.runner().Run(commandCtx, binary, args...)
	if err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timeout: %w", binary, context.DeadlineExceeded)
		}
		return nil, fmt.Errorf("%s probe: %w", binary, err)
	}
	return output, nil
}

func parseVMStat(value string) (int64, int64, error) {
	pageSize := int64(0)
	pages := map[string]int64{}
	for _, line := range strings.Split(value, "\n") {
		if strings.Contains(line, "page size of") {
			fields := strings.Fields(line)
			for i, field := range fields {
				if field == "of" && i+1 < len(fields) {
					pageSize, _ = strconv.ParseInt(strings.Trim(fields[i+1], "."), 10, 64)
				}
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		parsed, _ := strconv.ParseInt(strings.Trim(strings.TrimSpace(parts[1]), "."), 10, 64)
		pages[strings.TrimSpace(parts[0])] = parsed
	}
	if pageSize <= 0 {
		return 0, 0, errors.New("vm_stat page size missing")
	}
	availablePages := pages["Pages free"] + pages["Pages speculative"] + pages["Pages inactive"]
	return availablePages * pageSize / 1048576, pages["Swapouts"], nil
}

func parseDisk(value string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) < 2 {
		return 0, errors.New("df output missing data")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, errors.New("df available blocks missing")
	}
	blocks, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df: %w", err)
	}
	return blocks / 1048576, nil
}

var swapUsedPattern = regexp.MustCompile(`used\s*=\s*([0-9.]+)([KMG])`)

func parseSwap(value string) (int64, error) {
	match := swapUsedPattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return 0, errors.New("swap usage missing")
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("parse swap: %w", err)
	}
	switch match[2] {
	case "K":
		number /= 1024
	case "G":
		number *= 1024
	}
	return int64(number), nil
}

var cpuIdlePattern = regexp.MustCompile(`([0-9.]+)%\s*idle`)

func parseCPU(value string) (float64, error) {
	match := cpuIdlePattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return 0, errors.New("CPU idle missing")
	}
	return strconv.ParseFloat(match[1], 64)
}

func parseLoad(value string) (float64, error) {
	trimmed := strings.Trim(value, "{} \n\t")
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return 0, errors.New("load average missing")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func (p *Probe) runner() CommandRunner {
	if p.Runner == nil {
		return ExecRunner{}
	}
	return p.Runner
}

func (p *Probe) diskPath() string {
	if p.DiskPath == "" {
		return "/"
	}
	return p.DiskPath
}

func (p *Probe) now() func() time.Time {
	if p.Now == nil {
		return func() time.Time { return time.Now().UTC() }
	}
	return func() time.Time { return p.Now().UTC() }
}
