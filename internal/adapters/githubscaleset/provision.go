package githubscaleset

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

const defaultRunnerGroupID = 1

var validScaleSetToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type scaleSetAdmin interface {
	GetRunnerGroupByName(context.Context, string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(context.Context, int, string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(context.Context, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(context.Context, int, *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
}

// ScaleSetSpec is a bounded desired-state description. An exact object is reused.
// Drift fails closed unless the provisioner is explicitly allowed to reconcile it,
// in which case the existing object is repaired in place.
type ScaleSetSpec struct {
	Name        string
	RunnerGroup string
	Labels      []string
}

// Provisioner plans and applies scale-set desired state.
//
// ReconcileDrift permits repairing an existing object whose labels, runner group,
// or runner setting no longer match configuration. Default false keeps the
// fail-closed behavior: an operator provisioning missing scale sets can never
// silently mutate an existing one. Repair updates in place rather than replacing,
// because GitHub routes queued jobs to a scale-set id and a replacement would
// orphan them.
type Provisioner struct {
	Client         scaleSetAdmin
	ReconcileDrift bool
}

type ScaleSetAction string

const (
	ScaleSetCreate ScaleSetAction = "create"
	ScaleSetReuse  ScaleSetAction = "reuse"
	ScaleSetUpdate ScaleSetAction = "update"
)

type ScaleSetPlan struct {
	Action ScaleSetAction
	ID     int
}

type GitHubAppAdminConfig struct {
	GitHubConfigURL string
	ClientID        string
	InstallationID  int64
	PrivateKey      *PrivateKeySecret
	System          string
	Version         string
	CommitSHA       string
	Subsystem       string
}

func NewProvisioner(c GitHubAppAdminConfig) (Provisioner, error) {
	if c.PrivateKey == nil || c.PrivateKey.reveal() == "" || strings.TrimSpace(c.GitHubConfigURL) == "" ||
		strings.TrimSpace(c.ClientID) == "" || c.InstallationID <= 0 {
		return Provisioner{}, operations.ErrInvalid
	}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: c.GitHubConfigURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: c.PrivateKey.reveal(),
		},
		SystemInfo: scaleset.SystemInfo{System: c.System, Version: c.Version, CommitSHA: c.CommitSHA, Subsystem: c.Subsystem},
	})
	if err != nil {
		return Provisioner{}, fmt.Errorf("create official GitHub App admin client: %w", err)
	}
	return Provisioner{Client: client}, nil
}

func (p Provisioner) Ensure(ctx context.Context, spec ScaleSetSpec) (scaleset.RunnerScaleSet, error) {
	inspection, err := p.inspect(ctx, spec)
	if err != nil {
		return scaleset.RunnerScaleSet{}, err
	}
	if inspection.current != nil {
		if !inspection.drifted {
			return *inspection.current, nil
		}
		// Repair in place. The write is verified rather than trusted: GitHub may
		// accept the call and still leave the object different, and reporting that
		// as provisioned would hide the very drift this path exists to remove.
		desired := inspection.desired
		desired.ID = inspection.current.ID
		updated, err := p.Client.UpdateRunnerScaleSet(ctx, inspection.current.ID, &desired)
		if err != nil {
			return scaleset.RunnerScaleSet{}, fmt.Errorf("reconcile runner scale set: %w", err)
		}
		if updated == nil || updated.ID != inspection.current.ID || !exactScaleSet(*updated, inspection.desired) {
			return scaleset.RunnerScaleSet{}, operations.ErrUncertain
		}
		return *updated, nil
	}
	created, err := p.Client.CreateRunnerScaleSet(ctx, &inspection.desired)
	if err != nil {
		return scaleset.RunnerScaleSet{}, fmt.Errorf("create runner scale set: %w", err)
	}
	if created == nil || created.ID <= 0 || !exactScaleSet(*created, inspection.desired) {
		return scaleset.RunnerScaleSet{}, operations.ErrUncertain
	}
	return *created, nil
}

func (p Provisioner) Inspect(ctx context.Context, spec ScaleSetSpec) (ScaleSetPlan, error) {
	inspection, err := p.inspect(ctx, spec)
	if err != nil {
		return ScaleSetPlan{}, err
	}
	if inspection.current != nil {
		if inspection.drifted {
			return ScaleSetPlan{Action: ScaleSetUpdate, ID: inspection.current.ID}, nil
		}
		return ScaleSetPlan{Action: ScaleSetReuse, ID: inspection.current.ID}, nil
	}
	return ScaleSetPlan{Action: ScaleSetCreate}, nil
}

type inspection struct {
	desired scaleset.RunnerScaleSet
	current *scaleset.RunnerScaleSet
	// drifted marks an existing object that does not match desired state and that
	// the provisioner is permitted to repair. It is only ever set when
	// ReconcileDrift is enabled; otherwise drift has already failed closed.
	drifted bool
}

func (p Provisioner) inspect(ctx context.Context, spec ScaleSetSpec) (inspection, error) {
	if p.Client == nil || !validScaleSetToken.MatchString(spec.Name) {
		return inspection{}, operations.ErrInvalid
	}
	labels, err := desiredLabels(spec)
	if err != nil {
		return inspection{}, err
	}
	groupID := defaultRunnerGroupID
	groupName := strings.TrimSpace(spec.RunnerGroup)
	if groupName != "" && !strings.EqualFold(groupName, "default") {
		group, err := p.Client.GetRunnerGroupByName(ctx, groupName)
		if err != nil {
			return inspection{}, fmt.Errorf("resolve runner group: %w", err)
		}
		if group == nil || group.ID <= 0 {
			return inspection{}, operations.ErrUncertain
		}
		groupID = group.ID
	}
	existing, err := p.Client.GetRunnerScaleSet(ctx, groupID, spec.Name)
	if err != nil {
		return inspection{}, fmt.Errorf("look up runner scale set: %w", err)
	}
	desired := scaleset.RunnerScaleSet{Name: spec.Name, RunnerGroupID: groupID, Labels: labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}}
	if existing != nil {
		if !exactScaleSet(*existing, desired) {
			if !p.ReconcileDrift {
				return inspection{}, fmt.Errorf("runner scale set %q differs from desired state: %w", spec.Name, operations.ErrConflict)
			}
			return inspection{desired: desired, current: existing, drifted: true}, nil
		}
		return inspection{desired: desired, current: existing}, nil
	}
	return inspection{desired: desired}, nil
}

