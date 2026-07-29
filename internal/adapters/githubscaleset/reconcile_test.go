package githubscaleset

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// A scale set whose GitHub object no longer matches configuration cannot be
// repaired by the fleet. inspect() fails closed with ErrConflict, and the
// provisioner offers only create and reuse -- so drift is permanently
// unfixable through the fleet and needs out-of-band GitHub surgery the fleet does
// not expose. That is a liveness gap, not a safety one: the operator's only
// remaining option is to delete the object by hand.
//
// Failing closed remains the DEFAULT. Repair is opt-in, so an operator who runs
// provisioning to create missing scale sets can never silently mutate existing
// ones.

// driftedAdmin serves an existing scale set that differs from configuration and
// records what the provisioner asks it to write.
type driftedAdmin struct {
	fakeScaleSetAdmin
	updatedID     int
	updated       *scaleset.RunnerScaleSet
	updateErr     error
	updateResult  *scaleset.RunnerScaleSet
	updateCalls   int
	returnDrifted bool
}

func (d *driftedAdmin) UpdateRunnerScaleSet(_ context.Context, id int, set *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	d.updateCalls++
	d.updatedID = id
	d.updated = set
	if d.updateErr != nil {
		return nil, d.updateErr
	}
	if d.returnDrifted {
		// GitHub accepted the call but the object still does not match.
		return &scaleset.RunnerScaleSet{ID: id, Name: set.Name, RunnerGroupID: set.RunnerGroupID,
			Labels:        []scaleset.Label{{Name: "stale-label", Type: "System"}},
			RunnerSetting: set.RunnerSetting}, nil
	}
	if d.updateResult != nil {
		return d.updateResult, nil
	}
	result := *set
	result.ID = id
	return &result, nil
}

func driftedProvisioner(t *testing.T) (*driftedAdmin, Provisioner, ScaleSetSpec) {
	t.Helper()
	spec := ScaleSetSpec{Name: "trf-budgie-builder", Labels: []string{"self-hosted", "macOS", "macos-builder"}}
	admin := &driftedAdmin{fakeScaleSetAdmin: fakeScaleSetAdmin{
		existing: &scaleset.RunnerScaleSet{ID: 1, Name: spec.Name, RunnerGroupID: defaultRunnerGroupID,
			// One label short of desired: the drift.
			Labels:        []scaleset.Label{{Name: "self-hosted", Type: "System"}, {Name: "macOS", Type: "System"}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}},
	}}
	return admin, Provisioner{Client: admin}, spec
}

// TestInspectPlansDriftAsUpdate is the RED-first case: drift must be a plannable
// action so an operator can see it and choose to repair it, instead of an error
// that ends the run.
func TestInspectPlansDriftAsUpdate(t *testing.T) {
	admin, provisioner, spec := driftedProvisioner(t)
	provisioner.ReconcileDrift = true

	plan, err := provisioner.Inspect(context.Background(), spec)
	if err != nil {
		t.Fatalf("Inspect() = %v, want a plannable drift", err)
	}
	if plan.Action != ScaleSetUpdate {
		t.Fatalf("action = %q, want %q", plan.Action, ScaleSetUpdate)
	}
	if plan.ID != 1 {
		t.Fatalf("plan ID = %d, want the existing object's id", plan.ID)
	}
	if admin.updateCalls != 0 {
		t.Fatal("Inspect must not write: planning is read-only")
	}
}

// TestInspectStillFailsClosedByDefault proves the safety default is unchanged. An
// operator provisioning missing scale sets must never silently mutate an existing
// one, so drift keeps returning ErrConflict unless repair is explicitly enabled.
func TestInspectStillFailsClosedByDefault(t *testing.T) {
	_, provisioner, spec := driftedProvisioner(t)

	if _, err := provisioner.Inspect(context.Background(), spec); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("Inspect() = %v, want ErrConflict when repair is not enabled", err)
	}
}

// TestEnsureRepairsDriftInPlace proves the repair updates the existing object
// rather than replacing it. Preserving the scale-set ID matters: GitHub routes
// queued jobs to an id, and recreating the object would orphan them.
func TestEnsureRepairsDriftInPlace(t *testing.T) {
	admin, provisioner, spec := driftedProvisioner(t)
	provisioner.ReconcileDrift = true

	result, err := provisioner.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	if admin.updateCalls != 1 {
		t.Fatalf("update calls = %d, want exactly one", admin.updateCalls)
	}
	if admin.updatedID != 1 || result.ID != 1 {
		t.Fatalf("repair changed the scale-set id: updated=%d result=%d", admin.updatedID, result.ID)
	}
	if admin.created != nil {
		t.Fatal("repair must not create a second scale set")
	}
	// desiredLabels also advertises the scale-set name, so the repaired object must
	// carry every configured label plus that name.
	got := map[string]bool{}
	for _, label := range admin.updated.Labels {
		got[label.Name] = true
	}
	for _, want := range append(append([]string(nil), spec.Labels...), spec.Name) {
		if !got[want] {
			t.Fatalf("repaired labels %#v are missing %q", admin.updated.Labels, want)
		}
	}
}

// TestEnsureRefusesAnUnverifiedRepair proves the write is verified, not trusted. If
// GitHub accepts the update but the object still does not match, the result is
// uncertain and must not be reported as provisioned.
func TestEnsureRefusesAnUnverifiedRepair(t *testing.T) {
	admin, provisioner, spec := driftedProvisioner(t)
	provisioner.ReconcileDrift = true
	admin.returnDrifted = true

	if _, err := provisioner.Ensure(context.Background(), spec); !errors.Is(err, operations.ErrUncertain) {
		t.Fatalf("Ensure() = %v, want ErrUncertain when the repaired object still differs", err)
	}
}

// TestEnsurePropagatesARefusedRepair proves a rejected write surfaces rather than
// being swallowed into a false success.
func TestEnsurePropagatesARefusedRepair(t *testing.T) {
	admin, provisioner, spec := driftedProvisioner(t)
	provisioner.ReconcileDrift = true
	admin.updateErr = errors.New("insufficient permission")

	if _, err := provisioner.Ensure(context.Background(), spec); err == nil {
		t.Fatal("Ensure() swallowed a refused update")
	}
}

// TestEnsureLeavesAnExactScaleSetAlone proves repair is limited to drift: an
// object that already matches is reused untouched, so enabling reconciliation
// does not start rewriting healthy scale sets.
func TestEnsureLeavesAnExactScaleSetAlone(t *testing.T) {
	spec := ScaleSetSpec{Name: "trf-fleet-small", Labels: []string{"self-hosted", "linux-small"}}
	admin := &driftedAdmin{fakeScaleSetAdmin: fakeScaleSetAdmin{
		existing: &scaleset.RunnerScaleSet{ID: 5, Name: spec.Name, RunnerGroupID: defaultRunnerGroupID,
			Labels: []scaleset.Label{{Name: "linux-small", Type: "System"}, {Name: "self-hosted", Type: "System"},
				{Name: "trf-fleet-small", Type: "System"}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}},
	}}
	provisioner := Provisioner{Client: admin, ReconcileDrift: true}

	plan, err := provisioner.Inspect(context.Background(), spec)
	if err != nil || plan.Action != ScaleSetReuse {
		t.Fatalf("Inspect() = %q, %v; want reuse", plan.Action, err)
	}
	if _, err := provisioner.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	if admin.updateCalls != 0 {
		t.Fatalf("an exact scale set was rewritten %d time(s)", admin.updateCalls)
	}
}
