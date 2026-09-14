package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// withdrawnStatus is the mac studio of issue #320: under its disk reserve, every
// session released, nothing running, and still ticking. It is NOT ready and it
// IS healthy, and everything an operator or a release transaction reads has to
// keep those two apart.
func withdrawnStatus() adminapi.StatusEnvelope {
	status := healthyStatus()
	status.Data.Ready = adminapi.Check{Reasons: []string{"critical_observation_stale"}}
	status.Data.Healthy = &adminapi.Check{OK: true, Reasons: []string{}}
	status.Data.Instances = nil
	return status
}

func TestRequireHealthyAcceptsAWithdrawnNodeThatRequireReadyRefuses(t *testing.T) {
	status := withdrawnStatus()
	deps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: status, live: status.Data.Live, ready: status.Data.Ready}, nil
	}}
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"status", "--require-healthy"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("--require-healthy refused a healthy withdrawn node: exit %d", code)
	}
	stdout.Reset()
	if code := executeWith(context.Background(), []string{"status", "--require-ready"}, &stdout, &stderr, deps); code != exitDegraded {
		t.Fatalf("--require-ready accepted a node admitting nothing: exit %d", code)
	}
	// A node that has stopped ticking fails both. Health is a gate, not a way of
	// passing.
	broken := withdrawnStatus()
	broken.Data.Healthy = &adminapi.Check{Reasons: []string{"successful_tick_expired"}}
	brokenDeps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: broken, live: broken.Data.Live, ready: broken.Data.Ready}, nil
	}}
	stdout.Reset()
	if code := executeWith(context.Background(), []string{"status", "--require-healthy"}, &stdout, &stderr, brokenDeps); code != exitDegraded {
		t.Fatalf("--require-healthy accepted a daemon that stopped ticking: exit %d", code)
	}
	// A daemon older than ADR 0052 publishes no health field; readiness is the
	// only word it has for the question, so the flag reads that instead.
	older := withdrawnStatus()
	older.Data.Healthy = nil
	olderDeps := dependencies{newClient: func(string, time.Duration) (apiClient, error) {
		return fakeClient{status: older, live: older.Data.Live, ready: older.Data.Ready}, nil
	}}
	stdout.Reset()
	if code := executeWith(context.Background(), []string{"status", "--require-healthy"}, &stdout, &stderr, olderDeps); code != exitDegraded {
		t.Fatalf("--require-healthy invented health an older daemon never reported: exit %d", code)
	}
}

// The status header said only NOT READY, which reads as a fault. A node that is
// ticking while refusing work is not one, and the difference is now the
// difference between a release that installs and one that never can.
func TestStatusSaysANotReadyNodeIsStillHealthy(t *testing.T) {
	rendered := renderYieldStatus(withdrawnStatus())
	if !strings.Contains(rendered, "NOT READY") {
		t.Fatalf("a withdrawn node no longer reports NOT READY:\n%s", rendered)
	}
	if !strings.Contains(rendered, "healthy: the daemon is ticking") {
		t.Fatalf("a healthy withdrawn node read as broken:\n%s", rendered)
	}
	broken := withdrawnStatus()
	broken.Data.Healthy = &adminapi.Check{Reasons: []string{"successful_tick_expired"}}
	if strings.Contains(renderYieldStatus(broken), "healthy: the daemon is ticking") {
		t.Fatal("a daemon that stopped ticking claimed health")
	}
}

// The drain row is where an operator watches a node that is admitting nothing.
// With zero instances it must say whether the release can now land — which for
// a withdrawn node it can — or name what is still blocking the candidate,
// rather than showing a drain that appears able to run forever (#320).
func TestDoctorNamesTheStateOfADrainThatHasReachedZeroInstances(t *testing.T) {
	const reason = "this node is refusing admission to reach the quiescence v0.1.552 needs"
	drain := adminapi.Check{Reasons: []string{reason}}
	status := withdrawnStatus()
	status.Data.UpdateDrain = &adminapi.UpdateDrain{Draining: true, Candidate: "v0.1.552"}
	status.Data.UpdateDrainCheck = &drain
	detail := updateDrainDetail(status.Data, drain)
	if !strings.Contains(detail, "zero instances and healthy") || !strings.Contains(detail, "may apply now") {
		t.Fatalf("a drainable withdrawn node rendered as %q", detail)
	}
	assertDoctorNames(t, status, "update drain", "zero instances and healthy")

	blocked := status
	blocked.Data.Healthy = &adminapi.Check{Reasons: []string{"successful_tick_expired"}}
	if detail := updateDrainDetail(blocked.Data, drain); !strings.Contains(detail, "successful_tick_expired") ||
		!strings.Contains(detail, "blocked") {
		t.Fatalf("a blocked candidate rendered as %q", detail)
	}

	// A drain that still has work running is reported as the plain drain it is:
	// there is no candidate to unblock while an instance holds the node.
	busy := status
	busy.Data.Instances = []adminapi.Instance{{Profile: "maestro", Count: 1}}
	if detail := updateDrainDetail(busy.Data, drain); detail != reason {
		t.Fatalf("a busy drain rendered as %q", detail)
	}
}
