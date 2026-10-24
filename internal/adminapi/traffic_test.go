package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func bytesOf(value int64) *int64 { return &value }

// TestSummarizeTrafficGroupsByRepoWorkflowAndProfile answers the operator's
// question — which repo and workflow pulled the most bytes — and keeps
// unmeasured jobs out of the byte sums while still counting them.
func TestSummarizeTrafficGroupsByRepoWorkflowAndProfile(t *testing.T) {
	ota := "budgie-at/budgie/.github/workflows/ota.yml"
	jobs := []TrafficJob{
		{Repo: "budgie-at/budgie", Profile: "linux-4x8", WorkflowRef: ota + "@refs/pull/1650/merge", Measured: true,
			ReceiveBytes: bytesOf(1345000000), TransmitBytes: bytesOf(21000000)},
		{Repo: "budgie-at/budgie", Profile: "linux-4x8", WorkflowRef: ota + "@refs/pull/1653/merge", Measured: true,
			ReceiveBytes: bytesOf(1483000000), TransmitBytes: bytesOf(1000)},
		{Repo: "budgie-at/budgie", Profile: "linux-4x8", WorkflowRef: ota + "@refs/pull/1656/merge",
			Unmeasured: "exec_timeout"},
		{Repo: "o/small", Profile: "linux-2x4", Measured: true, ReceiveBytes: bytesOf(10), TransmitBytes: bytesOf(5)},
	}
	totals := SummarizeTraffic(jobs)
	if len(totals) != 2 {
		t.Fatalf("totals = %+v, want one row per repo/workflow/profile", totals)
	}
	top := totals[0]
	if top.Workflow != ota || top.Jobs != 3 || top.MeasuredJobs != 2 || top.ReceiveBytes != 2828000000 ||
		top.TransmitBytes != 21001000 {
		t.Fatalf("top = %+v, want the OTA workflow with its PR refs merged and unmeasured bytes excluded", top)
	}
	if totals[1].Repo != "o/small" || totals[1].Workflow != "" || totals[1].Jobs != 1 {
		t.Fatalf("second = %+v", totals[1])
	}
	if got := SummarizeTraffic(nil); got == nil || len(got) != 0 {
		t.Fatalf("no jobs is an empty, non-nil list: %#v", got)
	}
}

// TestUnmeasuredJobOmitsByteFields keeps an unreadable sample from reaching a
// consumer as zero bytes.
func TestUnmeasuredJobOmitsByteFields(t *testing.T) {
	encoded, _ := json.Marshal(TrafficJob{Instance: "trf-a", Unmeasured: "exec_failed"})
	if strings.Contains(string(encoded), "receiveBytes") || strings.Contains(string(encoded), "transmitBytes") {
		t.Fatalf("an unmeasured job must not carry byte fields: %s", encoded)
	}
}

func TestClientTrafficRequestsTheWindowAndValidatesTheEnvelope(t *testing.T) {
	var query string
	kind := TrafficKind
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TrafficEnvelope{APIVersion: APIVersion, Kind: kind})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Traffic(context.Background(), 24*time.Hour); err != nil {
		t.Fatalf("Traffic() = %v", err)
	}
	if query != TrafficPath+"?sinceSeconds=86400" {
		t.Fatalf("query = %q", query)
	}
	kind = "Status"
	if _, err := client.Traffic(context.Background(), time.Hour); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("a foreign envelope must be refused, got %v", err)
	}
	server.Close()
	if _, err := client.Traffic(context.Background(), time.Hour); err == nil {
		t.Fatal("an unreachable daemon must be an error")
	}
}
