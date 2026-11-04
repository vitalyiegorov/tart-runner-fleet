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
