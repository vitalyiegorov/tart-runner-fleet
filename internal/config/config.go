package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

type Resources struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memoryMb"`
}

type Profile struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Aliases are additional runner labels that resolve to this same profile and
	// therefore to the same scale set. They exist so a resource-explicit
	// canonical label can be adopted without rewriting every consumer workflow:
	// the retired role or tier name is listed here and keeps routing (ADR 0032).
	Aliases   []string  `json:"aliases,omitempty"`
	Resources Resources `json:"-"`
	CPU       int       `json:"cpu"`
	MemoryMiB int       `json:"memoryMb"`
	// DiskGiB is a minimum virtual-disk capacity. Zero preserves the base VM
	// for backward-compatible observe-mode decoding; authority requires an
	// explicit Linux floor so ephemeral runners cannot inherit a tiny image.
	DiskGiB   int `json:"diskGb,omitempty"`
	MaxActive int `json:"maxActive,omitempty"`
	// OccupancyBudgetSeconds is the wall-clock ceiling on how long one instance
	// of this profile may hold its resource vector before the fleet reclaims it
	// (ADR 0036). It has three distinguishable states on purpose:
	//
	//	nil  -- unstated; the platform default in internal/app applies
	//	0    -- stated as unbounded; this profile is never reaped for age
	//	n>0  -- this many seconds
	//
	// Unstated must stay omitempty and must not be materialized on encode, so a
	// configuration written by a newer release is still decodable by an older
	// strict one, exactly as githubSessionFailureWindowSeconds is.
	OccupancyBudgetSeconds *int `json:"occupancyBudgetSeconds,omitempty"`
}

func (p Profile) normalized() Profile {
	if p.Resources.CPU == 0 {
		p.Resources = Resources{CPU: p.CPU, MemoryMiB: p.MemoryMiB}
	}
	return p
}

// MinOccupancyBudget and MaxOccupancyBudget bound a stated ceiling. The floor
// keeps a typo from turning the fleet into a job killer: no profile in this
// fleet runs work that finishes inside five minutes reliably enough to be
// reaped at it. The ceiling is GitHub's own maximum job duration — past six
// hours the platform has already ended the job, so a longer budget can never
// fire and is a configuration mistake rather than a policy.
const (
	MinOccupancyBudget = 5 * time.Minute
	MaxOccupancyBudget = 6 * time.Hour
)

var errOccupancyBudgetRange = errors.New("occupancy budget must be 0 (unbounded) or between 300 and 21600 seconds")

// occupancyBudgetInRange accepts an unstated ceiling and an explicitly
// unbounded one; anything else must be a duration that could plausibly fire.
func (p Profile) occupancyBudgetInRange() bool {
	if p.OccupancyBudgetSeconds == nil || *p.OccupancyBudgetSeconds == 0 {
		return true
	}
	budget := time.Duration(*p.OccupancyBudgetSeconds) * time.Second
	return budget >= MinOccupancyBudget && budget <= MaxOccupancyBudget
}

// OccupancyBudget resolves the profile's ceiling against the default for the
// platform it runs on. A stated value always wins, including a stated zero,
// which is how an operator says "this profile is never reaped for age".
func (p Profile) OccupancyBudget(platform domain.Platform) time.Duration {
	if p.OccupancyBudgetSeconds != nil {
		return time.Duration(*p.OccupancyBudgetSeconds) * time.Second
	}
	return DefaultOccupancyBudget(platform)
}

// DefaultOccupancyBudget is the ceiling for a profile whose configuration does
// not state one. The two numbers are sized to the work each platform actually
// does on this fleet rather than to a round number:
//
//   - macOS runs the builders. An App Store archive plus upload legitimately
//     takes forty-plus minutes, and a maestro device suite is comparable, so the
//     ceiling is three times the longest healthy run we have measured.
//   - Linux runs the fleet's own CI and short jobs. An hour is already far
//     outside anything that has finished normally there.
//
// Both are well inside GitHub's six-hour job maximum, so the fleet reclaims the
// vector before the platform gives up, and both are far enough above real work
// that the budget is a backstop rather than a scheduling deadline (ADR 0036).
func DefaultOccupancyBudget(platform domain.Platform) time.Duration {
	if platform == domain.PlatformMacOS {
		return 2 * time.Hour
	}
	return time.Hour
}

type Target struct {
	Type                string                 `json:"type"`
	Slug                string                 `json:"slug"`
	MaxActive           int                    `json:"maxActive"`
	SchedulingClass     domain.SchedulingClass `json:"schedulingClass,omitempty"`
	DefaultLinuxProfile string                 `json:"defaultLinuxProfile,omitempty"`
	RunnerLabels        []string               `json:"runnerLabels,omitempty"`
}

func (t Target) normalized() Target {
	if t.SchedulingClass == "" {
		t.SchedulingClass = domain.SchedulingStandard
	}
	return t
}

type Linux struct {
	BaseVM       string
	VMPrefix     string
	MaxInstances int
	Capacity     Resources
	Profiles     []Profile
	// NestedVirtualization boots Linux guests with --nested so they can host
	// hardware-accelerated VMs of their own (KVM for Android emulators).
	// Apple's Virtualization framework only offers this to Linux guests.
	NestedVirtualization bool
	// SerialLogDirectory is where each Linux guest's serial console is written on
	// the host, one file per instance. Empty is off, which is every node today and
	// is byte-for-byte the argument vector the adapter has always passed.
	//
	// It exists for issue #236: a guest kernel panicked and the panic reached
	// nobody. Turning it on is only half the fix — the base image must also name
	// the console the VM actually exposes, which is a rebuild — and the flag is
	// unverified against this fleet's tart build, so a node enables it after
	// checking `tart run --help` (ADR 0040).
	SerialLogDirectory string
	// BaseImageCapabilities declares what BaseVM provides beyond the guest
	// contract this codebase already checks. ADR 0034 §4 says the canonical label
	// cannot lie about CPU, memory, OS, and architecture because all four are
	// derived; everything else in an image is invisible here, which is how two
	// nodes came to advertise `linux-xl` while only one carried a prewarmed
	// Redroid container (issue #202). It cannot be inferred — the daemon never
	// opens the image and the consumer that needs the capability lives in another
	// repository — so it is declared, exactly like the shared-label fact beside
	// it. Absent is a byte-for-byte no-op.
	BaseImageCapabilities []string
}

// ExecutorBackend names the execution technology of this node. It is empty on
// every node whose backend its operating system already decides -- macOS is
// Tart, and a Linux node that names none is ADR 0034's observe-only node with
// nothing to provision onto -- and it is `podman` on the x86 Linux node of
// issue #139.
type ExecutorBackend string

const (
	ExecutorNone   ExecutorBackend = ""
	ExecutorPodman ExecutorBackend = "podman"
)

// Executor is the container backend of a Linux node. It is absent from every
// other node's file, and an absent block is a node that can observe and nothing
// else -- which is exactly the state issue #138 left node B in.
//
// It is deliberately not part of the Linux block. `linux.baseVm` is a Tart base
// VM name and `linux.vmPrefix` is a Tart naming convention; neither means
// anything to a container runtime, and folding an OCI reference in beside them
// would make one key mean two things depending on the machine reading it.
type Executor struct {
	Backend ExecutorBackend
	// Image is the OCI reference every runner container is created from. On a
	// container node it takes the place of `linux.baseVm`, which stays in the
	// file because the schema requires it and means nothing there.
	Image string
	// Binary is the podman executable. Empty resolves `podman` through PATH,
	// which is what a distribution package installs.
	Binary string
	// KVMProfiles are the profile IDs whose containers are granted
	// `--device /dev/kvm`. ADR 0034 gives the device to the Android emulator
	// profile and to no other, so this is a list an operator writes down rather
	// than a fleet-wide switch.
	KVMProfiles []string
	// HoldCommand keeps a created container alive and idle so the JIT bootstrap
	// can be executed inside it. Empty means the adapter's default.
	HoldCommand []string
}

type MacOSAdmissionPolicy string

const (
	MacOSAdmissionShared    MacOSAdmissionPolicy = "shared"
	MacOSAdmissionExclusive MacOSAdmissionPolicy = "macos-exclusive"
)

type MacOS struct {
	Enabled         bool
	AdmissionPolicy MacOSAdmissionPolicy
	// MixedPlatformAdmission lets Linux runners fill the residual host envelope
	// beside a live macOS cohort (and a compatible macOS profile fill it beside
	// live Linux) instead of the platform-exclusive one-platform-per-tick model.
	// Default false preserves today's admission behavior byte-for-byte.
	MixedPlatformAdmission bool
	// MixedProfileCohorts lets two macOS profiles run side by side when their
	// exact vectors fit, completing for profiles what mixedPlatformAdmission did
	// for platforms. Default false preserves single-cohort drain-and-switch.
	MixedProfileCohorts  bool
	BaseVM               string
	VMPrefix             string
	Builder              Profile
	Maestro              Profile
	RootDiskOptions      string
	SharedDirectoryPath  string
	NestedVirtualization bool
	// BaseImageCapabilities declares what the macOS BaseVM provides. A node has
	// two base images, and each answers only for the scale sets whose profile it
	// boots: `xcode` is a fact about the macOS image and says nothing about the
	// Linux one. See Linux.BaseImageCapabilities for why it is declared rather
	// than inferred.
	BaseImageCapabilities []string
}

