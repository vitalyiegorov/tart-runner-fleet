package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

// wedgedSessionSource is the live-incident fixture: one official message
// session whose long poll always fails and whose broker-side release can also
// be refused, because GitHub no longer knows the session it is asked to delete.
type wedgedSessionSource struct {
	fakeSource
	handled  atomic.Int32
	err      error
	closeErr error
}

func (s *wedgedSessionSource) Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error {
	s.handled.Add(1)
	return s.err
}

func (s *wedgedSessionSource) Close(context.Context) error {
	s.closed.Add(1)
	return s.closeErr
}

// The closed-vocabulary reason must reach the service failure hook, which is
// the only log line an operator sees when one binding keeps failing.
var _ app.FailureReason = (*githubscaleset.SessionFailure)(nil)

type recoveryHarness struct {
	source  *recoveringScaleSetSource
	opens   atomic.Int32
	openErr func(int32) error
}

func newRecoveryHarness(t *testing.T, initial scaleSetSource, policy githubscaleset.SessionRecoveryPolicy) *recoveryHarness {
	t.Helper()
	harness := &recoveryHarness{}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial,
		open: func(context.Context) (scaleSetSource, error) {
			attempt := harness.opens.Add(1)
			if harness.openErr != nil {
				if openErr := harness.openErr(attempt); openErr != nil {
					return nil, openErr
				}
			}
			return &successfulSessionSource{}, nil
		}, limiter: make(chan struct{}, 1), policy: policy, now: func() time.Time {
			return time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
		}})
	if err != nil {
		t.Fatal(err)
	}
	harness.source = source
	return harness
}

func (h *recoveryHarness) handle(ctx context.Context) error {
	return h.source.Handle(ctx, func(context.Context, githubscaleset.Demand) error { return nil })
}

// Regression: GitHub invalidated the six fleet-repo broker sessions and then
// refused to delete them, so release-before-open withheld every replacement.
// Ingestion stayed dead for four hours until the daemon was restarted by hand.
func TestRecoveringSourceRecreatesSessionGitHubRefusesToRelease(t *testing.T) {
	ctx := context.Background()
	wedged := &wedgedSessionSource{
		err:      fmt.Errorf("get next message: %w", scaleset.MessageQueueTokenExpiredError),
		closeErr: errors.New("private broker response body"),
	}
	harness := newRecoveryHarness(t, wedged, githubscaleset.SessionRecoveryPolicy{
		MaxConsecutiveFailures: 5, FailureWindow: 5 * time.Minute})

	err := harness.handle(ctx)
	if githubscaleset.IngestFailureDetail(err) != githubscaleset.ReasonSessionExpired {
		t.Fatalf("first failure detail = %q", githubscaleset.IngestFailureDetail(err))
	}
	if harness.opens.Load() != 1 || wedged.closed.Load() != 1 {
		t.Fatalf("opens=%d closes=%d after a terminal session failure", harness.opens.Load(), wedged.closed.Load())
	}
	if err := harness.handle(ctx); err != nil {
		t.Fatalf("ingest did not resume on the replacement session: %v", err)
	}
	if harness.opens.Load() != 1 || wedged.handled.Load() != 1 {
		t.Fatalf("opens=%d wedged polls=%d, want exactly one recreate", harness.opens.Load(), wedged.handled.Load())
	}
}

func TestRecoveringSourceEscalatesAmbiguousFailuresToRecreate(t *testing.T) {
	ctx := context.Background()
	// The official client hides a failed session refresh behind an opaque
	// error, so this failure cannot be proven terminal.
	wedged := &wedgedSessionSource{err: errors.New("failed to refresh message session"),
		closeErr: errors.New("private broker response body")}
	harness := newRecoveryHarness(t, wedged, githubscaleset.SessionRecoveryPolicy{
		MaxConsecutiveFailures: 3, FailureWindow: time.Hour})

	for attempt := 1; attempt < 3; attempt++ {
		err := harness.handle(ctx)
		if githubscaleset.IngestFailureDetail(err) != githubscaleset.ReasonSessionReleaseFailed {
			t.Fatalf("attempt %d detail = %q", attempt, githubscaleset.IngestFailureDetail(err))
		}
		if harness.opens.Load() != 0 {
			t.Fatalf("attempt %d recreated before the bound: opens=%d", attempt, harness.opens.Load())
		}
	}
	err := harness.handle(ctx)
	if githubscaleset.IngestFailureDetail(err) != githubscaleset.ReasonRecreatedAfterFailures {
		t.Fatalf("escalated detail = %q", githubscaleset.IngestFailureDetail(err))
	}
	if harness.opens.Load() != 1 || wedged.handled.Load() != 3 {
		t.Fatalf("opens=%d polls=%d, want one bounded recreate", harness.opens.Load(), wedged.handled.Load())
	}
	if err := harness.handle(ctx); err != nil {
		t.Fatalf("ingest did not resume after escalation: %v", err)
	}
}

