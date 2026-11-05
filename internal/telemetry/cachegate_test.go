package telemetry

import (
	"strings"
	"testing"
	"time"
)

func TestCacheGateResult(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	if cacheGateResult(Snapshot{}) != nil {
		t.Fatal("an unpublished gate read as a verdict")
	}
	for name, metric := range map[string]CacheGateMetric{
		"none":     {},
		"verified": {URL: "http://192.168.1.64:3017/", Verified: true, ProbedAt: at},
	} {
		if got := cacheGateResult(Snapshot{CacheGatePublished: true, CacheGate: metric}); !got.OK {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	got := cacheGateResult(Snapshot{CacheGatePublished: true,
		CacheGate: CacheGateMetric{URL: "http://192.168.1.64:3017/", ProbedAt: at}})
	if got.OK || !strings.Contains(got.Reasons[0], "cache gate unreachable: http://192.168.1.64:3017/") {
		t.Fatalf("%+v", got)
	}
}

// TestAPodmanNodesFailedProofSaysWhatReallyHappens: on Podman the results URL is image
// ENV, so nothing was rewritten and only the npm setting changed.
func TestAPodmanNodesFailedProofSaysWhatReallyHappens(t *testing.T) {
	got := cacheGateResult(Snapshot{CacheGatePublished: true,
		CacheGate: CacheGateMetric{URL: "http://127.0.0.1:3017/", Container: true, ProbedAt: time.Unix(100, 0)}})
	if got.OK || !strings.Contains(got.Reasons[0], "cache gate unreachable") ||
		!strings.Contains(got.Reasons[0], "reach npm directly") || strings.Contains(got.Reasons[0], "use GitHub's results service") {
		t.Fatalf("%+v", got)
	}
}

// TestADownNPMRouteIsNeverSilent: the guests are fine and going direct, and at home
// that is every npm byte back on the uplink with no other signal.
func TestADownNPMRouteIsNeverSilent(t *testing.T) {
	const registry = "http://192.168.1.64:3017/npm/"
	down := CacheGateMetric{URL: "http://192.168.1.64:3017/", Verified: true, ProbedAt: time.Unix(100, 0), NPMRegistry: registry}
	got := cacheGateResult(Snapshot{CacheGatePublished: true, CacheGate: down})
	if got.OK || !strings.Contains(got.Reasons[0], "npm cache not ready: "+registry) {
		t.Fatalf("%+v", got)
	}
	up := down
	up.NPMReady = true
	if got := cacheGateResult(Snapshot{CacheGatePublished: true, CacheGate: up}); !got.OK {
		t.Fatalf("%+v", got)
	}
}

func TestPublishingOneGateAnswerKeepsTheOther(t *testing.T) {
	health, _ := newTestHealth(t)
	health.SetCacheGateNPM("http://192.168.1.64:3017/npm/", true, time.Unix(90, 0))
	health.SetCacheGate(CacheGateMetric{URL: "http://192.168.1.64:3017/", Verified: true, ProbedAt: time.Unix(100, 0)})
	gate := health.Snapshot().CacheGate
	if gate.NPMRegistry == "" || !gate.NPMReady || !gate.Verified {
		t.Fatalf("%+v", gate)
	}
	health.SetCacheGateNPM("http://192.168.1.64:3017/npm/", false, time.Unix(110, 0))
	gate = health.Snapshot().CacheGate
	if gate.NPMReady || !gate.Verified || !gate.ProbedAt.Equal(time.Unix(100, 0)) || !gate.NPMProbedAt.Equal(time.Unix(110, 0)) {
		t.Fatalf("%+v", gate)
	}
}

func TestTheNPMAnswerReachesTheStatusDocument(t *testing.T) {
	health, _ := newTestHealth(t)
	health.SetCacheGate(CacheGateMetric{URL: "http://192.168.1.64:3017/", Verified: true, ProbedAt: time.Unix(100, 0)})
	health.SetCacheGateNPM("http://192.168.1.64:3017/npm/", true, time.Unix(130, 0))
	gate := imageToolsStatus(health).Data.CacheGate
	if gate == nil || gate.NPMRegistry != "http://192.168.1.64:3017/npm/" || gate.NPMReady == nil || !*gate.NPMReady ||
		!gate.ProbedAt.Equal(time.Unix(100, 0)) || !gate.NPMProbedAt.Equal(time.Unix(130, 0)) {
		t.Fatalf("%+v", gate)
	}
}

func TestANodeWithNoNPMRouteStatesNothingAboutOne(t *testing.T) {
	health, _ := newTestHealth(t)
	health.SetCacheGate(CacheGateMetric{URL: "http://192.168.1.64:3017/", Verified: true, ProbedAt: time.Unix(100, 0)})
	gate := imageToolsStatus(health).Data.CacheGate
	if gate == nil || gate.NPMRegistry != "" || gate.NPMReady != nil || !gate.NPMProbedAt.IsZero() {
		t.Fatalf("%+v", gate)
	}
}

func TestCacheGateReachesTheStatusDocument(t *testing.T) {
	health, _ := newTestHealth(t)
	before := imageToolsStatus(health)
	if before.Data.CacheGateCheck != nil || before.Data.CacheGate != nil {
		t.Fatalf("published before anything looked: %+v", before.Data.CacheGate)
	}
	health.SetCacheGate(CacheGateMetric{URL: "http://192.168.1.64:3017/", ProbedAt: time.Unix(100, 0)})
	after := imageToolsStatus(health)
	if after.Data.CacheGateCheck == nil || after.Data.CacheGateCheck.OK || after.Data.CacheGate == nil ||
		after.Data.CacheGate.URL != "http://192.168.1.64:3017/" || after.Data.CacheGate.Verified {
		t.Fatalf("check = %+v, gate = %+v", after.Data.CacheGateCheck, after.Data.CacheGate)
	}
}

// TestEachProbeKeepsItsOwnTime: the identity proof and the npm route are asked at
// different moments, so a reason or a status field naming the other one's time reports
// a failure at a moment nothing failed.
func TestEachProbeKeepsItsOwnTime(t *testing.T) {
	const gateURL, registry = "http://192.168.1.64:3017/", "http://192.168.1.64:3017/npm/"
	earlier := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	later := earlier.Add(30 * time.Second)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339) }

	t.Run("a failed proof keeps its time past a later npm probe", func(t *testing.T) {
		health, _ := newTestHealth(t)
		health.SetCacheGate(CacheGateMetric{URL: gateURL, ProbedAt: earlier})
		health.SetCacheGateNPM(registry, false, later)
		status := imageToolsStatus(health)
		reason := status.Data.CacheGateCheck.Reasons[0]
		if !strings.Contains(reason, "did not prove its identity at "+stamp(earlier)) {
			t.Fatalf("reason = %q", reason)
		}
		if !status.Data.CacheGate.ProbedAt.Equal(earlier) {
			t.Fatalf("probedAt = %v, want the identity proof's %v", status.Data.CacheGate.ProbedAt, earlier)
		}
	})
	t.Run("a down npm route keeps its time past a later proof", func(t *testing.T) {
		health, _ := newTestHealth(t)
		health.SetCacheGateNPM(registry, false, earlier)
		health.SetCacheGate(CacheGateMetric{URL: gateURL, Verified: true, ProbedAt: later})
		status := imageToolsStatus(health)
		reason := status.Data.CacheGateCheck.Reasons[0]
		if !strings.Contains(reason, "npm cache not ready: "+registry+" did not answer at "+stamp(earlier)) {
			t.Fatalf("reason = %q", reason)
		}
		if !status.Data.CacheGate.ProbedAt.Equal(later) {
			t.Fatalf("probedAt = %v, want the identity proof's %v", status.Data.CacheGate.ProbedAt, later)
		}
	})
}
