package config

import (
	"bytes"
	"strings"
	"testing"
)

// ADR 0055 moved every Linux job to the Linux node. Once a Mac has no Linux
// consumer left, its Linux declaration is not a smaller configuration — it is a
// false one: four profiles nothing routes to, a scale set nobody polls, and an
// 8 GiB base image that has cloned nothing in seven days. This file pins the
// shape that lets an operator delete it, and the shape that must still be
// refused afterwards.

// macOSOnlyNode is a Mac that has retired its Linux execution: no profiles and
// no base VM. It is the file an operator is meant to be able to write after ADR
// 0055's retirement procedure.
//
// It KEEPS the capacity envelope, and that is the subtle half of this feature.
// `maxLinuxCpu`, `maxLinuxMemoryMb`, and `maxLinuxWhenMacosIdle` are named for
// Linux but are the node's shared cross-platform admission envelope under ADR
// 0012; a macOS guest is charged against them too.
func macOSOnlyNode() Config {
	cfg := Default()
	linux := cfg.Linux
	cfg.Linux = Linux{VMPrefix: linux.VMPrefix, MaxInstances: linux.MaxInstances, Capacity: linux.Capacity}
	return cfg
}

// TestRetiringLinuxMayNotRetireTheAdmissionEnvelope is the trap this feature
// sets for the operator who performs the retirement literally. Deleting every
// key with "Linux" in its name takes the node's whole admission envelope with
// it: `scheduler.staticFree` starts from it for EVERY platform, and
// `scheduler.elasticFree` seeds the elastic bound from it and then takes a
// minimum, so a zero envelope admits nothing under either policy.
//
// The resulting node starts, reports healthy, polls its scale sets and runs
// nothing, forever — an empty queue on a healthy host, which is what a fault
// looks like (#230). It is refused at the file instead.
func TestRetiringLinuxMayNotRetireTheAdmissionEnvelope(t *testing.T) {
	tests := map[string]func(*Config){
		"no envelope cpu":    func(c *Config) { c.Linux.Capacity.CPU = 0 },
		"no envelope memory": func(c *Config) { c.Linux.Capacity.MemoryMiB = 0 },
		"no slots":           func(c *Config) { c.Linux.MaxInstances = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := macOSOnlyNode()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() accepted a macOS-only node that can admit nothing at all")
			}
		})
	}
}

// TestAMacMayDeclareNoLinuxExecution is the whole point. Before this change
// Validate demanded a Linux base VM of every node, so a Mac could not say the
// true thing — that it boots macOS guests and nothing else.
func TestAMacMayDeclareNoLinuxExecution(t *testing.T) {
	cfg := macOSOnlyNode()
	if cfg.ExecutesLinux() {
		t.Fatalf("a node with no linux profiles executes no Linux: %+v", cfg.Linux)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a macOS-only node to be valid", err)
	}
}

// TestANodeMustHaveAtLeastOneExecutionTechnology is the floor under the
// relaxation. Dropping the Linux requirement must not make an empty node valid:
// a node that can boot neither platform can never serve a job, and it would
// look exactly like a healthy idle one.
func TestANodeMustHaveAtLeastOneExecutionTechnology(t *testing.T) {
	cfg := macOSOnlyNode()
	cfg.MacOS = MacOS{AdmissionPolicy: MacOSAdmissionShared}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a node with no execution technology at all")
	}
	if !strings.Contains(err.Error(), "execution technology") {
		t.Fatalf("Validate() = %v, want the error to name the missing execution technology", err)
	}
}