type Timeouts struct {
	GitHub time.Duration
	Tart   time.Duration
	Boot   time.Duration
	// Assigned bounds how long an instance may sit in the Assigned state with
	// no job ever starting before it becomes eligible for evidence-gated
	// recovery. Legitimate assignment-to-job-start is seconds; the default is
	// deliberately generous so only genuine zombies are reclaimed.
	Assigned time.Duration
}

type Guards struct {
	MinFreeDiskGiB        int
	MinAvailableMemoryMiB int
	MaxSwapUsedMiB        int
	MaxLoadAverage        float64
	MinCPUIdlePercent     float64
	// PressureMemoryAccounting selects the kernel memory-pressure availability
	// signal in the macOS host probe instead of the legacy vm_stat page formula.
	// Default false preserves legacy behavior byte-for-byte; it is enabled
	// per-host in fleet.json.
	PressureMemoryAccounting bool
	// ElasticHostEnvelope sizes the fleet against the observed physical host and
	// its measured idle CPU instead of a static configured envelope, so the fleet
	// runs as a second pilot on a machine with its own interactive tenant: it
	// expands into idle capacity and yields as the host gets busy. MaxLinuxCPU and
	// MaxLinuxMemoryMiB become Linux-only caps rather than the shared
	// cross-platform envelope. Default false preserves ADR 0012 behavior
	// byte-for-byte; it is enabled per-host in fleet.json.
	ElasticHostEnvelope bool
}

const (
	defaultMinAvailableMemoryMiB = 1024
	defaultMaxSwapUsedMiB        = 2048
	defaultMaxLoadAverage        = 9
	defaultMinCPUIdlePercent     = 5
)

// SessionRecovery bounds how long one GitHub Actions Scale Set broker session
// may keep failing ingestion before the controller discards it and creates a
// replacement, even when the failure cannot be proven terminal. A session
// GitHub refuses to release would otherwise pin an entire scope's ingestion to
// a dead handle until an operator restarts the daemon.
type SessionRecovery struct {
	// MaxConsecutiveFailures is the number of consecutive ingest failures for
	// one binding that forces a session recreate.
	MaxConsecutiveFailures int
	// FailureWindow is the elapsed time since the first failure of the current
	// run that forces a session recreate, whichever bound is reached first.
	FailureWindow time.Duration
}

const (
	defaultSessionMaxIngestFailures = 5
	defaultSessionFailureWindow     = 5 * time.Minute
	maxSessionMaxIngestFailures     = 100
	minSessionFailureWindow         = 30 * time.Second
	maxSessionFailureWindow         = time.Hour
)

// GuestLiveness bounds how long a `running` instance may go on holding its
// vector after its guest has stopped answering at all.
//
// It exists for issue #236: a `--privileged` container panicked its guest's
// kernel through `/proc/sysrq-trigger`, the panicked kernel hung forever, and
// the fleet held 6 CPU / 12288 MiB per instance until GitHub's own grace timer
// failed the job sixteen to eighteen minutes later — eight times, with no daemon
// log line for any of them. `tart list` reported the VM `running` throughout;
// `tart exec` was refused within seconds.
//
// The two bounds are what separate a dead guest from a slow one, and they are
// operator-visible because that judgement is the whole risk of the mechanism.
type GuestLiveness struct {
	// ConsecutiveRefusals is how many probes in an unbroken run must be refused
	// outright before the guest is called dead. Only a refused transport counts;
	// a probe that answered, and one that ran out of its own deadline against a
	// saturated guest, both clear the run.
	ConsecutiveRefusals int
	// Window is how long that unbroken run must have lasted, so a control loop
	// that ticks quickly can never convert seconds of silence into a verdict.
	Window time.Duration
	// ProbeTimeout bounds one probe. It is deliberately short: the probe is a
	// trivial command, and a long deadline would spend the tick waiting on a guest
	// that has nothing to say. Exceeding it is an inconclusive observation, never
	// a refusal.
	ProbeTimeout time.Duration
}

const (
	// Five refusals over ninety seconds against a twenty-second poll interval is
	// roughly a hundred seconds to a verdict, against the sixteen to eighteen
	// minutes GitHub took. It is far longer than any guest-agent restart and far
	// shorter than the shortest job this fleet has ever run.
	defaultGuestLivenessRefusals    = 5
	defaultGuestLivenessWindow      = 90 * time.Second
	defaultGuestLivenessProbe       = 5 * time.Second
	minGuestLivenessRefusals        = 3
	maxGuestLivenessRefusals        = 100
	minGuestLivenessWindow          = 30 * time.Second
	maxGuestLivenessWindow          = time.Hour
	minGuestLivenessProbeTimeout    = time.Second
	maxGuestLivenessProbeTimeout    = time.Minute
	guestLivenessDisabledRefusals   = -1
	guestLivenessDisabledWindowSecs = -1
)

// Enabled reports whether this node probes its guests at all. An operator
// disables the mechanism by stating `guestLivenessRefusals: -1`, which is a
// statement rather than an omission: an absent setting is the default bound,
// exactly as an absent occupancy budget is the platform default (ADR 0036).
func (g GuestLiveness) Enabled() bool {
	return g.ConsecutiveRefusals > 0 && g.Window > 0 && g.ProbeTimeout > 0
}

// validate refuses a bound that would make the mechanism either trigger-happy or
// useless. The floors are what stop a typo turning the fleet into a job killer:
// fewer than three refusals is one guest-agent restart away from a false
// positive, and a window under thirty seconds is shorter than a single tick on
// some nodes. The ceilings are what stop a bound that can never fire being
// mistaken for one that protects anything.
func (g GuestLiveness) validate() error {
	if !g.Enabled() {
		if g.ConsecutiveRefusals != 0 || g.Window != 0 || g.ProbeTimeout != 0 {
			return errors.New("guest liveness must state a refusal count, a window, and a probe timeout, or none of the three")
		}
		return nil
	}
	if g.ConsecutiveRefusals < minGuestLivenessRefusals || g.ConsecutiveRefusals > maxGuestLivenessRefusals {
		return errors.New("guest liveness refusals must be between 3 and 100")
	}
	if g.Window < minGuestLivenessWindow || g.Window > maxGuestLivenessWindow {
		return errors.New("guest liveness window must be between 30 and 3600 seconds")
	}
	if g.ProbeTimeout < minGuestLivenessProbeTimeout || g.ProbeTimeout > maxGuestLivenessProbeTimeout {
		return errors.New("guest liveness probe timeout must be between 1 and 60 seconds")
	}
	return nil
}

const (
	ScopeRepository   = "repository"
	ScopeOrganization = "organization"
)

type GitHubApp struct {
	ClientID        string `json:"clientId"`
	KeychainService string `json:"keychainService"`
	KeychainAccount string `json:"keychainAccount"`
	PrivateKeyFile  string `json:"privateKeyFile,omitempty"`
}

type GitHubInstallation struct {
	Name           string `json:"name"`
	InstallationID int64  `json:"installationId"`
}

// GitHubScope is one GitHub Actions registration boundary. A scale set cannot
// cross this boundary or the GitHub App installation that owns it. Its
// ScaleSets list is also the scope's opt-in: a scope exposes exactly the
// profile variants it lists, so adding a variant to the fleet costs one scale
// set in the scopes that want it rather than one in every scope (ADR 0032).
type GitHubScope struct {
	Name         string     `json:"name"`
	Kind         string     `json:"kind"`
	ConfigURL    string     `json:"configUrl"`
	Installation string     `json:"installation"`
	RunnerGroup  string     `json:"runnerGroup,omitempty"`
	Targets      []string   `json:"targets"`
	ScaleSets    []ScaleSet `json:"scaleSets"`
}

type GitHub struct {
	// Legacy single-scope fields remain decodable for observe-mode and rolling
	// migration. Authority mode rejects mixing them with the scoped model.
	ConfigURL       string     `json:"configUrl"`
	Owner           string     `json:"owner"`
	ClientID        string     `json:"clientId"`
	InstallationID  int64      `json:"installationId"`
	KeychainService string     `json:"keychainService"`
	KeychainAccount string     `json:"keychainAccount"`
	ScaleSets       []ScaleSet `json:"scaleSets"`

	SessionOwner string `json:"sessionOwner"`
	// CanonicalJobInventory enables the bounded REST job inventory and replaces
	// scale-set lookahead with truthful advertised capacity. It is deliberately
	// opt-in so a release can land before GitHub App permissions and the live
	// configuration are migrated together through shadow and canary gates.
	CanonicalJobInventory bool                 `json:"canonicalJobInventory,omitempty"`
	App                   GitHubApp            `json:"app"`
	Installations         []GitHubInstallation `json:"installations"`
	Scopes                []GitHubScope        `json:"scopes"`
}

