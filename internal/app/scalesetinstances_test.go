package app

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// TestScaleSetInstancesAttributesEachInstanceToTheSetThatServesIt is the
// observation the stranded-set detector stands on (issue #336, ADR 0056).
//
// Every production node binds one profile to many scale sets — node-b binds
// `linux-4x8` from six scopes, the mac mini binds `maestro` from budgie and
// pony — so a per-profile instance count can decide nothing about one set. The
// instance names the repo it runs for, and the binding names the repos it
// serves, which attributes each instance to exactly one set.
func TestScaleSetInstancesAttributesEachInstanceToTheSetThatServesIt(t *testing.T) {
	profile := domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}
	bindings := []Binding{
		{ScaleSetID: 3, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"}, Profile: profile},
		{ScaleSetID: 7, Scope: "pony", Targets: []string{"vitalyiegorov/pony"}, Profile: profile},
	}
	observed := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	instances := domain.Fresh([]domain.Instance{
		{ID: "trf-1", Repo: "vitalyiegorov/budgie", Profile: "maestro", State: domain.InstanceRunning},
		{ID: "trf-2", Repo: "vitalyiegorov/budgie", Profile: "maestro", State: domain.InstanceOnlineIdle},
		// A terminal instance holds nothing and serves nobody.
		{ID: "trf-3", Repo: "vitalyiegorov/pony", Profile: "maestro", State: domain.InstanceDeleted},
	}, observed)

	rows := ScaleSetInstances(bindings, instances)

	if len(rows) != 2 {
		t.Fatalf("every binding is reported, including the empty one: %#v", rows)
	}
	if rows[0].Scope != "budgie" || rows[0].ScaleSetID != 3 || rows[0].Count != 2 {
		t.Fatalf("budgie's set runs both instances: %#v", rows[0])
	}
	// The row is present and zero, which is an observation. A missing row would
	// be an absence, and the two must never be confused (contributor rule 4).
	if rows[1].Scope != "pony" || rows[1].ScaleSetID != 7 || rows[1].Count != 0 {
		t.Fatalf("pony's set runs none, and says so: %#v", rows[1])
	}
}

// An inventory that could not be observed reports NOTHING, never a fleet of
// empty sets: a node that cannot see its own VMs must not be able to conclude
// that a scale set has no runner.
func TestScaleSetInstancesReportsNothingWhenTheInventoryIsUnavailable(t *testing.T) {
	bindings := []Binding{{ScaleSetID: 3, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"},
		Profile: domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}}}

	if rows := ScaleSetInstances(bindings, domain.Unavailable[[]domain.Instance]("tart is unreachable")); rows != nil {
		t.Fatalf("an unavailable inventory observes nothing: %#v", rows)
	}
	if rows := ScaleSetInstances(nil, domain.Fresh([]domain.Instance{}, time.Now())); len(rows) != 0 {
		t.Fatalf("a node that binds nothing reports nothing: %#v", rows)
	}
}

// An instance whose repo no binding serves is counted against no set rather
// than against the first one that shares its profile.
func TestScaleSetInstancesLeavesAnUnservedRepoUncounted(t *testing.T) {
	profile := domain.Profile{ID: "maestro", Route: "macos-maestro", Platform: domain.PlatformMacOS}
	bindings := []Binding{{ScaleSetID: 3, Scope: "budgie", Targets: []string{"vitalyiegorov/budgie"}, Profile: profile}}
	instances := domain.Fresh([]domain.Instance{
		{ID: "trf-9", Repo: "vitalyiegorov/retired", Profile: "maestro", State: domain.InstanceRunning}}, time.Now())

	rows := ScaleSetInstances(bindings, instances)

	if len(rows) != 1 || rows[0].Count != 0 {
		t.Fatalf("an instance no binding serves counts against nothing: %#v", rows)
	}
}
