package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// capabilityGrammar recognises a guest capability identifier: a lowercase,
// hyphen-separated name such as `redroid-android` or `container-runtime`. It is
// deliberately narrower than the runner-label grammar. A capability is compared
// for exact equality across two nodes' configuration files, so a vocabulary that
// admitted case or punctuation variants would let `Redroid-Android` and
// `redroid_android` describe the same image without matching each other, which
// is precisely the class of mistake this check exists to catch.
var capabilityGrammar = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// legacyScaleSetScope names the pre-scope flat list in an error message. It is
// not a GitHub scope; it is where the scale sets of a configuration written
// before the scoped model live, and they are checked exactly the same way.
const legacyScaleSetScope = "github.scaleSets"

// ScopedScaleSet is one scale set together with a printable name for the place
// it was declared. It is the unit both the per-node capability check and the
// cross-node parity rule iterate over, so neither has to know that a node's
// scale sets live in two different shapes.
type ScopedScaleSet struct {
	// Scope is the printable location, not the GitHub scope name: it is
	// `github.scaleSets` for the legacy flat list.
	Scope string
	// ScopeName is the GitHub scope name, empty for the legacy flat list. It is
	// the identity half of ADR 0034 §2's `(scope, scale-set name)` ownership pair.
	ScopeName string
	ScaleSet  ScaleSet
}

// Name is the scale set's own name, falling back to its profile so a legacy
// entry that carries no name is still nameable in an error.
func (s ScopedScaleSet) Name() string {
	if name := strings.TrimSpace(s.ScaleSet.Name); name != "" {
		return name
	}
	return s.ScaleSet.Profile
}

// ScopedScaleSets returns every scale set this node owns, in file order: the
// legacy flat list first, then each scope's list.
func (c Config) ScopedScaleSets() []ScopedScaleSet {
	sets := make([]ScopedScaleSet, 0, len(c.GitHub.ScaleSets)+len(c.GitHub.Scopes))
	for _, scaleSet := range c.GitHub.ScaleSets {
		sets = append(sets, ScopedScaleSet{Scope: legacyScaleSetScope, ScaleSet: scaleSet})
	}
	for _, scope := range c.GitHub.Scopes {
		for _, scaleSet := range scope.ScaleSets {
			sets = append(sets, ScopedScaleSet{Scope: fmt.Sprintf("GitHub scope %q", scope.Name),
				ScopeName: scope.Name, ScaleSet: scaleSet})
		}
	}
	return sets
}

func (s ScaleSet) clone() ScaleSet {
	out := s
	out.Labels = append([]string(nil), s.Labels...)
	out.RequiresCapabilities = append([]string(nil), s.RequiresCapabilities...)
	return out
}

// baseImage is one of a node's two guest images together with what its operator
// says it provides. A node has two, and each answers only for the scale sets
// whose profile it boots: asking the Linux image whether it carries `xcode` is
// not a stricter check, it is the wrong question.
type baseImage struct {
	// Platform is the lowercase platform name used in messages and as the key the
	// cross-node rule partitions by.
	Platform string
	VM       string
	declares map[string]struct{}
}

func (b baseImage) declaration() string {
	return fmt.Sprintf("%s base image %q", b.Platform, b.VM)
}

const (
	capabilityPlatformLinux = "linux"
	capabilityPlatformMacOS = "macOS"
)

// baseImages reads both declarations and proves each is a well-formed set before
// anything is compared against it. A malformed declaration is reported as the
// declaration error it is rather than as the missing capability it also causes.
func (c Config) baseImages() (map[string]baseImage, error) {
	images := make(map[string]baseImage, 2)
	for _, image := range []baseImage{
		{Platform: capabilityPlatformLinux, VM: c.Linux.BaseVM},
		{Platform: capabilityPlatformMacOS, VM: c.MacOS.BaseVM},
	} {
		declared := c.Linux.BaseImageCapabilities
		if image.Platform == capabilityPlatformMacOS {
			declared = c.MacOS.BaseImageCapabilities
		}
		set, err := capabilitySet(image.Platform+" base image", declared)
		if err != nil {
			return nil, err
		}
		image.declares = set
		images[image.Platform] = image
	}
	return images, nil
}

// capabilitySet validates one list of capability identifiers and indexes it.
// Syntax, emptiness, and duplication are all rejected here, on the declaring and
// the requiring side alike, because a list that cannot be compared exactly is
// worth nothing to a rule whose whole content is set comparison.
func capabilitySet(owner string, capabilities []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !capabilityGrammar.MatchString(capability) {
			return nil, fmt.Errorf("%s declares capability %q, which is not a lowercase identifier "+
				"matching %s", owner, capability, capabilityGrammar.String())
		}
		if _, duplicate := set[capability]; duplicate {
			return nil, fmt.Errorf("%s lists capability %q twice", owner, capability)
		}
		set[capability] = struct{}{}
	}
	return set, nil
}

// capabilityPlatform answers which of the node's two base images boots a given
// profile. The answer is the existing profile resolution and nothing new: a
// scale set's profile is a Linux profile, the macOS builder, or the macOS
// maestro, and on a node with `macosBurst.enabled: false` the last two do not
// exist at all.
func (c Config) capabilityPlatform(profile string) (string, bool) {
	for _, declared := range c.Linux.Profiles {
		if declared.ID == profile {
			return capabilityPlatformLinux, true
		}
	}
	if c.MacOS.Enabled && (profile == c.MacOS.Builder.ID || profile == c.MacOS.Maestro.ID) {
		return capabilityPlatformMacOS, true
	}
	return "", false
}

// validateCapabilities refuses a scale set that requires something the image for
// its own platform does not provide. It sits beside the existing profile and
// scale-set cross-checks for the reason ADR 0034's amendment gives: this is a
// configuration mistake, it costs nothing at runtime, and `fleet config validate`
// reaches it before the daemon starts.
func (c Config) validateCapabilities() error {
	images, err := c.baseImages()
	if err != nil {
		return err
	}
	for _, scoped := range c.ScopedScaleSets() {
		required, err := capabilitySet(fmt.Sprintf("scale set %q in %s", scoped.Name(), scoped.Scope),
			scoped.ScaleSet.RequiresCapabilities)
		if err != nil {
			return err
		}
		if len(required) == 0 {
			continue
		}
		platform, known := c.capabilityPlatform(scoped.ScaleSet.Profile)
		if !known {
			return fmt.Errorf("scale set %q in %s requires guest capabilities, but its profile %q is not a "+
				"profile this node declares, so no base image answers for it",
				scoped.Name(), scoped.Scope, scoped.ScaleSet.Profile)
		}
		if err := images[platform].check(scoped, required); err != nil {
			return err
		}
	}
	return nil
}

// check compares one scale set's requirements against this image, naming the
// first capability the image does not carry. The message states every fact an
// operator needs to act without opening another file: which scale set, in which
// scope, which capability, which labels that scale set advertises — the label is
// the interface a consumer actually names — and which of the node's two base
// images was consulted.
func (b baseImage) check(scoped ScopedScaleSet, required map[string]struct{}) error {
	for _, capability := range sortedCapabilities(required) {
		if _, declared := b.declares[capability]; declared {
			continue
		}
		return fmt.Errorf("scale set %q in %s requires capability %q, which the %s does not declare; "+
			"that scale set advertises [%s]",
			scoped.Name(), scoped.Scope, capability, b.declaration(), strings.Join(scoped.ScaleSet.Labels, " "))
	}
	return nil
}

func sortedCapabilities(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for capability := range set {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}
