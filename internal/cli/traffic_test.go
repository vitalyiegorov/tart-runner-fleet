package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

func trafficDeps(client fakeClient) dependencies {
	deps := defaultDependencies()
	deps.newClient = func(string, time.Duration) (apiClient, error) { return client, nil }
	return deps
}

func sampleTraffic() adminapi.TrafficEnvelope {
	rx, tx := int64(2828000000), int64(21001000)
	return adminapi.TrafficEnvelope{APIVersion: adminapi.APIVersion, Kind: adminapi.TrafficKind, Data: adminapi.Traffic{
		Totals: []adminapi.TrafficTotal{{Repo: "budgie-at/budgie", Workflow: "budgie-at/budgie/.github/workflows/ota.yml",
			Profile: "linux-4x8", Jobs: 3, MeasuredJobs: 2, UnmeasuredJobs: 1, ReceiveBytes: rx, TransmitBytes: tx}},
		Jobs: []adminapi.TrafficJob{{Instance: "trf-a", Measured: true, ReceiveBytes: &rx, TransmitBytes: &tx}}}}
}

// TestTrafficAnswersWhichWorkflowPulledTheMostBytes is the operator's
// question from issue #414, in both output modes.
func TestTrafficAnswersWhichWorkflowPulledTheMostBytes(t *testing.T) {
	var window time.Duration
	client := fakeClient{traffic: sampleTraffic(), trafficWindow: &window}
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"traffic", "--since", "24h"}, &stdout, &stderr,
		trafficDeps(client)); code != exitSuccess {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if window != 24*time.Hour {
		t.Fatalf("window = %v", window)
	}
	for _, want := range []string{"REPO", "budgie-at/budgie", ".github/workflows/ota.yml", "linux-4x8", "2.83 GB",
		"21.00 MB", "1 unmeasured"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("table missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := executeWith(context.Background(), []string{"traffic", "--output", "json"}, &stdout, &stderr,
		trafficDeps(client)); code != exitSuccess || window != 24*time.Hour {
		t.Fatalf("json exit %d window %v", code, window)
	}
	var decoded adminapi.TrafficEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil || len(decoded.Data.Totals) != 1 {
		t.Fatalf("json = %s (%v)", stdout.String(), err)
	}
}

func TestTrafficSaysTruncatedAndRefusesBadInput(t *testing.T) {
	traffic := sampleTraffic()
	traffic.Data.Truncated = true
	traffic.Data.Totals = nil
	var stdout, stderr bytes.Buffer
	if code := executeWith(context.Background(), []string{"traffic"}, &stdout, &stderr,
		trafficDeps(fakeClient{traffic: traffic})); code != exitSuccess ||
		!strings.Contains(stdout.String(), "truncated") || !strings.Contains(stdout.String(), "no samples") {
		t.Fatalf("exit %d:\n%s", code, stdout.String())
	}
	for _, args := range [][]string{{"traffic", "--since", "0s"}, {"traffic", "--since", "800h"}, {"traffic", "extra"}} {
		if code := executeWith(context.Background(), args, &stdout, &stderr, trafficDeps(fakeClient{})); code != exitUsage {
			t.Fatalf("%v exit = %d, want usage", args, code)
		}
	}
	if code := executeWith(context.Background(), []string{"traffic"}, &stdout, &stderr,
		trafficDeps(fakeClient{err: errors.New("down")})); code == exitSuccess {
		t.Fatal("an unreachable daemon must not exit 0")
	}
	deps := defaultDependencies()
	deps.newClient = func(string, time.Duration) (apiClient, error) { return nil, errors.New("bad endpoint") }
	if code := executeWith(context.Background(), []string{"traffic"}, &stdout, &stderr, deps); code != exitUnavailable {
		t.Fatalf("connect failure exit = %d", code)
	}
	if code := executeWith(context.Background(), []string{"traffic", "-o", "json"}, errorWriter{}, &stderr,
		trafficDeps(fakeClient{traffic: traffic})); code == exitSuccess {
		t.Fatal("a broken stdout must not exit 0")
	}
}

func TestFormatBytesScales(t *testing.T) {
	for value, want := range map[int64]string{0: "0 B", 999: "999 B", 1500: "1.50 kB", 314828521: "314.83 MB",
		1340000000: "1.34 GB", 2_000_000_000_000: "2.00 TB"} {
		if got := formatBytes(value); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", value, got, want)
		}
	}
}
