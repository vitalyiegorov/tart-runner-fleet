package app

import (
	"sort"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// ScopeInstance is how many live instances this node holds for ONE scale set it
// serves. It is the instance-side twin of ScopeQueue, and for the same reason:
// an aggregate by profile cannot answer a question about one set, and every
// production node binds one profile to several sets.
type ScopeInstance struct {
	Scope      string
	Profile    domain.ProfileID
	ScaleSetID int64
	Count      int
}

// ScaleSetInstances attributes each live instance to the binding that serves
// it: the one whose scope targets the instance's repository and whose profile
// the instance runs. Every binding is reported, including the ones running
// nothing, because a set with no instance is an observation and a set that is
// missing is an absence -- the stranded-set detector (ADR 0056) turns exactly
// one of those into a finding.
//
// An inventory that could not be observed reports NOTHING. A node that cannot
// see its own VMs must never be able to conclude that a scale set has no
// runner (contributor rule 4).
func ScaleSetInstances(bindings []Binding, instances domain.Observation[[]domain.Instance]) []ScopeInstance {
	if instances.State == domain.ObservationUnavailable || len(bindings) == 0 {
		return nil
	}
	rows := make([]ScopeInstance, 0, len(bindings))
	for _, binding := range bindings {
		row := ScopeInstance{Scope: binding.Scope, Profile: binding.Profile.ID, ScaleSetID: binding.ScaleSetID}
		for _, instance := range instances.Value {
			if !instance.Live() || instance.Profile != binding.Profile.ID {
				continue
			}
			for _, target := range binding.Targets {
				if target == instance.Repo {
					row.Count++
					break
				}
			}
		}
		rows = append(rows, row)
	}
	// Deterministic order, exactly as ScopeQueue rows are ordered, so operators,
	// JSON consumers and replay fixtures never depend on binding order.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope < rows[j].Scope
		}
		if rows[i].Profile != rows[j].Profile {
			return rows[i].Profile < rows[j].Profile
		}
		return rows[i].ScaleSetID < rows[j].ScaleSetID
	})
	return rows
}
