package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

type DemandStore interface {
	ApplyDemandBatch(context.Context, int64, int64, []operations.DemandEvent) (operations.DemandBatchResult, error)
	ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error)
}

type DemandStatisticsStore interface {
	PutDemandStatistics(context.Context, int64, operations.DemandStatistics) (bool, error)
	DemandStatistics(context.Context, int64) (operations.DemandStatistics, error)
}

type GitHubJobStore interface {
	ReconcileGitHubJobSnapshot(context.Context, time.Time, map[int64][]operations.GitHubJobObservation) (bool, error)
	QueuedGitHubJobs(context.Context, int64) ([]operations.GitHubJobObservation, error)
}

// GhostDemandStore retires demand that complete REST snapshots have proven
// absent. It is optional: a store without it keeps every durable demand, which
// is the pre-issue-#113 behaviour and always the safe direction.
type GhostDemandStore interface {
	ExpireGhostDemands(context.Context, int64, operations.GhostDemandCriteria) (int64, error)
}

type GitHubQueueSnapshot interface {
	ObservedAt() time.Time
	QueuedJobs() []githubscaleset.WorkflowJob
}

type DemandProjector interface {
	ProjectDemandEvent(context.Context, int64, operations.DemandEvent) error
}

type MessageSource interface {
	Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error
}

type Binding struct {
	StoreKey       int64
	ScaleSetID     int64
	Scope          string
	Targets        []string
	ScaleSetLabels []string
	RequiredLabels []string
	Profile        domain.Profile
	// SharedLabels marks a scale set another node also advertises in this scope
	// (ADR 0034 as amended). It is the one thing the REST inventory lane cannot
	// observe for itself, and it is what bounds this binding's claim on the
	// scope's queue to the share GitHub gave it (issue #153).
	SharedLabels bool
}

func (b Binding) valid() bool {
	return b.durableKey() > 0 && b.ScaleSetID > 0 && b.Profile.ID != "" && b.Profile.Route != "" &&
		(b.Profile.Platform == domain.PlatformLinux || b.Profile.Platform == domain.PlatformMacOS)
}

func (b Binding) durableKey() int64 {
	if b.StoreKey > 0 {
		return b.StoreKey
	}
	return b.ScaleSetID
}

func (b Binding) accepts(repo string) bool {
	if len(b.Targets) == 0 {
		return true
	}
	for _, target := range b.Targets {
		if strings.EqualFold(target, repo) {
			return true
		}
	}
	return false
}

