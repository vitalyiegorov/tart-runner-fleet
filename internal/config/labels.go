package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// A canonical runner label states a profile's resource vector and nothing else:
// `trf-<os>-<arch>-<cpu>x<ramGiB>`, so `trf-linux-arm64-4x8` is four vCPU and
// eight GiB. The vocabulary is generative — a new shape needs no new adjective
// — and it stays free of priority as ADR 0012 requires, because the scheduler
// reads the configured vector and never a label. Role and tier names such as
// `linux-large` survive as aliases that resolve to the same profile and
// therefore to the same scale set (ADR 0032).
const canonicalLabelPrefix = "trf"

// guestArchARM64 and guestArchAMD64 are the architecture component of every
// canonical label, and the whole vocabulary a node may declare. ADR 0032 spelled
// the component out "so a host of another architecture could never advertise its
// shapes under these names", and ADR 0034 §4 made it a property of the node: a
// Tart host builds on Apple's Virtualization framework and boots arm64 guests, a
// container node on x86_64 runs amd64 ones.
//
// The vocabulary is closed on purpose. An arch-pinned consumer asks for a
// canonical label by name, so a node spelling the same machine `x86_64` would
// publish a second name for one architecture and no workflow could ask for both.
const (
	guestArchARM64 = "arm64"
	guestArchAMD64 = "amd64"
)

// defaultGuestArch is what a node that declares nothing boots. It is arm64
// because every node this fleet has ever run is Apple silicon, so an absent
// declaration keeps deriving the labels those nodes already advertise.
const defaultGuestArch = guestArchARM64

// canonicalLinuxOS and canonicalMacOS are the operating-system components of a
// canonical label. They are lower-case and stable; `runs-on` matching is
// case-insensitive, but a stable spelling keeps generated labels comparable.
const (
	canonicalLinuxOS = "linux"
	canonicalMacOS   = "macos"
)

const mibPerGiB = 1024

// canonicalLabelGrammar recognises a label that claims to describe a resource
// vector. A configured label that matches it must equal the label derived from
// the profile's own vector; that is how the scheme stops a configuration from
// advertising a shape it does not provision. A label that does not match is an
// ordinary alias and is carried through untouched, which is why configurations
// written before this scheme keep validating unchanged.
var canonicalLabelGrammar = regexp.MustCompile(`^(?i:trf)-[A-Za-z0-9]+-[A-Za-z0-9_]+-[0-9]+x[0-9]+$`)

// runnerLabelGrammar is the token GitHub accepts for a scale-set label. The
// provisioning adapter enforces the same shape against the live API; checking
// it here turns a rejected provisioning run into a configuration error the
// operator sees before any GitHub mutation is attempted.
var runnerLabelGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// LabelSet is every runner label that resolves to one profile: the canonical
// resource label, then each alias in configuration order. All of them route to
// the same scale set, so a workflow may ask for a shape by its canonical name
// or by any legacy name the operator kept.
type LabelSet struct {
	Canonical string
	Aliases   []string
}

// All returns the canonical label followed by every alias.
func (s LabelSet) All() []string { return s.Advertise(nil) }