type ScaleSet struct {
	Profile     string   `json:"profile"`
	Name        string   `json:"name,omitempty"`
	ID          int      `json:"id"`
	MaxCapacity int      `json:"maxCapacity"`
	Labels      []string `json:"labels,omitempty"`
	// SharedLabels declares that another node owns a scale set advertising these
	// same labels in this same scope. ADR 0034 as amended permits that topology
	// because GitHub places the work itself, but the REST inventory lane cannot
	// see the peer: it attributes repository-wide queued jobs by label match, so
	// without this declaration both nodes would derive demand for one job
	// (issue #153). It cannot be inferred — a node is not aware of another node
	// by construction — so it is declared, exactly like the inventory flag it
	// requires.
	SharedLabels bool `json:"sharedLabels,omitempty"`
	// RequiresCapabilities declares what the labels below need from the guest
	// image, beyond the resource vector the canonical label already states. It is
	// the requiring half of Linux.BaseImageCapabilities and, like SharedLabels, it
	// records a fact no artifact of this fleet can derive: the requirement lives
	// in a consumer's workflow in another repository. Absent is a byte-for-byte
	// no-op.
	RequiresCapabilities []string `json:"requiresCapabilities,omitempty"`
}

type Config struct {
	PollInterval   time.Duration
	ReservationAge time.Duration
	// HostBudget is an explicit ceiling on the TOTAL admission envelope of this
	// node -- every platform charged against it together -- for a machine the
	// fleet shares with work it does not own. It is a static bound at idle, which
	// is precisely what the host pressure guardrails cannot express: those read
	// whole-host signals and narrow admission as a co-tenant gets busy, so they
	// protect the co-tenant dynamically but leave the fleet free to take the
	// entire machine whenever the co-tenant happens to be quiet.
	//
	// The zero value means unset and imposes no bound, which is today's behavior
	// byte-for-byte: the envelope stays the physical machine (or the configured
	// constant under the static model). Rollback is removing the setting.
	HostBudget      Resources
	Linux           Linux
	Executor        Executor
	MacOS           MacOS
	GitHub          GitHub
	Timeouts        Timeouts
	Guards          Guards
	SessionRecovery SessionRecovery
	// GuestLiveness bounds how long an instance whose guest has stopped answering
	// may go on holding its vector (ADR 0040). An absent block is the shipped
	// default bound, not an absent one.
	GuestLiveness GuestLiveness
	Targets       []Target
	// Priority declares which demand outranks which and how fast a waiting
	// demand escalates (issue #224). An absent block is the fleet this code
	// always was: one default tier and aged FIFO.
	Priority Priority
}

type wireExecutor struct {
	Backend     ExecutorBackend `json:"backend"`
	Image       string          `json:"image,omitempty"`
	Binary      string          `json:"binary,omitempty"`
	KVMProfiles []string        `json:"kvmProfiles,omitempty"`
	HoldCommand []string        `json:"holdCommand,omitempty"`
}

type wireConfig struct {
	BaseVM string `json:"baseVm"`
	// BaseImageCapabilities is omitted when empty so a node that declares none
	// encodes no key at all, which is what keeps this feature a byte-for-byte
	// no-op for a release older than it decoding with DisallowUnknownFields.
	BaseImageCapabilities []string `json:"baseImageCapabilities,omitempty"`
	VMPrefix              string   `json:"vmPrefix"`
	PollSeconds           int      `json:"pollSeconds"`
	MaxLinuxWhenMacOSIdle int      `json:"maxLinuxWhenMacosIdle"`
	MaxLinuxCPU           int      `json:"maxLinuxCpu"`
	MaxLinuxMemoryMiB     int      `json:"maxLinuxMemoryMb"`
	// HostBudget is a pointer so an unset budget is absent from the encoded file
	// rather than present as a zero object: a release older than this setting
	// decodes with DisallowUnknownFields and would refuse the key outright.
	HostBudget              *Resources `json:"hostBudget,omitempty"`
	LinuxReservationAgeSecs int        `json:"linuxReservationAgeSeconds"`
	LinuxProfiles           []Profile  `json:"linuxProfiles"`
	// Executor is a pointer so a node with no container backend encodes no key
	// at all: a release older than issue #139 decodes with DisallowUnknownFields
	// and would refuse the block outright.
	Executor                  *wireExecutor `json:"executor,omitempty"`
	LinuxNestedVirtualization bool          `json:"linuxNestedVirtualization,omitempty"`
	LinuxSerialLogDirectory   string        `json:"linuxSerialLogDirectory,omitempty"`
	MinFreeDiskGiB            int           `json:"minFreeDiskGb"`
	MinAvailableMemoryMiB     int           `json:"minAvailableMemoryMb,omitempty"`
	MaxSwapUsedMiB            int           `json:"maxSwapUsedMb,omitempty"`
	MaxLoadAverage            float64       `json:"maxLoadAverage,omitempty"`
	MinCPUIdlePercent         float64       `json:"minCpuIdlePercent,omitempty"`
	PressureMemoryAccounting  bool          `json:"pressureMemoryAccounting,omitempty"`
	ElasticHostEnvelope       bool          `json:"elasticHostEnvelope,omitempty"`
	GitHubTimeoutSeconds      int           `json:"githubTimeoutSeconds"`
	TartControlTimeoutSeconds int           `json:"tartControlTimeoutSeconds"`
	BootTimeoutSeconds        int           `json:"bootTimeoutSeconds"`
	AssignedTimeoutSeconds    int           `json:"assignedTimeoutSeconds,omitempty"`
	// GitHubSessionMaxIngestFailures and GitHubSessionFailureWindowSeconds are
	// omitted while they hold the shipped defaults so a rewritten file stays
	// decodable by older strict releases.
	GitHubSessionMaxIngestFailures    int `json:"githubSessionMaxIngestFailures,omitempty"`
	GitHubSessionFailureWindowSeconds int `json:"githubSessionFailureWindowSeconds,omitempty"`
	// The guest-liveness bounds are omitted while they hold the shipped defaults,
	// for the same reason: a rewritten file must stay decodable by an older strict
	// release. A stated -1 disables the mechanism outright.
	GuestLivenessRefusals            int `json:"guestLivenessRefusals,omitempty"`
	GuestLivenessWindowSeconds       int `json:"guestLivenessWindowSeconds,omitempty"`
	GuestLivenessProbeTimeoutSeconds int `json:"guestLivenessProbeTimeoutSeconds,omitempty"`
	MacOSBurst                       struct {
		Enabled                bool                 `json:"enabled"`
		AdmissionPolicy        MacOSAdmissionPolicy `json:"admissionPolicy,omitempty"`
		MixedPlatformAdmission bool                 `json:"mixedPlatformAdmission,omitempty"`
		MixedProfileCohorts    bool                 `json:"mixedProfileCohorts,omitempty"`
		BaseVM                 string               `json:"baseVm"`
		BaseImageCapabilities  []string             `json:"baseImageCapabilities,omitempty"`
		VMPrefix               string               `json:"vmPrefix"`
		Builder                Profile              `json:"builder"`
		Maestro                Profile              `json:"maestro"`
		RootDiskOptions        string               `json:"rootDiskOptions,omitempty"`
		SharedDirectoryPath    string               `json:"sharedDirectoryPath,omitempty"`
		NestedVirtualization   bool                 `json:"nestedVirtualization,omitempty"`
	} `json:"macosBurst"`
	GitHub  GitHub   `json:"github"`
	Targets []Target `json:"targets"`
	// Priority is a pointer so a fleet that declares no tier encodes no key at
	// all: a release older than issue #224 decodes with DisallowUnknownFields
	// and would refuse the block outright.
	Priority *wirePriority `json:"priority,omitempty"`
}

