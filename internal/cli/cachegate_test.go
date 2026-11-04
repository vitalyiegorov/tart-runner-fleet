package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func TestDoctorFailsOnAnUnverifiedCacheGate(t *testing.T) {
	const reason = "cache gate unreachable: http://192.168.1.64:3017/ did not prove its identity"
	status := healthyStatus()
	status.Data.CacheGate = &adminapi.CacheGate{URL: "http://192.168.1.64:3017/", ProbedAt: time.Unix(100, 0)}
	status.Data.CacheGateCheck = &adminapi.Check{Reasons: []string{reason}}
	assertDoctorNames(t, status, "cache gate", reason)
}

func TestDoctorStatesTheCacheGatePostureOnAPass(t *testing.T) {
	notReported := healthyStatus()
	none := healthyStatus()
	none.Data.CacheGate = &adminapi.CacheGate{}
	none.Data.CacheGateCheck = &adminapi.Check{OK: true}
	verified := healthyStatus()
	verified.Data.CacheGate = &adminapi.CacheGate{URL: "http://192.168.1.64:3017/", Verified: true}
	verified.Data.CacheGateCheck = &adminapi.Check{OK: true}
	for want, status := range map[string]adminapi.StatusEnvelope{
		"not reported by this daemon":        notReported,
		"no gate configured":                 none,
		"verified http://192.168.1.64:3017/": verified,
	} {
		if out := doctorTable(t, status, exitSuccess); !strings.Contains(out, want) {
			t.Fatalf("want %q:\n%s", want, out)
		}
	}
}
