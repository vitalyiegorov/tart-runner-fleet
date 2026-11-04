package adminapi

import "testing"

func TestEffectiveCacheGateCheck(t *testing.T) {
	if got := (Status{}).EffectiveCacheGateCheck(); !got.OK || got.Reasons == nil {
		t.Fatalf("absent = %+v", got)
	}
	want := Check{Reasons: []string{"cache gate unreachable"}}
	if got := (Status{CacheGateCheck: &want}).EffectiveCacheGateCheck(); got.OK || got.Reasons[0] != want.Reasons[0] {
		t.Fatalf("published = %+v", got)
	}
}