func Decode(r io.Reader) (Config, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var w wireConfig
	if err := dec.Decode(&w); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return Config{}, err
	}
	cfg := Config{
		PollInterval:   time.Duration(w.PollSeconds) * time.Second,
		ReservationAge: time.Duration(w.LinuxReservationAgeSecs) * time.Second,
		HostBudget:     hostBudget(w.HostBudget),
		Executor:       decodeExecutor(w.Executor),
		Linux: Linux{BaseVM: w.BaseVM, VMPrefix: w.VMPrefix, MaxInstances: w.MaxLinuxWhenMacOSIdle,
			Capacity: Resources{CPU: w.MaxLinuxCPU, MemoryMiB: w.MaxLinuxMemoryMiB}, Profiles: normalizeProfiles(w.LinuxProfiles),
			NestedVirtualization:  w.LinuxNestedVirtualization,
			SerialLogDirectory:    w.LinuxSerialLogDirectory,
			BaseImageCapabilities: append([]string(nil), w.BaseImageCapabilities...)},
		MacOS: MacOS{Enabled: w.MacOSBurst.Enabled, AdmissionPolicy: normalizeMacOSAdmissionPolicy(w.MacOSBurst.AdmissionPolicy),
			MixedPlatformAdmission: w.MacOSBurst.MixedPlatformAdmission, MixedProfileCohorts: w.MacOSBurst.MixedProfileCohorts,
			BaseVM: w.MacOSBurst.BaseVM, VMPrefix: w.MacOSBurst.VMPrefix,
			Builder: w.MacOSBurst.Builder.normalized(), Maestro: w.MacOSBurst.Maestro.normalized(),
			RootDiskOptions: w.MacOSBurst.RootDiskOptions, SharedDirectoryPath: w.MacOSBurst.SharedDirectoryPath,
			NestedVirtualization:  w.MacOSBurst.NestedVirtualization,
			BaseImageCapabilities: append([]string(nil), w.MacOSBurst.BaseImageCapabilities...)},
		GitHub: w.GitHub,
		Timeouts: Timeouts{GitHub: secondsOr(w.GitHubTimeoutSeconds, 15), Tart: secondsOr(w.TartControlTimeoutSeconds, 45),
			Boot: secondsOr(w.BootTimeoutSeconds, 180), Assigned: secondsOr(w.AssignedTimeoutSeconds, 900)},
		Guards: normalizeGuards(w), SessionRecovery: normalizeSessionRecovery(w),
		GuestLiveness: normalizeGuestLiveness(w), Targets: normalizeTargets(w.Targets),
		Priority: decodePriority(w.Priority),
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Encode writes the stable on-disk JSON representation. Runtime-only resource
// fields and durations are projected back through wireConfig so a decoded file
// can be safely updated with server-assigned scale-set IDs and persisted.
func Encode(w io.Writer, cfg Config) error {
	if w == nil {
		return errors.New("config writer is required")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate config before encoding: %w", err)
	}
	pollSeconds, err := wholeSeconds("poll interval", cfg.PollInterval)
	if err != nil {
		return err
	}
	reservationSeconds, err := wholeSeconds("reservation age", cfg.ReservationAge)
	if err != nil {
		return err
	}
	githubSeconds, err := wholeSeconds("GitHub timeout", cfg.Timeouts.GitHub)
	if err != nil {
		return err
	}
	tartSeconds, err := wholeSeconds("Tart timeout", cfg.Timeouts.Tart)
	if err != nil {
		return err
	}
	bootSeconds, err := wholeSeconds("boot timeout", cfg.Timeouts.Boot)
	if err != nil {
		return err
	}
	assignedSeconds, err := wholeSeconds("assigned timeout", cfg.Timeouts.Assigned)
	if err != nil {
		return err
	}
	sessionWindowSeconds, err := wholeSeconds("session failure window", cfg.SessionRecovery.FailureWindow)
	if err != nil {
		return err
	}
	priority, err := encodePriority(cfg.Priority)
	if err != nil {
		return err
	}
	guestLiveness, err := encodeGuestLiveness(cfg.GuestLiveness)
	if err != nil {
		return err
	}

	wire := wireConfig{
		BaseVM: cfg.Linux.BaseVM, VMPrefix: cfg.Linux.VMPrefix,
		PollSeconds: pollSeconds, MaxLinuxWhenMacOSIdle: cfg.Linux.MaxInstances,
		MaxLinuxCPU: cfg.Linux.Capacity.CPU, MaxLinuxMemoryMiB: cfg.Linux.Capacity.MemoryMiB,
		BaseImageCapabilities:   append([]string(nil), cfg.Linux.BaseImageCapabilities...),
		LinuxReservationAgeSecs: reservationSeconds, LinuxProfiles: encodeProfiles(cfg.Linux.Profiles),
		MinFreeDiskGiB: cfg.Guards.MinFreeDiskGiB, MinAvailableMemoryMiB: cfg.Guards.MinAvailableMemoryMiB,
		MaxSwapUsedMiB: cfg.Guards.MaxSwapUsedMiB, MaxLoadAverage: cfg.Guards.MaxLoadAverage,
		MinCPUIdlePercent: cfg.Guards.MinCPUIdlePercent, PressureMemoryAccounting: cfg.Guards.PressureMemoryAccounting,
		ElasticHostEnvelope:       cfg.Guards.ElasticHostEnvelope,
		GitHubTimeoutSeconds:      githubSeconds,
		TartControlTimeoutSeconds: tartSeconds, BootTimeoutSeconds: bootSeconds, AssignedTimeoutSeconds: assignedSeconds,
		GitHub: cfg.GitHub, Targets: normalizeTargets(cfg.Targets), Priority: priority,
		GuestLivenessRefusals:            guestLiveness.refusals,
		GuestLivenessWindowSeconds:       guestLiveness.windowSeconds,
		GuestLivenessProbeTimeoutSeconds: guestLiveness.probeSeconds,
	}
	// Only non-default recovery bounds are emitted; see wireConfig.
	if cfg.SessionRecovery.MaxConsecutiveFailures != defaultSessionMaxIngestFailures {
		wire.GitHubSessionMaxIngestFailures = cfg.SessionRecovery.MaxConsecutiveFailures
	}
	if cfg.SessionRecovery.FailureWindow != defaultSessionFailureWindow {
		wire.GitHubSessionFailureWindowSeconds = sessionWindowSeconds
	}
	if cfg.HostBudget != (Resources{}) {
		budget := cfg.HostBudget
		wire.HostBudget = &budget
	}
	if cfg.Executor.Backend != ExecutorNone {
		wire.Executor = &wireExecutor{Backend: cfg.Executor.Backend, Image: cfg.Executor.Image,
			Binary: cfg.Executor.Binary, KVMProfiles: append([]string(nil), cfg.Executor.KVMProfiles...),
			HoldCommand: append([]string(nil), cfg.Executor.HoldCommand...)}
	}
	wire.MacOSBurst.Enabled = cfg.MacOS.Enabled
	if cfg.MacOS.AdmissionPolicy == MacOSAdmissionExclusive {
		wire.MacOSBurst.AdmissionPolicy = MacOSAdmissionExclusive
	}
	wire.MacOSBurst.MixedPlatformAdmission = cfg.MacOS.MixedPlatformAdmission
	wire.MacOSBurst.MixedProfileCohorts = cfg.MacOS.MixedProfileCohorts
	wire.MacOSBurst.BaseVM = cfg.MacOS.BaseVM
	wire.MacOSBurst.BaseImageCapabilities = append([]string(nil), cfg.MacOS.BaseImageCapabilities...)
	wire.MacOSBurst.VMPrefix = cfg.MacOS.VMPrefix
	wire.MacOSBurst.Builder = encodeProfile(cfg.MacOS.Builder)
	wire.MacOSBurst.Maestro = encodeProfile(cfg.MacOS.Maestro)
	wire.MacOSBurst.RootDiskOptions = cfg.MacOS.RootDiskOptions
	wire.MacOSBurst.SharedDirectoryPath = cfg.MacOS.SharedDirectoryPath
	wire.LinuxNestedVirtualization = cfg.Linux.NestedVirtualization
	wire.LinuxSerialLogDirectory = cfg.Linux.SerialLogDirectory

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(wire); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

// wireGuestLiveness is the encoded form of the bound: the three fields that go
// on the wire, each zero while it holds its shipped default so a rewritten file
// stays byte-identical for a node that never stated one.
type wireGuestLiveness struct {
	refusals      int
	windowSeconds int
	probeSeconds  int
}

func encodeGuestLiveness(liveness GuestLiveness) (wireGuestLiveness, error) {
	if !liveness.Enabled() {
		// A disabled bound must survive a round trip as a disabled bound, so the
		// sentinel is written rather than omitted.
		return wireGuestLiveness{refusals: guestLivenessDisabledRefusals}, nil
	}
	windowSeconds, err := wholeSeconds("guest liveness window", liveness.Window)
	if err != nil {
		return wireGuestLiveness{}, err
	}
	probeSeconds, err := wholeSeconds("guest liveness probe timeout", liveness.ProbeTimeout)
	if err != nil {
		return wireGuestLiveness{}, err
	}
	encoded := wireGuestLiveness{}
	if liveness.ConsecutiveRefusals != defaultGuestLivenessRefusals {
		encoded.refusals = liveness.ConsecutiveRefusals
	}
	if liveness.Window != defaultGuestLivenessWindow {
		encoded.windowSeconds = windowSeconds
	}
	if liveness.ProbeTimeout != defaultGuestLivenessProbe {
		encoded.probeSeconds = probeSeconds
	}
	return encoded, nil
}

func wholeSeconds(name string, value time.Duration) (int, error) {
	if value%time.Second != 0 {
		return 0, fmt.Errorf("%s must use whole seconds", name)
	}
	return int(value / time.Second), nil
}

func encodeProfiles(profiles []Profile) []Profile {
	out := make([]Profile, len(profiles))
	for i, profile := range profiles {
		out[i] = encodeProfile(profile)
	}
	return out
}

func encodeProfile(profile Profile) Profile {
	profile = profile.normalized()
	profile.CPU = profile.Resources.CPU
	profile.MemoryMiB = profile.Resources.MemoryMiB
	return profile
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return errors.New("decode config: multiple JSON values")
}

func secondsOr(value, fallback int) time.Duration {
	if value == 0 {
		value = fallback
	}
	return time.Duration(value) * time.Second
}

func normalizeGuards(w wireConfig) Guards {
	guards := Guards{MinFreeDiskGiB: w.MinFreeDiskGiB, MinAvailableMemoryMiB: w.MinAvailableMemoryMiB,
		MaxSwapUsedMiB: w.MaxSwapUsedMiB, MaxLoadAverage: w.MaxLoadAverage, MinCPUIdlePercent: w.MinCPUIdlePercent,
		PressureMemoryAccounting: w.PressureMemoryAccounting, ElasticHostEnvelope: w.ElasticHostEnvelope}
	if guards.MinAvailableMemoryMiB == 0 {
		guards.MinAvailableMemoryMiB = defaultMinAvailableMemoryMiB
	}
	if guards.MaxSwapUsedMiB == 0 {
		guards.MaxSwapUsedMiB = defaultMaxSwapUsedMiB
	}
	if guards.MaxLoadAverage == 0 {
		guards.MaxLoadAverage = defaultMaxLoadAverage
	}
	if guards.MinCPUIdlePercent == 0 {
		guards.MinCPUIdlePercent = defaultMinCPUIdlePercent
	}
	return guards
}

// hostBudget reads the optional ceiling. An absent object is the zero vector,
// which imposes no bound anywhere; a present one is carried through untouched so
// Validate reports what the operator actually wrote rather than a normalized
// version of it.
func hostBudget(budget *Resources) Resources {
	if budget == nil {
		return Resources{}
	}
	return *budget
}

func normalizeSessionRecovery(w wireConfig) SessionRecovery {
	recovery := SessionRecovery{MaxConsecutiveFailures: w.GitHubSessionMaxIngestFailures,
		FailureWindow: time.Duration(w.GitHubSessionFailureWindowSeconds) * time.Second}
	if recovery.MaxConsecutiveFailures == 0 {
		recovery.MaxConsecutiveFailures = defaultSessionMaxIngestFailures
	}
	if recovery.FailureWindow == 0 {
		recovery.FailureWindow = defaultSessionFailureWindow
	}
	return recovery
}

// normalizeGuestLiveness projects the three optional wire fields onto the
// runtime bound. An omitted field is the shipped default; the sentinel -1 is an
// operator explicitly stating that this node does not probe its guests, and it
// disables the mechanism rather than defaulting it back on. A destructive bound
// must be as easy to turn off as it was to leave alone.
func normalizeGuestLiveness(w wireConfig) GuestLiveness {
	if w.GuestLivenessRefusals == guestLivenessDisabledRefusals || w.GuestLivenessWindowSeconds == guestLivenessDisabledWindowSecs {
		return GuestLiveness{}
	}
	liveness := GuestLiveness{ConsecutiveRefusals: w.GuestLivenessRefusals,
		Window:       time.Duration(w.GuestLivenessWindowSeconds) * time.Second,
		ProbeTimeout: time.Duration(w.GuestLivenessProbeTimeoutSeconds) * time.Second}
	if liveness.ConsecutiveRefusals == 0 {
		liveness.ConsecutiveRefusals = defaultGuestLivenessRefusals
	}
	if liveness.Window == 0 {
		liveness.Window = defaultGuestLivenessWindow
	}
	if liveness.ProbeTimeout == 0 {
		liveness.ProbeTimeout = defaultGuestLivenessProbe
	}
	return liveness
}

func normalizeProfiles(in []Profile) []Profile {
	out := make([]Profile, len(in))
	for i, p := range in {
		out[i] = p.normalized()
	}
	return out
}

func normalizeTargets(in []Target) []Target {
	out := make([]Target, len(in))
	for i, target := range in {
		out[i] = target.normalized()
	}
	return out
}

func normalizeMacOSAdmissionPolicy(policy MacOSAdmissionPolicy) MacOSAdmissionPolicy {
	if policy == "" {
		return MacOSAdmissionShared
	}
	return policy
}

// decodeExecutor projects the optional wire block onto the runtime struct. An
// absent block and a block naming no backend are the same node: one that cannot
// bring an instance into existence.
func decodeExecutor(w *wireExecutor) Executor {
	if w == nil {
		return Executor{}
	}
	return Executor{Backend: w.Backend, Image: w.Image, Binary: w.Binary,
		KVMProfiles: append([]string(nil), w.KVMProfiles...),
		HoldCommand: append([]string(nil), w.HoldCommand...)}
}

func Default() Config {
	return Config{
		PollInterval: 20 * time.Second, ReservationAge: 5 * time.Minute,
		Linux: Linux{BaseVM: "linux-runner-base", VMPrefix: "gha-linux", MaxInstances: 4,
			Capacity: Resources{CPU: 8, MemoryMiB: 16384}, Profiles: []Profile{
				{ID: "small", Label: "linux-small", Resources: Resources{CPU: 1, MemoryMiB: 2048}, DiskGiB: 50},
				{ID: "medium", Label: "linux-medium", Resources: Resources{CPU: 2, MemoryMiB: 4096}, DiskGiB: 50},
				{ID: "large", Label: "linux-large", Resources: Resources{CPU: 4, MemoryMiB: 8192}, DiskGiB: 50},
			}},
		MacOS: MacOS{Enabled: true, AdmissionPolicy: MacOSAdmissionShared, BaseVM: "macos-tartelet-base", VMPrefix: "gha-macos",
			Builder: Profile{ID: "builder", Label: "macos-builder", Resources: Resources{CPU: 8, MemoryMiB: 12288}, MaxActive: 1},
			Maestro: Profile{ID: "maestro", Label: "macos-maestro", Resources: Resources{CPU: 4, MemoryMiB: 7168}, MaxActive: 2}},
		Timeouts: Timeouts{GitHub: 15 * time.Second, Tart: 45 * time.Second, Boot: 3 * time.Minute, Assigned: 15 * time.Minute},
		Guards: Guards{MinFreeDiskGiB: 60, MinAvailableMemoryMiB: defaultMinAvailableMemoryMiB,
			MaxSwapUsedMiB: defaultMaxSwapUsedMiB, MaxLoadAverage: defaultMaxLoadAverage,
			MinCPUIdlePercent: defaultMinCPUIdlePercent},
		SessionRecovery: SessionRecovery{MaxConsecutiveFailures: defaultSessionMaxIngestFailures,
			FailureWindow: defaultSessionFailureWindow},
		GuestLiveness: GuestLiveness{ConsecutiveRefusals: defaultGuestLivenessRefusals,
			Window: defaultGuestLivenessWindow, ProbeTimeout: defaultGuestLivenessProbe},
		Targets: []Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4, SchedulingClass: domain.SchedulingStandard}},
	}
}

