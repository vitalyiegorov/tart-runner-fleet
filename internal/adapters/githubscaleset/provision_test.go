package githubscaleset

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type fakeScaleSetAdmin struct {
	group        *scaleset.RunnerGroup
	existing     *scaleset.RunnerScaleSet
	created      *scaleset.RunnerScaleSet
	groupName    string
	groupID      int
	lookupName   string
	err          error
	groupErr     error
	lookupErr    error
	createErr    error
	createResult *scaleset.RunnerScaleSet
}

func (f *fakeScaleSetAdmin) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	f.groupName = name
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return f.group, f.err
}

func (f *fakeScaleSetAdmin) GetRunnerScaleSet(_ context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	f.groupID, f.lookupName = groupID, name
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	return f.existing, f.err
}

func (f *fakeScaleSetAdmin) CreateRunnerScaleSet(_ context.Context, value *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = value
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.createResult != nil {
		return f.createResult, nil
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

func TestProvisionerReusesGitHubLowercaseSystemLabels(t *testing.T) {
	spec := ScaleSetSpec{Name: "trf-builder", Labels: []string{"self-hosted", "macOS", "ARM64", "macos-builder"}}
	fake := &fakeScaleSetAdmin{existing: &scaleset.RunnerScaleSet{
		ID: 1, Name: spec.Name, RunnerGroupID: defaultRunnerGroupID,
		Labels: []scaleset.Label{
			{Name: "self-hosted", Type: "system"},
			{Name: "macOS", Type: "system"},
			{Name: "ARM64", Type: "system"},
			{Name: "macos-builder", Type: "system"},
			{Name: spec.Name, Type: "system"},
		},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}}

	plan, err := (Provisioner{Client: fake}).Inspect(context.Background(), spec)
	if err != nil || plan.Action != ScaleSetReuse || plan.ID != 1 {
		t.Fatalf("Inspect(lowercase system labels) = %#v, %v", plan, err)
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

func TestNewProvisionerValidatesAndBuildsOfficialBoundary(t *testing.T) {
	key := NewPrivateKeySecret("PRIVATE-KEY-SENTINEL")
	for _, cfg := range []GitHubAppAdminConfig{
		{},
		{PrivateKey: NewPrivateKeySecret("")},
		{PrivateKey: key, ClientID: "client", InstallationID: 1},
		{PrivateKey: key, GitHubConfigURL: "https://github.com/o/r", InstallationID: 1},
		{PrivateKey: key, GitHubConfigURL: "https://github.com/o/r", ClientID: "client"},
	} {
		if _, err := NewProvisioner(cfg); !errors.Is(err, operations.ErrInvalid) {
			t.Fatalf("NewProvisioner(%#v) error = %v", cfg, err)
		}
	}
	if _, err := NewProvisioner(GitHubAppAdminConfig{PrivateKey: key, GitHubConfigURL: "://", ClientID: "client", InstallationID: 1}); err == nil || strings.Contains(err.Error(), "PRIVATE-KEY-SENTINEL") {
		t.Fatalf("invalid URL error = %v", err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	secret := NewPrivateKeySecret(string(encoded))
	t.Cleanup(secret.Destroy)
	provisioner, err := NewProvisioner(GitHubAppAdminConfig{PrivateKey: secret, GitHubConfigURL: "https://github.com/o/r", ClientID: "client", InstallationID: 1,
		System: "fleet", Version: "v1", CommitSHA: "abc", Subsystem: "provisioner"})
	if err != nil || provisioner.Client == nil {
		t.Fatalf("NewProvisioner() = %#v, %v", provisioner, err)
	}
}

func TestProvisionerFailsClosedAtEveryOfficialMutationBoundary(t *testing.T) {
	spec := ScaleSetSpec{Name: "trf-small", RunnerGroup: "fleet", Labels: []string{"self-hosted", "linux-small"}}
	want := errors.New("github unavailable")
	for _, tt := range []struct {
		name string
		fake *fakeScaleSetAdmin
	}{
		{name: "group lookup error", fake: &fakeScaleSetAdmin{groupErr: want}},
		{name: "missing group", fake: &fakeScaleSetAdmin{}},
		{name: "invalid group", fake: &fakeScaleSetAdmin{group: &scaleset.RunnerGroup{}}},
		{name: "scale set lookup error", fake: &fakeScaleSetAdmin{group: &scaleset.RunnerGroup{ID: 7}, lookupErr: want}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (Provisioner{Client: tt.fake}).Inspect(context.Background(), spec)
			if tt.name == "group lookup error" || tt.name == "scale set lookup error" {
				if !errors.Is(err, want) {
					t.Fatalf("Inspect() error = %v", err)
				}
			} else if !errors.Is(err, operations.ErrUncertain) {
				t.Fatalf("Inspect() error = %v", err)
			}
			if tt.fake.created != nil {
				t.Fatal("read-only inspection created a scale set")
			}
		})
	}

	for _, tt := range []struct {
		name string
		fake *fakeScaleSetAdmin
		want error
	}{
		{name: "create error", fake: &fakeScaleSetAdmin{createErr: want}, want: want},
		{name: "nil create result", fake: &fakeScaleSetAdmin{createResult: nil, createErr: nil}, want: nil},
		{name: "zero create id", fake: &fakeScaleSetAdmin{createResult: &scaleset.RunnerScaleSet{Name: "trf-small", RunnerGroupID: 1}}, want: operations.ErrUncertain},
		{name: "drifted create result", fake: &fakeScaleSetAdmin{createResult: &scaleset.RunnerScaleSet{ID: 9, Name: "wrong", RunnerGroupID: 1}}, want: operations.ErrUncertain},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A nil createResult uses the fake's normal valid result; explicitly
			// return nil through createErr is represented by this small wrapper.
			if tt.name == "nil create result" {
				client := nilCreateAdmin{fakeScaleSetAdmin: *tt.fake}
				_, err := (Provisioner{Client: &client}).Ensure(context.Background(), ScaleSetSpec{Name: "trf-small", Labels: []string{"self-hosted"}})
				if !errors.Is(err, operations.ErrUncertain) {
					t.Fatalf("Ensure() error = %v", err)
				}
				return
			}
			_, err := (Provisioner{Client: tt.fake}).Ensure(context.Background(), ScaleSetSpec{Name: "trf-small", Labels: []string{"self-hosted"}})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Ensure() error = %v, want %v", err, tt.want)
			}
		})
	}
}

type nilCreateAdmin struct{ fakeScaleSetAdmin }

func (f *nilCreateAdmin) CreateRunnerScaleSet(context.Context, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	return nil, nil
}

func TestExactScaleSetNormalizesOnlySafeSystemLabels(t *testing.T) {
	desired := scaleset.RunnerScaleSet{ID: 0, Name: "trf", RunnerGroupID: 1,
		Labels:        []scaleset.Label{{Name: "trf", Type: "System"}, {Name: "self-hosted", Type: "System"}},
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}
	actual := desired
	actual.ID = 7
	actual.Labels = []scaleset.Label{{Name: "self-hosted"}, {Name: "trf"}}
	if !exactScaleSet(actual, desired) {
		t.Fatal("safe empty label types were not normalized")
	}
	for _, mutate := range []func(*scaleset.RunnerScaleSet){
		func(v *scaleset.RunnerScaleSet) { v.ID = 0 },
		func(v *scaleset.RunnerScaleSet) { v.Name = "other" },
		func(v *scaleset.RunnerScaleSet) { v.RunnerGroupID = 2 },
		func(v *scaleset.RunnerScaleSet) { v.RunnerSetting.DisableUpdate = false },
		func(v *scaleset.RunnerScaleSet) { v.Labels[0].Type = "Custom" },
		func(v *scaleset.RunnerScaleSet) { v.Labels[0].Name = "bad label" },
	} {
		candidate := actual
		candidate.Labels = append([]scaleset.Label(nil), actual.Labels...)
		mutate(&candidate)
		if exactScaleSet(candidate, desired) {
			t.Fatalf("unsafe scale set accepted: %#v", candidate)
		}
	}
	badDesired := desired
	badDesired.Labels = []scaleset.Label{{Name: "bad label", Type: "System"}}
	if exactScaleSet(actual, badDesired) {
		t.Fatal("unsafe desired label accepted")
	}
}
