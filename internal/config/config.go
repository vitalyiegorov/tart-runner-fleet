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
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Resources Resources `json:"-"`
	CPU       int       `json:"cpu"`
	MemoryMiB int       `json:"memoryMb"`
	// DiskGiB is a minimum virtual-disk capacity. Zero preserves the base VM
	// for backward-compatible observe-mode decoding; authority requires an
	// explicit Linux floor so ephemeral runners cannot inherit a tiny image.
	DiskGiB   int `json:"diskGb,omitempty"`
	MaxActive int `json:"maxActive,omitempty"`
}

func (p Profile) normalized() Profile {
	if p.Resources.CPU == 0 {
		p.Resources = Resources{CPU: p.CPU, MemoryMiB: p.MemoryMiB}
	}
	return p
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
// cross this boundary or the GitHub App installation that owns it.
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
}

type Config struct {
	PollInterval    time.Duration
	ReservationAge  time.Duration
	Linux           Linux
	MacOS           MacOS
	GitHub          GitHub
	Timeouts        Timeouts
	Guards          Guards
	SessionRecovery SessionRecovery
	Targets         []Target
}

type wireConfig struct {
	BaseVM                    string    `json:"baseVm"`
	VMPrefix                  string    `json:"vmPrefix"`
	PollSeconds               int       `json:"pollSeconds"`
	MaxLinuxWhenMacOSIdle     int       `json:"maxLinuxWhenMacosIdle"`
	MaxLinuxCPU               int       `json:"maxLinuxCpu"`
	MaxLinuxMemoryMiB         int       `json:"maxLinuxMemoryMb"`
	LinuxReservationAgeSecs   int       `json:"linuxReservationAgeSeconds"`
	LinuxProfiles             []Profile `json:"linuxProfiles"`
	LinuxNestedVirtualization bool      `json:"linuxNestedVirtualization,omitempty"`
	MinFreeDiskGiB            int       `json:"minFreeDiskGb"`
	MinAvailableMemoryMiB     int       `json:"minAvailableMemoryMb,omitempty"`
	MaxSwapUsedMiB            int       `json:"maxSwapUsedMb,omitempty"`
	MaxLoadAverage            float64   `json:"maxLoadAverage,omitempty"`
	MinCPUIdlePercent         float64   `json:"minCpuIdlePercent,omitempty"`
	PressureMemoryAccounting  bool      `json:"pressureMemoryAccounting,omitempty"`
	ElasticHostEnvelope       bool      `json:"elasticHostEnvelope,omitempty"`
	GitHubTimeoutSeconds      int       `json:"githubTimeoutSeconds"`
	TartControlTimeoutSeconds int       `json:"tartControlTimeoutSeconds"`
	BootTimeoutSeconds        int       `json:"bootTimeoutSeconds"`
	AssignedTimeoutSeconds    int       `json:"assignedTimeoutSeconds,omitempty"`
	// GitHubSessionMaxIngestFailures and GitHubSessionFailureWindowSeconds are
	// omitted while they hold the shipped defaults so a rewritten file stays
	// decodable by older strict releases.
	GitHubSessionMaxIngestFailures    int `json:"githubSessionMaxIngestFailures,omitempty"`
	GitHubSessionFailureWindowSeconds int `json:"githubSessionFailureWindowSeconds,omitempty"`
	MacOSBurst                        struct {
		Enabled                bool                 `json:"enabled"`
		AdmissionPolicy        MacOSAdmissionPolicy `json:"admissionPolicy,omitempty"`
		MixedPlatformAdmission bool                 `json:"mixedPlatformAdmission,omitempty"`
		MixedProfileCohorts    bool                 `json:"mixedProfileCohorts,omitempty"`
		BaseVM                 string               `json:"baseVm"`
		VMPrefix               string               `json:"vmPrefix"`
		Builder                Profile              `json:"builder"`
		Maestro                Profile              `json:"maestro"`
		RootDiskOptions        string               `json:"rootDiskOptions,omitempty"`
		SharedDirectoryPath    string               `json:"sharedDirectoryPath,omitempty"`
		NestedVirtualization   bool                 `json:"nestedVirtualization,omitempty"`
	} `json:"macosBurst"`
	GitHub  GitHub   `json:"github"`
	Targets []Target `json:"targets"`
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
		Linux: Linux{BaseVM: w.BaseVM, VMPrefix: w.VMPrefix, MaxInstances: w.MaxLinuxWhenMacOSIdle,
			Capacity: Resources{CPU: w.MaxLinuxCPU, MemoryMiB: w.MaxLinuxMemoryMiB}, Profiles: normalizeProfiles(w.LinuxProfiles),
			NestedVirtualization: w.LinuxNestedVirtualization},
		MacOS: MacOS{Enabled: w.MacOSBurst.Enabled, AdmissionPolicy: normalizeMacOSAdmissionPolicy(w.MacOSBurst.AdmissionPolicy),
			MixedPlatformAdmission: w.MacOSBurst.MixedPlatformAdmission, MixedProfileCohorts: w.MacOSBurst.MixedProfileCohorts,
			BaseVM: w.MacOSBurst.BaseVM, VMPrefix: w.MacOSBurst.VMPrefix,
			Builder: w.MacOSBurst.Builder.normalized(), Maestro: w.MacOSBurst.Maestro.normalized(),
			RootDiskOptions: w.MacOSBurst.RootDiskOptions, SharedDirectoryPath: w.MacOSBurst.SharedDirectoryPath,
			NestedVirtualization: w.MacOSBurst.NestedVirtualization},
		GitHub: w.GitHub,
		Timeouts: Timeouts{GitHub: secondsOr(w.GitHubTimeoutSeconds, 15), Tart: secondsOr(w.TartControlTimeoutSeconds, 45),
			Boot: secondsOr(w.BootTimeoutSeconds, 180), Assigned: secondsOr(w.AssignedTimeoutSeconds, 900)},
		Guards: normalizeGuards(w), SessionRecovery: normalizeSessionRecovery(w), Targets: normalizeTargets(w.Targets),
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

	wire := wireConfig{
		BaseVM: cfg.Linux.BaseVM, VMPrefix: cfg.Linux.VMPrefix,
		PollSeconds: pollSeconds, MaxLinuxWhenMacOSIdle: cfg.Linux.MaxInstances,
		MaxLinuxCPU: cfg.Linux.Capacity.CPU, MaxLinuxMemoryMiB: cfg.Linux.Capacity.MemoryMiB,
		LinuxReservationAgeSecs: reservationSeconds, LinuxProfiles: encodeProfiles(cfg.Linux.Profiles),
		MinFreeDiskGiB: cfg.Guards.MinFreeDiskGiB, MinAvailableMemoryMiB: cfg.Guards.MinAvailableMemoryMiB,
		MaxSwapUsedMiB: cfg.Guards.MaxSwapUsedMiB, MaxLoadAverage: cfg.Guards.MaxLoadAverage,
		MinCPUIdlePercent: cfg.Guards.MinCPUIdlePercent, PressureMemoryAccounting: cfg.Guards.PressureMemoryAccounting,
		ElasticHostEnvelope:       cfg.Guards.ElasticHostEnvelope,
		GitHubTimeoutSeconds:      githubSeconds,
		TartControlTimeoutSeconds: tartSeconds, BootTimeoutSeconds: bootSeconds, AssignedTimeoutSeconds: assignedSeconds,
		GitHub: cfg.GitHub, Targets: normalizeTargets(cfg.Targets),
	}
	// Only non-default recovery bounds are emitted; see wireConfig.
	if cfg.SessionRecovery.MaxConsecutiveFailures != defaultSessionMaxIngestFailures {
		wire.GitHubSessionMaxIngestFailures = cfg.SessionRecovery.MaxConsecutiveFailures
	}
	if cfg.SessionRecovery.FailureWindow != defaultSessionFailureWindow {
		wire.GitHubSessionFailureWindowSeconds = sessionWindowSeconds
	}
	wire.MacOSBurst.Enabled = cfg.MacOS.Enabled
	if cfg.MacOS.AdmissionPolicy == MacOSAdmissionExclusive {
		wire.MacOSBurst.AdmissionPolicy = MacOSAdmissionExclusive
	}
	wire.MacOSBurst.MixedPlatformAdmission = cfg.MacOS.MixedPlatformAdmission
	wire.MacOSBurst.MixedProfileCohorts = cfg.MacOS.MixedProfileCohorts
	wire.MacOSBurst.BaseVM = cfg.MacOS.BaseVM
	wire.MacOSBurst.VMPrefix = cfg.MacOS.VMPrefix
	wire.MacOSBurst.Builder = encodeProfile(cfg.MacOS.Builder)
	wire.MacOSBurst.Maestro = encodeProfile(cfg.MacOS.Maestro)
	wire.MacOSBurst.RootDiskOptions = cfg.MacOS.RootDiskOptions
	wire.MacOSBurst.SharedDirectoryPath = cfg.MacOS.SharedDirectoryPath
	wire.LinuxNestedVirtualization = cfg.Linux.NestedVirtualization

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(wire); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
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
		Targets: []Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4, SchedulingClass: domain.SchedulingStandard}},
	}
}

