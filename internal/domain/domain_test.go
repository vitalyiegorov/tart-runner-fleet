package domain

import (
	"strings"
	"testing"
	"time"
)

func TestDemandKeyIdentityAndValidation(t *testing.T) {
	key := DemandKey{Repo: "owner/repo", RunID: 42, Attempt: 3, JobID: 99}
	if got, want := key.String(), "owner/repo/42/3/99"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}

	invalid := []DemandKey{
		{},
		{Repo: "owner/repo", RunID: 0, Attempt: 1, JobID: 1},
		{Repo: "owner/repo", RunID: 1, Attempt: 0, JobID: 1},
		{Repo: "owner/repo", RunID: 1, Attempt: 1, JobID: 0},
	}
	for _, candidate := range invalid {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("invalid key accepted: %#v", candidate)
		}
	}
}

func TestObservationFreshnessIsExplicit(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	fresh := Fresh([]int{1, 2}, now)
	if !fresh.Usable() || fresh.State != ObservationFresh || len(fresh.Value) != 2 {
		t.Fatalf("unexpected fresh observation: %#v", fresh)
	}
	stale := Stale([]int{1}, now.Add(-time.Minute), "deadline exceeded")
	if stale.Usable() || stale.State != ObservationStale || stale.Reason == "" {
		t.Fatalf("unexpected stale observation: %#v", stale)
	}
	unavailable := Unavailable[[]int]("GitHub unavailable")
	if unavailable.Usable() || unavailable.State != ObservationUnavailable || unavailable.ObservedAt != (time.Time{}) {
		t.Fatalf("unexpected unavailable observation: %#v", unavailable)
	}
}

func TestResourceVectorArithmetic(t *testing.T) {
	capacity := Resources{CPU: 8, MemoryMB: 16_384, Slots: 4}
	used := Resources{CPU: 6, MemoryMB: 12_288, Slots: 3}
	if !capacity.CanFit(used) {
		t.Fatal("capacity should fit used resources")
	}
	remaining, ok := capacity.Sub(used)
	if !ok || remaining != (Resources{CPU: 2, MemoryMB: 4_096, Slots: 1}) {
		t.Fatalf("remaining = %#v, %v", remaining, ok)
	}
	if _, ok := used.Sub(capacity); ok {
		t.Fatal("underflow must be rejected")
	}
	if got := used.Add(remaining); got != capacity {
		t.Fatalf("sum = %#v, want %#v", got, capacity)
	}
}

