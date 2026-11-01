package scheduler

import (
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestIssue461HostTenantMemoryIsNotReleasedByLinuxDrain replays Omarchy on
// 2026-11-01: admission open (memory above its reserve), no guests, yet the
// measured residual left by a host tenant -- 1088 MiB -- fits no Linux
// profile. The standing offer promises a job a wait for this node's own Linux
// work; a host tenant's hold is not that work, so such a set must not be
// promised a slot. What the node's own Linux guests hold still drains.
func TestIssue461HostTenantMemoryIsNotReleasedByLinuxDrain(t *testing.T) {
	cfg := testConfig()
	large := runningInstance(cfg, "large-1", "a/repo", "large")
	withAvailable := func(memoryMB int, instances ...domain.Instance) Input {
		in := input(nil, instances, State{})
		in.Host.Value.Available.MemoryMB = memoryMB
		return in
	}
	cases := map[string]struct {
		in      Input
		profile domain.ProfileID
		want    bool
	}{
		"a host tenant leaves 1088 MiB: no 4x8 fits however long Linux drains": {
			in: withAvailable(1_088), profile: "large", want: false},
		"a host tenant leaves 1088 MiB: no 2x4 fits either": {
			in: withAvailable(1_088), profile: "medium", want: false},
		"a host tenant leaves room for a 2x4 but not a 4x8": {
			in: withAvailable(5_000), profile: "medium", want: true},
		"what a running Linux guest holds is released by its drain": {
			in: withAvailable(1_088, large), profile: "large", want: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := FitsOnceLinuxDrains(tc.in, tc.profile); got != tc.want {
				t.Fatalf("FitsOnceLinuxDrains(%s) = %v, want %v", tc.profile, got, tc.want)
			}
		})
	}
}
