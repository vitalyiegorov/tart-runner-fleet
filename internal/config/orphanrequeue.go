package config

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// OrphanRequeue grants no authority by default. Patterns are case-insensitive * and ? globs; / is ordinary.
type OrphanRequeue struct {
	Mode             string            `json:"mode,omitempty"`
	ThresholdMinutes int               `json:"thresholdMinutes,omitempty"`
	CooldownMinutes  int               `json:"cooldownMinutes,omitempty"`
	RunsPerHour      int               `json:"runsPerHour,omitempty"`
	Exclusions       []OrphanExclusion `json:"exclusions,omitempty"`
}
type OrphanExclusion struct {
	Repository string `json:"repository,omitempty"`
	Workflow   string `json:"workflow,omitempty"`
	Job        string `json:"job,omitempty"`
}

func (c OrphanRequeue) Validate() error {
	if c.Mode != "" && c.Mode != "off" && c.Mode != "observe" && c.Mode != "act" {
		return errors.New("orphanRequeue.mode must be off, observe or act")
	}
	if c.ThresholdMinutes < 0 || c.CooldownMinutes < 0 || c.RunsPerHour < 0 {
		return errors.New("orphanRequeue limits must be positive when supplied")
	}
	for _, x := range c.Exclusions {
		for _, pattern := range []string{x.Repository, x.Workflow, x.Job} {
			if strings.ContainsAny(pattern, `[]\`) {
				return errors.New("invalid orphanRequeue exclusion pattern")
			}
		}
	}
	return nil
}
func (c OrphanRequeue) Threshold(slo time.Duration) time.Duration {
	n := c.ThresholdMinutes
	if n == 0 {
		n = 15
	}
	return max(time.Duration(n)*time.Minute, slo)
}
func (c OrphanRequeue) Cooldown() time.Duration {
	n := c.CooldownMinutes
	if n == 0 {
		n = 30
	}
	return time.Duration(n) * time.Minute
}
func (c OrphanRequeue) Limit() int {
	if c.RunsPerHour == 0 {
		return 3
	}
	return c.RunsPerHour
}
func (c OrphanRequeue) Excluded(repo, workflow string, jobs []string) bool {
	for _, x := range c.Exclusions {
		if !orphanPattern(x.Repository, repo) || !orphanPattern(x.Workflow, workflow) {
			continue
		}
		if x.Job == "" {
			return true
		}
		for _, job := range jobs {
			if orphanPattern(x.Job, job) {
				return true
			}
		}
	}
	return false
}
func orphanPattern(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	pattern = strings.ToLower(pattern)
	value = strings.ToLower(value)
	expression := strings.ReplaceAll(strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*"), `\?`, ".")
	ok, _ := regexp.MatchString("(?s)^"+expression+"$", value)
	return ok
}

func (g GitHub) validateOrphanRequeue() error {
	if (g.OrphanRequeue.ThresholdMinutes > 0 || g.OrphanRequeue.Mode == "act" || g.OrphanRequeue.Mode == "observe") && g.OrphanRequeue.Threshold(0) < g.ParkedScaleSetAuditInterval() {
		return errors.New("orphanRequeue threshold must cover the scale-set audit interval")
	}
	if err := g.OrphanRequeue.Validate(); err != nil {
		return err
	}
	if g.OrphanRequeue.Mode != "" && g.OrphanRequeue.Mode != "off" && (!g.CanonicalJobInventory || len(g.Scopes) == 0 || g.ParkedScaleSetAuditInterval() <= 0) {
		return errors.New("orphanRequeue requires canonicalJobInventory, scoped bindings and an enabled scale-set audit")
	}
	return nil
}