func (c Config) Clone() Config {
	out := c
	out.Linux.Profiles = append([]Profile(nil), c.Linux.Profiles...)
	out.GitHub.ScaleSets = append([]ScaleSet(nil), c.GitHub.ScaleSets...)
	for i := range out.GitHub.ScaleSets {
		out.GitHub.ScaleSets[i].Labels = append([]string(nil), c.GitHub.ScaleSets[i].Labels...)
	}
	out.GitHub.Installations = append([]GitHubInstallation(nil), c.GitHub.Installations...)
	out.GitHub.Scopes = append([]GitHubScope(nil), c.GitHub.Scopes...)
	for i := range out.GitHub.Scopes {
		out.GitHub.Scopes[i].Targets = append([]string(nil), c.GitHub.Scopes[i].Targets...)
		out.GitHub.Scopes[i].ScaleSets = append([]ScaleSet(nil), c.GitHub.Scopes[i].ScaleSets...)
		for j := range out.GitHub.Scopes[i].ScaleSets {
			out.GitHub.Scopes[i].ScaleSets[j].Labels = append([]string(nil), c.GitHub.Scopes[i].ScaleSets[j].Labels...)
		}
	}
	out.Targets = append([]Target(nil), c.Targets...)
	for i := range out.Targets {
		out.Targets[i].RunnerLabels = append([]string(nil), c.Targets[i].RunnerLabels...)
	}
	return out
}

