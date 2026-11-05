package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// runnerRefusalTimeout bounds the one read at teardown. Teardown proceeds whatever
// it finds, and a guest that cannot answer in time has established nothing.
const runnerRefusalTimeout = 5 * time.Second

// guestRunnerRefusalReader asks a container guest's runner log whether the broker
// refused it (ADR 0094). The command is fixed and its output passes the domain's
// grammar, so no guest text reaches a log line or a status document.
type guestRunnerRefusalReader struct {
	Runner   executor.CommandRunner
	Refusals *app.RunnerRefusals
}

func (r guestRunnerRefusalReader) ReadRunnerRefusal(ctx context.Context, instance operations.Instance) {
	if domain.ValidateInstanceName(instance.ID) != nil {
		return
	}
	attempt, cancel := context.WithTimeout(ctx, runnerRefusalTimeout)
	defer cancel()
	// grep's non-zero exit is the ordinary "no such line", and a failed read proves
	// no more than that, so an error is simply no evidence.
	output, err := r.Runner.Run(attempt, append([]string{"exec", instance.ID}, domain.RunnerRefusalLogCommand()...)...)
	if err != nil {
		return
	}
	if version, refused := domain.ParseRunnerRefusal(output); refused {
		r.Refusals.Record(instance.Platform, version, instance.ID)
	}
}

// runnerRefusalReader is nil on a node that must not read its guests: a Tart node,
// where each `tart exec` is vsock traffic (issue #478) and the guest has usually
// powered itself off by the time a drain could ask (ADR 0080). Such a node relies
// on the empty-exit backoff and the release calendar instead.
func (d dependencies) runnerRefusalReader(cfg config.Config, refusals *app.RunnerRefusals) lifecycle.RunnerRefusalReader {
	if d.guestExec == nil || (d.hostTraffic != nil && d.hostTraffic(cfg) != nil) {
		return nil
	}
	runner := d.guestExec(cfg)
	if runner == nil {
		return nil
	}
	return guestRunnerRefusalReader{Runner: runner, Refusals: refusals}
}

// runnerReleases is the last release calendar read. Nil until one succeeds, which
// leaves the install window unjudged rather than passed by a calendar nobody read.
type runnerReleases struct {
	current atomic.Pointer[[]config.RunnerRelease]
}

func (r *runnerReleases) load() []config.RunnerRelease {
	if r == nil {
		return nil
	}
	if current := r.current.Load(); current != nil {
		return *current
	}
	return nil
}

// runnerReleaseInterval is how often the calendar is read. Releases come weeks apart
// and the window it judges is thirty days, so nothing is gained by asking sooner.
const runnerReleaseInterval = 6 * time.Hour

// runnerReleaseObserver reads the public `actions/runner` release calendar on a slow
// cadence. It is an ingester for the reason parkedScaleSetAuditor is. A failed read keeps the last calendar and is said out loud,
// and it never degrades the node: this is a warning about the future, not a fact
// about the present.
type runnerReleaseObserver struct {
	releases *runnerReleases
	fetch    func(context.Context) ([]config.RunnerRelease, error)
	now      func() time.Time
	warn     func(string, ...any)
	next     time.Time
}

func (o *runnerReleaseObserver) Ingest(ctx context.Context) error {
	if wait := o.next.Sub(o.now()); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	o.next = o.now().Add(runnerReleaseInterval)
	releases, err := o.fetch(ctx)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		o.warn("runner release calendar unavailable, so the install window stays as last read", "error", err)
	default:
		o.releases.current.Store(&releases)
	}
	return nil
}

const (
	runnerReleasesURL     = "https://api.github.com/repos/actions/runner/releases?per_page=30"
	runnerReleasesTimeout = 15 * time.Second
	runnerReleasesLimit   = 4 << 20
)

