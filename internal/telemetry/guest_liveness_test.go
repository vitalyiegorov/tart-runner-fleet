package telemetry

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
)

// deadGuest is the 2026-08-16 incident (issue #236) as telemetry sees it: an
// instance holding 6 CPU and 12288 MiB whose guest kernel panicked, five
// consecutive probes refused over two minutes, against a bound of five over
// ninety seconds.
func deadGuest() GuestSilenceMetric {
	return GuestSilenceMetric{Instance: "trf-xl-0aacdbcc6653bd8a", Profile: "builder",
		Repo: "rnw-community/rnw-community", CPU: 6, MemoryMB: 12_288,
		Refusals: 5, Silence: 2 * time.Minute, RequiredRefusals: 5, Window: 90 * time.Second,
		Unresponsive: true, RunID: 31_939_037_119, JobID: 93_540_000_001}
}

// watchedGuest has missed some probes and satisfied neither bound. It is the
// reading that must never wake anyone: a guest-agent hiccup on a busy host.
func watchedGuest() GuestSilenceMetric {
	return GuestSilenceMetric{Instance: "trf-small-9a1c", Profile: "linux-small", Repo: "sudoku-repo/builder",
		CPU: 2, MemoryMB: 4096, Refusals: 2, Silence: 40 * time.Second,
		RequiredRefusals: 5, Window: 90 * time.Second}
}

func guestMetricsBody(t *testing.T, health *Health) string {
	t.Helper()
	response := request(t, occupancyServer(t, health), http.MethodGet, adminapi.MetricsPath)
	defer func() { _ = response.Body.Close() }()
	return readAllString(t, response)
}

// A guest that answers again must disappear from the document. A merged update
// could not express recovery, so a guest that came back would keep reporting a
// silence that has ended.
func TestSetGuestSilencesReplacesTheWholeSet(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetGuestSilences([]GuestSilenceMetric{deadGuest(), watchedGuest()}); err != nil {
		t.Fatalf("SetGuestSilences() = %v", err)
	}
	if got := health.Snapshot().GuestSilences; len(got) != 2 {
		t.Fatalf("published silences = %#v, want both", got)
	}
	if err := health.SetGuestSilences(nil); err != nil {
		t.Fatalf("SetGuestSilences(nil) = %v", err)
	}
	if got := health.Snapshot().GuestSilences; len(got) != 0 {
		t.Fatalf("a recovered guest must disappear; got %#v", got)
	}
}

// The check fails on the verdict and nothing else. A partial run of refusals is
// the fleet watching, not the fleet reporting.
func TestGuestLivenessFailsOnlyOnTheVerdict(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetGuestSilences([]GuestSilenceMetric{watchedGuest()}); err != nil {
		t.Fatal(err)
	}
	if result := health.GuestLiveness(); !result.OK {
		t.Fatalf("a guest inside both bounds must not fail the check; got %#v", result)
	}
	if err := health.SetGuestSilences([]GuestSilenceMetric{deadGuest(), watchedGuest()}); err != nil {
		t.Fatal(err)
	}
	result := health.GuestLiveness()
	if result.OK || len(result.Reasons) != 1 {
		t.Fatalf("a declared-dead guest must fail the check exactly once; got %#v", result)
	}
	reason := result.Reasons[0]
	for _, want := range []string{"trf-xl-0aacdbcc6653bd8a", "2m0s", "5 consecutive refusals",
		"6 cpu / 12288 MiB", "run 31939037119", "job 93540000001"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the reason must name %q; got %q", want, reason)
		}
	}
}

