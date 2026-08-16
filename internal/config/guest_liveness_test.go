package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// decodeWith rewrites the shipped default document, so every case here decodes a
// file an operator could actually have written rather than a struct literal that
// skipped the wire.
func decodeWith(t *testing.T, mutate func(map[string]any)) (Config, error) {
	t.Helper()
	var buffer bytes.Buffer
	if err := Encode(&buffer, Default()); err != nil {
		t.Fatalf("encode default: %v", err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(buffer.Bytes(), &document); err != nil {
		t.Fatalf("decode default document: %v", err)
	}
	mutate(document)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encode document: %v", err)
	}
	return Decode(bytes.NewReader(encoded))
}

// An omitted block is the shipped bound, not an absent one. A destructive
// mechanism gets a default only because the failure it prevents is worse and
// deterministic: eight runners, sixteen to eighteen minutes each, no log.
func TestGuestLivenessDefaultsWithoutBeingStated(t *testing.T) {
	cfg, err := decodeWith(t, func(map[string]any) {})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := GuestLiveness{ConsecutiveRefusals: defaultGuestLivenessRefusals,
		Window: defaultGuestLivenessWindow, ProbeTimeout: defaultGuestLivenessProbe}
	if cfg.GuestLiveness != want || !cfg.GuestLiveness.Enabled() {
		t.Fatalf("an unstated guest-liveness bound must default; got %#v", cfg.GuestLiveness)
	}
}

// The default must not appear in a rewritten file, or an older strict release
// stops being able to decode a configuration this one wrote.
func TestADefaultGuestLivenessBoundIsNotWrittenToTheFile(t *testing.T) {
	var buffer bytes.Buffer
	if err := Encode(&buffer, Default()); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(buffer.String(), "guestLiveness") {
		t.Fatalf("a default bound must encode no key at all; got %s", buffer.String())
	}
}

func TestAStatedGuestLivenessBoundSurvivesARoundTrip(t *testing.T) {
	cfg, err := decodeWith(t, func(document map[string]any) {
		document["guestLivenessRefusals"] = 7.0
		document["guestLivenessWindowSeconds"] = 300.0
		document["guestLivenessProbeTimeoutSeconds"] = 9.0
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := GuestLiveness{ConsecutiveRefusals: 7, Window: 5 * time.Minute, ProbeTimeout: 9 * time.Second}
	if cfg.GuestLiveness != want {
		t.Fatalf("stated bound = %#v, want %#v", cfg.GuestLiveness, want)
	}
	var buffer bytes.Buffer
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatalf("encode: %v", err)
	}
	round, err := Decode(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if round.GuestLiveness != want {
		t.Fatalf("round trip = %#v, want %#v", round.GuestLiveness, want)
	}
}

// Turning a destructive mechanism off must be as easy as leaving it alone, and
// it must survive being written back — an operator who disabled it does not want
// the next `fleet config` write to re-enable it.
func TestGuestLivenessIsDisabledByAnExplicitSentinel(t *testing.T) {
	cfg, err := decodeWith(t, func(document map[string]any) {
		document["guestLivenessRefusals"] = -1.0
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.GuestLiveness.Enabled() || cfg.GuestLiveness != (GuestLiveness{}) {
		t.Fatalf("a stated -1 must disable the mechanism; got %#v", cfg.GuestLiveness)
	}
	var buffer bytes.Buffer
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(buffer.String(), `"guestLivenessRefusals": -1`) {
		t.Fatalf("a disabled bound must be written, not omitted; got %s", buffer.String())
	}
	round, err := Decode(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if round.GuestLiveness.Enabled() {
		t.Fatalf("a disabled bound must stay disabled across a round trip; got %#v", round.GuestLiveness)
	}
	// A window stated as -1 disables it too: either half of the bound is enough
	// to say "this node does not probe its guests".
	cfg, err = decodeWith(t, func(document map[string]any) { document["guestLivenessWindowSeconds"] = -1.0 })
	if err != nil || cfg.GuestLiveness.Enabled() {
		t.Fatalf("a stated -1 window must disable the mechanism; got %#v err=%v", cfg.GuestLiveness, err)
	}
}

func TestGuestLivenessRejectsABoundThatCouldNeverBeRight(t *testing.T) {
	for _, test := range []struct {
		name     string
		liveness GuestLiveness
		valid    bool
	}{
		{name: "the shipped default", valid: true, liveness: Default().GuestLiveness},
		{name: "disabled", valid: true, liveness: GuestLiveness{}},
		{name: "at the floors", valid: true, liveness: GuestLiveness{ConsecutiveRefusals: minGuestLivenessRefusals,
			Window: minGuestLivenessWindow, ProbeTimeout: minGuestLivenessProbeTimeout}},
		{name: "at the ceilings", valid: true, liveness: GuestLiveness{ConsecutiveRefusals: maxGuestLivenessRefusals,
			Window: maxGuestLivenessWindow, ProbeTimeout: maxGuestLivenessProbeTimeout}},
		{name: "two refusals is one agent restart from a false positive", valid: false,
			liveness: GuestLiveness{ConsecutiveRefusals: 2, Window: time.Minute, ProbeTimeout: time.Second}},
		{name: "a window shorter than a tick", valid: false,
			liveness: GuestLiveness{ConsecutiveRefusals: 5, Window: 29 * time.Second, ProbeTimeout: time.Second}},
		{name: "a window that could never fire", valid: false,
			liveness: GuestLiveness{ConsecutiveRefusals: 5, Window: 2 * time.Hour, ProbeTimeout: time.Second}},
		{name: "a probe deadline that outlasts a tick", valid: false,
			liveness: GuestLiveness{ConsecutiveRefusals: 5, Window: time.Minute, ProbeTimeout: 2 * time.Minute}},
		{name: "half a bound is not a bound", valid: false,
			liveness: GuestLiveness{ConsecutiveRefusals: 5}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.GuestLiveness = test.liveness
			if err := cfg.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, want valid=%v", err, test.valid)
			}
		})
	}
}

func TestGuestLivenessRefusesSubSecondBounds(t *testing.T) {
	cfg := Default()
	cfg.GuestLiveness.Window = 90*time.Second + time.Millisecond
	if err := Encode(&bytes.Buffer{}, cfg); err == nil {
		t.Fatal("a sub-second window must not encode")
	}
	cfg = Default()
	cfg.GuestLiveness.ProbeTimeout = 5*time.Second + time.Millisecond
	if err := Encode(&bytes.Buffer{}, cfg); err == nil {
		t.Fatal("a sub-second probe timeout must not encode")
	}
}