func TestRecoveringSourceRetriesOpenWithoutReleasingTwice(t *testing.T) {
	ctx := context.Background()
	wedged := &wedgedSessionSource{err: errors.New("failed to refresh message session")}
	harness := newRecoveryHarness(t, wedged, githubscaleset.SessionRecoveryPolicy{
		MaxConsecutiveFailures: 5, FailureWindow: 5 * time.Minute})
	harness.openErr = func(attempt int32) error {
		if attempt == 1 {
			return errors.New("private broker response body")
		}
		return nil
	}

	err := harness.handle(ctx)
	if githubscaleset.IngestFailureDetail(err) != githubscaleset.ReasonSessionCreateFailed {
		t.Fatalf("open failure detail = %q", githubscaleset.IngestFailureDetail(err))
	}
	if wedged.closed.Load() != 1 {
		t.Fatalf("releases=%d after a failed replacement", wedged.closed.Load())
	}
	// The broker already released this session. Deleting it a second time would
	// fail permanently, so the next attempt must only open a replacement.
	if err := harness.handle(ctx); err == nil {
		t.Fatal("released session reported a successful poll")
	}
	if wedged.closed.Load() != 1 || harness.opens.Load() != 2 {
		t.Fatalf("releases=%d opens=%d, want one release and a retried open", wedged.closed.Load(), harness.opens.Load())
	}
	if err := harness.handle(ctx); err != nil {
		t.Fatalf("ingest did not resume after the retried open: %v", err)
	}
	if err := harness.source.Close(ctx); err != nil || wedged.closed.Load() != 1 {
		t.Fatalf("shutdown close err=%v releases=%d", err, wedged.closed.Load())
	}
}

