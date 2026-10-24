package operations

import (
	"testing"
	"time"
)

func TestTrafficSampleValidKeepsMeasuredAndUnmeasuredApart(t *testing.T) {
	at := time.Unix(1, 0)
	for name, testCase := range map[string]struct {
		sample TrafficSample
		valid  bool
	}{
		"measured":                {TrafficSample{InstanceID: "a", SampledAt: at, Measured: true, ReceiveBytes: 1}, true},
		"unmeasured":              {TrafficSample{InstanceID: "a", SampledAt: at, Unmeasured: TrafficExecFailed}, true},
		"no instance":             {TrafficSample{SampledAt: at, Measured: true}, false},
		"no time":                 {TrafficSample{InstanceID: "a", Measured: true}, false},
		"measured with reason":    {TrafficSample{InstanceID: "a", SampledAt: at, Measured: true, Unmeasured: TrafficExecFailed}, false},
		"negative":                {TrafficSample{InstanceID: "a", SampledAt: at, Measured: true, TransmitBytes: -1}, false},
		"unmeasured with bytes":   {TrafficSample{InstanceID: "a", SampledAt: at, Unmeasured: TrafficUnreadable, ReceiveBytes: 1}, false},
		"unmeasured, open reason": {TrafficSample{InstanceID: "a", SampledAt: at, Unmeasured: "because"}, false},
	} {
		if got := testCase.sample.Valid(); got != testCase.valid {
			t.Errorf("%s: Valid() = %t, want %t", name, got, testCase.valid)
		}
	}
}
