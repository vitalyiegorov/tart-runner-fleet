package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestPolicyStatesAMissingKeyAsFalse is the 2026-08-23 state in one assertion:
// the mac studio's file had no `mixedPlatformAdmission`, the mac mini's had it
// true, and a missing optional key is indistinguishable from a deliberate one in
// the file. The projection is what removes the ambiguity, so the absent key must
// be published as `false` rather than omitted (issue #304).
func TestPolicyStatesAMissingKeyAsFalse(t *testing.T) {
	lean := ProjectPolicy(Default())
	if lean.MacOSBurst.MixedPlatformAdmission {
		t.Fatal("a configuration that never states the key projected it true")
	}
	encoded := string(lean.CanonicalJSON())
	if !strings.Contains(encoded, `"mixedPlatformAdmission":false`) {
		t.Fatalf("an unstated key was omitted rather than stated false: %s", encoded)
	}

	stated := Default()
	stated.MacOS.MixedPlatformAdmission = true
	if digest := ProjectPolicy(stated).Digest(); digest == lean.Digest() {
		t.Fatal("two nodes that disagree on the key of issue #304 share a digest")
	}
}

// TestPolicyDigestIsDeterministic pins the property the whole comparison rests
// on: the same configuration projects the same bytes every time, whatever order
// its maps happen to be ranged in.
func TestPolicyDigestIsDeterministic(t *testing.T) {
	cfg := Default()
	cfg.Linux.BaseImageCapabilities = []string{"redroid-android", "container-runtime"}
	cfg.Executor = Executor{Backend: ExecutorPodman, Image: "ghcr.io/example/runner:v1",
		Binary: "/usr/bin/podman", KVMProfiles: []string{"medium", "large"}}
	cfg.Targets = append(cfg.Targets, Target{Type: "repo", Slug: "owner/second", MaxActive: 2,
		SchedulingClass: domain.SchedulingStandard})
	first := ProjectPolicy(cfg)
	for attempt := 0; attempt < 8; attempt++ {
		if again := ProjectPolicy(cfg); again.Digest() != first.Digest() {
			t.Fatalf("digest is not deterministic: %s != %s", again.Digest(), first.Digest())
		}
	}
	if got := first.KVMProfiles; got[0] != "large" || got[1] != "medium" {
		t.Fatalf("kvm profiles are not sorted: %v", got)
	}
	if got := first.BaseImageCapabilities.Linux; got[0] != "container-runtime" {
		t.Fatalf("capabilities are not sorted: %v", got)
	}
}

// TestPolicyPublishesNoCredentialAndNoHostPath is the constraint issue #304
// states outright. A credential in this document would be a leak; a per-host
// path in it would make every honest pair of nodes disagree, which teaches an
// operator to ignore the answer and is its own kind of failure.
func TestPolicyPublishesNoCredentialAndNoHostPath(t *testing.T) {
	cfg := Default()
	cfg.StateDir = "/var/lib/secret-state-dir"
	cfg.Linux.SerialLogDirectory = "/var/log/secret-console-dir"
	cfg.MacOS.SharedDirectoryPath = "/Users/secret-shared-dir"
	cfg.Executor.Binary = "/opt/secret-podman"
	cfg.GitHub.App = GitHubApp{ClientID: "secret-client-id", KeychainService: "secret-keychain",
		KeychainAccount: "secret-account", PrivateKeyFile: "/etc/secret-key.pem"}
	cfg.GitHub.Installations = []GitHubInstallation{{Name: "secret-installation", InstallationID: 99}}
	encoded := string(ProjectPolicy(cfg).CanonicalJSON())
	for _, forbidden := range []string{"secret-state-dir", "secret-console-dir", "secret-shared-dir",
		"secret-podman", "secret-client-id", "secret-keychain", "secret-account", "secret-key.pem",
		"secret-installation"} {
		if strings.Contains(encoded, forbidden) {
			t.Errorf("policy published %q: %s", forbidden, encoded)
		}
	}
	// The serial sink is published as a posture, which is the fleet-wide fact, and
	// never as the directory, which is the per-host one.
	if !strings.Contains(encoded, `"serialLogEnabled":true`) {
		t.Errorf("the serial-log posture was not published at all: %s", encoded)
	}
}