func (c Config) Clone() Config {
	out := c
	out.Executor.KVMProfiles = append([]string(nil), c.Executor.KVMProfiles...)
	out.Executor.HoldCommand = append([]string(nil), c.Executor.HoldCommand...)
	out.Linux.Profiles = append([]Profile(nil), c.Linux.Profiles...)
	for i := range out.Linux.Profiles {
		out.Linux.Profiles[i].Aliases = append([]string(nil), c.Linux.Profiles[i].Aliases...)
		out.Linux.Profiles[i].OccupancyBudgetSeconds = cloneBudget(c.Linux.Profiles[i].OccupancyBudgetSeconds)
	}
	out.Linux.BaseImageCapabilities = append([]string(nil), c.Linux.BaseImageCapabilities...)
	out.MacOS.BaseImageCapabilities = append([]string(nil), c.MacOS.BaseImageCapabilities...)
	out.MacOS.Builder.Aliases = append([]string(nil), c.MacOS.Builder.Aliases...)
	out.MacOS.Maestro.Aliases = append([]string(nil), c.MacOS.Maestro.Aliases...)
	out.MacOS.Builder.OccupancyBudgetSeconds = cloneBudget(c.MacOS.Builder.OccupancyBudgetSeconds)
	out.MacOS.Maestro.OccupancyBudgetSeconds = cloneBudget(c.MacOS.Maestro.OccupancyBudgetSeconds)
	out.GitHub.ScaleSets = append([]ScaleSet(nil), c.GitHub.ScaleSets...)
	for i := range out.GitHub.ScaleSets {
		out.GitHub.ScaleSets[i] = c.GitHub.ScaleSets[i].clone()
	}
	out.GitHub.Installations = append([]GitHubInstallation(nil), c.GitHub.Installations...)
	out.GitHub.Scopes = append([]GitHubScope(nil), c.GitHub.Scopes...)
	for i := range out.GitHub.Scopes {
		out.GitHub.Scopes[i].Targets = append([]string(nil), c.GitHub.Scopes[i].Targets...)
		out.GitHub.Scopes[i].ScaleSets = append([]ScaleSet(nil), c.GitHub.Scopes[i].ScaleSets...)
		for j := range out.GitHub.Scopes[i].ScaleSets {
			out.GitHub.Scopes[i].ScaleSets[j] = c.GitHub.Scopes[i].ScaleSets[j].clone()
		}
	}
	out.Targets = append([]Target(nil), c.Targets...)
	for i := range out.Targets {
		out.Targets[i].RunnerLabels = append([]string(nil), c.Targets[i].RunnerLabels...)
	}
	out.Priority = c.Priority.clone()
	return out
}

