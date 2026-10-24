package config

import (
	"fmt"
	"regexp"
	"strings"
)

// toolVersionGrammar recognises one exact tool version, `tool@version`: the
// capability grammar for the name and dot-separated decimal components for the
// version. It deliberately admits no range (`22.x`), no `v` prefix and no
// suffix, because the rule over it is exact set membership. A setup action asked
// for `22.22.2` downloads 22.22.2 even when 22.22.3 or 24.x is cached beside it,
// so the only comparison that predicts a download is equality, and a vocabulary
// with two spellings of one version would let a gap read as covered.
var toolVersionGrammar = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?@[0-9]+(\.[0-9]+)*$`)

// ToolGap is one exact tool version the scale sets routed to an image require
// and the image does not carry, with every scale set that pins it.
type ToolGap struct {
	Tool       string
	RequiredBy []string
}

// ImageTools is one guest image this node boots, the exact tool versions its
// operator declared it carries, and every required version it lacks. It is the
// single value the tool-drift rule is stated over (ADR 0079): the daemon
// publishes it, `fleet doctor` renders it, and neither re-derives the verdict.
type ImageTools struct {
	// Platform is `linux` or `macOS`, the capability rule's vocabulary.
	Platform string
	VM       string
	// Declared is the image's declaration, sorted. Empty when it declares none.
	Declared []string
	// Missing is sorted by tool and empty when every requirement is carried.
	Missing []ToolGap
}

// Covered reports whether no scale set routed to this image pins a tool version
// the image does not carry, which is to say whether no job here is expected to
// download a tool the image could have provided.
func (i ImageTools) Covered() bool { return len(i.Missing) == 0 }

// Reason is the operator-facing explanation of an uncovered image, empty when
// it is covered. It names the image, each version it lacks, and the scale sets
// that pin them — what is needed to choose between rebuilding the image and
// correcting a stale requirement.
func (i ImageTools) Reason() string {
	if i.Covered() {
		return ""
	}
	tools := make([]string, 0, len(i.Missing))
	requiredBy := map[string]struct{}{}
	for _, gap := range i.Missing {
		tools = append(tools, gap.Tool)
		for _, name := range gap.RequiredBy {
			requiredBy[name] = struct{}{}
		}
	}
	return fmt.Sprintf("%s base image %q does not carry %s, pinned by scale sets %s; every such job downloads "+
		"it over the node's uplink", i.Platform, i.VM, strings.Join(tools, ", "),
		strings.Join(sortedCapabilities(requiredBy), ", "))
}

// ImageTools is every guest image this node actually boots, linux first, each
// judged against the RequiresTools of the scale sets routed to it. It produces
// rows for the same images RunnerImages does and for the same reason: an image
// nothing is routed to cannot cost a download. Decoding has already proved every
// requirement's profile resolves to one of them.
func (c Config) ImageTools() []ImageTools {
	images := make([]ImageTools, 0, 2)
	if c.ExecutesLinux() {
		images = append(images, c.imageTools(capabilityPlatformLinux, c.Linux.BaseVM, c.Linux.BaseImageTools))
	}
	if c.MacOS.Enabled {
		images = append(images, c.imageTools(capabilityPlatformMacOS, c.MacOS.BaseVM, c.MacOS.BaseImageTools))
	}
	return images
}

func (c Config) imageTools(platform, vm string, declared []string) ImageTools {
	carried := make(map[string]struct{}, len(declared))
	for _, tool := range declared {
		carried[tool] = struct{}{}
	}
	requiredBy := map[string]map[string]struct{}{}
	for _, scoped := range c.ScopedScaleSets() {
		if routed, _ := c.capabilityPlatform(scoped.ScaleSet.Profile); routed != platform {
			continue
		}
		for _, tool := range scoped.ScaleSet.RequiresTools {
			if _, ok := carried[tool]; ok {
				continue
			}
			if requiredBy[tool] == nil {
				requiredBy[tool] = map[string]struct{}{}
			}
			requiredBy[tool][scoped.Name()] = struct{}{}
		}
	}
	missing := make([]ToolGap, 0, len(requiredBy))
	for _, tool := range sortedCapabilities(toolSet(requiredBy)) {
		missing = append(missing, ToolGap{Tool: tool, RequiredBy: sortedCapabilities(requiredBy[tool])})
	}
	return ImageTools{Platform: platform, VM: vm, Declared: sortedCopy(declared), Missing: missing}
}

func toolSet(requiredBy map[string]map[string]struct{}) map[string]struct{} {
	set := make(map[string]struct{}, len(requiredBy))
	for tool := range requiredBy {
		set[tool] = struct{}{}
	}
	return set
}

// validateTools refuses a tool entry this package cannot compare exactly, and a
// requirement no base image answers for. It does NOT refuse a requirement the
// image lacks: that job still runs, it only downloads first, and a refusing
// decode would take a serving node off the air on its next restart over a
// bandwidth cost — the same reasoning ADR 0041 gives for a below-floor runner.
func (c Config) validateTools() error {
	for _, declared := range []struct {
		owner string
		tools []string
	}{
		{fmt.Sprintf("%s base image %q", capabilityPlatformLinux, c.Linux.BaseVM), c.Linux.BaseImageTools},
		{fmt.Sprintf("%s base image %q", capabilityPlatformMacOS, c.MacOS.BaseVM), c.MacOS.BaseImageTools},
	} {
		if err := validateToolList(declared.owner, declared.tools); err != nil {
			return err
		}
	}
	for _, scoped := range c.ScopedScaleSets() {
		owner := fmt.Sprintf("scale set %q in %s", scoped.Name(), scoped.Scope)
		if err := validateToolList(owner, scoped.ScaleSet.RequiresTools); err != nil {
			return err
		}
		if len(scoped.ScaleSet.RequiresTools) == 0 {
			continue
		}
		if _, known := c.capabilityPlatform(scoped.ScaleSet.Profile); !known {
			return fmt.Errorf("%s requires tools, but its profile %q is not a profile this node declares, so no "+
				"base image answers for it", owner, scoped.ScaleSet.Profile)
		}
	}
	return nil
}

func validateToolList(owner string, tools []string) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !toolVersionGrammar.MatchString(tool) {
			return fmt.Errorf("%s declares tool %q, which is not an exact tool@version matching %s",
				owner, tool, toolVersionGrammar.String())
		}
		if _, duplicate := seen[tool]; duplicate {
			return fmt.Errorf("%s lists tool %q twice", owner, tool)
		}
		seen[tool] = struct{}{}
	}
	return nil
}
