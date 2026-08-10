package telemetry

import (
	"strings"
	"testing"
	"time"
)

// wedgedDrain is the 2026-08-10 incident (issue #233) as telemetry sees it: one
// drain that has failed 67 times at the stop step over 82 minutes while its
// instance sits in `deregistering` holding the studio's entire budget.
func wedgedDrain() Stalled {
	return Stalled{Operation: "event-drain-trf-macos-6x12-f458a747883b9a0d", Kind: "deregister", Code: "stop",
		Instance: "trf-macos-6x12-f458a747883b9a0d", Attempts: 67, Retrying: 82 * time.Minute,
		DrainState: "deregistering", Held: 82 * time.Minute}
}

// TestProgressNamesTheInstanceStepAttemptsAndElapsedTime is the whole point.
// Finding this on the studio took SSH, a hand-copied SQLite file, and a read of
// the operations table, because `fleet doctor` reported only the queue symptom.
func TestProgressNamesTheInstanceStepAttemptsAndElapsedTime(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetStalled([]Stalled{wedgedDrain()}); err != nil {
		t.Fatalf("SetStalled() = %v", err)
	}
	result := health.Progress()
	if result.OK {
		t.Fatal("a drain failing for 82 minutes was reported as progress")
	}
	joined := strings.Join(result.Reasons, "\n")
	for _, want := range []string{"trf-macos-6x12-f458a747883b9a0d", "event-drain-trf-macos-6x12-f458a747883b9a0d",
		"deregister", "stop", "67", "1h22m0s", "deregistering"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("progress reasons missing %q:\n%s", want, joined)
		}
	}
	if len(result.Reasons) != 2 {
		t.Fatalf("reasons = %#v, want one for the operation and one for the held instance", result.Reasons)
	}
}

// TestProgressStaysQuietForWorkThatIsStillWorking pins the other side. A drain
// that has failed once, or an instance that entered a cleanup state a moment
// ago, is ordinary; a check that cries wolf there is a check nobody reads.
func TestProgressStaysQuietForWorkThatIsStillWorking(t *testing.T) {
	health, _ := newTestHealth(t)
	healthy := wedgedDrain()
	healthy.Attempts, healthy.Retrying, healthy.Held = 1, 20*time.Second, 20*time.Second
	if err := health.SetStalled([]Stalled{healthy}); err != nil {
		t.Fatal(err)
	}
	if result := health.Progress(); !result.OK {
		t.Fatalf("a drain on its second attempt was reported as a wedge: %#v", result.Reasons)
	}
}

// TestProgressNamesAnInstanceWhoseDrainHasAlreadyDeadLettered is the row that
// has no operation left to name, and is the moment the instance is most stuck:
// nothing will advance it without an operator.
func TestProgressNamesAnInstanceWhoseDrainHasAlreadyDeadLettered(t *testing.T) {
	health, _ := newTestHealth(t)
	parked := Stalled{Instance: "trf-macos-6x12-f458a747883b9a0d", DrainState: "deregistering", Held: 3 * time.Hour}
	if err := health.SetStalled([]Stalled{parked}); err != nil {
		t.Fatal(err)
	}
	result := health.Progress()
	if result.OK || len(result.Reasons) != 1 {
		t.Fatalf("a parked instance was not named: %#v", result)
	}
	if !strings.Contains(result.Reasons[0], "held in deregistering for 3h0m0s") {
		t.Fatalf("reason = %q", result.Reasons[0])
	}
}

// TestSetStalledReplacesTheWholeSet proves a drain that finished disappears
// from the document rather than being reported forever.
func TestSetStalledReplacesTheWholeSet(t *testing.T) {
	health, _ := newTestHealth(t)
	if err := health.SetStalled([]Stalled{wedgedDrain()}); err != nil {
		t.Fatal(err)
	}
	if err := health.SetStalled(nil); err != nil {
		t.Fatal(err)
	}
	if got := health.Snapshot().Stalled; len(got) != 0 {
		t.Fatalf("a cleared wedge lingered: %#v", got)
	}
	if !health.Progress().OK {
		t.Fatal("a cleared wedge still fails the check")
	}
}

// TestSetStalledRejectsAnythingUnbounded keeps upstream text and unbounded label
// cardinality out of the operator API, exactly as the dead-letter and occupancy
// setters do. A rejected observation must never masquerade as "everything is
// progressing", which is why rejection is outright rather than truncating.
func TestSetStalledRejectsAnythingUnbounded(t *testing.T) {
	health, _ := newTestHealth(t)
	badOperation, badKind, badCode := wedgedDrain(), wedgedDrain(), wedgedDrain()
	badOperation.Operation = "operation with spaces"
	badKind.Kind = "Deregister/../x"
	badCode.Code = "tart stop: connection to https://api.github.com refused"
	badInstance, badState, negative := wedgedDrain(), wedgedDrain(), wedgedDrain()
	badInstance.Instance = "../escape"
	badState.DrainState = "running"
	negative.Attempts = -1
	for name, row := range map[string]Stalled{"operation": badOperation, "kind": badKind, "code": badCode,
		"instance": badInstance, "drain state": badState, "attempts": negative} {
		t.Run(name, func(t *testing.T) {
			if err := health.SetStalled([]Stalled{row}); err == nil {
				t.Fatalf("SetStalled accepted %#v", row)
			}
		})
	}
	oversized := make([]Stalled, maxStalled+1)
	for i := range oversized {
		oversized[i] = wedgedDrain()
	}
	if err := health.SetStalled(oversized); err == nil {
		t.Fatal("SetStalled accepted an unbounded set")
	}
	if got := health.Snapshot().Stalled; len(got) != 0 {
		t.Fatalf("a rejected observation reached the document: %#v", got)
	}
}

func TestProgressThresholdsAreConfigurableAndValidated(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
	for name, config := range map[string]HealthConfig{
		"negative attempts": {StalledAttempts: -1},
		"negative hold":     {DrainHold: -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHealth(clock, config); err == nil {
				t.Fatal("invalid progress thresholds accepted")
			}
		})
	}
	health, err := NewHealth(clock, HealthConfig{StalledAttempts: 100, DrainHold: 3 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := health.SetStalled([]Stalled{wedgedDrain()}); err != nil {
		t.Fatal(err)
	}
	if !health.Progress().OK {
		t.Fatal("a fleet that raised both thresholds still failed the check")
	}
}