// cloneBudget copies the stated ceiling by value so a clone and its original
// cannot alias one operator setting.
func cloneBudget(seconds *int) *int {
	if seconds == nil {
		return nil
	}
	copied := *seconds
	return &copied
}

func (c Config) Validate() error {
	if c.PollInterval <= 0 || c.ReservationAge <= 0 {
		return errors.New("intervals must be positive")
	}
	if err := c.Priority.validate(); err != nil {
		return err
	}
	if c.Timeouts.GitHub <= 0 || c.Timeouts.Tart <= 0 || c.Timeouts.Boot <= 0 {
		return errors.New("timeouts must be positive")
	}
	if c.Timeouts.Assigned < time.Minute || c.Timeouts.Assigned > time.Hour {
		return errors.New("assigned timeout must be between 60 and 3600 seconds")
	}
	if c.SessionRecovery.MaxConsecutiveFailures < 1 || c.SessionRecovery.MaxConsecutiveFailures > maxSessionMaxIngestFailures {
		return errors.New("github session max ingest failures must be between 1 and 100")
	}
	if c.SessionRecovery.FailureWindow < minSessionFailureWindow || c.SessionRecovery.FailureWindow > maxSessionFailureWindow {
		return errors.New("github session failure window must be between 30 and 3600 seconds")
	}
	if err := c.GuestLiveness.validate(); err != nil {
		return err
	}
	if c.Linux.BaseVM == "" || c.Linux.VMPrefix == "" {
		return errors.New("linux base VM and prefix are required")
	}
	if c.Linux.MaxInstances < 1 || c.Linux.MaxInstances > 4 {
		return errors.New("linux max instances must be between 1 and 4")
	}
	if c.Linux.Capacity.CPU <= 0 || c.Linux.Capacity.MemoryMiB <= 0 {
		return errors.New("linux capacity must be positive")
	}
	if c.MacOS.NestedVirtualization {
		return errors.New("macOS guests do not support nested virtualization (Apple Virtualization framework limitation); use linuxNestedVirtualization")
	}
	if c.Guards.MinFreeDiskGiB <= 0 {
		return errors.New("disk reserve must be positive")
	}
	if c.MacOS.RootDiskOptions != "" && c.MacOS.RootDiskOptions != "sync=none" &&
		c.MacOS.RootDiskOptions != "sync=fsync" && c.MacOS.RootDiskOptions != "sync=full" {
		return errors.New("macOS root disk options must be sync=none, sync=fsync, or sync=full")
	}
	if c.MacOS.SharedDirectoryPath != "" && !filepath.IsAbs(c.MacOS.SharedDirectoryPath) {
		return errors.New("macOS shared directory path must be absolute")
	}
	policy := normalizeMacOSAdmissionPolicy(c.MacOS.AdmissionPolicy)
	if policy != MacOSAdmissionShared && policy != MacOSAdmissionExclusive {
		return fmt.Errorf("invalid macOS admission policy %q", c.MacOS.AdmissionPolicy)
	}
	if c.Guards.MinAvailableMemoryMiB < 0 || c.Guards.MaxSwapUsedMiB < 0 || c.Guards.MaxLoadAverage < 0 ||
		math.IsNaN(c.Guards.MaxLoadAverage) || math.IsInf(c.Guards.MaxLoadAverage, 0) ||
		c.Guards.MinCPUIdlePercent < 0 || c.Guards.MinCPUIdlePercent > 100 ||
		math.IsNaN(c.Guards.MinCPUIdlePercent) || math.IsInf(c.Guards.MinCPUIdlePercent, 0) {
		return errors.New("host pressure guards are invalid")
	}
	seenProfiles := map[string]struct{}{}
	for _, raw := range c.Linux.Profiles {
		p := raw.normalized()
		if p.ID == "" || p.Label == "" || p.Resources.CPU <= 0 || p.Resources.MemoryMiB <= 0 || p.DiskGiB < 0 {
			return errors.New("invalid linux profile")
		}
		if !p.occupancyBudgetInRange() {
			return fmt.Errorf("linux profile %s: %w", p.ID, errOccupancyBudgetRange)
		}
		if p.Resources.CPU > c.Linux.Capacity.CPU || p.Resources.MemoryMiB > c.Linux.Capacity.MemoryMiB {
			return fmt.Errorf("profile %s exceeds capacity", p.ID)
		}
		if _, ok := seenProfiles[p.ID]; ok {
			return fmt.Errorf("duplicate profile %s", p.ID)
		}
		seenProfiles[p.ID] = struct{}{}
	}
	seenTargets := map[string]struct{}{}
	for _, target := range c.Targets {
		if target.Type != "repo" || strings.Count(target.Slug, "/") != 1 || target.MaxActive <= 0 {
			return fmt.Errorf("invalid target %q", target.Slug)
		}
		schedulingClass := target.normalized().SchedulingClass
		if schedulingClass != domain.SchedulingStandard && schedulingClass != domain.SchedulingControlPlane {
			return fmt.Errorf("invalid scheduling class %q for target %s", target.SchedulingClass, target.Slug)
		}
		if _, ok := seenTargets[target.Slug]; ok {
			return fmt.Errorf("duplicate target %s", target.Slug)
		}
		seenTargets[target.Slug] = struct{}{}
	}
	if c.MacOS.Enabled {
		if c.MacOS.BaseVM == "" || c.MacOS.VMPrefix == "" {
			return errors.New("macOS base VM and prefix are required")
		}
		for _, p := range []Profile{c.MacOS.Builder.normalized(), c.MacOS.Maestro.normalized()} {
			if p.Label == "" || p.Resources.CPU <= 0 || p.Resources.MemoryMiB <= 0 || p.MaxActive <= 0 {
				return errors.New("invalid macOS profile")
			}
			if !p.occupancyBudgetInRange() {
				return fmt.Errorf("macOS profile %s: %w", p.ID, errOccupancyBudgetRange)
			}
		}
	}
	if err := c.validateHostBudget(); err != nil {
		return err
	}
	if err := c.validateExecutor(); err != nil {
		return err
	}
	if err := c.validateCapabilities(); err != nil {
		return err
	}
	// Label derivation runs last so a broken vector is reported as the vector
	// error it is, not as the naming failure it also causes.
	_, err := c.profileLabelSets()
	return err
}

// validateHostBudget checks the two things about a ceiling that are knowable
// from a file alone. Whether the budget fits the physical machine is not one of
// them -- `fleet config validate` decodes a configuration and never probes a
// host -- so that check lives at the runtime probe (see internal/app).
//
// The second rule is the ADR 0032 interaction. A profile is named by a runner
// label, GitHub routes jobs to it through a scale set, and a job routed to a
// shape whose vector cannot fit inside the budget can never be admitted on this
// node: not throttled, not delayed behind other work, but queued forever. That
// is a configuration error, so it is refused here rather than discovered as a
// starving queue. Only profiles this node actually exposes are checked, because
// a profile with no scale set anywhere receives no jobs -- which is what lets a
// budgeted node keep the mandatory macosBurst.builder in its file while serving
// maestro alone.
func (c Config) validateHostBudget() error {
	if c.HostBudget == (Resources{}) {
		return nil
	}
	if c.HostBudget.CPU <= 0 || c.HostBudget.MemoryMiB <= 0 {
		return fmt.Errorf("host budget must set a positive cpu and memoryMb, got %d cpu and %d MiB",
			c.HostBudget.CPU, c.HostBudget.MemoryMiB)
	}
	exposed := c.exposedProfiles()
	for _, raw := range c.budgetedProfiles() {
		profile := raw.normalized()
		if _, routed := exposed[profile.ID]; !routed {
			continue
		}
		if profile.Resources.CPU > c.HostBudget.CPU || profile.Resources.MemoryMiB > c.HostBudget.MemoryMiB {
			return fmt.Errorf("profile %s needs %d cpu and %d MiB, which can never fit the host budget of %d cpu and %d MiB",
				profile.ID, profile.Resources.CPU, profile.Resources.MemoryMiB, c.HostBudget.CPU, c.HostBudget.MemoryMiB)
		}
	}
	return nil
}

// budgetedProfiles is every profile this node can boot. macOS profiles count
// only when macOS is enabled, matching what the scheduler is given.
func (c Config) budgetedProfiles() []Profile {
	profiles := append([]Profile(nil), c.Linux.Profiles...)
	if c.MacOS.Enabled {
		profiles = append(profiles, c.MacOS.Builder, c.MacOS.Maestro)
	}
	return profiles
}

