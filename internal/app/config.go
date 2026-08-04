package app

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

func BuildSchedulerConfig(cfg config.Config) scheduler.Config {
	profiles := make(map[domain.ProfileID]domain.Profile, len(cfg.Linux.Profiles)+2)
	for _, profile := range cfg.Linux.Profiles {
		id := domain.ProfileID(profile.ID)
		profiles[id] = domain.Profile{ID: id, Platform: domain.PlatformLinux, Route: domain.Route(profile.Label),
			Resources: domain.Resources{CPU: profile.Resources.CPU, MemoryMB: profile.Resources.MemoryMiB, Slots: 1}, MaxActive: profile.MaxActive}
	}
	if cfg.MacOS.Enabled {
		for _, profile := range []config.Profile{cfg.MacOS.Builder, cfg.MacOS.Maestro} {
			id := domain.ProfileID(profile.ID)
			profiles[id] = domain.Profile{ID: id, Platform: domain.PlatformMacOS, Route: domain.Route(profile.Label),
				Resources: domain.Resources{CPU: profile.Resources.CPU, MemoryMB: profile.Resources.MemoryMiB, Slots: 1}, MaxActive: profile.MaxActive}
		}
	}
	caps := make(map[string]int, len(cfg.Targets))
	classes := make(map[string]domain.SchedulingClass, len(cfg.Targets))
	for _, target := range cfg.Targets {
		caps[target.Slug] = target.MaxActive
		class := target.SchedulingClass
		if class == "" {
			class = domain.SchedulingStandard
		}
		classes[target.Slug] = class
	}
	return scheduler.Config{LinuxCapacity: domain.Resources{CPU: cfg.Linux.Capacity.CPU, MemoryMB: cfg.Linux.Capacity.MemoryMiB, Slots: cfg.Linux.MaxInstances},
		FairnessAge: cfg.ReservationAge, AssignedTimeout: cfg.Timeouts.Assigned, RepoCaps: caps, RepoSchedulingClasses: classes, Profiles: profiles,
		MacOSExclusive:         cfg.MacOS.AdmissionPolicy == config.MacOSAdmissionExclusive,
		MixedPlatformAdmission: cfg.MacOS.MixedPlatformAdmission,
		MixedProfileCohorts:    cfg.MacOS.MixedProfileCohorts,
		ElasticHostEnvelope:    cfg.Guards.ElasticHostEnvelope,
		HostBudget:             domain.Resources{CPU: cfg.HostBudget.CPU, MemoryMB: cfg.HostBudget.MemoryMiB}}
}

// ValidateBindings runs the exact scheduler-config and binding construction the
// daemon performs at startup (see internal/daemon) and surfaces any invariant
// violation as an error. `fleet config validate` calls this so that a config it
// accepts can never crash-loop the authority daemon on the runtime invariants
// that Config.Validate does not cover (profile existence, positive durable IDs,
// scale-set identity collisions).
func ValidateBindings(cfg config.Config) error {
	_, err := BuildBindings(cfg, BuildSchedulerConfig(cfg))
	return err
}

func BuildBindings(cfg config.Config, schedulerConfig scheduler.Config) ([]Binding, error) {
	// A binding matches queued jobs against what GitHub advertises, so it reads
	// the same expanded label set the provisioner publishes: canonical resource
	// label plus every alias (ADR 0032).
	labelSets := cfg.ProfileLabelSets()
	if len(cfg.GitHub.Scopes) > 0 {
		bindings := make([]Binding, 0)
		seenKeys := map[int64]string{}
		for _, scope := range cfg.GitHub.Scopes {
			for _, scaleSet := range scope.ScaleSets {
				profile, err := resolveScaleSetProfile(schedulerConfig, scaleSet, scope.Name)
				if err != nil {
					return nil, err
				}
				key := scopedStoreKey(scope.Name, scaleSet.ID)
				identity := fmt.Sprintf("%s/%d", scope.Name, scaleSet.ID)
				if prior, duplicate := seenKeys[key]; duplicate {
					return nil, fmt.Errorf("durable scale-set identity collision %s and %s", prior, identity)
				}
				seenKeys[key] = identity
				bindings = append(bindings, Binding{StoreKey: key, ScaleSetID: int64(scaleSet.ID), Scope: scope.Name,
					Targets: append([]string(nil), scope.Targets...), Profile: profile,
					ScaleSetLabels: effectiveScaleSetLabels(scaleSet, labelSets[scaleSet.Profile])})
			}
		}
		return bindings, nil
	}
	bindings := make([]Binding, 0, len(cfg.GitHub.ScaleSets))
	for _, scaleSet := range cfg.GitHub.ScaleSets {
		profile, err := resolveScaleSetProfile(schedulerConfig, scaleSet, "")
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, Binding{StoreKey: int64(scaleSet.ID), ScaleSetID: int64(scaleSet.ID),
			ScaleSetLabels: effectiveScaleSetLabels(scaleSet, labelSets[scaleSet.Profile]), Profile: profile})
	}
	return bindings, nil
}

// resolveScaleSetProfile enforces the two independent runtime invariants that
// previously shared one opaque "invalid scale set profile" message: the profile
// referenced by the scale set must exist in the scheduler profile map, and the
// scale set must carry a positive durable ID. Scale set names, profile IDs, and
// scope names are non-secret and safe to include for actionable diagnostics.
func resolveScaleSetProfile(schedulerConfig scheduler.Config, scaleSet config.ScaleSet, scope string) (domain.Profile, error) {
	where := ""
	if scope != "" {
		where = fmt.Sprintf(" in scope %q", scope)
	}
	profile, ok := schedulerConfig.Profiles[domain.ProfileID(scaleSet.Profile)]
	if !ok {
		return domain.Profile{}, fmt.Errorf("scale set %q%s references unknown profile %q", scaleSet.Name, where, scaleSet.Profile)
	}
	if scaleSet.ID <= 0 {
		return domain.Profile{}, fmt.Errorf("scale set %q%s (profile %q) has non-positive durable ID %d; provision it first", scaleSet.Name, where, scaleSet.Profile, scaleSet.ID)
	}
	return profile, nil
}

func effectiveScaleSetLabels(scaleSet config.ScaleSet, labelSet config.LabelSet) []string {
	labels := labelSet.Advertise(scaleSet.Labels)
	name := strings.TrimSpace(scaleSet.Name)
	if name == "" {
		return labels
	}
	for _, label := range labels {
		if strings.EqualFold(label, name) {
			return labels
		}
	}
	return append(labels, name)
}

func scopedStoreKey(scope string, scaleSetID int) int64 {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", scope, scaleSetID)))
	key := int64(binary.BigEndian.Uint64(digest[:8]) & (^uint64(0) >> 1))
	if key == 0 {
		return 1
	}
	return key
}
