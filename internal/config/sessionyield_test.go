package config

import (
	"strings"
	"testing"
	"time"
)

// The yield bounds are hysteresis, and hysteresis with a zero or absurd bound is
// not hysteresis. They are validated like every other guard rather than trusted
// to be sensible.

func TestSessionYieldBoundsAreValidated(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"blocked bound too small", func(c *Config) { c.SessionYield.BlockedFor = time.Second }, "blocked bound"},
		{"blocked bound too large", func(c *Config) { c.SessionYield.BlockedFor = 2 * time.Hour }, "blocked bound"},
		{"healthy bound too small", func(c *Config) { c.SessionYield.HealthyFor = 0 }, "healthy bound"},
		{"healthy bound too large", func(c *Config) { c.SessionYield.HealthyFor = 2 * time.Hour }, "healthy bound"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want a yield bound failure")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() = %v, want it to name the %s", err, test.want)
			}
		})
	}
}

// Withdrawal is on by default: a node hoarding jobs it cannot run is the
// failure this defaults against, so silence in the file means the policy is
// live.
func TestSessionYieldDefaultsToEnabledAndRoundTrips(t *testing.T) {
	cfg := Default()
	if !cfg.SessionYield.Enabled {
		t.Fatal("a default node holds jobs it cannot run")
	}
	if cfg.SessionYield.BlockedFor != 10*time.Minute || cfg.SessionYield.HealthyFor != 2*time.Minute {
		t.Fatalf("default bounds are %v/%v", cfg.SessionYield.BlockedFor, cfg.SessionYield.HealthyFor)
	}

	cfg.SessionYield = SessionYield{Enabled: false, BlockedFor: 90 * time.Second, HealthyFor: 45 * time.Second}
	var buffer strings.Builder
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	encoded := buffer.String()
	for _, want := range []string{"sessionYieldDisabled", "sessionYieldBlockedForSeconds", "sessionYieldHealthyForSeconds"} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("a non-default policy omitted %s from the file:\n%s", want, encoded)
		}
	}
	decoded, err := Decode(strings.NewReader(encoded))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.SessionYield != cfg.SessionYield {
		t.Fatalf("round trip produced %+v, want %+v", decoded.SessionYield, cfg.SessionYield)
	}
}

// A file that says nothing about the policy gets the shipped one, and writing
// that file back must not start naming defaults it never carried.
func TestSessionYieldDefaultsAreOmittedFromARewrittenFile(t *testing.T) {
	cfg := Default()
	var buffer strings.Builder
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(buffer.String(), "sessionYield") {
		t.Fatalf("a default policy was written to the file:\n%s", buffer.String())
	}
}

// A duration the wire format cannot express is refused rather than silently
// truncated: a bound rounded to zero is not the bound the operator wrote.
func TestSessionYieldSubSecondBoundsAreRefusedByEncode(t *testing.T) {
	for _, test := range []struct {
		name  string
		yield SessionYield
	}{
		{"blocked", SessionYield{Enabled: true, BlockedFor: 90*time.Second + 500*time.Millisecond, HealthyFor: 2 * time.Minute}},
		{"healthy", SessionYield{Enabled: true, BlockedFor: 10 * time.Minute, HealthyFor: 45*time.Second + 250*time.Millisecond}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.SessionYield = test.yield
			var buffer strings.Builder
			if err := Encode(&buffer, cfg); err == nil {
				t.Fatal("Encode accepted a bound it cannot express in whole seconds")
			}
		})
	}
}
