package config

import (
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// A target's scheduling class decides whether its jobs outrank another
// repository's, so an unrecognised one must be refused at load rather than
// normalised into the standard class and quietly changing who waits.
func TestValidateRefusesAnUnknownSchedulingClass(t *testing.T) {
	cfg := Default()
	cfg.Targets = []Target{{Type: "repo", Slug: "vitalyiegorov/suuudokuuu", MaxActive: 4,
		SchedulingClass: domain.SchedulingClass("urgent")}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a scheduling class the scheduler cannot order")
	}
	if !strings.Contains(err.Error(), "urgent") || !strings.Contains(err.Error(), "suuudokuuu") {
		t.Fatalf("Validate() = %v, want it to name both the class and the target", err)
	}

	for _, class := range []domain.SchedulingClass{domain.SchedulingStandard, domain.SchedulingControlPlane} {
		cfg.Targets = []Target{{Type: "repo", Slug: "vitalyiegorov/suuudokuuu", MaxActive: 4,
			SchedulingClass: class}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected the %s class: %v", class, err)
		}
	}
}
