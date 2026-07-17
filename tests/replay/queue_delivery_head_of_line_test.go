package replay

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
)

func TestScaleSetQueueLookaheadIncidentReplay(t *testing.T) {
	cfg := config.Default()
	cfg.Targets = []config.Target{{Type: "repo", Slug: "budgie-at/budgie", MaxActive: 4}}
	cfg.GitHub = config.GitHub{
		SessionOwner:  "fleet-macmini",
		App:           config.GitHubApp{ClientID: "Iv1.test", KeychainService: "fleet", KeychainAccount: "app"},
		Installations: []config.GitHubInstallation{{Name: "budgie", InstallationID: 1}},
		Scopes: []config.GitHubScope{{
			Name: "budgie-org", Kind: config.ScopeOrganization, ConfigURL: "https://github.com/budgie-at",
			Installation: "budgie", Targets: []string{"budgie-at/budgie"},
			ScaleSets: []config.ScaleSet{
				{Profile: "small", Name: "trf-budgie-small", ID: 5, MaxCapacity: 5, Labels: []string{"self-hosted", "linux-small"}},
				{Profile: "medium", Name: "trf-budgie-medium", ID: 7, MaxCapacity: 5, Labels: []string{"self-hosted", "linux-medium"}},
				{Profile: "large", Name: "trf-budgie-large", ID: 6, MaxCapacity: 3, Labels: []string{"self-hosted", "linux-large"}},
				{Profile: "builder", Name: "trf-budgie-builder", ID: 1, MaxCapacity: 1, Labels: []string{"self-hosted", "macOS", "ARM64", "macos-builder"}},
				{Profile: "maestro", Name: "trf-budgie-maestro", ID: 3, MaxCapacity: 3, Labels: []string{"self-hosted", "macOS", "ARM64", "macos-maestro"}},
			},
		}},
	}

	err := cfg.ValidateAuthority()
	if err == nil || !strings.Contains(err.Error(), "queue lookahead") {
		t.Fatalf("maxCapacity=1 incident was accepted: %v", err)
	}

	cfg.GitHub.Scopes[0].ScaleSets[3].MaxCapacity = 2
	if err := cfg.ValidateAuthority(); err != nil {
		t.Fatalf("one queued builder lookahead was rejected: %v", err)
	}
	if cfg.MacOS.Builder.MaxActive != 1 {
		t.Fatalf("builder runtime capacity changed to %d", cfg.MacOS.Builder.MaxActive)
	}
}
