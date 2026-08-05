package config

import (
	"sort"
	"strings"
	"testing"
)

// node builds one named node from a mutation of the two-platform fixture, so a
// case states only the fact it is about.
func node(name string, mutate func(*Config)) NodeConfig {
	cfg := capabilityNode()
	mutate(&cfg)
	if err := cfg.Validate(); err != nil {
		panic(name + ": " + err.Error())
	}
	return NodeConfig{Node: name, Config: cfg}
}

// linuxNode owns one linux-xl scale set of its own. Two nodes may advertise one
// label — that is the federation ADR 0034 permits — but they own separate scale
// sets, which is what the ownership half of CheckFleet asserts.
func linuxNode(name string, declares []string, requires ...string) NodeConfig {
	return node(name, func(cfg *Config) {
		cfg.Linux.BaseImageCapabilities = declares
		*cfg = withScope(*cfg, "sudoku", named(linuxScaleSet(requires...), "trf-sudoku-xl-"+name))
	})
}

func named(scaleSet ScaleSet, name string) ScaleSet {
	scaleSet.Name = name
	return scaleSet
}

func withScope(cfg Config, scope string, scaleSets ...ScaleSet) Config {
	cfg.GitHub.Scopes = []GitHubScope{{Name: scope, Kind: ScopeRepository,
		ConfigURL: "https://github.com/vitalyiegorov/suuudokuuu", Installation: "personal",
		Targets: []string{"vitalyiegorov/suuudokuuu"}, ScaleSets: scaleSets}}
	return cfg
}

