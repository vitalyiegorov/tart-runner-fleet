package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The parked-scale-set audit cadence has three readings and they must not
// collide: absence is the default cadence, an explicit 0 is the operator turning
// the audit off, and anything else is what they asked for. A plain integer could
// not tell the first two apart, and reading "unset" as "off" is how a detector
// stops existing without anyone deciding it should (issue #164).
func TestTheAuditCadenceDistinguishesAbsenceFromOff(t *testing.T) {
	var github GitHub
	if interval := github.ParkedScaleSetAuditInterval(); interval != DefaultParkedScaleSetAuditInterval {
		t.Fatalf("absence is the default cadence: %s", interval)
	}
	for minutes, want := range map[int]time.Duration{0: 0, 1: time.Minute, 15: 15 * time.Minute, 1440: 24 * time.Hour} {
		github.ParkedScaleSetAuditMinutes = &minutes
		if interval := github.ParkedScaleSetAuditInterval(); interval != want {
			t.Fatalf("%d minutes = %s, want %s", minutes, interval, want)
		}
	}
}

// A cadence outside the bounds is refused rather than clamped or read as off.
func TestAnImplausibleAuditCadenceIsRefused(t *testing.T) {
	for _, minutes := range []int{-1, 1441} {
		cfg := Default()
		value := minutes
		cfg.GitHub.ParkedScaleSetAuditMinutes = &value
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "parked scale set audit minutes") {
			t.Fatalf("%d minutes must be refused: %v", minutes, err)
		}
	}
}

// The cadence survives the on-disk round trip, and a clone owns its own copy —
// a shared pointer would let one configuration's cadence change another's.
func TestTheAuditCadenceRoundTripsAndIsCloned(t *testing.T) {
	cfg := Default()
	minutes := 30
	cfg.GitHub.ParkedScaleSetAuditMinutes = &minutes

	var encoded bytes.Buffer
	if err := Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"parkedScaleSetAuditMinutes": 30`) {
		t.Fatalf("the cadence must be written: %s", encoded.String())
	}
	decoded, err := Decode(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.GitHub.ParkedScaleSetAuditInterval() != 30*time.Minute {
		t.Fatalf("the cadence must survive: %s", decoded.GitHub.ParkedScaleSetAuditInterval())
	}

	clone := decoded.Clone()
	*clone.GitHub.ParkedScaleSetAuditMinutes = 5
	if decoded.GitHub.ParkedScaleSetAuditInterval() != 30*time.Minute {
		t.Fatal("a clone must not share the original's cadence")
	}
	bare := Default().Clone()
	if bare.GitHub.ParkedScaleSetAuditMinutes != nil {
		t.Fatal("cloning an absent cadence must not invent one")
	}
}