func TestRecoveringSourceLeavesHealthySessionAlone(t *testing.T) {
	ctx := context.Background()
	healthy := &successfulSessionSource{}
	harness := newRecoveryHarness(t, healthy, githubscaleset.SessionRecoveryPolicy{
		MaxConsecutiveFailures: 1, FailureWindow: 5 * time.Minute})
	for range 4 {
		if err := harness.handle(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if harness.opens.Load() != 0 || healthy.closed.Load() != 0 || healthy.handled.Load() != 4 {
		t.Fatalf("opens=%d closes=%d polls=%d, want an untouched healthy session",
			harness.opens.Load(), healthy.closed.Load(), healthy.handled.Load())
	}
}

// durableBroker models the delivery contract that must survive a recreate: the
// committed cursor advances only after an acknowledgement, so an unacknowledged
// message is redelivered to the replacement session exactly once.
type durableBroker struct {
	mu      sync.Mutex
	cursor  int
	pending int
	acks    []int
}

func (b *durableBroker) open(context.Context) (scaleSetSource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &brokerGeneration{broker: b, cursor: b.cursor}, nil
}

func (b *durableBroker) ack(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acks = append(b.acks, id)
	b.cursor = id
}

type brokerGeneration struct {
	fakeSource
	broker *durableBroker
	cursor int
}

func (g *brokerGeneration) Handle(ctx context.Context, commit func(context.Context, githubscaleset.Demand) error) error {
	if g.cursor >= g.broker.pending {
		return nil
	}
	if err := commit(ctx, githubscaleset.Demand{MessageID: g.broker.pending}); err != nil {
		return fmt.Errorf("commit demand message %d: %w", g.broker.pending, err)
	}
	g.cursor = g.broker.pending
	g.broker.ack(g.broker.pending)
	return nil
}

func TestRecoveringSourceKeepsAtLeastOnceDeliveryAcrossRecreate(t *testing.T) {
	ctx := context.Background()
	broker := &durableBroker{pending: 7}
	initial, err := broker.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: initial, open: broker.open,
		limiter: make(chan struct{}, 1), policy: githubscaleset.SessionRecoveryPolicy{MaxConsecutiveFailures: 1,
			FailureWindow: 5 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	commits := 0
	commit := func(context.Context, githubscaleset.Demand) error {
		commits++
		if commits == 1 {
			return errors.New("durable commit failed")
		}
		return nil
	}
	if err := source.Handle(ctx, commit); err == nil {
		t.Fatal("failed durable commit reported success")
	}
	if len(broker.acks) != 0 || broker.cursor != 0 {
		t.Fatalf("acks=%v cursor=%d after a failed commit", broker.acks, broker.cursor)
	}
	if err := source.Handle(ctx, commit); err != nil {
		t.Fatalf("replacement session did not redeliver: %v", err)
	}
	if len(broker.acks) != 1 || broker.acks[0] != 7 || broker.cursor != 7 {
		t.Fatalf("acks=%v cursor=%d, want message 7 acknowledged exactly once", broker.acks, broker.cursor)
	}
	if err := source.Handle(ctx, commit); err != nil || len(broker.acks) != 1 {
		t.Fatalf("drained session err=%v acks=%v", err, broker.acks)
	}
}

func TestBoundIngesterPublishesClosedVocabularyPerBinding(t *testing.T) {
	ctx := context.Background()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"},
		CriticalObservations: []string{"github-1", "github-2"}})
	if err != nil {
		t.Fatal(err)
	}
	binding := func(key int64) app.Binding {
		return app.Binding{StoreKey: key, ScaleSetID: key,
			Profile: domain.Profile{ID: "small", Route: "linux-small", Platform: domain.PlatformLinux}}
	}
	wedged := &wedgedSessionSource{err: fmt.Errorf("poll: %w", scaleset.MessageQueueTokenExpiredError),
		closeErr: errors.New("private broker response body")}
	broken := newRecoveryHarness(t, wedged, githubscaleset.SessionRecoveryPolicy{MaxConsecutiveFailures: 5,
		FailureWindow: 5 * time.Minute})
	healthyHarness := newRecoveryHarness(t, &successfulSessionSource{}, githubscaleset.SessionRecoveryPolicy{
		MaxConsecutiveFailures: 5, FailureWindow: 5 * time.Minute})
	coordinator := app.DemandCoordinator{Store: memoryDemandStore{}}
	failing := boundIngester{coordinator: coordinator, binding: binding(1), source: broken.source,
		health: health, observation: "github-1"}
	healthy := boundIngester{coordinator: coordinator, binding: binding(2), source: healthyHarness.source,
		health: health, observation: "github-2"}

	if _, err := failing.IngestChanged(ctx); err == nil {
		t.Fatal("wedged binding reported a successful ingest")
	}
	if err := healthy.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	observations := health.Snapshot().Observations
	if observations["github-1"].Freshness != telemetry.ObservationUnavailable ||
		observations["github-1"].Detail != githubscaleset.ReasonSessionExpired {
		t.Fatalf("wedged observation = %+v", observations["github-1"])
	}
	if observations["github-2"].Freshness != telemetry.ObservationFresh || observations["github-2"].Detail != "" {
		t.Fatalf("healthy binding disturbed by another scope: %+v", observations["github-2"])
	}
}

// memoryDemandStore is the minimal durable surface boundIngester needs. It
// never fails, so every observation above is attributable to the session path.
type memoryDemandStore struct{}

func (memoryDemandStore) ApplyDemandBatch(context.Context, int64, int64, []operations.DemandEvent) (operations.DemandBatchResult, error) {
	return operations.DemandBatchResult{}, nil
}

func (memoryDemandStore) ActiveDemands(context.Context, int64) ([]operations.DemandRecord, error) {
	return nil, nil
}