// fetchRunnerReleases reads the calendar without a credential: the repository is
// public, so this spends none of the App's budget, and one call per node per six
// hours sits far inside the anonymous limit.
func fetchRunnerReleases(ctx context.Context) ([]config.RunnerRelease, error) {
	attempt, cancel := context.WithTimeout(ctx, runnerReleasesTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(attempt, http.MethodGet, runnerReleasesURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner releases: status %d", response.StatusCode)
	}
	return parseRunnerReleases(io.LimitReader(response.Body, runnerReleasesLimit))
}

var runnerReleaseTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// parseRunnerReleases keeps stable releases only: a draft or prerelease starts no
// install clock, and v2.338.1 was a prerelease for exactly the four days between two
// stable ones. An answer with none in it is refused rather than read as "no newer
// release exists", which is what an empty calendar would otherwise mean.
func parseRunnerReleases(body io.Reader) ([]config.RunnerRelease, error) {
	var raw []struct {
		Tag         string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("runner releases: %w", err)
	}
	releases := make([]config.RunnerRelease, 0, len(raw))
	for _, release := range raw {
		if release.Draft || release.Prerelease || !runnerReleaseTag.MatchString(release.Tag) || release.PublishedAt.IsZero() {
			continue
		}
		releases = append(releases, config.RunnerRelease{Version: strings.TrimPrefix(release.Tag, "v"), PublishedAt: release.PublishedAt})
	}
	if len(releases) == 0 {
		return nil, errors.New("runner releases: no stable release in the answer")
	}
	return releases, nil
}

// recordRunnerImages re-judges each image every tick, because a refusal and the
// calendar are live facts where the declared version is not. The verdict is still
// config.RunnerImage.Reason's alone.
func (e engineTicker) recordRunnerImages(result app.TickResult) {
	images := e.runnerImages.RunnerImages()
	metrics := make([]telemetry.RunnerImageMetric, 0, len(images))
	releases := e.releases.load()
	for _, image := range images {
		image.Releases, image.Now = releases, result.At
		metric := telemetry.RunnerImageMetric{Platform: image.Platform, VM: image.VM, Version: image.Version, Floor: image.Floor}
		if refusal, refused := result.RunnerRefusals[runnerImagePlatform(image.Platform)]; refused {
			image.Refusal = &refusal
			metric.RefusedVersion, metric.RefusedAt, metric.NextProbeAt = refusal.Version, refusal.At, refusal.RetryAt()
			if e.reporter != nil {
				e.reporter.reportRunnerRefusal(image.Platform, image.VM, refusal)
			}
		}
		metric.Reason = image.Reason()
		metrics = append(metrics, metric)
	}
	if err := e.health.SetRunnerImages(metrics); err != nil && e.reporter != nil && e.reporter.logger != nil {
		e.reporter.logger.Warn("runner image verdicts were refused", "error", err)
	}
}

// runnerImagePlatform maps the config vocabulary onto the domain's. They differ in
// one letter's case, and a miss here would hide a refusal.
func runnerImagePlatform(platform string) domain.Platform {
	return domain.Platform(strings.ToLower(platform))
}

// recordEmptyExitBackoffs publishes the demand the fleet stopped spending instances
// on back to back, and says so once per attempt rather than once per tick.
func (e engineTicker) recordEmptyExitBackoffs(result app.TickResult) {
	metrics := make([]telemetry.EmptyExitBackoffMetric, 0, len(result.EmptyExitBackoffs))
	for _, backoff := range result.EmptyExitBackoffs {
		metrics = append(metrics, telemetry.EmptyExitBackoffMetric{Demand: backoff.Demand.String(), Scope: backoff.Scope,
			Profile: string(backoff.Profile), Exits: backoff.Exits, LastExitAt: backoff.LastAt, RetryAt: backoff.RetryAt})
		if e.reporter != nil {
			e.reporter.reportEmptyExitBackoff(backoff)
		}
	}
	e.health.SetEmptyExitBackoffs(metrics)
}

// reportRunnerRefusal says once per verdict that an image is refused.
func (r *failureReporter) reportRunnerRefusal(platform, vm string, refusal domain.RunnerRefusal) {
	if r.logger == nil || !r.admit("runner-refusal\x00"+platform+"\x00"+refusal.At.String()) {
		return
	}
	r.logger.Warn("runner image refused by GitHub, launches held", "platform", platform, "image", vm,
		"version", refusal.Version, "instance", refusal.Instance, "nextProbe", refusal.RetryAt().UTC().Format(time.RFC3339))
}

// reportEmptyExitBackoff says once per attempt that a demand is being held.
func (r *failureReporter) reportEmptyExitBackoff(backoff app.EmptyExitBackoff) {
	if r.logger == nil || !r.admit("empty-exit\x00"+backoff.Demand.String()+"\x00"+backoff.LastAt.String()) {
		return
	}
	r.logger.Warn("demand instances keep exiting without a job, holding it", "reason", "demand_empty_exit_backoff",
		"demand", backoff.Demand.String(), "scope", backoff.Scope, "profile", string(backoff.Profile),
		"exits", backoff.Exits, "retryAt", backoff.RetryAt.UTC().Format(time.RFC3339))
}