func TestCheckFleetComparesDeclarationsAcrossNodes(t *testing.T) {
	tests := []struct {
		name          string
		nodes         []NodeConfig
		wantFragments []string
		wantClean     bool
	}{
		{name: "no nodes at all", wantClean: true},
		{name: "one node cannot violate parity with itself", wantClean: true,
			nodes: []NodeConfig{linuxNode("mac-mini", []string{"redroid-android"}, "redroid-android")}},
		{name: "two nodes with equal capabilities behind one label", wantClean: true,
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"container-runtime", "redroid-android"}, "redroid-android"),
				linuxNode("mac-studio", []string{"container-runtime", "redroid-android"}),
			}},
		// The 2026-08-04 incident, reduced to its configuration: node A declares
		// the capability, node C does not, and both advertise linux-xl.
		{name: "two nodes, one shared label, unequal capabilities",
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"container-runtime", "redroid-android"}, "redroid-android"),
				linuxNode("mac-studio", []string{"container-runtime"}),
			},
			wantFragments: []string{"linux-xl", "redroid-android", "mac-studio", "mac-mini", "trf-sudoku-xl-mac-mini"}},
		{name: "a label only one node advertises is not compared", wantClean: true,
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"redroid-android"}, "redroid-android"),
				node("geekom", func(cfg *Config) {
					cfg.MacOS.Enabled = false
					cfg.Linux.Profiles = []Profile{{ID: "linux-2x4", Label: "trf-linux-arm64-2x4",
						Aliases: []string{"linux-medium"}, Resources: Resources{CPU: 2, MemoryMiB: 4096}, DiskGiB: 50}}
					cfg.Linux.Capacity = Resources{CPU: 2, MemoryMiB: 4096}
					*cfg = withScope(*cfg, "sudoku", ScaleSet{Profile: "linux-2x4", Name: "trf-sudoku-medium",
						ID: 3, MaxCapacity: 2, Labels: []string{"self-hosted", "linux-medium"}})
				}),
			}},
		// `self-hosted` is on every scale set by construction, so comparing parity
		// through it would demand that every node in a deliberately heterogeneous
		// fleet carry every capability in it.
		{name: "the universal self-hosted label does not federate two nodes", wantClean: true,
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"redroid-android"}, "redroid-android"),
				node("geekom", func(cfg *Config) {
					cfg.MacOS.Enabled = false
					cfg.Linux.Profiles = []Profile{{ID: "linux-2x4", Label: "trf-linux-arm64-2x4",
						Resources: Resources{CPU: 2, MemoryMiB: 4096}, DiskGiB: 50}}
					cfg.Linux.Capacity = Resources{CPU: 2, MemoryMiB: 4096}
					*cfg = withScope(*cfg, "sudoku", ScaleSet{Profile: "linux-2x4", Name: "trf-sudoku-medium",
						ID: 3, MaxCapacity: 2, Labels: []string{"self-hosted", "trf-linux-arm64-2x4"}})
				}),
			}},
		// The two images are compared separately: a macOS requirement can never be
		// answered by a Linux declaration, however complete that one is.
		{name: "parity is per platform",
			nodes: []NodeConfig{
				node("mac-mini", func(cfg *Config) {
					cfg.MacOS.BaseImageCapabilities = []string{"maestro-cli"}
					*cfg = withScope(*cfg, "sudoku", named(macosScaleSet("maestro-cli"), "trf-sudoku-maestro-mini"))
				}),
				node("mac-studio", func(cfg *Config) {
					cfg.Linux.BaseImageCapabilities = []string{"maestro-cli"}
					*cfg = withScope(*cfg, "sudoku", named(macosScaleSet(), "trf-sudoku-maestro-studio"))
				}),
			},
			wantFragments: []string{"macos-maestro", "maestro-cli", "mac-studio", "macOS", "macos-tartelet-base"}},
		// A node whose own image lacks what its own scale set requires is refused
		// by Config.Validate, so this pair cannot exist in a validated fleet. The
		// cross-node rule still reports the peers only, so one mistake is never
		// counted twice under two different names.
		{name: "the requiring node's own gap belongs to Validate",
			nodes: []NodeConfig{
				{Node: "mac-mini", Config: withScope(capabilityNode(), "sudoku",
					named(linuxScaleSet("redroid-android"), "trf-sudoku-xl-mini"))},
				{Node: "mac-studio", Config: withScope(capabilityNode(), "sudoku",
					named(linuxScaleSet(), "trf-sudoku-xl-studio"))},
			},
			wantFragments: []string{"mac-studio", "redroid-android"}},
		{name: "a scale-set name may be claimed by exactly one node",
			nodes: []NodeConfig{
				node("mac-mini", func(cfg *Config) { *cfg = withScope(*cfg, "sudoku", linuxScaleSet()) }),
				node("mac-studio", func(cfg *Config) { *cfg = withScope(*cfg, "sudoku", linuxScaleSet()) }),
			},
			wantFragments: []string{"trf-sudoku-xl-studio", "sudoku", "mac-mini", "mac-studio"}},
		{name: "the legacy flat list is owned exactly once too",
			nodes: []NodeConfig{
				node("mac-mini", func(cfg *Config) { cfg.GitHub.ScaleSets = []ScaleSet{linuxScaleSet()} }),
				node("mac-studio", func(cfg *Config) { cfg.GitHub.ScaleSets = []ScaleSet{linuxScaleSet()} }),
			},
			wantFragments: []string{"trf-sudoku-xl-studio", legacyScaleSetScope, "mac-mini", "mac-studio"}},
		{name: "a scale set named the same in two different scopes is two scale sets", wantClean: true,
			nodes: []NodeConfig{
				node("mac-mini", func(cfg *Config) { *cfg = withScope(*cfg, "sudoku", linuxScaleSet()) }),
				node("mac-studio", func(cfg *Config) { *cfg = withScope(*cfg, "budgie", linuxScaleSet()) }),
			}},
		// A scale set naming a profile the node does not declare is a per-node
		// error too, and no base image answers for it, so it takes no part here.
		{name: "a scale set with an undeclared profile is skipped", wantClean: true,
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"redroid-android"}, "redroid-android"),
				{Node: "mac-studio", Config: func() Config {
					set := named(linuxScaleSet("redroid-android"), "trf-sudoku-xl-studio")
					set.Profile = "linux-nowhere"
					return withScope(capabilityNode(), "sudoku", set)
				}()},
			}},
		{name: "every duplicated claim is reported, in a stable order",
			nodes: []NodeConfig{
				node("mac-mini", func(cfg *Config) {
					*cfg = withScope(*cfg, "sudoku", linuxScaleSet(), macosScaleSet())
				}),
				node("mac-studio", func(cfg *Config) {
					*cfg = withScope(*cfg, "sudoku", linuxScaleSet(), macosScaleSet())
				}),
			},
			wantFragments: []string{"trf-sudoku-xl-studio", "trf-sudoku-maestro", "mac-mini", "mac-studio"}},
		// A declaration that does not parse is a per-node error; the cross-node
		// rule treats that image as declaring nothing rather than inventing a
		// second account of the same mistake.
		{name: "a malformed declaration is not a second story",
			nodes: []NodeConfig{
				linuxNode("mac-mini", []string{"redroid-android"}, "redroid-android"),
				{Node: "mac-studio", Config: func() Config {
					cfg := withScope(capabilityNode(), "sudoku", named(linuxScaleSet(), "trf-sudoku-xl-studio"))
					cfg.Linux.BaseImageCapabilities = []string{"Redroid_Android"}
					return cfg
				}()},
			},
			wantFragments: []string{"mac-studio", "redroid-android", "linux-xl"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations := CheckFleet(test.nodes)
			err := violations.Err()
			if test.wantClean {
				if !violations.Empty() || err != nil {
					t.Fatalf("CheckFleet() = %v, want no violations", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckFleet() found nothing, want a violation")
			}
			for _, fragment := range test.wantFragments {
				if !strings.Contains(err.Error(), fragment) {
					t.Errorf("CheckFleet() = %q, missing %q", err, fragment)
				}
			}
		})
	}
}

// TestCheckFleetIsDeterministic pins the ordering, because a rule whose report
// changes between runs cannot be diffed and cannot be pasted into an issue.
func TestCheckFleetIsDeterministic(t *testing.T) {
	nodes := []NodeConfig{
		linuxNode("mac-mini", []string{"container-runtime", "redroid-android"}, "redroid-android", "container-runtime"),
		linuxNode("mac-studio", nil),
		linuxNode("geekom", nil),
	}
	first := CheckFleet(nodes).Err()
	if first == nil {
		t.Fatal("CheckFleet() found nothing")
	}
	reversed := []NodeConfig{nodes[2], nodes[1], nodes[0]}
	second := CheckFleet(reversed).Err()
	if second == nil || first.Error() != second.Error() {
		t.Fatalf("CheckFleet() is order dependent:\n%v\n%v", first, second)
	}
	if !sort.StringsAreSorted(violationKeys(CheckFleet(nodes).Capabilities)) {
		t.Error("capability violations are not sorted")
	}
}

func violationKeys(violations []CapabilityViolation) []string {
	keys := make([]string, len(violations))
	for i, violation := range violations {
		keys[i] = violation.String()
	}
	return keys
}