// exposedProfiles is every profile GitHub can route a job to on this node: the
// profile of each scale set, in the legacy flat list and in every scope.
// validateExecutor checks the container backend the way a file can be checked:
// the backend is one this build implements, an image is present and cannot be
// read as a command-line option, and every profile granted /dev/kvm is a profile
// this node actually declares.
//
// The last rule is the one worth having. ADR 0034 gives the device to the
// Android emulator profile alone, and a typo in that list would silently grant
// nothing -- a workflow that fails deep inside an emulator boot rather than at
// the configuration that caused it.
func (c Config) validateExecutor() error {
	if c.Executor.Backend == ExecutorNone {
		if c.Executor.Image != "" || c.Executor.Binary != "" ||
			len(c.Executor.KVMProfiles) > 0 || len(c.Executor.HoldCommand) > 0 {
			return errors.New("executor settings require an executor backend")
		}
		return nil
	}
	if c.Executor.Backend != ExecutorPodman {
		return fmt.Errorf("unsupported executor backend %q", c.Executor.Backend)
	}
	if err := domain.ValidateImageReference(c.Executor.Image); err != nil {
		return fmt.Errorf("executor image: %w", err)
	}
	declared := make(map[string]struct{}, len(c.Linux.Profiles))
	for _, profile := range c.Linux.Profiles {
		declared[profile.normalized().ID] = struct{}{}
	}
	for _, profile := range c.Executor.KVMProfiles {
		if _, ok := declared[profile]; !ok {
			return fmt.Errorf("executor grants /dev/kvm to undeclared profile %q", profile)
		}
	}
	for _, argument := range c.Executor.HoldCommand {
		if strings.TrimSpace(argument) == "" {
			return errors.New("executor hold command contains an empty argument")
		}
	}
	return nil
}

func (c Config) exposedProfiles() map[string]struct{} {
	exposed := make(map[string]struct{}, len(c.GitHub.ScaleSets))
	for _, scaleSet := range c.GitHub.ScaleSets {
		exposed[scaleSet.Profile] = struct{}{}
	}
	for _, scope := range c.GitHub.Scopes {
		for _, scaleSet := range scope.ScaleSets {
			exposed[scaleSet.Profile] = struct{}{}
		}
	}
	return exposed
}

// ValidateAuthority adds requirements for making GitHub or Tart mutations.
// Base validation intentionally remains usable during observe-only migration.
func (c Config) ValidateAuthority() error {
	if err := c.Validate(); err != nil {
		return err
	}
	for _, profile := range c.Linux.Profiles {
		if profile.DiskGiB <= 0 {
			return fmt.Errorf("linux profile %s requires a positive disk floor in authority mode", profile.ID)
		}
	}
	if c.GitHub.multiScopeConfigured() {
		if c.GitHub.legacyConfigured() {
			return errors.New("legacy and multi-scope GitHub authority configuration cannot be mixed")
		}
		return c.validateMultiScopeAuthority()
	}
	return c.validateLegacyAuthority()
}

