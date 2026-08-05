package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// NodeConfig is one node's configuration together with the name it is known by
// — a file name under `config/nodes/`, or whatever the caller passed on the
// command line. The name exists only so a violation can say which machine to go
// and fix; nothing here reads another node at runtime, and no daemon ever calls
// any of this. ADR 0034 §9 forbids cross-node coupling, and configuration held
// in one process by an operator or by CI is not coupling: it is the one place
// where the fleet exists as a single artifact.
type NodeConfig struct {
	Node   string
	Config Config
}

// CapabilityViolation is one node that advertises a label without declaring a
// capability that another node's scale set requires behind that same label.
type CapabilityViolation struct {
	// Platform is which of the two base images was compared.
	Platform string
	// Label is the shared advertised label, in the spelling of the node that
	// sorts first.
	Label string
	// Capability is the identifier the requiring node's scale set names.
	Capability string
	// RequiredBy, Scope, and ScaleSet identify the declaration that requires it.
	RequiredBy string
	Scope      string
	ScaleSet   string
	// MissingOn is the node whose base image does not declare it, and BaseVM is
	// that node's image for this platform.
	MissingOn string
	BaseVM    string
}

func (v CapabilityViolation) String() string {
	return fmt.Sprintf("node %q advertises %q without declaring capability %q on its %s base image %q, "+
		"which node %q requires through scale set %q in %s",
		v.MissingOn, v.Label, v.Capability, v.Platform, v.BaseVM, v.RequiredBy, v.ScaleSet, v.Scope)
}

// OwnershipViolation is one `(scope, scale-set name)` pair claimed by more than
// one node. ADR 0034 §2 makes that pair the unit of ownership, and GitHub agrees
// with a 409: two daemons long-polling one scale set evict each other in a loop.
type OwnershipViolation struct {
	Scope    string
	ScaleSet string
	Nodes    []string
}

func (v OwnershipViolation) String() string {
	return fmt.Sprintf("scale set %q in %s is claimed by nodes %s; a scale set is served by exactly one node",
		v.ScaleSet, v.Scope, strings.Join(v.Nodes, ", "))
}

// FleetViolations is everything the cross-node rules found, in a deterministic
// order so a report can be diffed between runs.
type FleetViolations struct {
	Capabilities []CapabilityViolation
	Ownership    []OwnershipViolation
}

func (v FleetViolations) Empty() bool { return len(v.Capabilities) == 0 && len(v.Ownership) == 0 }

// Err reduces the findings to one error, or nil. Every violation is reported,
// not just the first: these are configuration facts an operator repairs in a
// single edit, and stopping at the first would hide the rest behind three more
// runs of the same command.
func (v FleetViolations) Err() error {
	if v.Empty() {
		return nil
	}
	lines := make([]string, 0, len(v.Ownership)+len(v.Capabilities))
	for _, violation := range v.Ownership {
		lines = append(lines, violation.String())
	}
	for _, violation := range v.Capabilities {
		lines = append(lines, violation.String())
	}
	return errors.New(strings.Join(lines, "\n"))
}

// CheckFleet applies the two rules that are knowable only when every node is in
// hand at once.
//
// The first is ADR 0034's parity invariant: for any label advertised by more
// than one node, the union of `requiresCapabilities` over every scale set
// advertising it must be declared by every node that advertises it. It is
// applied per platform, because a node's two base images answer separate
// questions, and it compares the labels a scale set actually publishes — the
// configured ones plus the canonical label and every alias — since those are
// what GitHub matches a job against.
//
// The second is ADR 0034 §2's ownership invariant, asserted here because this is
// the only place with every node's file open.
//
// A node whose own image lacks what its own scale set requires is Config.Validate's
// business and is deliberately not repeated here, so one mistake is reported
// once under one name.
func CheckFleet(nodes []NodeConfig) FleetViolations {
	advertisers, requirements := indexFleet(nodes)
	return FleetViolations{
		Capabilities: capabilityViolations(nodes, advertisers, requirements),
		Ownership:    ownershipViolations(nodes),
	}
}

// universalRunnerLabel is the one label this codebase requires of every scale
// set (see validateScaleSetLabels), so every node carries it by construction.
// Comparing parity through it would not be a stricter rule, it would be a
// different one: "every node must declare every capability in the fleet, on
// every platform", which contradicts the heterogeneous partition ADR 0034 §3
// designs for — node B is an x86 Linux machine that will never carry what an
// Apple-silicon image carries.
//
// The residual limit is worth stating plainly: a workflow whose `runs-on` names
// nothing but `self-hosted` can still be placed on any node, and this rule does
// not protect it. Nothing configuration-side can, because such a job has
// expressed no requirement to compare against.
const universalRunnerLabel = "self-hosted"