func (c Config) Validate() error {
	if c.PollInterval <= 0 || c.ReservationAge <= 0 {
		return errors.New("intervals must be positive")
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
		}
	}
	return nil
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

	profiles := c.authorityProfiles()
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
		if err := validateScopeScaleSets(scope, profiles, capacities, targetCapacities, github.CanonicalJobInventory); err != nil {
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

func (c Config) authorityProfiles() map[string]string {
	profiles := make(map[string]string, len(c.Linux.Profiles)+2)
	for _, profile := range c.Linux.Profiles {
		profiles[profile.ID] = profile.Label
	}
	if c.MacOS.Enabled {
		profiles[c.MacOS.Builder.ID] = c.MacOS.Builder.Label
		profiles[c.MacOS.Maestro.ID] = c.MacOS.Maestro.Label
	}
	return profiles
}

func (c Config) authorityProfileCapacities() map[string]int {
	capacities := make(map[string]int, len(c.Linux.Profiles)+2)
	for _, profile := range c.Linux.Profiles {
		limit := c.Linux.MaxInstances
		limit = min(limit, c.Linux.Capacity.CPU/profile.Resources.CPU)
		limit = min(limit, c.Linux.Capacity.MemoryMiB/profile.Resources.MemoryMiB)
		capacities[profile.ID] = limit
	}
	if c.MacOS.Enabled {
		capacities[c.MacOS.Builder.ID] = c.MacOS.Builder.MaxActive
		capacities[c.MacOS.Maestro.ID] = c.MacOS.Maestro.MaxActive
	}
	return capacities
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

func validateScopeScaleSets(scope GitHubScope, profiles map[string]string, capacities, targetCapacities map[string]int,
	canonicalJobInventory bool) error {
	seenProfiles := make(map[string]struct{}, len(scope.ScaleSets))
	seenNames := make(map[string]struct{}, len(scope.ScaleSets))
	scopeCapacity := 0
	for _, target := range scope.Targets {
		scopeCapacity += targetCapacities[folded(target)]
	}
	for _, scaleSet := range scope.ScaleSets {
		route, known := profiles[scaleSet.Profile]
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
		if _, duplicate := seenProfiles[scaleSet.Profile]; duplicate {
			return fmt.Errorf("duplicate scale set profile %q in GitHub scope %q", scaleSet.Profile, scope.Name)
		}
		name := folded(scaleSet.Name)
		if _, duplicate := seenNames[name]; duplicate {
			return fmt.Errorf("duplicate scale set name %q in GitHub scope %q", scaleSet.Name, scope.Name)
		}
		if err := validateScaleSetLabels(scope.Name, scaleSet, route); err != nil {
			return err
		}
		seenProfiles[scaleSet.Profile] = struct{}{}
		seenNames[name] = struct{}{}
	}
	if len(seenProfiles) != len(profiles) {
		return fmt.Errorf("every enabled profile requires one scale set in GitHub scope %q", scope.Name)
	}
	return nil
}

func validateScaleSetLabels(scopeName string, scaleSet ScaleSet, route string) error {
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
		seen[key] = struct{}{}
		hasSelfHosted = hasSelfHosted || key == "self-hosted"
		hasRoute = hasRoute || strings.EqualFold(label, route)
	}
	if !hasSelfHosted || !hasRoute {
		return fmt.Errorf("scale set %q in GitHub scope %q requires self-hosted and profile route labels", scaleSet.Name, scopeName)
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