// TestPolicyProjectsEveryDecidingSetting walks the fields a scheduling decision
// reads and asserts each arrived, because a projection that silently drops one
// is a comparison that silently passes.
func TestPolicyProjectsEveryDecidingSetting(t *testing.T) {
	cfg := Default()
	cfg.HostBudget = Resources{CPU: 10, MemoryMiB: 20480}
	cfg.GuestArch = "amd64"
	cfg.MacOS = MacOS{}
	cfg.Guards.ElasticHostEnvelope, cfg.Guards.PressureMemoryAccounting = true, true
	cfg.GitHub.CanonicalJobInventory = true
	cfg.GitHub.ScaleSets = []ScaleSet{{Profile: "small", Name: "set", ID: 1, MaxCapacity: 1,
		Labels: []string{"self-hosted", "linux-small"}}}
	cfg.Priority = Priority{EscalateAfter: 10 * time.Minute,
		Tiers: []domain.PriorityTier{{Name: "release"}, {Name: "batch"}}}
	policy := ProjectPolicy(cfg)

	if policy.HostBudget != (PolicyResources{CPU: 10, MemoryMiB: 20480}) {
		t.Errorf("host budget = %+v", policy.HostBudget)
	}
	if policy.LinuxCapacity != (PolicyLinuxCapacity{CPU: 8, MemoryMiB: 16384, MaxInstances: 4}) {
		t.Errorf("linux capacity = %+v", policy.LinuxCapacity)
	}
	if policy.HostPressureFloors.MinFreeDiskGiB != 60 || policy.HostPressureFloors.MaxLoadAverage != 9 ||
		policy.HostPressureFloors.MaxSwapUsedMiB != 2048 || policy.HostPressureFloors.MinAvailableMemoryMiB != 1024 ||
		policy.HostPressureFloors.MinCPUIdlePercent != 5 {
		t.Errorf("host pressure floors = %+v", policy.HostPressureFloors)
	}
	if !policy.ElasticHostEnvelope || !policy.PressureMemoryAccounting || !policy.CanonicalJobInventory {
		t.Errorf("a stated mode was dropped: %+v", policy)
	}
	if policy.PollSeconds != 20 || policy.ReservationAgeSeconds != 300 {
		t.Errorf("tick interval %d, reservation age %d", policy.PollSeconds, policy.ReservationAgeSeconds)
	}
	if !policy.SessionYield.Enabled || policy.SessionYield.BlockedForSeconds != 600 ||
		policy.SessionYield.HealthyForSeconds != 120 {
		t.Errorf("session yield = %+v", policy.SessionYield)
	}
	if !policy.UpdateDrain.Enabled || policy.UpdateDrain.PendingForSeconds != 1800 ||
		policy.UpdateDrain.MaxWaitSeconds != 7200 || policy.UpdateDrain.CooldownSeconds != 3600 {
		t.Errorf("update drain = %+v", policy.UpdateDrain)
	}
	if target := policy.Targets["repo:owner/repo"]; target.MaxActive != 4 ||
		target.SchedulingClass != string(domain.SchedulingStandard) {
		t.Errorf("target = %+v of %+v", target, policy.Targets)
	}
	if profile := policy.Profiles["medium"]; profile != (PolicyProfile{CPU: 2, MemoryMiB: 4096}) {
		t.Errorf("profile = %+v of %+v", profile, policy.Profiles)
	}
	if policy.GuestArch != "amd64" {
		t.Errorf("guest arch = %q", policy.GuestArch)
	}
	if len(policy.AdvertisedLabels) == 0 || policy.AdvertisedLabels[0] != "linux-small" {
		t.Errorf("advertised labels = %v", policy.AdvertisedLabels)
	}
	// Two scale sets behind one profile advertise one set of labels, not two. ADR
	// 0034 permits that topology, and a node that listed a name twice would
	// disagree with a peer that lists it once, for no reason an operator could act
	// on.
	shared := cfg
	shared.GitHub.ScaleSets = append(append([]ScaleSet(nil), cfg.GitHub.ScaleSets...),
		ScaleSet{Profile: "small", Name: "second", ID: 2, MaxCapacity: 1,
			Labels: []string{"self-hosted", "linux-small"}})
	if duplicated := ProjectPolicy(shared); len(duplicated.AdvertisedLabels) != len(policy.AdvertisedLabels) {
		t.Errorf("a shared profile duplicated its labels: %v", duplicated.AdvertisedLabels)
	}
	if policy.Priority.EscalateAfterSeconds != 600 || strings.Join(policy.Priority.Tiers, ",") != "release,batch" {
		t.Errorf("priority = %+v", policy.Priority)
	}
}

// TestPolicyFieldsRoundTripTheCanonicalBytes covers the form every consumer
// reads: the status document carries the fields verbatim and the diff walks
// them, so the map and the digested bytes must be the same document.
func TestPolicyFieldsRoundTripTheCanonicalBytes(t *testing.T) {
	policy := ProjectPolicy(Default())
	fields := policy.Fields()
	burst, ok := fields["macosBurst"].(map[string]any)
	if !ok {
		t.Fatalf("macosBurst is not an object: %#v", fields["macosBurst"])
	}
	if mixed, stated := burst["mixedPlatformAdmission"]; !stated || mixed != false {
		t.Fatalf("mixedPlatformAdmission = %#v, stated=%t", mixed, stated)
	}
	reencoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(policy.CanonicalJSON(), &canonical); err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(expected) {
		t.Fatalf("fields and canonical bytes disagree:\n%s\n%s", reencoded, expected)
	}
	if len(policy.Digest()) != 64 {
		t.Fatalf("digest %q is not a hex sha256", policy.Digest())
	}
}

// TestPolicyProjectsAnUnsetSliceAsAnEmptyList keeps a node that declares nothing
// encoding `[]` rather than `null`: a reader that must special-case null is a
// reader that will eventually read it as "no opinion".
func TestPolicyProjectsAnUnsetSliceAsAnEmptyList(t *testing.T) {
	encoded := string(ProjectPolicy(Default()).CanonicalJSON())
	if strings.Contains(encoded, "null") {
		t.Fatalf("policy encoded a null: %s", encoded)
	}
}
