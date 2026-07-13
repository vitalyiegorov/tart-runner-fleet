package app

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

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
		FairnessAge: cfg.ReservationAge, RepoCaps: caps, RepoSchedulingClasses: classes, Profiles: profiles}
}

func BuildBindings(cfg config.Config, schedulerConfig scheduler.Config) ([]Binding, error) {
	if len(cfg.GitHub.Scopes) > 0 {
		bindings := make([]Binding, 0)
		seenKeys := map[int64]string{}
		for _, scope := range cfg.GitHub.Scopes {
			for _, scaleSet := range scope.ScaleSets {
				profile, ok := schedulerConfig.Profiles[domain.ProfileID(scaleSet.Profile)]
				if !ok || scaleSet.ID <= 0 {
					return nil, fmt.Errorf("invalid scale set profile %q in scope %q", scaleSet.Profile, scope.Name)
				}
				key := scopedStoreKey(scope.Name, scaleSet.ID)
				identity := fmt.Sprintf("%s/%d", scope.Name, scaleSet.ID)
				if prior, duplicate := seenKeys[key]; duplicate {
					return nil, fmt.Errorf("durable scale-set identity collision %s and %s", prior, identity)
				}
				seenKeys[key] = identity
				bindings = append(bindings, Binding{StoreKey: key, ScaleSetID: int64(scaleSet.ID), Scope: scope.Name,
					Targets: append([]string(nil), scope.Targets...), Profile: profile})
			}
		}
		return bindings, nil
	}
	bindings := make([]Binding, 0, len(cfg.GitHub.ScaleSets))
	for _, scaleSet := range cfg.GitHub.ScaleSets {
		profile, ok := schedulerConfig.Profiles[domain.ProfileID(scaleSet.Profile)]
		if !ok || scaleSet.ID <= 0 {
			return nil, fmt.Errorf("invalid scale set profile %q", scaleSet.Profile)
		}
		bindings = append(bindings, Binding{StoreKey: int64(scaleSet.ID), ScaleSetID: int64(scaleSet.ID), Profile: profile})
	}
	return bindings, nil
}

func scopedStoreKey(scope string, scaleSetID int) int64 {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", scope, scaleSetID)))
	key := int64(binary.BigEndian.Uint64(digest[:8]) & uint64(^uint64(0)>>1))
	if key == 0 {
		return 1
	}
	return key
}