func (b Binding) acceptsLabels(labels []string) bool {
	for _, required := range b.RequiredLabels {
		found := false
		for _, label := range labels {
			if strings.EqualFold(label, required) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// matchesRESTLabels mirrors GitHub's scale-set label compatibility rule: a
// queued job is eligible when every label it requests is advertised by the
// scale set. RequiredLabels is an additional controller-side isolation guard
// used by exact-scope canaries. Bindings constructed by older callers without
// ScaleSetLabels retain the narrow profile-route fallback.
func (b Binding) matchesRESTLabels(labels []string) bool {
	if !b.acceptsLabels(labels) {
		return false
	}
	if len(b.ScaleSetLabels) == 0 {
		return containsFold(labels, string(b.Profile.Route))
	}
	for _, requested := range labels {
		if !containsFold(b.ScaleSetLabels, requested) {
			return false
		}
	}
	return true
}

type DemandCoordinator struct {
	Store            DemandStore
	Projector        DemandProjector
	Now              func() time.Time
	StatisticsMaxAge time.Duration
	GhostAbsence     time.Duration
	StrictJobRouting bool
	// Priority classifies a demand into the tier configuration declared for it
	// (issue #224). The zero policy leaves every demand in the default tier,
	// which is what keeps a fleet that declares no tier on pure aged FIFO.
	Priority domain.PriorityPolicy
	// OnSequenceReset reports that a broker restarted its message-id sequence and
	// the fleet adopted a new inbox generation for this binding. The store heals
	// itself, but a rare durable event that repairs a three-day outage class must
	// never be silent: this is the seam that names the binding it happened to
	// (issue #165). Optional; nil means the daemon wired no reporter.
	OnSequenceReset func(Binding, operations.DemandSequenceReset)
}

const defaultStatisticsMaxAge = 2 * time.Minute

// defaultGhostAbsence and minGhostObservations bound how much REST evidence
// retires demand GitHub still advertises. The REST scope is polled at most
// every 30s, so the default window is dozens of complete snapshots in which a
// genuinely queued job would have appeared; the minimum observation count is
// the independent second bound that keeps one lonely snapshot after a long
// outage from concluding anything on its own.
const (
	defaultGhostAbsence  = 15 * time.Minute
	minGhostObservations = 3
)

func (c DemandCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c DemandCoordinator) statisticsMaxAge() time.Duration {
	if c.StatisticsMaxAge > 0 {
		return c.StatisticsMaxAge
	}
	return defaultStatisticsMaxAge
}

func (c DemandCoordinator) ghostAbsence() time.Duration {
	if c.GhostAbsence > 0 {
		return c.GhostAbsence
	}
	return defaultGhostAbsence
}

// ErrDemandStatisticsUnavailable marks a binding whose scale-set statistics
// are missing, stale, or ahead of the clock. Broker statistics arrive only
// with message activity, so a quiet queue ages past any freshness budget
// while its demand is still durable; callers must fail closed for that
// binding alone instead of failing the whole reconciliation tick, or the
// scheduler deadlocks fleet-wide until new messages arrive (issue #67).
// It wraps operations.ErrUncertain so existing uncertainty handling holds.
var ErrDemandStatisticsUnavailable = fmt.Errorf("demand statistics unavailable: %w", operations.ErrUncertain)

type QueueSummary struct {
	Count  int
	Oldest time.Time
	// Delivered is how many of this queue's jobs the node's own BROKER SESSION
	// handed it, and Observed is how many GitHub's REST view says are queued for
	// it. Count is the larger of the two, which is the right number to schedule
	// against and the wrong number to diagnose with.
	//
	// The gap between them is the whole of issue #292's first ask. On 2026-08-26
	// a scale set received no job for four hours while GitHub showed a matching
	// job queued the entire time; every signal read healthy from the node's own
	// chair, because the node's queue was the thing that was empty. Taking the
	// maximum makes the SLO correct and makes the divergence invisible, so both
	// terms are carried and the difference is reported rather than absorbed.
	Delivered int
	Observed  int
	// Tiers breaks the same queue down by the priority tier each waiting demand
	// was classified into (issue #224). It is empty for a fleet that declares no
	// tier, so an operator surface that never had this column keeps not having
	// it. Counts are per tier by the same rule the whole summary uses: the
	// broker's delivered demand and REST's complete view, whichever is larger.
	Tiers []QueueTier
}

// QueueTier is one priority tier's share of one queue. Rank travels with the
// name so every renderer can order tiers the way the operator declared them --
// highest first -- without knowing the configuration.
type QueueTier struct {
	Tier   string
	Rank   int
	Count  int
	Oldest time.Time
}

// ScopeQueue is one binding's queue depth attributed to the scope and scale set
// that own it, so an idle scope stays distinguishable from a busy one sharing the
// same profile.
type ScopeQueue struct {
	Scope      string
	Profile    domain.ProfileID
	ScaleSetID int64
	Count      int
	Oldest     time.Time
	Tiers      []QueueTier
	// Delivered and Observed are QueueSummary's two terms, carried per binding
	// because that is the granularity a starved session has: one scale set stops
	// being offered jobs while every other set on the node is fine (issue #292).
	Delivered int
	Observed  int
	// SharedLabels reports that this scale set is not alone on its labels, so a
	// job GitHub shows queued here may legitimately be the sibling node's to run.
	// It is what keeps the divergence from being read as this node's fault.
	SharedLabels bool
}

func (c DemandCoordinator) QueueSummary(ctx context.Context, binding Binding, executable []domain.Demand) (QueueSummary, error) {
	summary := QueueSummary{Count: len(executable), Delivered: len(executable)}
	for _, demand := range executable {
		if summary.Oldest.IsZero() || demand.CreatedAt.Before(summary.Oldest) {
			summary.Oldest = demand.CreatedAt
		}
	}
	store, ok := c.Store.(GitHubJobStore)
	if !ok {
		summary.Tiers = c.queueTiers(executable, nil)
		return summary, nil
	}
	jobs, err := store.QueuedGitHubJobs(ctx, binding.durableKey())
	if err != nil {
		return QueueSummary{}, err
	}
	summary.Observed = len(jobs)
	if len(jobs) > summary.Count {
		summary.Count = len(jobs)
	}
	for _, job := range jobs {
		if job.QueueTimeExact && (summary.Oldest.IsZero() || job.CreatedAt.Before(summary.Oldest)) {
			summary.Oldest = job.CreatedAt
		}
	}
	summary.Tiers = c.queueTiers(executable, jobs)
	return summary, nil
}

// tierTally accumulates one lane's view of one tier.
type tierTally struct {
	rank   int
	count  int
	oldest time.Time
}

func (t *tierTally) observe(rank int, createdAt time.Time, exact bool) {
	t.rank = rank
	t.count++
	if exact && !createdAt.IsZero() && (t.oldest.IsZero() || createdAt.Before(t.oldest)) {
		t.oldest = createdAt
	}
}

// queueTiers decomposes a queue by priority tier. A fleet that declares no tier
// gets nothing: there is exactly one tier then, and a column that always reads
// "default" is noise in the one view an operator reads during an incident.
//
// The two lanes are merged the way the aggregate merges them. The broker
// delivers demand the fleet owns; REST observes the whole queue including jobs
// no message has arrived for yet. Neither is a superset of the other in every
// state, so a tier's depth is the larger count and its oldest is the earlier
// exactly-known enqueue time.
func (c DemandCoordinator) queueTiers(executable []domain.Demand, jobs []operations.GitHubJobObservation) []QueueTier {
	if !c.Priority.Declared() {
		return nil
	}
	delivered := make(map[string]*tierTally, len(c.Priority.Tiers)+1)
	observed := make(map[string]*tierTally, len(c.Priority.Tiers)+1)
	for _, demand := range executable {
		tally(delivered, demand.Priority).observe(demand.Priority.Rank, demand.CreatedAt, true)
	}
	for _, job := range jobs {
		priority := c.Priority.Classify(domain.DemandFacts{Repo: job.Owner + "/" + job.Repository,
			WorkflowRef: job.WorkflowRef, JobName: job.DisplayName})
		tally(observed, priority).observe(priority.Rank, job.CreatedAt, job.QueueTimeExact)
	}
	tiers := make([]QueueTier, 0, len(delivered)+len(observed))
	for name, lane := range delivered {
		tiers = append(tiers, mergeTier(name, lane, observed[name]))
	}
	for name, lane := range observed {
		if delivered[name] == nil {
			tiers = append(tiers, mergeTier(name, lane, nil))
		}
	}
	// Highest tier first, then by name, so the order is the operator's and never
	// a map iteration's.
	sort.Slice(tiers, func(i, j int) bool {
		if tiers[i].Rank != tiers[j].Rank {
			return tiers[i].Rank > tiers[j].Rank
		}
		return tiers[i].Tier < tiers[j].Tier
	})
	return tiers
}

func tally(lanes map[string]*tierTally, priority domain.Priority) *tierTally {
	name := domain.PriorityTierName(priority)
	if lanes[name] == nil {
		lanes[name] = &tierTally{}
	}
	return lanes[name]
}

func mergeTier(name string, delivered, observed *tierTally) QueueTier {
	tier := QueueTier{Tier: name, Rank: delivered.rank, Count: delivered.count, Oldest: delivered.oldest}
	if observed == nil {
		return tier
	}
	if observed.count > tier.Count {
		tier.Count = observed.count
	}
	if !observed.oldest.IsZero() && (tier.Oldest.IsZero() || observed.oldest.Before(tier.Oldest)) {
		tier.Oldest = observed.oldest
	}
	return tier
}

// ReconcileQueuedJobs adds REST's complete queue view without granting it
// lifecycle authority. Jobs are routed through the same scope and profile
// predicates as scale-set demand.
func (c DemandCoordinator) ReconcileQueuedJobs(ctx context.Context, bindings []Binding, snapshot GitHubQueueSnapshot) (bool, error) {
	store, ok := c.Store.(GitHubJobStore)
	if !ok || snapshot == nil || snapshot.ObservedAt().IsZero() {
		return false, operations.ErrInvalid
	}
	observations := make(map[int64][]operations.GitHubJobObservation, len(bindings))
	// shared collects the scale sets this snapshot proved are not alone on their
	// labels, which is the case a declaration cannot cover: two bindings of one
	// node, and the two-node topology as the simulator expresses it.
	shared := make(map[int64]bool, len(bindings))
	for _, binding := range bindings {
		if !binding.valid() {
			return false, operations.ErrInvalid
		}
		observations[binding.durableKey()] = nil
	}
	for _, job := range snapshot.QueuedJobs() {
		matched, err := matchingBindings(bindings, job)
		if err != nil {
			return false, err
		}
		if len(matched) == 0 {
			if c.StrictJobRouting && containsFold(job.Labels, "self-hosted") {
				return false, fmt.Errorf("self-hosted GitHub job %d matches no configured scale set: %w", job.ID, operations.ErrUncertain)
			}
			continue
		}
		for _, binding := range matched {
			key := binding.durableKey()
			if len(matched) > 1 {
				// A second claimant is the shared label observed rather than
				// declared, and it binds every set that matched.
				shared[key] = true
			}
			observations[key] = append(observations[key], operations.GitHubJobObservation{WorkflowJobID: job.ID,
				Owner: job.Repository.Owner, Repository: job.Repository.Name, WorkflowRunID: job.RunID,
				RunAttempt: job.RunAttempt, DisplayName: job.Name, Labels: append([]string(nil), job.Labels...),
				Status: job.Status, CreatedAt: job.CreatedAt, QueueTimeExact: job.QueueTimeExact})
		}
	}
	for _, binding := range bindings {
		key := binding.durableKey()
		if !binding.SharedLabels && !shared[key] {
			continue
		}
		bounded, err := c.boundToOwnShare(ctx, binding, observations[key])
		if err != nil {
			return false, err
		}
		observations[key] = bounded
	}
	changed, err := store.ReconcileGitHubJobSnapshot(ctx, snapshot.ObservedAt(), observations)
	if err != nil {
		return changed, err
	}
	expired, err := c.expireGhostDemand(ctx, bindings, snapshot.ObservedAt())
	return changed || expired, err
}

// matchingBindings returns every scale set a repository-wide queued job may
// belong to.
//
// ADR 0034 as amended permits two nodes to own two scale sets in ONE scope
// carrying identical labels, because GitHub itself places the work between them
// by each set's last-advertised capacity. Under that topology a label match is
// no longer a routing decision, so failing the whole scope observation closed on
// a second match — which is every job in a federated scope — would make the REST
// lane unusable exactly where it is a precondition.
//
// Interchangeability is what makes the second match benign, and it is narrow:
// the bindings must serve the same scope with the same profile, so the job runs
// on the same shape of guest whichever set receives it. A job matching two
// PROFILES names two different VM shapes, and a job matching two SCOPES names a
// repository routed twice; neither is federation and neither is disambiguated by
// any capacity number, so both remain the ADR 0015 conflict.
func matchingBindings(bindings []Binding, job githubscaleset.WorkflowJob) ([]*Binding, error) {
	var matched []*Binding
	repository := job.Repository.Owner + "/" + job.Repository.Name
	for i := range bindings {
		binding := &bindings[i]
		if !binding.accepts(repository) || !binding.matchesRESTLabels(job.Labels) {
			continue
		}
		if len(matched) > 0 && !interchangeable(*matched[0], *binding) {
			return nil, fmt.Errorf("GitHub job %d matches scale sets %d and %d: %w",
				job.ID, matched[0].ScaleSetID, binding.ScaleSetID, operations.ErrConflict)
		}
		matched = append(matched, binding)
	}
	return matched, nil
}

// interchangeable reports whether two bindings are two federated peers serving
// one queue rather than two distinct routes that happen to overlap.
func interchangeable(left, right Binding) bool {
	return strings.EqualFold(left.Scope, right.Scope) && left.Profile.ID == right.Profile.ID
}

// boundToOwnShare caps what one binding may claim out of a REST scope
// observation at that scale set's OWN advertised share (issue #153).
//
// The REST lane attributes repository-wide queued jobs by label match, which
// under a shared label hands every node the whole scope backlog: both would
// derive demand for one job and each would spawn a guest for it, while GitHub
// had already given it to exactly one of them. The bound is already ingested —
// every broker message carries this scale set's own statistics — and it is the
// same expression the broker lane is bounded by in QueuedDemands: work offered
// to this set, plus work assigned to it that has not started. In the shape the
// #144 spike measured, where GitHub assigns rather than offers and Available is
// zero, that number IS statistics.totalAssignedJobs.
//
// Two rules make the bound safe to apply.
//
// A job this binding's own durable demand already names is never surrendered,
// whatever the count says. The broker is the mutation authority (ADR 0015) and
// its word on which jobs belong to this scale set outranks any statistic; it is
// also what makes the bound safe against ADR 0026, because a demand whose job
// is still queued is always corroborated by the snapshot that carries it and can
// never be expired by a truncation.
//
// And statistics that are absent, stale, or ahead of the clock bound the claim
// to that vouched set alone rather than to nothing. On a shared label an
// unbounded claim IS the defect, and a scale set with no fresh statistics has no
// evidence of a share beyond the work its broker already named. Under-claiming
// costs a queue-depth report the peer node is making anyway; over-claiming costs
// a duplicate guest. An unreadable statistics store is different from an absent
// one and still fails the whole observation: not knowing is not evidence.
func (c DemandCoordinator) boundToOwnShare(ctx context.Context, binding Binding,
	jobs []operations.GitHubJobObservation,
) ([]operations.GitHubJobObservation, error) {
	if len(jobs) == 0 {
		return jobs, nil
	}
	statisticsStore, ok := c.Store.(DemandStatisticsStore)
	if !ok {
		return jobs, nil
	}
	share := 0
	statistics, err := statisticsStore.DemandStatistics(ctx, binding.durableKey())
	switch {
	case err == nil:
		share = c.observedShare(statistics)
	case errors.Is(err, operations.ErrNotFound):
	default:
		return nil, err
	}
	if len(jobs) <= share {
		return jobs, nil
	}
	vouched, err := c.vouchedJobs(ctx, binding)
	if err != nil {
		return nil, err
	}
	kept := make([]operations.GitHubJobObservation, 0, len(jobs))
	contested := make([]operations.GitHubJobObservation, 0, len(jobs))
	for _, job := range jobs {
		if vouched[restCorrelationKey(job.Owner, job.Repository, job.WorkflowRunID, job.DisplayName)] ||
			vouched[restJobKey(job.WorkflowJobID)] {
			kept = append(kept, job)
			continue
		}
		contested = append(contested, job)
	}
	// Oldest first, then by stable numeric identity: the scope's queue is served
	// in age order everywhere else, and a truncation must name one exact subset
	// however the REST page happened to be ordered.
	sort.Slice(contested, func(i, j int) bool {
		if !contested[i].CreatedAt.Equal(contested[j].CreatedAt) {
			return contested[i].CreatedAt.Before(contested[j].CreatedAt)
		}
		return contested[i].WorkflowJobID < contested[j].WorkflowJobID
	})
	for _, job := range contested {
		if len(kept) >= share {
			break
		}
		kept = append(kept, job)
	}
	return kept, nil
}

// observedShare is how much of a scope's queue this scale set may still be
// waiting on: work GitHub has offered it, plus work GitHub has assigned it that
// has not started. Statistics that are stale or ahead of the clock report no
// share at all, on the same freshness rule QueuedDemands bounds admission by.
func (c DemandCoordinator) observedShare(statistics operations.DemandStatistics) int {
	now := c.now()
	if !statistics.Valid() || statistics.ObservedAt.IsZero() || statistics.ObservedAt.After(now) ||
		now.Sub(statistics.ObservedAt) > c.statisticsMaxAge() {
		return 0
	}
	return max(statistics.Available+statistics.Assigned-statistics.Running, 0)
}

// vouchedJobs is the set of queued jobs this binding's own durable demand
// names. The broker is the mutation authority (ADR 0015), so its word on which
// jobs belong to this scale set outranks any count derived from statistics.
func (c DemandCoordinator) vouchedJobs(ctx context.Context, binding Binding) (map[string]bool, error) {
	records, err := c.Store.ActiveDemands(ctx, binding.durableKey())
	if err != nil {
		return nil, err
	}
	vouched := make(map[string]bool, 2*len(records))
	for _, record := range records {
		vouched[restCorrelationKey(record.Owner, record.Repository, record.WorkflowRunID, record.DisplayName)] = true
		if record.WorkflowJobID > 0 {
			// A demand a previous snapshot already correlated carries REST's own
			// stable identity, which survives a display name the two lanes spell
			// differently.
			vouched[restJobKey(record.WorkflowJobID)] = true
		}
	}
	return vouched, nil
}

// restCorrelationKey is the identity a REST observation and a broker demand
// share before either has learned the other's numeric job ID. It is deliberately
// the same tuple the durable logical key is built from, minus the fields REST
// does not carry.
func restCorrelationKey(owner, repository string, runID int64, displayName string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repository) +
		"\x00" + strconv.FormatInt(runID, 10) + "\x00" + displayName
}

// restJobKey names a workflow job by the stable numeric identity REST supplies,
// which a demand carries only once a previous snapshot has correlated it.
func restJobKey(workflowJobID int64) string {
	return "job\x00" + strconv.FormatInt(workflowJobID, 10)
}

// expireGhostDemand retires demand this snapshot has now proven absent for
// longer than the ghost window. It runs only behind a committed, complete REST
// scope observation, so freshness is a property of the caller rather than a
// clock comparison: the same snapshot that failed to find the job is the one
// that dates the conclusion.
func (c DemandCoordinator) expireGhostDemand(ctx context.Context, bindings []Binding, observedAt time.Time) (bool, error) {
	store, ok := c.Store.(GhostDemandStore)
	if !ok {
		return false, nil
	}
	observed := observedAt.UTC()
	criteria := operations.GhostDemandCriteria{ObservedAt: observed, AbsentBefore: observed.Add(-c.ghostAbsence()),
		MinObservations: minGhostObservations}
	expired := false
	for _, binding := range bindings {
		count, err := store.ExpireGhostDemands(ctx, binding.durableKey(), criteria)
		if err != nil {
			return expired, err
		}
		expired = expired || count > 0
	}
	return expired, nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func (c DemandCoordinator) IngestOnce(ctx context.Context, binding Binding, source MessageSource) error {
	_, err := c.IngestOnceResult(ctx, binding, source)
	return err
}

// IngestOnceResult reports whether the source durably committed previously
// unseen demand before it returned. The boolean remains true when a later
// acknowledgement fails, allowing reconciliation to wake for already-durable
// work while message redelivery is retried independently.
func (c DemandCoordinator) IngestOnceResult(ctx context.Context, binding Binding, source MessageSource) (bool, error) {
	if c.Store == nil || source == nil || !binding.valid() {
		return false, operations.ErrInvalid
	}
	changed := false
	err := source.Handle(ctx, func(ctx context.Context, demand githubscaleset.Demand) error {
		if demand.Statistics.MessageID > 0 {
			statisticsStore, ok := c.Store.(DemandStatisticsStore)
			if !ok {
				return fmt.Errorf("demand statistics store unavailable: %w", operations.ErrUncertain)
			}
			statisticsChanged, err := statisticsStore.PutDemandStatistics(ctx, binding.durableKey(), operations.DemandStatistics{
				MessageID: int64(demand.Statistics.MessageID), Available: demand.Statistics.Available,
				Acquired: demand.Statistics.Acquired, Assigned: demand.Statistics.Assigned, Running: demand.Statistics.Running,
				Registered: demand.Statistics.Registered, Busy: demand.Statistics.Busy, Idle: demand.Statistics.Idle,
				ObservedAt: c.now(),
			})
			changed = changed || statisticsChanged
			if err != nil {
				return err
			}
		}
		events := make([]operations.DemandEvent, 0, len(demand.Events))
		for _, event := range demand.Events {
			if !binding.accepts(event.Owner+"/"+event.Repository) || !binding.acceptsLabels(event.Labels) {
				continue
			}
			events = append(events, convertEvent(event))
		}
		batch, err := c.Store.ApplyDemandBatch(ctx, binding.durableKey(), int64(demand.MessageID), events)
		changed = changed || batch.Applied
		if batch.Reset.Detected && c.OnSequenceReset != nil {
			c.OnSequenceReset(binding, batch.Reset)
		}
		if err != nil {
			return err
		}
		projector := c.Projector
		if projector == nil {
			projector, _ = c.Store.(DemandProjector)
		}
		if projector != nil {
			for _, event := range events {
				if err := projector.ProjectDemandEvent(ctx, binding.durableKey(), event); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return changed, err
}

func convertEvent(event githubscaleset.JobEvent) operations.DemandEvent {
	return operations.DemandEvent{Kind: operations.DemandEventKind(event.Kind), RunnerRequestID: event.RunnerRequestID,
		Owner: event.Owner, Repository: event.Repository, WorkflowRunID: event.WorkflowRunID, JobID: event.JobID,
		DisplayName: event.DisplayName, WorkflowRef: event.WorkflowRef, EventName: event.EventName,
		Labels: append([]string(nil), event.Labels...), QueueTime: event.QueueTime,
		RunnerID: event.RunnerID, RunnerName: event.RunnerName, Result: event.Result}
}

func (c DemandCoordinator) QueuedDemands(ctx context.Context, binding Binding) ([]domain.Demand, error) {
	if c.Store == nil || !binding.valid() {
		return nil, operations.ErrInvalid
	}
	records, err := c.Store.ActiveDemands(ctx, binding.durableKey())
	if err != nil {
		return nil, err
	}
	// A scale-set message may rotate request IDs for one logical workflow job.
	// Keep only its newest actionable request while preserving canonical age.
	canonical := make(map[string]operations.DemandRecord, len(records))
	for _, record := range records {
		if record.Status != operations.DemandJobAvailable {
			continue
		}
		if !binding.accepts(record.Owner+"/"+record.Repository) || !binding.acceptsLabels(record.Labels) {
			continue
		}
		createdAt := record.FirstQueueTime
		if createdAt.IsZero() {
			createdAt = record.QueueTime
		}
		if record.RunnerRequestID <= 0 || record.WorkflowRunID <= 0 || record.Owner == "" || record.Repository == "" || createdAt.IsZero() {
			return nil, fmt.Errorf("incomplete durable demand %d: %w", record.RunnerRequestID, operations.ErrUncertain)
		}
		key := record.LogicalKey
		if key == "" {
			key = fmt.Sprintf("request:%d", record.RunnerRequestID)
		}
		previous, exists := canonical[key]
		if !exists || record.UpdatedAt.After(previous.UpdatedAt) || (record.UpdatedAt.Equal(previous.UpdatedAt) && record.RunnerRequestID > previous.RunnerRequestID) {
			canonical[key] = record
		}
	}
	selected := make([]operations.DemandRecord, 0, len(canonical))
	for _, record := range canonical {
		selected = append(selected, record)
	}
	sort.Slice(selected, func(i, j int) bool {
		left, right := selected[i].FirstQueueTime, selected[j].FirstQueueTime
		if left.IsZero() {
			left = selected[i].QueueTime
		}
		if right.IsZero() {
			right = selected[j].QueueTime
		}
		if left.Equal(right) {
			return selected[i].RunnerRequestID < selected[j].RunnerRequestID
		}
		return left.Before(right)
	})
	if len(selected) == 0 {
		return []domain.Demand{}, nil
	}
	statisticsStore, ok := c.Store.(DemandStatisticsStore)
	if !ok {
		return nil, fmt.Errorf("scale-set statistics unavailable: %w", operations.ErrUncertain)
	}
	statistics, statisticsErr := statisticsStore.DemandStatistics(ctx, binding.durableKey())
	if statisticsErr != nil {
		if errors.Is(statisticsErr, operations.ErrNotFound) {
			return c.convertDemandRecords(binding, selected), fmt.Errorf("scale-set statistics not observed: %w", ErrDemandStatisticsUnavailable)
		}
		return nil, statisticsErr
	}
	now := c.now()
	if !statistics.Valid() || statistics.ObservedAt.IsZero() || statistics.ObservedAt.After(now) ||
		now.Sub(statistics.ObservedAt) > c.statisticsMaxAge() {
		return c.convertDemandRecords(binding, selected), fmt.Errorf("scale-set statistics are stale or invalid: %w", ErrDemandStatisticsUnavailable)
	}
	normalLimit := statistics.Available
	preassignedLimit := statistics.Assigned - statistics.Running
	if preassignedLimit < 0 {
		preassignedLimit = 0
	}
	bounded := make([]operations.DemandRecord, 0, min(len(selected), normalLimit+preassignedLimit))
	for _, record := range selected {
		if githubscaleset.IsPreassignedRequestID(record.RunnerRequestID) {
			if preassignedLimit == 0 {
				continue
			}
			preassignedLimit--
		} else {
			if normalLimit == 0 {
				continue
			}
			normalLimit--
		}
		bounded = append(bounded, record)
	}
	return c.convertDemandRecords(binding, bounded), nil
}

func (c DemandCoordinator) convertDemandRecords(binding Binding, records []operations.DemandRecord) []domain.Demand {
	result := make([]domain.Demand, 0, len(records))
	for _, record := range records {
		createdAt := record.FirstQueueTime
		if createdAt.IsZero() {
			createdAt = record.QueueTime
		}
		result = append(result, domain.Demand{
			// operations.DemandRecord.DemandKey is the single derivation shared with
			// the durable rebind in the inbox: an instance bound to a demand and the
			// queue entry for that demand must name one key, or plannableDemands
			// cannot see that the demand is already incarnated.
			Key:       record.DemandKey(),
			CreatedAt: createdAt.UTC(), Profile: binding.Profile.ID, Route: binding.Profile.Route, Platform: binding.Profile.Platform,
			Event: event(record.EventName), RunStatus: domain.RunQueued,
			// Classification happens exactly once, where a durable row becomes a
			// schedulable demand, from facts the scale-set message already carried.
			Priority: c.Priority.Classify(domain.DemandFacts{Repo: record.Repo(),
				WorkflowRef: record.WorkflowRef, JobName: record.DisplayName}),
		})
	}
	return result
}

func event(name string) domain.Event {
	switch strings.ToLower(name) {
	case "pull_request", "pull_request_target", "merge_group":
		return domain.EventPullRequest
	case "schedule":
		return domain.EventSchedule
	default:
		return domain.EventPush
	}
}
