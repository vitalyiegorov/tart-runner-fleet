package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
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
	MaxActive int       `json:"maxActive,omitempty"`
}

func (p Profile) normalized() Profile {
	if p.Resources.CPU == 0 {
		p.Resources = Resources{CPU: p.CPU, MemoryMiB: p.MemoryMiB}
	}
	return p
}

type Target struct {
	Type                string   `json:"type"`
	Slug                string   `json:"slug"`
	MaxActive           int      `json:"maxActive"`
	DefaultLinuxProfile string   `json:"defaultLinuxProfile,omitempty"`
	RunnerLabels        []string `json:"runnerLabels,omitempty"`
}

type Linux struct {
	BaseVM       string
	VMPrefix     string
	MaxInstances int
	Capacity     Resources
	Profiles     []Profile
}

type MacOS struct {
	Enabled  bool
	BaseVM   string
	VMPrefix string
	Builder  Profile
	Maestro  Profile
}

type Timeouts struct {
	GitHub time.Duration
	Tart   time.Duration
	Boot   time.Duration
}

type Guards struct {
	MinFreeDiskGiB int
}

type GitHub struct {
	ConfigURL       string     `json:"configUrl"`
	Owner           string     `json:"owner"`
	ClientID        string     `json:"clientId"`
	InstallationID  int64      `json:"installationId"`
	KeychainService string     `json:"keychainService"`
	KeychainAccount string     `json:"keychainAccount"`
	ScaleSets       []ScaleSet `json:"scaleSets"`
}

type ScaleSet struct {
	Profile     string `json:"profile"`
	ID          int    `json:"id"`
	MaxCapacity int    `json:"maxCapacity"`
}

type Config struct {
	PollInterval   time.Duration
	ReservationAge time.Duration
	Linux          Linux
	MacOS          MacOS
	GitHub         GitHub
	Timeouts       Timeouts
	Guards         Guards
	Targets        []Target
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
	MinFreeDiskGiB            int       `json:"minFreeDiskGb"`
	GitHubTimeoutSeconds      int       `json:"githubTimeoutSeconds"`
	TartControlTimeoutSeconds int       `json:"tartControlTimeoutSeconds"`
	BootTimeoutSeconds        int       `json:"bootTimeoutSeconds"`
	MacOSBurst                struct {
		Enabled  bool    `json:"enabled"`
		BaseVM   string  `json:"baseVm"`
		VMPrefix string  `json:"vmPrefix"`
		Builder  Profile `json:"builder"`
		Maestro  Profile `json:"maestro"`
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
			Capacity: Resources{CPU: w.MaxLinuxCPU, MemoryMiB: w.MaxLinuxMemoryMiB}, Profiles: normalizeProfiles(w.LinuxProfiles)},
		MacOS: MacOS{Enabled: w.MacOSBurst.Enabled, BaseVM: w.MacOSBurst.BaseVM, VMPrefix: w.MacOSBurst.VMPrefix,
			Builder: w.MacOSBurst.Builder.normalized(), Maestro: w.MacOSBurst.Maestro.normalized()},
		GitHub:   w.GitHub,
		Timeouts: Timeouts{GitHub: secondsOr(w.GitHubTimeoutSeconds, 15), Tart: secondsOr(w.TartControlTimeoutSeconds, 45), Boot: secondsOr(w.BootTimeoutSeconds, 180)},
		Guards:   Guards{MinFreeDiskGiB: w.MinFreeDiskGiB}, Targets: w.Targets,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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

func normalizeProfiles(in []Profile) []Profile {
	out := make([]Profile, len(in))
	for i, p := range in {
		out[i] = p.normalized()
	}
	return out
}

func Default() Config {
	return Config{
		PollInterval: 20 * time.Second, ReservationAge: 5 * time.Minute,
		Linux: Linux{BaseVM: "linux-runner-base", VMPrefix: "gha-linux", MaxInstances: 4,
			Capacity: Resources{CPU: 8, MemoryMiB: 16384}, Profiles: []Profile{
				{ID: "small", Label: "linux-small", Resources: Resources{CPU: 1, MemoryMiB: 2048}},
				{ID: "medium", Label: "linux-medium", Resources: Resources{CPU: 2, MemoryMiB: 4096}},
				{ID: "large", Label: "linux-large", Resources: Resources{CPU: 4, MemoryMiB: 8192}},
			}},
		MacOS: MacOS{Enabled: true, BaseVM: "macos-tartelet-base", VMPrefix: "gha-macos",
			Builder: Profile{ID: "builder", Label: "macos-builder", Resources: Resources{CPU: 8, MemoryMiB: 12288}, MaxActive: 1},
			Maestro: Profile{ID: "maestro", Label: "macos-maestro", Resources: Resources{CPU: 4, MemoryMiB: 7168}, MaxActive: 2}},
		Timeouts: Timeouts{GitHub: 15 * time.Second, Tart: 45 * time.Second, Boot: 3 * time.Minute},
		Guards:   Guards{MinFreeDiskGiB: 60},
		Targets:  []Target{{Type: "repo", Slug: "owner/repo", MaxActive: 4}},
	}
}

func (c Config) Clone() Config {
	out := c
	out.Linux.Profiles = append([]Profile(nil), c.Linux.Profiles...)
	out.GitHub.ScaleSets = append([]ScaleSet(nil), c.GitHub.ScaleSets...)
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
	if c.Linux.BaseVM == "" || c.Linux.VMPrefix == "" {
		return errors.New("linux base VM and prefix are required")
	}
	if c.Linux.MaxInstances < 1 || c.Linux.MaxInstances > 4 {
		return errors.New("linux max instances must be between 1 and 4")
	}
	if c.Linux.Capacity.CPU <= 0 || c.Linux.Capacity.MemoryMiB <= 0 {
		return errors.New("linux capacity must be positive")
	}
	if c.Guards.MinFreeDiskGiB <= 0 {
		return errors.New("disk reserve must be positive")
	}
	seenProfiles := map[string]struct{}{}
	for _, raw := range c.Linux.Profiles {
		p := raw.normalized()
		if p.ID == "" || p.Label == "" || p.Resources.CPU <= 0 || p.Resources.MemoryMiB <= 0 {
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
	if c.GitHub.ConfigURL == "" || c.GitHub.Owner == "" || c.GitHub.ClientID == "" || c.GitHub.InstallationID <= 0 ||
		c.GitHub.KeychainService == "" || c.GitHub.KeychainAccount == "" {
		return errors.New("complete GitHub App and Keychain configuration is required")
	}
	profiles := make(map[string]struct{}, len(c.Linux.Profiles)+2)
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
		if !known || scaleSet.ID <= 0 || scaleSet.MaxCapacity < 0 {
			return fmt.Errorf("invalid scale set for profile %q", scaleSet.Profile)
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
