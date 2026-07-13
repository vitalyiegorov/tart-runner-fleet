package app

import (
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
	bindings := make([]Binding, 0, len(cfg.GitHub.ScaleSets))
	for _, scaleSet := range cfg.GitHub.ScaleSets {
		profile, ok := schedulerConfig.Profiles[domain.ProfileID(scaleSet.Profile)]
		if !ok || scaleSet.ID <= 0 {
			return nil, fmt.Errorf("invalid scale set profile %q", scaleSet.Profile)
		}
		bindings = append(bindings, Binding{ScaleSetID: int64(scaleSet.ID), Profile: profile})
	}
	return bindings, nil
}