// A rejected observation must never masquerade as "every guest is answering".
func TestSetGuestSilencesRejectsAnythingUnbounded(t *testing.T) {
	health, _ := newTestHealth(t)
	oversized := make([]GuestSilenceMetric, maxOccupancy+1)
	for index := range oversized {
		oversized[index] = watchedGuest()
	}
	if err := health.SetGuestSilences(oversized); err == nil {
		t.Fatal("an over-long set must be rejected outright")
	}
	for _, test := range []struct {
		name   string
		mutate func(*GuestSilenceMetric)
	}{
		{name: "negative refusals", mutate: func(m *GuestSilenceMetric) { m.Refusals = -1 }},
		{name: "negative silence", mutate: func(m *GuestSilenceMetric) { m.Silence = -time.Second }},
		{name: "negative bound", mutate: func(m *GuestSilenceMetric) { m.RequiredRefusals = -1 }},
		{name: "negative window", mutate: func(m *GuestSilenceMetric) { m.Window = -time.Second }},
		{name: "negative cpu", mutate: func(m *GuestSilenceMetric) { m.CPU = -1 }},
		{name: "negative memory", mutate: func(m *GuestSilenceMetric) { m.MemoryMB = -1 }},
		{name: "an instance id nothing minted", mutate: func(m *GuestSilenceMetric) { m.Instance = "../etc/passwd" }},
		{name: "a profile this fleet does not configure", mutate: func(m *GuestSilenceMetric) { m.Profile = "unknown" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			metric := watchedGuest()
			test.mutate(&metric)
			if err := health.SetGuestSilences([]GuestSilenceMetric{metric}); err == nil {
				t.Fatalf("%s must be rejected", test.name)
			}
		})
	}
	if got := health.Snapshot().GuestSilences; len(got) != 0 {
		t.Fatalf("a rejected set must publish nothing; got %#v", got)
	}
}

func TestGuestSilenceMetricsCarryTheMeasurementAndItsBound(t *testing.T) {
	health, _ := newTestHealth(t)
	if got := guestMetricsBody(t, health); strings.Contains(got, "fleet_instance_guest_silence_seconds") {
		t.Fatal("a fleet whose guests all answer publishes no silence series at all")
	}
	if err := health.SetGuestSilences([]GuestSilenceMetric{deadGuest(), watchedGuest()}); err != nil {
		t.Fatal(err)
	}
	body := guestMetricsBody(t, health)
	for _, want := range []string{
		`fleet_instance_guest_silence_seconds{profile="builder",instance="trf-xl-0aacdbcc6653bd8a"} 120` + "\n",
		`fleet_instance_guest_probe_refusals{profile="builder",instance="trf-xl-0aacdbcc6653bd8a"} 5` + "\n",
		`fleet_instance_guest_probe_refusals_required{profile="builder",instance="trf-xl-0aacdbcc6653bd8a"} 5` + "\n",
		`fleet_instance_guest_unresponsive{profile="builder",instance="trf-xl-0aacdbcc6653bd8a"} 1` + "\n",
		`fleet_instance_guest_unresponsive{profile="linux-small",instance="trf-small-9a1c"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics must publish %q; got:\n%s", want, body)
		}
	}
}

// The status document is where the unbounded facts travel: the job that died
// with the guest is not a metric label.
func TestTheStatusDocumentNamesTheJobThatDiedWithTheGuest(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetGuestSilences([]GuestSilenceMetric{deadGuest()}); err != nil {
		t.Fatal(err)
	}
	envelope := statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		health.GuestLiveness(), health.RunnerVersions(), health.GuestConsole())
	rows := envelope.Data.GuestSilences
	if len(rows) != 1 {
		t.Fatalf("guest silences = %#v", rows)
	}
	row := rows[0]
	if row.RunID != 31_939_037_119 || row.JobID != 93_540_000_001 || !row.Unresponsive ||
		row.Refusals != 5 || row.RequiredRefusals != 5 || row.SilenceSeconds != 120 || row.WindowSeconds != 90 {
		t.Fatalf("the row must carry the job and the whole probe timeline; got %#v", row)
	}
	if envelope.Data.EffectiveGuestLiveness().OK {
		t.Fatalf("the check must travel beside the rows; got %#v", envelope.Data.GuestLivenessCheck)
	}
	// A fleet whose guests all answer emits exactly the document older clients saw.
	if err := health.SetGuestSilences(nil); err != nil {
		t.Fatal(err)
	}
	envelope = statusEnvelope(health.Snapshot(), "v", "authority", HealthResult{OK: true}, HealthResult{OK: true},
		HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true}, HealthResult{OK: true},
		health.GuestLiveness(), health.RunnerVersions(), health.GuestConsole())
	if envelope.Data.GuestSilences != nil {
		t.Fatalf("nil must stay nil; got %#v", envelope.Data.GuestSilences)
	}
}

// A daemon that never probed a guest must not render as one that found every
// guest alive.
func TestAnOlderDaemonPublishesNoGuestVerdict(t *testing.T) {
	if got := (adminapi.Status{}).EffectiveGuestLiveness(); !got.OK || len(got.Reasons) != 0 {
		t.Fatalf("an absent check must render as unspecified; got %#v", got)
	}
}