// Advertise returns the labels a scale set for this profile must publish: the
// labels the configuration already lists, then the canonical label and every
// alias that is not among them. Order is deterministic and duplicates are
// removed case-insensitively, because GitHub treats runner labels that way.
func (s LabelSet) Advertise(configured []string) []string {
	labels := make([]string, 0, len(configured)+len(s.Aliases)+1)
	seen := make(map[string]struct{}, cap(labels))
	add := func(label string) {
		key := folded(label)
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	for _, label := range configured {
		add(label)
	}
	add(s.Canonical)
	for _, alias := range s.Aliases {
		add(alias)
	}
	return labels
}

// Contains reports whether label names this profile, canonically or by alias.
func (s LabelSet) Contains(label string) bool {
	if strings.EqualFold(label, s.Canonical) {
		return true
	}
	for _, alias := range s.Aliases {
		if strings.EqualFold(label, alias) {
			return true
		}
	}
	return false
}

// canonicalLabel derives the one label that describes a vector exactly. Memory
// must be a whole number of GiB: a label that rounded would be a label that
// lies, and every shape the fleet has ever shipped is already GiB-aligned.
func canonicalLabel(operatingSystem, architecture string, resources Resources) (string, error) {
	if resources.CPU <= 0 || resources.MemoryMiB <= 0 {
		return "", errors.New("a profile needs a positive CPU and memory vector before it can be named")
	}
	if resources.MemoryMiB%mibPerGiB != 0 {
		return "", fmt.Errorf("memory %d MiB is not a whole GiB, so no canonical label describes it exactly", resources.MemoryMiB)
	}
	return fmt.Sprintf("%s-%s-%s-%dx%d", canonicalLabelPrefix, operatingSystem, architecture,
		resources.CPU, resources.MemoryMiB/mibPerGiB), nil
}

// GuestArchOrDefault is the architecture of every guest this node boots, and so
// the architecture component of every label it derives. It is always populated:
// a node that declares nothing boots Apple silicon, which is what every node
// this fleet has run does.
func (c Config) GuestArchOrDefault() string {
	if arch := strings.TrimSpace(c.GuestArch); arch != "" {
		return arch
	}
	return defaultGuestArch
}

// validateGuestArch refuses an architecture no node boots and a macOS burst on a
// node that is not Apple silicon. Both are the same rule as the vector check
// below: a derived label may not name a machine that does not exist.
func (c Config) validateGuestArch() error {
	arch := c.GuestArchOrDefault()
	if arch != guestArchARM64 && arch != guestArchAMD64 {
		return fmt.Errorf("guest architecture %q is not one of %q or %q", c.GuestArch, guestArchARM64, guestArchAMD64)
	}
	if c.MacOS.Enabled && arch != guestArchARM64 {
		return fmt.Errorf("a macOS guest is Apple silicon by construction, so a node booting %s guests cannot enable macosBurst", arch)
	}
	return nil
}

// labelSet derives one profile's complete label set and proves the
// configuration is not lying about its shape. The configured `label` is the
// first alias, so an existing file keeps routing exactly as it did while the
// canonical label is attached beside it.
func (p Profile) labelSet(operatingSystem, architecture string) (LabelSet, error) {
	profile := p.normalized()
	canonical, err := canonicalLabel(operatingSystem, architecture, profile.Resources)
	if err != nil {
		return LabelSet{}, fmt.Errorf("profile %s: %w", profile.ID, err)
	}
	set := LabelSet{Canonical: canonical}
	seen := map[string]struct{}{folded(canonical): {}}
	for _, alias := range append([]string{profile.Label}, profile.Aliases...) {
		if err := checkAlias(profile, alias, canonical); err != nil {
			return LabelSet{}, err
		}
		key := folded(alias)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		set.Aliases = append(set.Aliases, alias)
	}
	return set, nil
}

func checkAlias(profile Profile, alias, canonical string) error {
	if strings.TrimSpace(alias) == "" {
		return fmt.Errorf("profile %s has an empty runner label", profile.ID)
	}
	if !runnerLabelGrammar.MatchString(alias) {
		return fmt.Errorf("profile %s label %q is not a valid GitHub runner label", profile.ID, alias)
	}
	if canonicalLabelGrammar.MatchString(alias) && !strings.EqualFold(alias, canonical) {
		return fmt.Errorf("profile %s label %q describes a vector it does not have; %d vCPU and %d MiB is %q",
			profile.ID, alias, profile.Resources.CPU, profile.Resources.MemoryMiB, canonical)
	}
	return nil
}

// ProfileLabelSets returns the label set of every enabled profile, keyed by
// profile ID. It is total on a validated configuration, because Validate has
// already proved each vector names exactly one canonical label and that no two
// profiles claim the same name. Provisioning and runtime job matching both read
// it, so a scale set advertises exactly the names that resolve to it.
func (c Config) ProfileLabelSets() map[string]LabelSet {
	sets, _ := c.profileLabelSets()
	return sets
}

func (c Config) profileLabelSets() (map[string]LabelSet, error) {
	sets := make(map[string]LabelSet, len(c.Linux.Profiles)+2)
	claimed := make(map[string]string, len(c.Linux.Profiles)+2)
	arch := c.GuestArchOrDefault()
	for _, profile := range c.Linux.Profiles {
		if err := addLabelSet(sets, claimed, canonicalLinuxOS, arch, profile); err != nil {
			return sets, err
		}
	}
	if !c.MacOS.Enabled {
		return sets, nil
	}
	for _, profile := range []Profile{c.MacOS.Builder, c.MacOS.Maestro} {
		if err := addLabelSet(sets, claimed, canonicalMacOS, arch, profile); err != nil {
			return sets, err
		}
	}
	return sets, nil
}

// addLabelSet records one profile's labels and rejects a vocabulary collision.
// Two profiles that answer to the same name would make routing ambiguous long
// before GitHub ever saw the duplicate.
func addLabelSet(sets map[string]LabelSet, claimed map[string]string, operatingSystem, architecture string,
	profile Profile) error {
	set, err := profile.labelSet(operatingSystem, architecture)
	if err != nil {
		return err
	}
	for _, label := range set.All() {
		if owner, taken := claimed[folded(label)]; taken {
			return fmt.Errorf("runner label %q resolves to both profile %s and profile %s", label, owner, profile.ID)
		}
		claimed[folded(label)] = profile.ID
	}
	sets[profile.ID] = set
	return nil
}
