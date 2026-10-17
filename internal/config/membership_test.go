package config

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/membership"
)

// ADR 0063: the watchdog is opt-in. A file that says nothing is the node it was
// before, and rewriting that file must not start naming a mode it never chose.
func TestMembershipIsOffByDefaultAndOmittedFromARewrittenFile(t *testing.T) {
	cfg := Default()
	if cfg.Membership != membership.Off {
		t.Fatalf("a default node runs the membership watchdog: %q", cfg.Membership)
	}
	var buffer strings.Builder
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(buffer.String(), "membership") {
		t.Fatalf("an unset membership was written to the file:\n%s", buffer.String())
	}
}

// scopedDefault is the shipped default with one scope, which is what the
// scale-set audit -- the watchdog's only evidence -- reads.
func scopedDefault() Config {
	cfg := Default()
	cfg.GitHub = GitHub{SessionOwner: "fleet-session",
		App:           GitHubApp{ClientID: "client", KeychainService: "fleet", KeychainAccount: "app"},
		Installations: []GitHubInstallation{{Name: "personal", InstallationID: 101}},
		Scopes: []GitHubScope{{Name: "knee", Kind: ScopeRepository, ConfigURL: "https://github.com/owner/knee",
			Installation: "personal", Targets: []string{"owner/repo"},
			ScaleSets: []ScaleSet{{Profile: "small", Name: "knee-small", ID: 19, MaxCapacity: 4,
				Labels: []string{"self-hosted", "linux-small"}}}}}}
	return cfg
}

func TestMembershipRoundTrips(t *testing.T) {
	for _, mode := range []membership.Mode{membership.Stationary, membership.Roaming} {
		cfg := scopedDefault()
		cfg.Membership = mode
		var buffer strings.Builder
		if err := Encode(&buffer, cfg); err != nil {
			t.Fatalf("Encode(%s): %v", mode, err)
		}
		if !strings.Contains(buffer.String(), `"membership": "`+string(mode)+`"`) {
			t.Fatalf("mode %s missing from the file:\n%s", mode, buffer.String())
		}
		decoded, err := Decode(strings.NewReader(buffer.String()))
		if err != nil || decoded.Membership != mode {
			t.Fatalf("round trip of %s = %q, %v", mode, decoded.Membership, err)
		}
	}
}

func TestMembershipRefusesAnUnknownModeAndAWatchdogWithNoEvidence(t *testing.T) {
	cfg := Default()
	cfg.Membership = "laptop"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "membership") {
		t.Fatalf("an unknown mode must be refused, got %v", err)
	}
	cfg = Default()
	cfg.Membership = membership.Roaming
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "github.scopes") {
		t.Fatalf("a watchdog on a node the audit cannot read must be refused, got %v", err)
	}
	cfg = scopedDefault()
	cfg.Membership = membership.Roaming
	off := 0
	cfg.GitHub.ParkedScaleSetAuditMinutes = &off
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "parkedScaleSetAuditMinutes") {
		t.Fatalf("a watchdog whose evidence is disabled must be refused, got %v", err)
	}
	var buffer strings.Builder
	cfg = Default()
	if err := Encode(&buffer, cfg); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(buffer.String(), "{", `{"membership": "laptop",`, 1)
	if _, err := Decode(strings.NewReader(body)); err == nil {
		t.Fatal("Decode accepted an unknown membership mode")
	}
}
