package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DefaultRunnerVersionFloor is the `actions/runner` version GitHub's minimum
// version enforcement makes the hard bar for a self-hosted runner to register at
// all. Below it the runner is not degraded, it is invisible: the registration is
// refused, and on this fleet — where every guest registers from a JIT
// configuration at admission time and carries `"DisableUpdate":"True"` — there
// is no long-lived registration to grandfather in and no self-update that could
// climb out of it. Brownouts enforcing it begin 24 Aug 2026 and enforcement is
// permanent from 25 Sep 2026 (issue #206).
//
// It is a constant rather than a computed thing because the number is GitHub's
// to choose and nothing this daemon can measure would derive it.
const DefaultRunnerVersionFloor = "2.329.0"

// runnerVersionGrammar recognises an `actions/runner` release version: exactly
// three dot-separated decimal components, no `v` prefix and no pre-release
// suffix, because that is the shape every release in `actions/runner` has ever
// had and the shape the image build scripts write into their manifests. It is
// deliberately strict: a value this package cannot order is a value the floor
// check would have to skip, and a skipped compliance check is the failure mode
// issue #206 exists to remove.
var runnerVersionGrammar = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// RunnerImage is one of a node's guest images together with the `actions/runner`
// version its operator declared it carries and the floor that version is judged
// against. It is the single value the floor rule is stated over: the daemon
// publishes it, `fleet doctor` renders it, and neither re-derives the verdict.
type RunnerImage struct {
	// Platform is `linux` or `macOS`, the same vocabulary the capability rule
	// partitions base images by.
	Platform string
	// VM is the base image the platform boots.
	VM string
	// Version is the declared `actions/runner` version, empty when the node
	// declares none.
	Version string
	// Floor is the version Version is judged against, always populated.
	Floor string
}

// Compliant reports whether this image may still register runners with GitHub.
// An undeclared version is NOT compliant. That is the one place this rule is
// deliberately harsher than the fleet's usual "absence is a pass" convention,
// and it is harsher for the reason the issue was filed: an unknown runner
// version already looked exactly like a healthy one, on both nodes, for two
// months.
func (i RunnerImage) Compliant() bool { return i.Reason() == "" }

// Reason is the operator-facing explanation of a failing image, empty when the
// image is compliant. It names the image, what it carries, and the bar, because
// those are the three facts needed to decide whether to rebuild.
func (i RunnerImage) Reason() string {
	if i.Version == "" {
		return fmt.Sprintf("%s base image %q declares no runner version, so its brownout compliance "+
			"cannot be judged", i.Platform, i.VM)
	}
	if compareRunnerVersions(i.Version, i.Floor) >= 0 {
		return ""
	}
	return fmt.Sprintf("%s base image %q carries actions/runner %s, below the %s floor; GitHub refuses "+
		"registration below it, so every job routed to this image fails", i.Platform, i.VM, i.Version, i.Floor)
}

// RunnerVersionFloorOrDefault is the floor this node judges its images against:
// the operator's override when one is stated, the shipped constant otherwise.
//
// The override exists because the bar moves on GitHub's calendar, not on this
// fleet's release calendar — the same announcement that sets 2.329.0 also
// requires each new release be installed within 30 days of publication, so the
// number an operator must act on changes at least monthly. Without the override,
// raising the floor would mean cutting and rolling out a fleet release during a
// brownout week.
func (c Config) RunnerVersionFloorOrDefault() string {
	if floor := strings.TrimSpace(c.RunnerVersionFloor); floor != "" {
		return floor
	}
	return DefaultRunnerVersionFloor
}

// RunnerImages is every guest image this node actually boots, in a stable order,
// each paired with the floor it is judged against. A node with `macosBurst`
// disabled boots no macOS guest, so no macOS row is produced: an image nothing
// is routed to is not a compliance hole, and reporting one would make the check
// unreadable on the Linux-only nodes.
func (c Config) RunnerImages() []RunnerImage {
	floor := c.RunnerVersionFloorOrDefault()
	images := []RunnerImage{{Platform: capabilityPlatformLinux, VM: c.Linux.BaseVM,
		Version: strings.TrimSpace(c.Linux.BaseImageRunnerVersion), Floor: floor}}
	if c.MacOS.Enabled {
		images = append(images, RunnerImage{Platform: capabilityPlatformMacOS, VM: c.MacOS.BaseVM,
			Version: strings.TrimSpace(c.MacOS.BaseImageRunnerVersion), Floor: floor})
	}
	return images
}

// validateRunnerVersions refuses a declaration or a floor this package cannot
// order, and nothing else. In particular it does NOT refuse a declaration below
// the floor: see ADR 0041. A below-floor image is an operational state that
// `fleet doctor` must shout about while the node keeps serving whatever GitHub
// still lets it serve; making it a decode failure would take a running node off
// the air on its next restart — and this daemon restarts itself on every
// auto-update — turning a warning into the outage the check exists to prevent.
func (c Config) validateRunnerVersions() error {
	for _, declared := range []struct{ owner, version string }{
		{fmt.Sprintf("%s base image %q", capabilityPlatformLinux, c.Linux.BaseVM), c.Linux.BaseImageRunnerVersion},
		{fmt.Sprintf("%s base image %q", capabilityPlatformMacOS, c.MacOS.BaseVM), c.MacOS.BaseImageRunnerVersion},
	} {
		if declared.version == "" {
			continue
		}
		if !runnerVersionGrammar.MatchString(declared.version) {
			return fmt.Errorf("%s declares runner version %q, which is not an actions/runner release version "+
				"matching %s", declared.owner, declared.version, runnerVersionGrammar.String())
		}
	}
	if c.RunnerVersionFloor != "" && !runnerVersionGrammar.MatchString(c.RunnerVersionFloor) {
		return fmt.Errorf("runnerVersionFloor %q is not an actions/runner release version matching %s",
			c.RunnerVersionFloor, runnerVersionGrammar.String())
	}
	return nil
}

// compareRunnerVersions orders two versions the grammar has already accepted,
// returning the usual -1/0/+1. Both operands are known well-formed by the time
// this runs — the decode path validates them and the floor falls back to a
// constant — so an unparsable component can only mean the grammar and this
// function disagree, which orders the value last rather than panicking.
func compareRunnerVersions(left, right string) int {
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftPart, leftErr := strconv.Atoi(leftParts[index])
		rightPart, rightErr := strconv.Atoi(rightParts[index])
		if leftErr != nil || rightErr != nil {
			return -1
		}
		if leftPart != rightPart {
			if leftPart < rightPart {
				return -1
			}
			return 1
		}
	}
	return len(leftParts) - len(rightParts)
}
