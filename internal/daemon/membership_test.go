package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/membership"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

var (
	memberT0  = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	memberSet = membership.Key{Scope: "knee", ID: 19}
	otherSet  = membership.Key{Scope: "knee", ID: 20}
)

func membershipHealth(t *testing.T) *telemetry.Health {
	t.Helper()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	return health
}

// publishAudit stands in for the parked-scale-set auditor: the same two calls,
// in the same order, it makes after a completed audit.
func publishAudit(t *testing.T, health *telemetry.Health, at time.Time, wedged ...membership.Key) {
	t.Helper()
	rows := make([]telemetry.StrandedScaleSetMetric, 0, len(wedged))
	for _, key := range wedged {
		rows = append(rows, telemetry.StrandedScaleSetMetric{Scope: key.Scope, ScaleSetID: key.ID, Name: "trf-knee",
			Assigned: 1, Busy: 1, ObservedAt: at, Reason: "wedged"})
	}
	if err := health.SetStrandedScaleSets(rows); err != nil {
		t.Fatal(err)
	}
	if err := health.SetParkedScaleSets(nil, at); err != nil {
		t.Fatal(err)
	}
}

func TestMembershipControllerIsAbsentUnlessDeclared(t *testing.T) {
	sources := map[membership.Key]yieldableSource{memberSet: &fakeYieldSource{}}
	if newMembershipController(membership.Off, 15*time.Minute, sources, nil) != nil {
		t.Fatal("a node that declared no membership got a watchdog")
	}
	if newMembershipController(membership.Stationary, 15*time.Minute, nil, nil) != nil {
		t.Fatal("a node with no sessions got a watchdog")
	}
	var absent *membershipController
	if absent.excludes(&fakeYieldSource{}) {
		t.Fatal("an absent watchdog excluded something")
	}
	absent.Apply(context.Background(), telemetry.Snapshot{}, memberT0, false)
}

// The whole ladder through the daemon: heal recycles one session, exclusion
// releases exactly that session through the yield controller, the elapsed
// exclusion rejoins it, and each transition is published and logged.
func TestMembershipLadderDrivesOneBindingsSession(t *testing.T) {
	reporter, logged := silenceReporter()
	health := membershipHealth(t)
	wedged, healthy := &fakeYieldSource{}, &fakeYieldSource{}
	member := newMembershipController(membership.Roaming, 15*time.Minute,
		map[membership.Key]yieldableSource{memberSet: wedged, otherSet: healthy}, reporter)
	yield := &sessionYieldController{state: newSessionYieldState(yieldPolicy()),
		sources: []yieldableSource{wedged, healthy}, excluded: member.excludes}
	ticker := engineTicker{health: health, reporter: reporter, yield: yield, membership: member}
	serving := func(at time.Time) app.TickResult {
		return app.TickResult{At: at, Host: domain.Host{Pressure: domain.HostPressure{AdmissionAllowed: true}}}
	}

	publishAudit(t, health, memberT0, memberSet)
	ticker.applySessionYield(context.Background(), serving(memberT0), 0)
	if wedged.suspends != 1 || wedged.resumes != 1 || wedged.Suspended() || healthy.suspends != 0 {
		t.Fatalf("a heal must recycle exactly the wedged session: wedged=%+v healthy=%+v", wedged, healthy)
	}
	if metric := health.Snapshot().Membership; metric == nil || metric.Mode != "roaming" || metric.State != "healing" {
		t.Fatalf("published membership = %+v", metric)
	}

	at := memberT0.Add(15 * time.Minute)
	publishAudit(t, health, at, memberSet)
	ticker.applySessionYield(context.Background(), serving(at), 0)
	if !wedged.Suspended() || healthy.Suspended() {
		t.Fatalf("exclusion must release only the wedged session: wedged=%v healthy=%v", wedged.Suspended(), healthy.Suspended())
	}
	if total, withdrawn := yield.Bindings(); total != 2 || withdrawn != 1 {
		t.Fatalf("bindings = %d/%d", withdrawn, total)
	}
	if metric := health.Snapshot().SessionYield; metric == nil || metric.Yielded {
		t.Fatalf("one excluded set is not a withdrawn node: %+v", metric)
	}
	if metric := health.Snapshot().Membership; metric.State != "excluded" || len(metric.Bindings) != 1 ||
		metric.Bindings[0].ScaleSetID != 19 || metric.Bindings[0].Exclusions != 1 {
		t.Fatalf("published membership = %+v", metric)
	}

	rejoin := at.Add(30 * time.Minute)
	ticker.applySessionYield(context.Background(), serving(rejoin), 0)
	if wedged.Suspended() {
		t.Fatal("an elapsed exclusion must reopen the session")
	}
	if metric := health.Snapshot().Membership; metric.State != "probation" {
		t.Fatalf("published membership = %+v", metric)
	}
	for _, want := range []string{"scale-set membership heal", "scale-set membership exclude", "scale-set membership rejoin", "scope=knee", "scaleSet=19"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log is missing %q:\n%s", want, logged.String())
		}
	}
}

