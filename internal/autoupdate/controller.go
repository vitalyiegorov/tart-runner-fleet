// Package autoupdate performs fail-safe, generation-based fleet controller updates.
package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrInvalidGeneration = errors.New("autoupdate: invalid generation")
	ErrModeChange        = errors.New("autoupdate: update cannot change controller mode")
	ErrDowngrade         = errors.New("autoupdate: release must move forward")
)

// Generation is the complete binary/config/service tuple that must survive a
// reboot. A generation is never partially promoted.
type Generation struct {
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	ReleaseDir string `json:"releaseDir"`
	ConfigPath string `json:"configPath"`
	Endpoint   string `json:"endpoint"`
}

func (g Generation) validate() error {
	if strings.TrimSpace(g.Version) == "" || (g.Mode != "observe" && g.Mode != "shadow" && g.Mode != "canary" && g.Mode != "authority") ||
		!filepath.IsAbs(g.ReleaseDir) || !filepath.IsAbs(g.ConfigPath) || !strings.HasPrefix(g.Endpoint, "unix:///") {
		return ErrInvalidGeneration
	}
	return nil
}

// Host owns the side effects behind an update. Prepare must durably stage both
// generations before Activate may stop the old service. Rollback must restore
// the previous binary, config, mode, and boot plist as one unit.
type Host interface {
	Current(context.Context) (Generation, error)
	Validate(context.Context, Generation) error
	Prepare(context.Context, Generation, Generation) error
	Activate(context.Context, Generation) error
	Ready(context.Context, Generation) error
	Commit(context.Context, Generation) error
	Rollback(context.Context, Generation) error
}

// Controller executes one serialized release update.
type Controller struct{ Host Host }

func (c Controller) Apply(ctx context.Context, candidate Generation) error {
	if c.Host == nil {
		return ErrInvalidGeneration
	}
	if err := candidate.validate(); err != nil {
		return err
	}
	current, err := c.Host.Current(ctx)
	if err != nil {
		return fmt.Errorf("read installed generation: %w", err)
	}
	if err := current.validate(); err != nil {
		return fmt.Errorf("installed generation: %w", err)
	}
	if current.Version == candidate.Version && current.Mode == candidate.Mode && current.ReleaseDir == candidate.ReleaseDir &&
		current.ConfigPath == candidate.ConfigPath && current.Endpoint == candidate.Endpoint {
		return nil
	}
	if current.Mode != candidate.Mode {
		return ErrModeChange
	}
	order, err := compareVersions(current.Version, candidate.Version)
	configOnly := current.Version == candidate.Version && current.ReleaseDir == candidate.ReleaseDir &&
		current.Endpoint == candidate.Endpoint
	if err != nil || order > 0 || (order == 0 && !configOnly) {
		return ErrDowngrade
	}
	if err := c.Host.Validate(ctx, candidate); err != nil {
		return fmt.Errorf("validate candidate: %w", err)
	}
	if err := c.Host.Prepare(ctx, current, candidate); err != nil {
		return fmt.Errorf("prepare update: %w", err)
	}
	if err := c.Host.Activate(ctx, candidate); err != nil {
		return c.rollback(ctx, current, fmt.Errorf("activate candidate: %w", err))
	}
	if err := c.Host.Ready(ctx, candidate); err != nil {
		return c.rollback(ctx, current, fmt.Errorf("candidate readiness: %w", err))
	}
	if err := c.Host.Commit(ctx, candidate); err != nil {
		return c.rollback(ctx, current, fmt.Errorf("commit update: %w", err))
	}
	return nil
}

func compareVersions(left, right string) (int, error) {
	parse := func(value string) ([]int, error) {
		core := strings.TrimPrefix(strings.SplitN(strings.SplitN(value, "+", 2)[0], "-", 2)[0], "v")
		parts := strings.Split(core, ".")
		result := make([]int, len(parts))
		for index, part := range parts {
			number, err := strconv.Atoi(part)
			if part == "" || err != nil || number < 0 {
				return nil, ErrInvalidGeneration
			}
			result[index] = number
		}
		return result, nil
	}
	leftParts, err := parse(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parse(right)
	if err != nil {
		return 0, err
	}
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for index := 0; index < length; index++ {
		var leftPart, rightPart int
		if index < len(leftParts) {
			leftPart = leftParts[index]
		}
		if index < len(rightParts) {
			rightPart = rightParts[index]
		}
		if leftPart < rightPart {
			return -1, nil
		}
		if leftPart > rightPart {
			return 1, nil
		}
	}
	return 0, nil
}

func (c Controller) rollback(ctx context.Context, current Generation, cause error) error {
	if err := c.Host.Rollback(context.WithoutCancel(ctx), current); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback generation: %w", err))
	}
	return cause
}
