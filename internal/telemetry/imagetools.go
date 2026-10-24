package telemetry

import "regexp"

// ImageToolsMetric is one guest image's declared tool versions, the required
// versions it lacks, and the verdict, as internal/config computed them (ADR
// 0079). Reason is carried rather than recomputed for the reason
// RunnerImageMetric gives: the rule is stated once, and this package only
// transports it.
type ImageToolsMetric struct {
	Platform string
	VM       string
	Declared []string
	Missing  []string
	// Reason is empty exactly when Missing is.
	Reason string
}

// boundedToolVersion is the transport bound on one `tool@version` entry. It is
// looser than the configuration grammar on purpose: this package refuses what
// it cannot publish safely, and the configuration owns what is meaningful.
var boundedToolVersion = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}@[0-9][0-9.]{0,31}$`)

// maxImageTools bounds each list so a producer cannot publish an unbounded
// status document, and maxImageToolsReason bounds the carried explanation.
const (
	maxImageTools       = 64
	maxImageToolsReason = 2048
)

// SetImageTools publishes the tool coverage of every base image this node
// boots. It replaces the whole set, so a rebuilt image stops being reported as
// drifted, and it refuses a set it cannot read rather than publishing part of
// it: a partial set would render a gap as coverage.
func (h *Health) SetImageTools(images []ImageToolsMetric) error {
	recorded := make([]ImageToolsMetric, 0, len(images))
	seen := make(map[string]struct{}, len(images))
	for _, metric := range images {
		if _, ok := runnerVersionPlatforms[metric.Platform]; !ok {
			return errInvalidMetric
		}
		if _, duplicate := seen[metric.Platform]; duplicate {
			return errInvalidMetric
		}
		if !boundedResourceID.MatchString(metric.VM) || !boundedTools(metric.Declared) ||
			!boundedTools(metric.Missing) || len(metric.Reason) > maxImageToolsReason ||
			(len(metric.Missing) == 0) != (metric.Reason == "") {
			return errInvalidMetric
		}
		seen[metric.Platform] = struct{}{}
		recorded = append(recorded, ImageToolsMetric{Platform: metric.Platform, VM: metric.VM,
			Declared: append([]string(nil), metric.Declared...), Missing: append([]string(nil), metric.Missing...),
			Reason: metric.Reason})
	}
	h.mu.Lock()
	h.revision++
	h.imageTools = recorded
	h.imageToolsPublished = true
	h.mu.Unlock()
	return nil
}

func boundedTools(tools []string) bool {
	if len(tools) > maxImageTools {
		return false
	}
	for _, tool := range tools {
		if !boundedToolVersion.MatchString(tool) {
			return false
		}
	}
	return true
}

// imageToolsResult judges the published set, or returns nil when nothing has
// been published: a daemon that has not looked has no verdict, and turning that
// into a pass is the silence issue #414 is about.
func imageToolsResult(snapshot Snapshot) *HealthResult {
	if !snapshot.ImageToolsPublished {
		return nil
	}
	reasons := []string{}
	for _, metric := range snapshot.ImageTools {
		if metric.Reason != "" {
			reasons = append(reasons, metric.Reason)
		}
	}
	return &HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}
