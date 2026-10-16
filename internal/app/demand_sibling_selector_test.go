package app

import (
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"testing"
)

func TestForeignSelectorProofNeverNamesALocalOrUnknownRoute(t *testing.T) {
	binding := Binding{Scope: "scope", ScaleSetID: 1, Targets: []string{"o/r"}, ScaleSetLabels: []string{"local"}}
	job := githubscaleset.WorkflowJob{Repository: githubscaleset.Repository{Owner: "o", Name: "r"}, Labels: []string{"peer"}}
	for _, proof := range []ForeignScaleSetSelector{{}, {Scope: "scope", ID: 2}, {Scope: "scope", Name: "peer"}, {ID: 2, Name: "peer"}, {Scope: "other", ID: 2, Name: "peer"}, {Scope: "scope", ID: 1, Name: "peer"}, {Scope: "scope", ID: 2, Name: "unknown"}} {
		c := DemandCoordinator{ForeignSelectors: []ForeignScaleSetSelector{proof}}
		if c.jobSelectsForeignSet([]Binding{binding}, job) {
			t.Fatalf("unsafe proof accepted: %+v", proof)
		}
	}
	c := DemandCoordinator{ForeignSelectors: []ForeignScaleSetSelector{{Scope: "scope", ID: 2, Name: "peer"}}}
	binding.ScaleSetLabels = append(binding.ScaleSetLabels, "peer")
	if c.jobSelectsForeignSet([]Binding{binding}, job) {
		t.Fatal("local label mistaken for foreign-only selector")
	}
	binding.ScaleSetLabels = []string{"local"}
	binding.Targets = []string{"other/repo"}
	if c.jobSelectsForeignSet([]Binding{binding}, job) {
		t.Fatal("unserved repository mistaken for same scope")
	}
}