func desiredLabels(spec ScaleSetSpec) ([]scaleset.Label, error) {
	names := append([]string(nil), spec.Labels...)
	names = append(names, spec.Name)
	for _, name := range names {
		if !validScaleSetToken.MatchString(name) {
			return nil, operations.ErrInvalid
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	labels := make([]scaleset.Label, 0, len(names))
	for _, name := range names {
		labels = append(labels, scaleset.Label{Name: name, Type: "System"})
	}
	return labels, nil
}

func exactScaleSet(actual, desired scaleset.RunnerScaleSet) bool {
	if actual.ID <= 0 || actual.Name != desired.Name || actual.RunnerGroupID != desired.RunnerGroupID ||
		actual.RunnerSetting.DisableUpdate != desired.RunnerSetting.DisableUpdate {
		return false
	}
	actualLabels := append([]scaleset.Label(nil), actual.Labels...)
	desiredLabels := append([]scaleset.Label(nil), desired.Labels...)
	normalize := func(labels []scaleset.Label) bool {
		for i := range labels {
			if labels[i].Type == "" {
				labels[i].Type = "System"
			}
			if !strings.EqualFold(labels[i].Type, "System") || !validScaleSetToken.MatchString(labels[i].Name) {
				return false
			}
			labels[i].Type = "System"
		}
		slices.SortFunc(labels, func(a, b scaleset.Label) int { return strings.Compare(a.Name, b.Name) })
		return true
	}
	return normalize(actualLabels) && normalize(desiredLabels) && slices.Equal(actualLabels, desiredLabels)
}