// labelKey identifies one advertised label on one platform. Labels are folded
// because GitHub matches `runs-on` case-insensitively, so two spellings of one
// label are one label and must not escape the rule by disagreeing about case.
type labelKey struct {
	platform string
	label    string
}

// requirement is one scale set's demand for one capability behind one label.
type requirement struct {
	key        labelKey
	label      string
	capability string
	node       string
	scope      string
	scaleSet   string
}

// indexFleet walks every node's scale sets once and records, per platform and
// label, which nodes advertise it and what is required behind it.
func indexFleet(nodes []NodeConfig) (map[labelKey]map[string]string, []requirement) {
	advertisers := map[labelKey]map[string]string{}
	var requirements []requirement
	for _, node := range nodes {
		labelSets := node.Config.ProfileLabelSets()
		for _, scoped := range node.Config.ScopedScaleSets() {
			platform, known := node.Config.capabilityPlatform(scoped.ScaleSet.Profile)
			if !known {
				// An unknown profile is a per-node error Config.Validate names
				// precisely; guessing a platform for it here would report it twice
				// and in a vaguer way.
				continue
			}
			for _, label := range labelSets[scoped.ScaleSet.Profile].Advertise(scoped.ScaleSet.Labels) {
				if folded(label) == universalRunnerLabel {
					continue
				}
				key := labelKey{platform: platform, label: folded(label)}
				if advertisers[key] == nil {
					advertisers[key] = map[string]string{}
				}
				advertisers[key][node.Node] = label
				for _, capability := range scoped.ScaleSet.RequiresCapabilities {
					requirements = append(requirements, requirement{key: key, label: label, capability: capability,
						node: node.Node, scope: scoped.Scope, scaleSet: scoped.Name()})
				}
			}
		}
	}
	return advertisers, requirements
}

// capabilityViolations reports every peer that advertises a shared label without
// carrying what someone else requires behind it.
func capabilityViolations(nodes []NodeConfig, advertisers map[labelKey]map[string]string,
	requirements []requirement) []CapabilityViolation {
	declarations := declaredCapabilities(nodes)
	var violations []CapabilityViolation
	for _, required := range requirements {
		peers := advertisers[required.key]
		if len(peers) < 2 {
			continue
		}
		for _, peer := range sortedNames(peers) {
			if peer == required.node {
				continue
			}
			image := declarations[nodeImage{node: peer, platform: required.key.platform}]
			if _, declared := image.declares[required.capability]; declared {
				continue
			}
			violations = append(violations, CapabilityViolation{Platform: required.key.platform,
				Label: displayLabel(peers), Capability: required.capability,
				RequiredBy: required.node, Scope: required.scope, ScaleSet: required.scaleSet,
				MissingOn: peer, BaseVM: image.VM})
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	return violations
}

// displayLabel picks one spelling of a folded label deterministically, so the
// report does not depend on which node happened to be read first. It is only
// ever called for a label at least two nodes advertise, so the map is never
// empty.
func displayLabel(peers map[string]string) string {
	spellings := make([]string, 0, len(peers))
	for _, spelling := range peers {
		spellings = append(spellings, spelling)
	}
	sort.Strings(spellings)
	return spellings[0]
}

type nodeImage struct {
	node     string
	platform string
}

func declaredCapabilities(nodes []NodeConfig) map[nodeImage]baseImage {
	declarations := make(map[nodeImage]baseImage, 2*len(nodes))
	for _, node := range nodes {
		images, err := node.Config.baseImages()
		if err != nil {
			// A malformed declaration is a per-node error; the comparison simply
			// treats that image as declaring nothing rather than inventing a second
			// account of the same mistake.
			continue
		}
		for platform, image := range images {
			declarations[nodeImage{node: node.Node, platform: platform}] = image
		}
	}
	return declarations
}

// ownershipViolations reports each `(scope, scale-set name)` pair more than one
// node claims.
func ownershipViolations(nodes []NodeConfig) []OwnershipViolation {
	type owner struct {
		scope    string
		scaleSet string
	}
	claims := map[owner]map[string]struct{}{}
	display := map[owner]OwnershipViolation{}
	for _, node := range nodes {
		for _, scoped := range node.Config.ScopedScaleSets() {
			key := owner{scope: folded(scoped.ScopeName), scaleSet: folded(scoped.Name())}
			if claims[key] == nil {
				claims[key] = map[string]struct{}{}
				display[key] = OwnershipViolation{Scope: scoped.Scope, ScaleSet: scoped.Name()}
			}
			claims[key][node.Node] = struct{}{}
		}
	}
	var violations []OwnershipViolation
	for key, owners := range claims {
		if len(owners) < 2 {
			continue
		}
		violation := display[key]
		violation.Nodes = sortedNames(owners)
		violations = append(violations, violation)
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	return violations
}

func sortedNames[V any](set map[string]V) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
