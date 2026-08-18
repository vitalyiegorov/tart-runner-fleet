package telemetry

// RunnerImageMetric is one guest image's `actions/runner` version as the node
// declared it, together with the floor it was judged against and the verdict.
//
// Reason is carried rather than recomputed on purpose. The floor rule is stated
// once, in internal/config, as a predicate over the operator's own declaration;
// this package transports that answer so the metric, the `fleet doctor` finding
// and the configuration cannot drift into three different opinions about which
// image is behind. It is the same discipline the occupancy and reservation
// metrics follow: project a pure decision, never re-derive one.
type RunnerImageMetric struct {
	// Platform is `linux` or `macOS` and is the row's identity: a node has one
	// image per platform, so two rows for one platform is a producer bug.
	Platform string
	// VM is the base image name.
	VM string
	// Version is the declared runner version, empty when the node declares none —
	// which is itself a failing verdict and carries a Reason.
	Version string
	// Floor is the version Version was judged against, always populated.
	Floor string
	// Reason is empty when the image is compliant, and is the operator-facing
	// explanation otherwise.
	Reason string
}

// runnerVersionPlatforms is the closed vocabulary of image platforms. It is
// closed because Platform reaches the metrics endpoint as a label, and this
// codebase admits no unbounded label.
var runnerVersionPlatforms = map[string]struct{}{"linux": {}, "macOS": {}}

// maxRunnerImageReason bounds the carried explanation. A producer that hands
// over an unbounded string is refused rather than truncated, so a mangled row
// can never be published as a shorter, still-plausible one.
const maxRunnerImageReason = 512

// SetRunnerImages publishes what every base image this node boots carries. It
// replaces the whole set: an image that was rebuilt must stop being reported as
// behind, and a merged map would keep the stale finding alive forever.
//
// A set it cannot read is refused outright rather than partially published. The
// alternative is worse than useless here — an unpublished set renders as "no
// image is behind", which is exactly the silence issue #206 was filed about.
func (h *Health) SetRunnerImages(images []RunnerImageMetric) error {
	recorded := make([]RunnerImageMetric, 0, len(images))
	seen := make(map[string]struct{}, len(images))
	for _, metric := range images {
		if _, ok := runnerVersionPlatforms[metric.Platform]; !ok {
			return errInvalidMetric
		}
		if _, duplicate := seen[metric.Platform]; duplicate {
			return errInvalidMetric
		}
		if !boundedResourceID.MatchString(metric.VM) || metric.Floor == "" || len(metric.Reason) > maxRunnerImageReason {
			return errInvalidMetric
		}
		if metric.Version != "" && !boundedResourceID.MatchString(metric.Version) {
			return errInvalidMetric
		}
		seen[metric.Platform] = struct{}{}
		recorded = append(recorded, metric)
	}
	h.mu.Lock()
	h.revision++
	h.runnerImages = recorded
	h.mu.Unlock()
	return nil
}

// RunnerVersions reports every base image GitHub's minimum version enforcement
// would refuse. A daemon that has recorded nothing yet passes vacuously — that
// is the handoff half every check in this package carries, and it is the only
// absence treated as a pass: once a node HAS published its images, an image with
// no declared version fails, because "unknown" reading as "healthy" is the whole
// of what went wrong.
func (h *Health) RunnerVersions() HealthResult {
	h.mu.RLock()
	defer h.mu.RUnlock()
	reasons := []string{}
	for _, metric := range h.runnerImages {
		if metric.Reason == "" {
			continue
		}
		reasons = append(reasons, metric.Reason)
	}
	return HealthResult{OK: len(reasons) == 0, Reasons: reasons}
}