// TestDeclaredLinuxProfilesStillRequireTheLinuxBlock keeps the old rule exactly
// where it still applies. Relaxing "every node needs a Linux base VM" to "every
// node that boots Linux guests needs one" must not let a node declare profiles
// it has no image, capacity, or slot count for.
func TestDeclaredLinuxProfilesStillRequireTheLinuxBlock(t *testing.T) {
	tests := map[string]func(*Config){
		"no base VM":       func(c *Config) { c.Linux.BaseVM = "" },
		"no slots":         func(c *Config) { c.Linux.MaxInstances = 0 },
		"too many slots":   func(c *Config) { c.Linux.MaxInstances = 5 },
		"no capacity cpu":  func(c *Config) { c.Linux.Capacity.CPU = 0 },
		"no capacity ram":  func(c *Config) { c.Linux.Capacity.MemoryMiB = 0 },
		"no vm prefix":     func(c *Config) { c.Linux.VMPrefix = "" },
		"profile no label": func(c *Config) { c.Linux.Profiles[0].Label = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if !cfg.ExecutesLinux() {
				t.Fatal("the fixture must still declare Linux profiles")
			}
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

// TestAMacOSOnlyNodeStillNamesItsGuests pins the one Linux-block key that is
// NOT relaxed. `vmPrefix` is a required key of the schema on every node, and a
// node that stops declaring Linux profiles does not stop needing a well-formed
// file; leaving it required keeps one rule instead of two and costs an operator
// one line they already have.
func TestAMacOSOnlyNodeStillNamesItsGuests(t *testing.T) {
	cfg := macOSOnlyNode()
	cfg.Linux.VMPrefix = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a node with no vmPrefix")
	}
}

// TestAMacOSOnlyNodeRefusesAScaleSetItCannotServe is the safety half. A Mac
// that deletes its Linux profiles while a Linux scale set is still listed would
// otherwise start, poll a set it can never place, and hold GitHub's jobs
// forever — which is exactly the stranding of issue #164, self-inflicted. The
// error names the set so the operator knows which one to retire.
func TestAMacOSOnlyNodeRefusesAScaleSetItCannotServe(t *testing.T) {
	cfg := macOSOnlyNode()
	cfg.GitHub.Scopes = []GitHubScope{{Name: "suuudokuuu", ScaleSets: []ScaleSet{
		{Profile: "linux-6x12", Name: "trf-sudoku-xl-mini", ID: 101, MaxCapacity: 1},
	}}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a Linux scale set on a node with no Linux execution")
	}
	for _, want := range []string{"trf-sudoku-xl-mini", "linux-6x12", "suuudokuuu"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() = %v, want the error to name %q", err, want)
		}
	}
}

// TestAMacOSOnlyNodeServesItsMacOSScaleSets is the other direction: the new
// rule must refuse the sets this node cannot serve and no others.
func TestAMacOSOnlyNodeServesItsMacOSScaleSets(t *testing.T) {
	cfg := macOSOnlyNode()
	cfg.GitHub.Scopes = []GitHubScope{{Name: "suuudokuuu", ScaleSets: []ScaleSet{
		{Profile: cfg.MacOS.Maestro.ID, Name: "trf-sudoku-maestro-mini", ID: 102, MaxCapacity: 2},
		{Profile: cfg.MacOS.Builder.ID, Name: "trf-sudoku-builder-mini", ID: 103, MaxCapacity: 1},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a macOS-only node to serve its macOS scale sets", err)
	}
}

// TestALinuxNodeIsUnaffectedByTheRelaxation guards the node that still runs
// Linux: the new scale-set rule is scoped to nodes with no Linux execution, so
// node-b's file must validate exactly as it did.
func TestALinuxNodeIsUnaffectedByTheRelaxation(t *testing.T) {
	cfg := Default()
	cfg.MacOS = MacOS{AdmissionPolicy: MacOSAdmissionShared}
	cfg.GitHub.Scopes = []GitHubScope{{Name: "fleet", ScaleSets: []ScaleSet{
		{Profile: "small", Name: "trf-fleet-small", ID: 1, MaxCapacity: 2},
	}}}
	if !cfg.ExecutesLinux() {
		t.Fatal("the fixture must declare Linux profiles")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a Linux-only node to stay valid", err)
	}
}

// TestAMacOSOnlyFileDecodesAndRoundTrips proves the relaxation reaches the file
// an operator actually edits: the Linux keys may be absent outright, and what
// Encode writes back must decode again. A setting that only exists in a Go
// struct literal is not a setting an operator can use.
func TestAMacOSOnlyFileDecodesAndRoundTrips(t *testing.T) {
	const raw = `{
      "vmPrefix":"gha-macos",
      "pollSeconds":20, "linuxReservationAgeSeconds":300, "minFreeDiskGb":60,
      "maxLinuxWhenMacosIdle":2, "maxLinuxCpu":10, "maxLinuxMemoryMb":23552,
      "macosBurst":{"enabled":true,"baseVm":"macos-tartelet-base","vmPrefix":"gha-macos",
        "builder":{"id":"macos-6x12","label":"trf-macos-arm64-6x12","cpu":6,"memoryMb":12288,"maxActive":1},
        "maestro":{"id":"macos-4x7","label":"trf-macos-arm64-4x7","cpu":4,"memoryMb":7168,"maxActive":2}},
      "targets":[{"type":"repo","slug":"owner/repo","maxActive":2}]
    }`
	cfg, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode() = %v, want a file with no Linux keys to decode", err)
	}
	if cfg.ExecutesLinux() || cfg.Linux.BaseVM != "" || len(cfg.Linux.Profiles) != 0 {
		t.Fatalf("decoded Linux block = %+v, want none", cfg.Linux)
	}
	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	roundTripped, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("Decode(Encode()) = %v\n%s", err, encoded.String())
	}
	if roundTripped.ExecutesLinux() {
		t.Fatalf("round trip invented Linux execution:\n%s", encoded.String())
	}
}

// TestAMacOSOnlyNodeJudgesOnlyTheImageItBoots is rule 4 read the right way
// round. A node with no Linux execution has no Linux runner image, so it must
// publish no Linux compliance row — an absent image is not a below-floor one,
// and reporting one would make `fleet doctor` unreadable on the very node the
// retirement was for. It is the same rule RunnerImages already applies to a
// node with `macosBurst` disabled.
func TestAMacOSOnlyNodeJudgesOnlyTheImageItBoots(t *testing.T) {
	images := macOSOnlyNode().RunnerImages()
	if len(images) != 1 {
		t.Fatalf("RunnerImages() = %+v, want exactly the macOS image", images)
	}
	if images[0].Platform != capabilityPlatformMacOS {
		t.Fatalf("RunnerImages()[0] = %+v, want the macOS image", images[0])
	}
}

// TestRetiringLinuxChangesThePublishedPolicy keeps ADR 0034's drift detector
// honest. `fleet config policy` exists because no node can tell whether its own
// configuration is the right one; a retirement that left the digest untouched
// would be invisible to the one check built to see it (#304).
func TestRetiringLinuxChangesThePublishedPolicy(t *testing.T) {
	withLinux := ProjectPolicy(Default())
	without := ProjectPolicy(macOSOnlyNode())
	if withLinux.Digest() == without.Digest() {
		t.Fatal("retiring Linux execution left the published policy digest unchanged")
	}
	if len(without.Profiles) != 0 {
		t.Fatalf("policy profiles = %+v, want none on a macOS-only node", without.Profiles)
	}
	// The envelope stays published, because the node still has one: it is the
	// shared admission bound every macOS guest is charged against, not a
	// statement that this node boots Linux.
	if without.LinuxCapacity == (PolicyLinuxCapacity{}) {
		t.Fatalf("policy dropped the node's admission envelope: %+v", without.LinuxCapacity)
	}
}