func TestExplicitInstanceLifecycleTransitionMatrixIsExhaustive(t *testing.T) {
	states := []InstanceState{
		InstancePlanned, InstanceCloning, InstanceBooting, InstanceReachable,
		InstanceRegistering, InstanceOnlineIdle, InstanceAssigned, InstanceRunning,
		InstanceDraining, InstanceDeregistering, InstanceStopping, InstanceDeleted,
		InstanceFailed,
	}
	allowed := map[[2]InstanceState]bool{
		{InstancePlanned, InstanceCloning}:        true,
		{InstancePlanned, InstanceDraining}:       true,
		{InstanceCloning, InstanceBooting}:        true,
		{InstanceCloning, InstanceDraining}:       true,
		{InstanceBooting, InstanceReachable}:      true,
		{InstanceBooting, InstanceDraining}:       true,
		{InstanceReachable, InstanceRegistering}:  true,
		{InstanceReachable, InstanceDraining}:     true,
		{InstanceRegistering, InstanceOnlineIdle}: true,
		{InstanceRegistering, InstanceAssigned}:   true,
		{InstanceOnlineIdle, InstanceAssigned}:    true,
		{InstanceAssigned, InstanceRunning}:       true,
		{InstanceAssigned, InstanceOnlineIdle}:    true,
		{InstanceAssigned, InstanceDraining}:      true,
		{InstanceRunning, InstanceDraining}:       true,
		{InstanceOnlineIdle, InstanceDraining}:    true,
		{InstanceDraining, InstanceDeregistering}: true,
		// Recovery-drain abort: fresh evidence disproved the drain premise.
		{InstanceDraining, InstanceRunning}:       true,
		{InstanceDeregistering, InstanceStopping}: true,
		{InstanceStopping, InstanceDeleted}:       true,
		{InstanceFailed, InstancePlanned}:         true,
	}
	for _, from := range states {
		if from != InstanceDeleted && from != InstanceFailed {
			allowed[[2]InstanceState{from, InstanceFailed}] = true
		}
		for _, to := range states {
			if got, want := from.CanTransitionTo(to), allowed[[2]InstanceState{from, to}]; got != want {
				t.Errorf("CanTransitionTo(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestFailedInstanceRetrySemantics(t *testing.T) {
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	instance := Instance{State: InstanceFailed, Attempts: 2, RetryAt: now}
	if !instance.CanRetry(now, 3) {
		t.Fatal("eligible failed instance should retry")
	}
	instance.RetryAt = now.Add(time.Second)
	if instance.CanRetry(now, 3) {
		t.Fatal("retry before RetryAt was allowed")
	}
	instance.RetryAt = now
	instance.Attempts = 3
	if instance.CanRetry(now, 3) {
		t.Fatal("retry at max attempts was allowed")
	}
	instance.State = InstanceRunning
	instance.Attempts = 0
	if instance.CanRetry(now, 3) || instance.CanRetry(now, 0) {
		t.Fatal("non-failed or disabled retry was allowed")
	}
}

func TestHostModeDerivesFromLiveInstancesIncludingMixedCapacity(t *testing.T) {
	linux := Instance{ID: "linux", Platform: PlatformLinux, State: InstanceRunning}
	mac := Instance{ID: "mac", Platform: PlatformMacOS, State: InstanceOnlineIdle}

	if mode, err := DeriveHostMode(nil); err != nil || mode != HostIdle {
		t.Fatalf("empty mode = %s, %v", mode, err)
	}
	if mode, err := DeriveHostMode([]Instance{linux}); err != nil || mode != HostLinux {
		t.Fatalf("linux mode = %s, %v", mode, err)
	}
	if mode, err := DeriveHostMode([]Instance{mac}); err != nil || mode != HostMacOS {
		t.Fatalf("mac mode = %s, %v", mode, err)
	}
	if mode, err := DeriveHostMode([]Instance{linux, mac}); err != nil || mode != HostMixed {
		t.Fatalf("mixed mode = %s, %v", mode, err)
	}
	terminal := Instance{ID: "done", Platform: PlatformLinux, State: InstanceDeleted}
	if mode, err := DeriveHostMode([]Instance{terminal}); err != nil || mode != HostIdle {
		t.Fatalf("terminal mode = %s, %v", mode, err)
	}
	unknown := Instance{ID: "unknown", Platform: Platform("other"), State: InstanceOnlineIdle}
	if _, err := DeriveHostMode([]Instance{unknown}); err == nil {
		t.Fatal("unknown live platform accepted")
	}
}

func TestStoppedTeardownInstanceDoesNotConsumeHostCapacity(t *testing.T) {
	for _, state := range []InstanceState{InstanceDraining, InstanceDeregistering, InstanceStopping} {
		orphan := Instance{ID: "orphan", Platform: PlatformMacOS, State: state, Power: InstancePowerStopped}
		if orphan.ConsumesHostResources() {
			t.Fatalf("stopped %s instance consumed host resources", state)
		}
		if mode, err := DeriveHostMode([]Instance{orphan}); err != nil || mode != HostIdle {
			t.Fatalf("stopped %s mode = %s, %v", state, mode, err)
		}
	}

	for _, instance := range []Instance{
		{ID: "running-drain", Platform: PlatformMacOS, State: InstanceDraining, Power: InstancePowerRunning},
		{ID: "unknown-drain", Platform: PlatformMacOS, State: InstanceDraining, Power: InstancePowerUnknown},
		{ID: "stopped-assigned", Platform: PlatformMacOS, State: InstanceAssigned, Power: InstancePowerStopped},
		{ID: "absent-assigned", Platform: PlatformMacOS, State: InstanceAssigned, Power: InstancePowerAbsent},
	} {
		if !instance.ConsumesHostResources() {
			t.Fatalf("unsafe instance stopped consuming resources: %#v", instance)
		}
	}
}

// A VM a successful Tart enumeration proves absent occupies strictly less of the
// host than one it proves stopped, so absence during cleanup must free the vector
// wherever stopped already frees it — otherwise a deleted VM would pin the host
// to its platform harder than a live one and starve the other platform. Outside a
// cleanup state, and for an unknown power reading, the fail-closed answer is
// unchanged (asserted above).
func TestAbsentTeardownInstanceDoesNotConsumeHostCapacity(t *testing.T) {
	for _, state := range []InstanceState{InstanceDraining, InstanceDeregistering, InstanceStopping} {
		absent := Instance{ID: "absent", Platform: PlatformMacOS, State: state, Power: InstancePowerAbsent}
		if absent.ConsumesHostResources() {
			t.Fatalf("absent %s instance consumed host resources", state)
		}
		if mode, err := DeriveHostMode([]Instance{absent}); err != nil || mode != HostIdle {
			t.Fatalf("absent %s mode = %s, %v", state, mode, err)
		}
	}
}

// The instance grammar is the fleet's one identity rule, shared by the guest
// name, the GitHub runner name, and the durable row's primary key. It lives here
// rather than in a backend adapter precisely so a second backend inherits it
// unchanged; tests/contract additionally proves it is legal as a container name.
func TestInstanceNameGrammar(t *testing.T) {
	for _, name := range []string{"a", "trf-small-1", "trf-macos-builder-096ffcb3a52d8624", "A.b_c-9",
		strings.Repeat("z", 128)} {
		if err := ValidateInstanceName(name); err != nil {
			t.Fatalf("rejected %q: %v", name, err)
		}
	}
	// "." and ".." match the character class but are path traversal, and a name
	// is interpolated into a filesystem-backed guest identity on at least one
	// backend, so they are refused explicitly rather than by luck.
	for _, name := range []string{"", ".", "..", "-leading", ".leading", "has space", "has/slash",
		strings.Repeat("z", 129)} {
		if err := ValidateInstanceName(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

// TestImageReferenceGrammarAcceptsBothBackendsImages pins the rule issue #139
// moved out of internal/lifecycle. An OCI reference is not an instance name, and
// a lifecycle that validated it as one could never provision a container node.
func TestImageReferenceGrammarAcceptsBothBackendsImages(t *testing.T) {
	for _, image := range []string{
		"linux-runner-base", "macos-tartelet-base",
		"docker.io/library/ubuntu:24.04", "ghcr.io/vitalyiegorov/trf-runner-amd64:2026-08",
		"ghcr.io/o/r@sha256:0123456789abcdef", "localhost/runner", "../relative-but-harmless",
	} {
		if err := ValidateImageReference(image); err != nil {
			t.Errorf("ValidateImageReference(%q) = %v", image, err)
		}
	}
	for _, image := range []string{"", "   ", " padded ", "padded ", "-rm", "--privileged",
		"two words", "line\nbreak", "tab\there", "carriage\rreturn", "null\x00byte"} {
		if err := ValidateImageReference(image); err == nil {
			t.Errorf("ValidateImageReference(%q) accepted", image)
		}
	}
}
