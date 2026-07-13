package githubscaleset

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeScaleSetAdmin struct {
	group      *scaleset.RunnerGroup
	existing   *scaleset.RunnerScaleSet
	created    *scaleset.RunnerScaleSet
	groupName  string
	groupID    int
	lookupName string
	err        error
}

func (f *fakeScaleSetAdmin) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	f.groupName = name
	return f.group, f.err
}

func (f *fakeScaleSetAdmin) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.groupID, f.lookupName = groupID, name
	return f.existing, f.err
}

func (f *fakeScaleSetAdmin) CreateRunnerScaleSet(_ context.Context, value *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = value
	if f.err != nil {
		return nil, f.err
	}
	result := *value
	result.ID = 41
	return &result, nil
}

func TestProvisionerCreatesOrReusesExactScaleSet(t *testing.T) {
	ctx := context.Background()
	spec := ScaleSetSpec{Name: "trf-small", RunnerGroup: "default", Labels: []string{"linux-small", "self-hosted", "linux-tiered", "self-hosted"}}
	fake := &fakeScaleSetAdmin{}
	provisioner := Provisioner{Client: fake}
	created, err := provisioner.Ensure(ctx, spec)
	if err != nil || created.ID != 41 || fake.groupID != 1 || fake.lookupName != spec.Name {
		t.Fatalf("Ensure(create) = %#v, %v; fake=%#v", created, err, fake)
	}
	wantLabels := []scaleset.Label{{Name: "linux-small", Type: "System"}, {Name: "linux-tiered", Type: "System"}, {Name: "self-hosted", Type: "System"}, {Name: "trf-small", Type: "System"}}
	if fake.created.Name != spec.Name || fake.created.RunnerGroupID != 1 || !fake.created.RunnerSetting.DisableUpdate || !reflect.DeepEqual(fake.created.Labels, wantLabels) {
		t.Fatalf("created = %#v", fake.created)
	}

	fake.existing = &created
	reused, err := provisioner.Ensure(ctx, spec)
	if err != nil || reused.ID != fake.existing.ID {
		t.Fatalf("Ensure(reuse) = %#v, %v", reused, err)
	}
}

func TestProvisionerInspectIsReadOnly(t *testing.T) {
	fake := &fakeScaleSetAdmin{}
	provisioner := Provisioner{Client: fake}
	spec := ScaleSetSpec{Name: "trf-small", Labels: []string{"self-hosted", "linux-small"}}
	plan, err := provisioner.Inspect(context.Background(), spec)
	if err != nil || plan.Action != ScaleSetCreate || plan.ID != 0 || fake.created != nil {
		t.Fatalf("Inspect(create) = %#v, %v created=%#v", plan, err, fake.created)
	}
	created, err := provisioner.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fake.existing = &created
	plan, err = provisioner.Inspect(context.Background(), spec)
	if err != nil || plan.Action != ScaleSetReuse || plan.ID != created.ID || fake.created == nil {
		t.Fatalf("Inspect(reuse) = %#v, %v", plan, err)
	}
}

func TestProvisionerResolvesNamedGroupAndFailsClosedOnDrift(t *testing.T) {
	fake := &fakeScaleSetAdmin{group: &scaleset.RunnerGroup{ID: 7, Name: "fleet"}}
	provisioner := Provisioner{Client: fake}
	spec := ScaleSetSpec{Name: "trf-medium", RunnerGroup: "fleet", Labels: []string{"self-hosted", "linux-medium"}}
	if _, err := provisioner.Ensure(context.Background(), spec); err != nil || fake.groupName != "fleet" || fake.groupID != 7 {
		t.Fatalf("named group = %q/%d, %v", fake.groupName, fake.groupID, err)
	}
	fake.existing = &scaleset.RunnerScaleSet{ID: 9, Name: spec.Name, RunnerGroupID: 7, Labels: []scaleset.Label{{Name: "wrong", Type: "System"}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}
	if _, err := provisioner.Ensure(context.Background(), spec); !errors.Is(err, operations.ErrConflict) {
		t.Fatalf("drift error = %v", err)
	}
}

func TestProvisionerRejectsInvalidInputsAndUpstreamErrors(t *testing.T) {
	for _, spec := range []ScaleSetSpec{{}, {Name: "bad name", Labels: []string{"x"}}, {Name: "ok", Labels: []string{"bad label"}}} {
		if _, err := (Provisioner{Client: &fakeScaleSetAdmin{}}).Ensure(context.Background(), spec); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("spec %#v error = %v", spec, err)
		}
	}
	if _, err := (Provisioner{}).Ensure(context.Background(), ScaleSetSpec{Name: "ok", Labels: []string{"self-hosted"}}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("nil client error = %v", err)
	}
	want := errors.New("github")
	if _, err := (Provisioner{Client: &fakeScaleSetAdmin{err: want}}).Ensure(context.Background(), ScaleSetSpec{Name: "ok", Labels: []string{"self-hosted"}}); !errors.Is(err, want) {
		t.Fatalf("upstream error = %v", err)
	}
}
