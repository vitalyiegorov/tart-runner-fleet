package podman

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
)

// A container is told the vector it was charged (issue #296, `vectorEnvironment`).
// This is the other half: it is SHOWN it.
//
// A cgroup quota bounds what a process may use; it does not change what the
// process can see. Inside a container charged 2 CPUs on this fleet's Linux node,
// `/proc/cpuinfo` lists all 24 host threads and `/proc/stat` carries a line for
// each, so every tool that sizes a worker pool by counting CPUs plans for a
// machine twelve times larger than the one it has. On 2026-08 three of four
// Playwright shards timed out in cascade — 12 to 17 minutes against 17 seconds
// for the lucky one — because Playwright's default worker count is
// `os.cpus().length / 2`, and `os.cpus()` counted 24 (issue #291).
//
// The env vars alone cannot fix that. `TRF_CPUS` is a fact a tool has to be
// configured to read, and the tools that broke are exactly the ones that were
// never configured. Narrowing what they count fixes them where they stand.
//
// Measured on node B, 2026-08-29, because three plausible mechanisms do not work
// and the fourth is not obvious:
//
//	--cpuset-cpus 0,1      fails outright: the cpuset controller is not delegated
//	                       to the rootless user slice, so runc cannot write it
//	--cgroupns=private     no effect on any reading
//	taskset -c 0,1         fixes nproc (24 -> 2); os.cpus() STILL counts 24
//	/proc/cpuinfo narrowed os.cpus() STILL counts 24 -- libuv does not count it
//	/proc/stat narrowed    os.cpus() counts 2
//
// libuv counts `cpuN` lines in `/proc/stat`, which is why the file nobody would
// think of is the one that matters. `/proc/cpuinfo` is narrowed beside it for
// consistency: a container reporting two CPUs in one file and twenty-four in the
// other invites a second afternoon of exactly this.
const (
	procCPUInfo = "/proc/cpuinfo"
	procStat    = "/proc/stat"
)

// vectorViewMounts is the bind-mount pair that narrows a container's CPU view to
// its vector, or nothing at all.
//
// Every failure returns no mounts and no error. A container that sees the host's
// CPUs is the behaviour this fleet had for its whole life and is a performance
// defect; a container that will not start is a failed job. The trade is not
// close, so this never reports a reason to refuse.
func (a *Adapter) vectorViewMounts(spec executor.InstanceSpec) []string {
	if a.VectorViewDir == "" || spec.CPU <= 0 {
		return nil
	}
	cpuinfo, stat, ok := a.writeVectorView(spec.CPU)
	if !ok {
		return nil
	}
	return []string{
		"-v", cpuinfo + ":" + procCPUInfo + ":ro",
		"-v", stat + ":" + procStat + ":ro",
	}
}

// writeVectorView materialises the two files for one CPU width, idempotently.
//
// They are keyed by width alone because their content depends on nothing else:
// two containers of the same profile share one pair, and a controller restart
// rewrites what it already wrote. Nothing here is per-instance, so nothing has
// to be cleaned up when an instance ends.
func (a *Adapter) writeVectorView(cpu int) (cpuinfoPath, statPath string, ok bool) {
	source := a.vectorViewSource()
	rawCPUInfo, err := source(procCPUInfo)
	if err != nil {
		return "", "", false
	}
	rawStat, err := source(procStat)
	if err != nil {
		return "", "", false
	}
	narrowedCPUInfo, cpuInfoOK := narrowCPUInfo(rawCPUInfo, cpu)
	narrowedStat, statOK := narrowStat(rawStat, cpu)
	// Both or neither. A container told two CPUs by one file and twenty-four by
	// the other is a worse place to debug from than one told twenty-four twice.
	if !cpuInfoOK || !statOK {
		return "", "", false
	}
	directory := filepath.Join(a.VectorViewDir, strconv.Itoa(cpu))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", false
	}
	cpuinfoPath = filepath.Join(directory, "cpuinfo")
	statPath = filepath.Join(directory, "stat")
	if !writeFileAtomic(cpuinfoPath, narrowedCPUInfo) || !writeFileAtomic(statPath, narrowedStat) {
		return "", "", false
	}
	return cpuinfoPath, statPath, true
}

func (a *Adapter) vectorViewSource() func(string) ([]byte, error) {
	if a.VectorViewSource != nil {
		return a.VectorViewSource
	}
	return os.ReadFile
}

// narrowCPUInfo keeps the first `cpu` processor blocks of `/proc/cpuinfo`.
//
// The blocks are already numbered from zero in host order, so keeping a prefix
// needs no renumbering and every field a tool reads — model name, flags, cache
// size — stays true of the cores the container is actually running on.
//
// A host with fewer blocks than the vector asks for is refused rather than
// padded: inventing a core is a lie a tool could act on, and the caller's answer
// to "not ok" is to narrow nothing, which is what the fleet did before.
func narrowCPUInfo(raw []byte, cpu int) ([]byte, bool) {
	blocks := 0
	var narrowed bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "processor") && strings.Contains(line, ":") {
			blocks++
			if blocks > cpu {
				break
			}
		}
		if blocks > 0 && blocks <= cpu {
			narrowed.WriteString(line)
			narrowed.WriteByte('\n')
		}
	}
	if scanner.Err() != nil || blocks < cpu {
		return nil, false
	}
	return narrowed.Bytes(), true
}

// narrowStat drops the per-CPU lines above the vector and keeps everything else.
//
// The aggregate `cpu` line, `btime`, `processes`, `procs_running` and the
// interrupt counters all survive, because nothing about them is a CPU count.
// What does not survive is motion: the file is written once and never updated,
// so a tool that computes utilisation from two samples reads zero.
//
// That cost is smaller than it looks. The counters a container reads here have
// always been the HOST's, across every job and tenant on the box — they were
// never about this container, and no fleet decision has ever read them. A static
// container-shaped file is wrong in a way an operator can predict; a live
// host-shaped one is wrong in a way that looks right.
func narrowStat(raw []byte, cpu int) ([]byte, bool) {
	kept := 0
	var narrowed bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if index, isCPU := statCPUIndex(line); isCPU {
			if index >= cpu {
				continue
			}
			kept++
		}
		narrowed.WriteString(line)
		narrowed.WriteByte('\n')
	}
	if scanner.Err() != nil || kept < cpu {
		return nil, false
	}
	return narrowed.Bytes(), true
}

// statCPUIndex reads the N of a `cpuN ...` line. The bare aggregate `cpu ` line
// is deliberately NOT one: it is a total, not a processor, and dropping it would
// break every reader for no gain.
func statCPUIndex(line string) (int, bool) {
	field, _, _ := strings.Cut(line, " ")
	if !strings.HasPrefix(field, "cpu") || field == "cpu" {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(field, "cpu"))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// writeFileAtomic replaces a file by rename so a container can never bind-mount
// a half-written one. A concurrent writer of the same width writes identical
// bytes, so the loser of the race is harmless.
func writeFileAtomic(path string, content []byte) bool {
	staging := path + ".staging"
	// 0644 because the reader is the container's user, which rootless podman maps
	// to a different uid than the one writing here.
	if os.WriteFile(staging, content, 0o644) != nil { // #nosec G306 -- read by the container's mapped user
		return false
	}
	if os.Rename(staging, path) != nil {
		_ = os.Remove(staging)
		return false
	}
	return true
}