func (c Config) validateLegacyAuthority() error {
	if c.GitHub.ConfigURL == "" || c.GitHub.Owner == "" || c.GitHub.ClientID == "" || c.GitHub.InstallationID <= 0 ||
		c.GitHub.KeychainService == "" || c.GitHub.KeychainAccount == "" {
		return errors.New("complete GitHub App and Keychain configuration is required")
	}
	profiles := make(map[string]struct{}, len(c.Linux.Profiles)+2)
	capacities := c.authorityProfileCapacities()
	targetCapacity := 0
	for _, target := range c.Targets {
		targetCapacity += target.MaxActive
	}
	for _, profile := range c.Linux.Profiles {
		profiles[profile.ID] = struct{}{}
	}
	if c.MacOS.Enabled {
		profiles[c.MacOS.Builder.ID] = struct{}{}
		profiles[c.MacOS.Maestro.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(c.GitHub.ScaleSets))
	for _, scaleSet := range c.GitHub.ScaleSets {
		_, known := profiles[scaleSet.Profile]
		runtimeCapacity := min(capacities[scaleSet.Profile], targetCapacity)
		if !known || scaleSet.ID <= 0 {
			return fmt.Errorf("invalid scale set for profile %q", scaleSet.Profile)
		}
		if c.GitHub.CanonicalJobInventory && scaleSet.MaxCapacity != runtimeCapacity {
			return fmt.Errorf("scale set for profile %q requires truthful capacity: maxCapacity=%d, runtime capacity=%d",
				scaleSet.Profile, scaleSet.MaxCapacity, runtimeCapacity)
		}
		if !c.GitHub.CanonicalJobInventory && scaleSet.MaxCapacity <= runtimeCapacity {
			return fmt.Errorf("scale set for profile %q requires queue lookahead until canonicalJobInventory is enabled: maxCapacity=%d, runtime capacity=%d",
				scaleSet.Profile, scaleSet.MaxCapacity, runtimeCapacity)
		}
		if _, duplicate := seen[scaleSet.Profile]; duplicate {
			return fmt.Errorf("duplicate scale set profile %q", scaleSet.Profile)
		}
		seen[scaleSet.Profile] = struct{}{}
	}
	if len(seen) != len(profiles) {
		return errors.New("every enabled profile requires one scale set")
	}
	return nil
}

func (g GitHub) multiScopeConfigured() bool {
	return g.SessionOwner != "" || g.App != (GitHubApp{}) || len(g.Installations) > 0 || len(g.Scopes) > 0
}

func (g GitHub) legacyConfigured() bool {
	return g.ConfigURL != "" || g.Owner != "" || g.ClientID != "" || g.InstallationID != 0 ||
		g.KeychainService != "" || g.KeychainAccount != "" || len(g.ScaleSets) > 0
}

func (c Config) validateMultiScopeAuthority() error {
	github := c.GitHub
	if strings.TrimSpace(github.SessionOwner) == "" || strings.TrimSpace(github.App.ClientID) == "" ||
		!github.App.hasCredentialSource() {
		return errors.New("complete multi-scope GitHub App, session owner, and private-key credential configuration is required")
	}
	if len(github.Installations) == 0 || len(github.Scopes) == 0 {
		return errors.New("multi-scope GitHub authority requires installations and scopes")
	}

	installations := make(map[string]struct{}, len(github.Installations))
	installationIDs := make(map[int64]struct{}, len(github.Installations))
	for _, installation := range github.Installations {
		key := folded(installation.Name)
		if key == "" || installation.InstallationID <= 0 {
			return fmt.Errorf("invalid GitHub installation %q", installation.Name)
		}
		if _, exists := installations[key]; exists {
			return fmt.Errorf("duplicate GitHub installation name %q", installation.Name)
		}
		if _, exists := installationIDs[installation.InstallationID]; exists {
			return fmt.Errorf("duplicate GitHub installation ID %d", installation.InstallationID)
		}
		installations[key] = struct{}{}
		installationIDs[installation.InstallationID] = struct{}{}
	}

	labelSets := c.ProfileLabelSets()
	capacities := c.authorityProfileCapacities()
	targets := make(map[string]string, len(c.Targets))
	targetCapacities := make(map[string]int, len(c.Targets))
	for _, target := range c.Targets {
		targets[folded(target.Slug)] = target.Slug
		targetCapacities[folded(target.Slug)] = target.MaxActive
	}
	assignedTargets := make(map[string]string, len(targets))
	scopeNames := make(map[string]struct{}, len(github.Scopes))
	scopeURLs := make(map[string]struct{}, len(github.Scopes))
	for _, scope := range github.Scopes {
		scopeName := folded(scope.Name)
		if scopeName == "" {
			return errors.New("GitHub scope name is required")
		}
		if _, exists := scopeNames[scopeName]; exists {
			return fmt.Errorf("duplicate GitHub scope name %q", scope.Name)
		}
		scopeNames[scopeName] = struct{}{}
		if _, exists := installations[folded(scope.Installation)]; !exists {
			return fmt.Errorf("GitHub scope %q references unknown installation %q", scope.Name, scope.Installation)
		}

		parsed, urlKey, err := parseScopeURL(scope)
		if err != nil {
			return fmt.Errorf("invalid GitHub scope %q: %w", scope.Name, err)
		}
		if _, exists := scopeURLs[urlKey]; exists {
			return fmt.Errorf("duplicate GitHub scope URL %q", scope.ConfigURL)
		}
		scopeURLs[urlKey] = struct{}{}
		if err := validateScopeTargets(scope, parsed, targets, assignedTargets); err != nil {
			return err
		}
		if err := validateScopeScaleSets(scope, labelSets, capacities, targetCapacities, github.CanonicalJobInventory); err != nil {
			return err
		}
	}
	for _, target := range c.Targets {
		if _, exists := assignedTargets[folded(target.Slug)]; !exists {
			return fmt.Errorf("target %q has no GitHub registration scope", target.Slug)
		}
	}
	return nil
}

func (a GitHubApp) hasCredentialSource() bool {
	if strings.TrimSpace(a.PrivateKeyFile) != "" {
		return true
	}
	return strings.TrimSpace(a.KeychainService) != "" && strings.TrimSpace(a.KeychainAccount) != ""
}

func (c Config) authorityProfileCapacities() map[string]int {
	capacities := make(map[string]int, len(c.Linux.Profiles)+2)
	for _, profile := range c.Linux.Profiles {
		limit := c.Linux.MaxInstances
		limit = min(limit, c.Linux.Capacity.CPU/profile.Resources.CPU)
		limit = min(limit, c.Linux.Capacity.MemoryMiB/profile.Resources.MemoryMiB)
		capacities[profile.ID] = c.budgetedCapacity(limit, profile)
	}
	if c.MacOS.Enabled {
		capacities[c.MacOS.Builder.ID] = c.budgetedCapacity(c.MacOS.Builder.MaxActive, c.MacOS.Builder)
		capacities[c.MacOS.Maestro.ID] = c.budgetedCapacity(c.MacOS.Maestro.MaxActive, c.MacOS.Maestro)
	}
	return capacities
}

// budgetedCapacity keeps ADR 0015's truthful-capacity promise on a budgeted
// node. A scale set advertising slots the envelope can never hold is advertising
// capacity this node cannot serve, and under canonicalJobInventory that number
// is what GitHub is told. An unset budget leaves the figure exactly as it was.
func (c Config) budgetedCapacity(limit int, raw Profile) int {
	if c.HostBudget == (Resources{}) {
		return limit
	}
	profile := raw.normalized()
	return min(limit, c.HostBudget.CPU/profile.Resources.CPU, c.HostBudget.MemoryMiB/profile.Resources.MemoryMiB)
}

func parseScopeURL(scope GitHubScope) (*url.URL, string, error) {
	parsed, err := url.Parse(scope.ConfigURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", errors.New("configUrl must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	parts := pathParts(parsed.Path)
	switch scope.Kind {
	case ScopeRepository:
		if len(parts) != 2 {
			return nil, "", errors.New("repository scope configUrl must identify owner/repository")
		}
		if scope.RunnerGroup != "" && !strings.EqualFold(scope.RunnerGroup, "default") {
			return nil, "", errors.New("repository scope supports only the default runner group")
		}
	case ScopeOrganization:
		if len(parts) != 1 {
			return nil, "", errors.New("organization scope configUrl must identify one organization")
		}
	default:
		return nil, "", fmt.Errorf("unsupported scope kind %q", scope.Kind)
	}
	key := strings.ToLower(parsed.Scheme + "://" + parsed.Host + "/" + strings.Join(parts, "/"))
	return parsed, key, nil
}

func validateScopeTargets(scope GitHubScope, parsed *url.URL, targets, assigned map[string]string) error {
	if len(scope.Targets) == 0 {
		return fmt.Errorf("GitHub scope %q must own at least one target", scope.Name)
	}
	parts := pathParts(parsed.Path)
	if scope.Kind == ScopeRepository && len(scope.Targets) != 1 {
		return fmt.Errorf("repository scope %q must own exactly one target", scope.Name)
	}
	for _, target := range scope.Targets {
		key := folded(target)
		canonical, exists := targets[key]
		if !exists {
			return fmt.Errorf("GitHub scope %q owns unknown target %q", scope.Name, target)
		}
		if owner, exists := assigned[key]; exists {
			return fmt.Errorf("target %q is owned by both GitHub scopes %q and %q", canonical, owner, scope.Name)
		}
		switch scope.Kind {
		case ScopeRepository:
			if !strings.EqualFold(strings.Join(parts, "/"), canonical) {
				return fmt.Errorf("repository scope %q URL does not match target %q", scope.Name, canonical)
			}
		case ScopeOrganization:
			owner, _, _ := strings.Cut(canonical, "/")
			if !strings.EqualFold(parts[0], owner) {
				return fmt.Errorf("organization scope %q cannot own target %q", scope.Name, canonical)
			}
		}
		assigned[key] = scope.Name
	}
	return nil
}

// validateScopeScaleSets checks one scope's opt-in list. A scope exposes the
// variants it lists and no others: requiring every enabled profile in every
// scope multiplied scale sets, sessions, and GitHub traffic by the size of the
// variant matrix for no operator benefit (ADR 0032). What still binds is that
// each listed variant exists, appears once, and carries labels that resolve to
// it, and that a scope exposes at least one variant — a scope with no scale set
// can never receive work.
func validateScopeScaleSets(scope GitHubScope, labelSets map[string]LabelSet, capacities, targetCapacities map[string]int,
	canonicalJobInventory bool) error {
	seenProfiles := make(map[string]struct{}, len(scope.ScaleSets))
	seenNames := make(map[string]struct{}, len(scope.ScaleSets))
	scopeCapacity := 0
	for _, target := range scope.Targets {
		scopeCapacity += targetCapacities[folded(target)]
	}
	for _, scaleSet := range scope.ScaleSets {
		_, known := labelSets[scaleSet.Profile]
		if !known || strings.TrimSpace(scaleSet.Name) == "" || scaleSet.ID < 0 || scaleSet.MaxCapacity <= 0 {
			return fmt.Errorf("invalid scale set for profile %q in GitHub scope %q", scaleSet.Profile, scope.Name)
		}
		runtimeCapacity := min(capacities[scaleSet.Profile], scopeCapacity)
		if canonicalJobInventory && scaleSet.MaxCapacity != runtimeCapacity {
			return fmt.Errorf("scale set %q in GitHub scope %q requires truthful capacity: maxCapacity=%d, runtime capacity=%d",
				scaleSet.Name, scope.Name, scaleSet.MaxCapacity, runtimeCapacity)
		}
		if !canonicalJobInventory && scaleSet.MaxCapacity <= runtimeCapacity {
			return fmt.Errorf("scale set %q in GitHub scope %q requires queue lookahead until canonicalJobInventory is enabled: maxCapacity=%d, runtime capacity=%d",
				scaleSet.Name, scope.Name, scaleSet.MaxCapacity, runtimeCapacity)
		}
		// ADR 0034's shared-label amendment states this as a precondition, not a
		// recommendation: GitHub splits a shared queue by each set's advertised
		// capacity, so two nodes both inflating that number for lookahead would
		// have GitHub split by fiction. Truthful capacity is what makes the split
		// mean anything, and it arrives with the inventory flag.
		if scaleSet.SharedLabels && !canonicalJobInventory {
			return fmt.Errorf("scale set %q in GitHub scope %q declares sharedLabels, which requires canonicalJobInventory and truthful capacity",
				scaleSet.Name, scope.Name)
		}
		if _, duplicate := seenProfiles[scaleSet.Profile]; duplicate {
			return fmt.Errorf("duplicate scale set profile %q in GitHub scope %q", scaleSet.Profile, scope.Name)
		}
		name := folded(scaleSet.Name)
		if _, duplicate := seenNames[name]; duplicate {
			return fmt.Errorf("duplicate scale set name %q in GitHub scope %q", scaleSet.Name, scope.Name)
		}
		if err := validateScaleSetLabels(scope.Name, scaleSet, labelSets); err != nil {
			return err
		}
		seenProfiles[scaleSet.Profile] = struct{}{}
		seenNames[name] = struct{}{}
	}
	if len(seenProfiles) == 0 {
		return fmt.Errorf("GitHub scope %q must expose at least one profile scale set", scope.Name)
	}
	return nil
}

// validateScaleSetLabels proves a scale set is reachable and unambiguous. It
// must advertise self-hosted and at least one name of its own profile — the
// canonical resource label or any alias, so a file written before ADR 0032
// still passes — and it must never advertise another profile's canonical label,
// which would route that shape's work to the wrong vector.
func validateScaleSetLabels(scopeName string, scaleSet ScaleSet, labelSets map[string]LabelSet) error {
	set := labelSets[scaleSet.Profile]
	seen := make(map[string]struct{}, len(scaleSet.Labels))
	hasSelfHosted, hasRoute := false, false
	for _, label := range scaleSet.Labels {
		key := folded(label)
		if key == "" {
			return fmt.Errorf("scale set %q in GitHub scope %q has an empty label", scaleSet.Name, scopeName)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("scale set %q in GitHub scope %q has duplicate label %q", scaleSet.Name, scopeName, label)
		}
		if err := checkForeignCanonicalLabel(scopeName, scaleSet, labelSets, label); err != nil {
			return err
		}
		seen[key] = struct{}{}
		hasSelfHosted = hasSelfHosted || key == "self-hosted"
		hasRoute = hasRoute || set.Contains(label)
	}
	if !hasSelfHosted || !hasRoute {
		return fmt.Errorf("scale set %q in GitHub scope %q requires self-hosted and a label of profile %q (canonical %q)",
			scaleSet.Name, scopeName, scaleSet.Profile, set.Canonical)
	}
	return nil
}

func checkForeignCanonicalLabel(scopeName string, scaleSet ScaleSet, labelSets map[string]LabelSet, label string) error {
	for id, other := range labelSets {
		if id != scaleSet.Profile && strings.EqualFold(label, other.Canonical) {
			return fmt.Errorf("scale set %q in GitHub scope %q advertises %q, the canonical label of profile %s",
				scaleSet.Name, scopeName, label, id)
		}
	}
	return nil
}

func pathParts(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func folded(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