// A withdrawn node (ADR 0047) holds no sessions to heal; the ladder waits for it
// to rejoin rather than reopening a session the node gave up on purpose.
func TestMembershipLadderIsFrozenWhileTheNodeIsWithdrawn(t *testing.T) {
	health := membershipHealth(t)
	source := &fakeYieldSource{}
	member := newMembershipController(membership.Stationary, 15*time.Minute,
		map[membership.Key]yieldableSource{memberSet: source}, nil)
	publishAudit(t, health, memberT0, memberSet)
	member.Apply(context.Background(), health.Snapshot(), memberT0, true)
	if source.suspends != 0 || source.resumes != 0 {
		t.Fatalf("a withdrawn node healed a session: %+v", source)
	}
	if summary := member.Apply(context.Background(), health.Snapshot(), memberT0, false); summary.State != membership.Healing {
		t.Fatalf("after rejoining the node, the ladder resumes: %+v", summary)
	}
}

// A heal GitHub refuses to release is still an attempt: the ladder counts what
// it tried, so a set that cannot even be recycled reaches exclusion on time.
func TestARefusedHealIsStillCountedAndLeavesTheSessionHeld(t *testing.T) {
	health := membershipHealth(t)
	stubborn := &fakeYieldSource{suspendErr: errors.New("broker refused the release")}
	member := newMembershipController(membership.Roaming, 15*time.Minute,
		map[membership.Key]yieldableSource{memberSet: stubborn}, nil)
	publishAudit(t, health, memberT0, memberSet)
	member.Apply(context.Background(), health.Snapshot(), memberT0, false)
	if stubborn.resumes != 0 {
		t.Fatal("a session whose release was refused must not be reopened over itself")
	}
	publishAudit(t, health, memberT0.Add(15*time.Minute), memberSet)
	if summary := member.Apply(context.Background(), health.Snapshot(), memberT0.Add(15*time.Minute), false); summary.State != membership.Excluded {
		t.Fatalf("roaming allows one heal; the second wedged audit excludes: %+v", summary)
	}
	if !member.excludes(stubborn) {
		t.Fatal("the excluded source must read as excluded to the yield controller")
	}
}

// An excluded binding has no session by design, and says so the way a
// withdrawn node does: stale, not unavailable, and without polling.
func TestAnExcludedBindingIsQuietRatherThanFailing(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"},
		CriticalObservations: []string{"github-7"}})
	if err != nil {
		t.Fatal(err)
	}
	source := &suspendedMessageSource{}
	ingester := boundIngester{binding: app.Binding{StoreKey: 7}, source: source, health: health,
		observation: "github-7", yield: newSessionYieldState(yieldPolicy())}
	changed, ingestErr := ingester.IngestChanged(context.Background())
	if ingestErr != nil || changed || source.polled {
		t.Fatalf("an excluded binding polled or failed: changed=%v err=%v polled=%v", changed, ingestErr, source.polled)
	}
	if observation := health.Snapshot().Observations["github-7"]; observation.Freshness != telemetry.ObservationStale ||
		observation.Detail != telemetry.SessionYieldedObservation {
		t.Fatalf("excluded binding observation = %+v", observation)
	}
}

func TestOnlyTheAuthorityRunsTheMembershipLadder(t *testing.T) {
	cfg := config.Default()
	cfg.Membership = membership.Roaming
	for mode, want := range map[reconcile.Mode]membership.Mode{reconcile.Authority: membership.Roaming,
		reconcile.Canary: membership.Off, reconcile.Shadow: membership.Off, reconcile.Observe: membership.Off} {
		if got := membershipMode(mode, cfg); got != want {
			t.Fatalf("membershipMode(%s) = %q, want %q", mode, got, want)
		}
	}
}

// The wiring end to end: an authority node that declared a membership starts
// with the ladder bound to every session it opened.
func TestRunWiresTheMembershipLadderOnAnAuthorityThatDeclaredOne(t *testing.T) {
	d := testDependencies(t)
	ready := make(chan struct{})
	var once sync.Once
	d.inventory = func(runtimeStore, config.Config, app.RecoveryObserver, *updateDrainController) app.Inventory {
		return notifyingInventory{ready: ready, once: &once}
	}
	d.newScaleSet = func(context.Context, githubscaleset.GitHubAppScaleSetConfig) (scaleSetSource, error) {
		return &fakeSource{}, nil
	}
	path := writeMultiScopeConfig(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Membership = membership.Roaming
	var encoded bytes.Buffer
	if err := config.Encode(&encoded, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, options{ConfigPath: path, DatabasePath: filepath.Join(t.TempDir(), "fleet.db"),
			HealthAddress: "127.0.0.1:0", Mode: reconcile.Authority}, d)
	}()
	select {
	case <-ready:
		cancel()
	case err := <-done:
		cancel()
		t.Fatalf("runtime stopped: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("runtime not ready")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type suspendedMessageSource struct{ stubMessageSource }

func (*suspendedMessageSource) Suspended() bool { return true }
