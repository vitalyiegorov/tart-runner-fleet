package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/telemetry"
)

func yieldPolicy() sessionYieldPolicy {
	return sessionYieldPolicy{Enabled: true, BlockedFor: 10 * time.Minute, HealthyFor: 2 * time.Minute}
}

func TestSessionYieldWithdrawsOnlyAfterSustainedBlockedIdleness(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	state := newSessionYieldState(yieldPolicy())

	if action := state.Observe(sessionYieldFacts{At: start, AdmissionAllowed: false}); action != sessionYieldNone {
		t.Fatalf("first blocked tick acted immediately: %s", action)
	}
	if action := state.Observe(sessionYieldFacts{At: start.Add(9 * time.Minute), AdmissionAllowed: false}); action != sessionYieldNone {
		t.Fatalf("withdrew before the bound elapsed: %s", action)
	}
	if state.Yielded() {
		t.Fatal("node reported yielded before withdrawing")
	}
	action := state.Observe(sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false})
	if action != sessionYieldWithdraw {
		t.Fatalf("blocked and idle for the full bound did not withdraw: %s", action)
	}
	if !state.Yielded() {
		t.Fatal("withdrawal is not reportable")
	}
	if repeat := state.Observe(sessionYieldFacts{At: start.Add(20 * time.Minute), AdmissionAllowed: false}); repeat != sessionYieldNone {
		t.Fatalf("withdrew twice: %s", repeat)
	}
}

func TestSessionYieldHoldsSessionsWhileWorkIsInFlight(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		facts sessionYieldFacts
	}{
		{"live instance", sessionYieldFacts{LiveInstances: 1}},
		{"retrying operation", sessionYieldFacts{BusyOperations: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newSessionYieldState(yieldPolicy())
			for minute := 0; minute <= 30; minute++ {
				facts := test.facts
				facts.At = start.Add(time.Duration(minute) * time.Minute)
				facts.AdmissionAllowed = false
				if action := state.Observe(facts); action != sessionYieldNone {
					t.Fatalf("withdrew the session running work reports through: %s", action)
				}
			}
			// The clock restarts when the work ends: a node must not withdraw the
			// instant its last instance retires.
			busyUntil := start.Add(30 * time.Minute)
			if action := state.Observe(sessionYieldFacts{At: busyUntil.Add(9 * time.Minute), AdmissionAllowed: false}); action != sessionYieldNone {
				t.Fatalf("counted busy minutes toward the blocked bound: %s", action)
			}
			if action := state.Observe(sessionYieldFacts{At: busyUntil.Add(10 * time.Minute), AdmissionAllowed: false}); action != sessionYieldWithdraw {
				t.Fatalf("never withdrew after the work drained: %s", action)
			}
		})
	}
}

func TestSessionYieldRejoinsOnlyAfterSustainedHealth(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	state := newSessionYieldState(yieldPolicy())
	state.Observe(sessionYieldFacts{At: start, AdmissionAllowed: false})
	if action := state.Observe(sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false}); action != sessionYieldWithdraw {
		t.Fatalf("setup did not withdraw: %s", action)
	}
	recovered := start.Add(11 * time.Minute)
	if action := state.Observe(sessionYieldFacts{At: recovered, AdmissionAllowed: true}); action != sessionYieldNone {
		t.Fatalf("rejoined on the first healthy tick: %s", action)
	}
	if action := state.Observe(sessionYieldFacts{At: recovered.Add(time.Minute), AdmissionAllowed: true}); action != sessionYieldNone {
		t.Fatalf("rejoined before the healthy bound elapsed: %s", action)
	}
	// A relapse restarts the healthy streak rather than carrying it.
	if action := state.Observe(sessionYieldFacts{At: recovered.Add(90 * time.Second), AdmissionAllowed: false}); action != sessionYieldNone {
		t.Fatalf("relapse produced an action: %s", action)
	}
	relapsed := recovered.Add(2 * time.Minute)
	if action := state.Observe(sessionYieldFacts{At: relapsed, AdmissionAllowed: true}); action != sessionYieldNone {
		t.Fatalf("carried the pre-relapse streak into a rejoin: %s", action)
	}
	action := state.Observe(sessionYieldFacts{At: relapsed.Add(2 * time.Minute), AdmissionAllowed: true})
	if action != sessionYieldRejoin {
		t.Fatalf("never rejoined after sustained health: %s", action)
	}
	if state.Yielded() {
		t.Fatal("still reports yielded after rejoining")
	}
}

func TestSessionYieldDisabledPolicyHoldsAndReleasesAYieldedNode(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	disabled := newSessionYieldState(sessionYieldPolicy{})
	for minute := 0; minute <= 60; minute++ {
		action := disabled.Observe(sessionYieldFacts{At: start.Add(time.Duration(minute) * time.Minute)})
		if action != sessionYieldNone {
			t.Fatalf("disabled policy acted: %s", action)
		}
	}

	// Disabling the policy on a node that has already withdrawn must bring it
	// back rather than strand it outside the fleet.
	state := newSessionYieldState(yieldPolicy())
	state.Observe(sessionYieldFacts{At: start, AdmissionAllowed: false})
	state.Observe(sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false})
	state.policy.Enabled = false
	if action := state.Observe(sessionYieldFacts{At: start.Add(11 * time.Minute), AdmissionAllowed: false}); action != sessionYieldRejoin {
		t.Fatalf("disabling the policy left the node withdrawn: %s", action)
	}
	if state.Yielded() {
		t.Fatal("still yielded after the policy was disabled")
	}
}

func TestSessionYieldTreatsABackwardClockAsARestartedStreak(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	state := newSessionYieldState(yieldPolicy())
	state.Observe(sessionYieldFacts{At: start, AdmissionAllowed: false})
	// A correction that moves the clock back must not hand the bound elapsed
	// time nobody waited through.
	if action := state.Observe(sessionYieldFacts{At: start.Add(-time.Hour), AdmissionAllowed: false}); action != sessionYieldNone {
		t.Fatalf("a backward clock produced an action: %s", action)
	}
	if action := state.Observe(sessionYieldFacts{At: start.Add(-time.Hour).Add(9 * time.Minute), AdmissionAllowed: false}); action != sessionYieldNone {
		t.Fatalf("withdrew on a streak the corrected clock never measured: %s", action)
	}
	if action := state.Observe(sessionYieldFacts{At: start.Add(-time.Hour).Add(10 * time.Minute), AdmissionAllowed: false}); action != sessionYieldWithdraw {
		t.Fatalf("never withdrew after the corrected clock earned the bound: %s", action)
	}
}

type fakeYieldSource struct {
	suspended  bool
	suspendErr error
	resumeErr  error
	suspends   int
	resumes    int
}

func (f *fakeYieldSource) Suspend(context.Context) error {
	f.suspends++
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspended = true
	return nil
}

func (f *fakeYieldSource) Resume(context.Context) error {
	f.resumes++
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.suspended = false
	return nil
}

func (f *fakeYieldSource) Suspended() bool { return f.suspended }

func TestSessionYieldControllerSuspendsEveryBindingAndRejoinsThemAll(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	sources := []*fakeYieldSource{{}, {}, {}}
	controller := &sessionYieldController{state: newSessionYieldState(yieldPolicy()),
		sources: []yieldableSource{sources[0], sources[1], sources[2]}}

	controller.Apply(context.Background(), sessionYieldFacts{At: start, AdmissionAllowed: false}, "disk reserve")
	for _, source := range sources {
		if source.Suspended() {
			t.Fatal("withdrew before the policy said to")
		}
	}
	if !controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false}, "disk reserve") {
		t.Fatal("controller did not report the node withdrawn")
	}
	for i, source := range sources {
		if !source.Suspended() {
			t.Fatalf("binding %d kept its session while the node was withdrawn", i)
		}
	}

	recovered := start.Add(20 * time.Minute)
	controller.Apply(context.Background(), sessionYieldFacts{At: recovered, AdmissionAllowed: true}, "")
	if yielded := controller.Apply(context.Background(), sessionYieldFacts{At: recovered.Add(2 * time.Minute), AdmissionAllowed: true}, ""); yielded {
		t.Fatal("still withdrawn after sustained health")
	}
	for i, source := range sources {
		if source.Suspended() {
			t.Fatalf("binding %d never rejoined", i)
		}
		if source.suspends != 1 || source.resumes != 1 {
			t.Fatalf("binding %d churned sessions: %d suspends, %d resumes", i, source.suspends, source.resumes)
		}
	}
}

func TestSessionYieldControllerRetriesABindingWhoseCloseWasRefused(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	stubborn := &fakeYieldSource{suspendErr: errors.New("broker refused the release")}
	willing := &fakeYieldSource{}
	controller := &sessionYieldController{state: newSessionYieldState(yieldPolicy()),
		sources: []yieldableSource{stubborn, willing}}

	controller.Apply(context.Background(), sessionYieldFacts{At: start, AdmissionAllowed: false}, "disk reserve")
	controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false}, "disk reserve")
	if !willing.Suspended() {
		t.Fatal("a refused close on one binding stopped the others from withdrawing")
	}
	if stubborn.Suspended() {
		t.Fatal("a refused close was recorded as a withdrawal")
	}

	// The next tick must try again rather than leave this node holding a session
	// it believes it dropped.
	stubborn.suspendErr = nil
	controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(11 * time.Minute), AdmissionAllowed: false}, "disk reserve")
	if !stubborn.Suspended() {
		t.Fatal("the refused binding was never retried")
	}
	if stubborn.suspends != 2 {
		t.Fatalf("expected exactly one retry, got %d attempts", stubborn.suspends)
	}
	if willing.suspends != 1 {
		t.Fatalf("re-suspended a binding that was already withdrawn: %d attempts", willing.suspends)
	}
}

func TestSessionYieldControllerReportsEachTransitionOnce(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	var actions []string
	controller := &sessionYieldController{state: newSessionYieldState(yieldPolicy()),
		sources: []yieldableSource{&fakeYieldSource{}},
		report: func(action sessionYieldAction, reason string, failures int) {
			actions = append(actions, action.String()+":"+reason)
		}}
	for minute := 0; minute <= 12; minute++ {
		controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(time.Duration(minute) * time.Minute),
			AdmissionAllowed: false}, "disk reserve")
	}
	recovered := start.Add(13 * time.Minute)
	for minute := 0; minute <= 3; minute++ {
		controller.Apply(context.Background(), sessionYieldFacts{At: recovered.Add(time.Duration(minute) * time.Minute),
			AdmissionAllowed: true}, "")
	}
	want := []string{"withdraw:disk reserve", "rejoin:"}
	if len(actions) != len(want) {
		t.Fatalf("reported %v, want exactly %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("reported %v, want %v", actions, want)
		}
	}
}

// stubMessageSource fails if it is ever polled. A withdrawn binding must not
// reach the broker at all: polling a session this node released on purpose is
// how a deliberate withdrawal comes to look like the outage it is not.
type stubMessageSource struct{ polled bool }

func (s *stubMessageSource) Handle(context.Context, func(context.Context, githubscaleset.Demand) error) error {
	s.polled = true
	return nil
}

func TestYieldedBindingIsQuietRatherThanFailing(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"},
		CriticalObservations: []string{"github-7"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	source := &stubMessageSource{}
	state := newSessionYieldState(yieldPolicy())
	ingester := boundIngester{binding: app.Binding{StoreKey: 7}, source: source, health: health,
		observation: "github-7", yield: state}

	// While serving, the binding reaches the coordinator. The zero-value
	// coordinator refuses, which is exactly the evidence wanted here: the call
	// was not short-circuited by a yield that had not happened.
	if _, ingestErr := ingester.IngestChanged(context.Background()); ingestErr == nil {
		t.Fatal("a serving binding short-circuited instead of consulting the coordinator")
	}

	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	state.Observe(sessionYieldFacts{At: start, AdmissionAllowed: false})
	state.Observe(sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false})
	if !state.Yielded() {
		t.Fatal("setup did not withdraw")
	}

	changed, yieldErr := ingester.IngestChanged(context.Background())
	if yieldErr != nil {
		t.Fatalf("a withdrawn binding reported a failure: %v", yieldErr)
	}
	if changed {
		t.Fatal("a withdrawn binding reported new demand")
	}
	if source.polled {
		t.Fatal("a withdrawn binding polled a session it had released")
	}
	snapshot := health.Snapshot()
	observation, ok := snapshot.Observations["github-7"]
	if !ok {
		t.Fatal("withdrawn binding published no observation at all")
	}
	if observation.Freshness != telemetry.ObservationStale {
		t.Fatalf("withdrawn binding freshness = %q, want stale: it observed nothing, and nothing failed", observation.Freshness)
	}
	if observation.Detail != "session_yielded" {
		t.Fatalf("withdrawn binding detail = %q, want session_yielded", observation.Detail)
	}
}

func TestRecoveringSourceSuspendReleasesAndResumeReopens(t *testing.T) {
	opened := 0
	first := &wedgedSessionSource{}
	second := &wedgedSessionSource{}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: first,
		open: func(context.Context) (scaleSetSource, error) {
			opened++
			return second, nil
		},
		limiter: make(chan struct{}, 1), now: time.Now})
	if err != nil {
		t.Fatalf("newRecoveringScaleSetSource: %v", err)
	}
	if source.Suspended() {
		t.Fatal("a fresh source reported itself withdrawn")
	}
	if err := source.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if first.closed.Load() == 0 {
		t.Fatal("suspending never released the session GitHub binds jobs to")
	}
	if !source.Suspended() {
		t.Fatal("a suspended source did not report itself withdrawn")
	}
	// Suspending twice must not close a session this node no longer holds.
	if err := source.Suspend(context.Background()); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}
	if closes := first.closed.Load(); closes != 1 {
		t.Fatalf("released the same session %d times", closes)
	}
	if err := source.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if opened != 1 || source.Suspended() {
		t.Fatalf("rejoining opened %d sessions and left suspended=%v", opened, source.Suspended())
	}
	if err := source.Resume(context.Background()); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if opened != 1 {
		t.Fatalf("resuming a serving source opened another session: %d", opened)
	}
}

func TestRecoveringSourceKeepsTheSessionWhenTheBrokerRefusesTheRelease(t *testing.T) {
	refusing := &wedgedSessionSource{closeErr: errors.New("broker refused")}
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: refusing,
		open:    func(context.Context) (scaleSetSource, error) { return &wedgedSessionSource{}, nil },
		limiter: make(chan struct{}, 1), now: time.Now})
	if err != nil {
		t.Fatalf("newRecoveringScaleSetSource: %v", err)
	}
	if err := source.Suspend(context.Background()); err == nil {
		t.Fatal("a refused release was reported as a withdrawal")
	}
	if source.Suspended() {
		t.Fatal("a session the broker still holds was recorded as released")
	}
}

func TestSessionYieldControllerIsInertWithoutSourcesOrState(t *testing.T) {
	if controller := newSessionYieldController(nil, nil, nil); controller != nil {
		t.Fatal("a controller was built without a policy")
	}
	if controller := newSessionYieldController(newSessionYieldState(yieldPolicy()), nil, nil); controller != nil {
		t.Fatal("a node owning no sessions was given something to withdraw")
	}
	var absent *sessionYieldController
	if absent.Apply(context.Background(), sessionYieldFacts{}, "") {
		t.Fatal("a nil controller claimed the node was withdrawn")
	}
	if total, withdrawn := absent.Bindings(); total != 0 || withdrawn != 0 {
		t.Fatalf("a nil controller counted %d/%d bindings", withdrawn, total)
	}
	if !absent.Since().IsZero() {
		t.Fatal("a nil controller dated a withdrawal")
	}
	var absentState *sessionYieldState
	if absentState.Observe(sessionYieldFacts{}) != sessionYieldNone {
		t.Fatal("a nil policy acted")
	}
	if absentState.Yielded() {
		t.Fatal("a nil policy reported a withdrawal")
	}
	if got := sessionYieldNone.String(); got != "none" {
		t.Fatalf("sessionYieldNone renders as %q", got)
	}
}

func TestApplySessionYieldPublishesThePostureAndNamesTheCause(t *testing.T) {
	reporter, logged := silenceReporter()
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	source := &fakeYieldSource{}
	state := newSessionYieldState(yieldPolicy())
	ticker := engineTicker{health: health, reporter: reporter,
		yield: &sessionYieldController{state: state, sources: []yieldableSource{source},
			report: func(action sessionYieldAction, reason string, failures int) {
				reporter.reportSessionYield(action.String(), reason, failures)
			}}}

	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	blocked := func(at time.Time) app.TickResult {
		return app.TickResult{At: at, Host: domain.Host{
			Pressure: domain.HostPressure{AdmissionAllowed: false, AdmissionReason: "disk reserve"}}}
	}
	ticker.applySessionYield(context.Background(), blocked(start), 0)
	if metric := health.Snapshot().SessionYield; metric == nil || metric.Yielded {
		t.Fatal("published a withdrawal before the policy reached its bound")
	}
	ticker.applySessionYield(context.Background(), blocked(start.Add(10*time.Minute)), 0)
	metric := health.Snapshot().SessionYield
	if metric == nil || !metric.Yielded {
		t.Fatal("a withdrawn node published no withdrawal")
	}
	if metric.Reason != "disk reserve" || metric.Bindings != 1 || metric.Withdrawn != 1 {
		t.Fatalf("published posture %+v does not describe the withdrawal", metric)
	}
	if metric.Since.IsZero() {
		t.Fatal("a withdrawal with no date cannot be aged by an operator")
	}
	if line := logged.String(); !strings.Contains(line, "scale-set sessions withdraw") || !strings.Contains(line, "disk reserve") {
		t.Fatalf("the transition was not logged with its cause: %q", line)
	}

	// A live instance holds the sessions open regardless of admission.
	running := blocked(start.Add(30 * time.Minute))
	ticker.applySessionYield(context.Background(), running, 0)
	if health.Snapshot().SessionYield.Yielded != true {
		t.Fatal("a withdrawn node rejoined without sustained health")
	}
}

func TestApplySessionYieldCountsOnlyLiveInstances(t *testing.T) {
	health, err := telemetry.NewHealth(wallClock{}, telemetry.HealthConfig{Profiles: []string{"small"}})
	if err != nil {
		t.Fatalf("NewHealth: %v", err)
	}
	source := &fakeYieldSource{}
	ticker := engineTicker{health: health,
		yield: &sessionYieldController{state: newSessionYieldState(yieldPolicy()), sources: []yieldableSource{source}}}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	live := domain.Instance{ID: "trf-small-1", Profile: "small", State: domain.InstanceRunning}
	result := func(at time.Time) app.TickResult {
		return app.TickResult{At: at, Instances: []domain.Instance{live},
			Host: domain.Host{Pressure: domain.HostPressure{AdmissionReason: "disk reserve"}}}
	}
	for minute := 0; minute <= 30; minute++ {
		ticker.applySessionYield(context.Background(), result(start.Add(time.Duration(minute)*time.Minute)), 0)
	}
	if source.Suspended() {
		t.Fatal("withdrew the session a live instance still reports completion through")
	}

	// A ticker without a controller is every mode that owns no sessions.
	quiet := engineTicker{health: health}
	quiet.applySessionYield(context.Background(), result(start), 0)
}

func TestRecoveringSourceRejoinFailureLeavesTheNodeWithdrawn(t *testing.T) {
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &wedgedSessionSource{},
		open:    func(context.Context) (scaleSetSource, error) { return nil, errors.New("broker unavailable") },
		limiter: make(chan struct{}, 1), now: time.Now})
	if err != nil {
		t.Fatalf("newRecoveringScaleSetSource: %v", err)
	}
	if err := source.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	// A rejoin whose open failed must not claim to be serving: the controller
	// retries on the next tick, and a source that lied here would never be
	// retried at all.
	if err := source.Resume(context.Background()); err == nil {
		t.Fatal("a failed open was reported as a rejoin")
	}
	if !source.Suspended() {
		t.Fatal("a source with no session reported itself serving")
	}
	// A closed source neither suspends nor resumes: it is over, not withdrawn.
	if err := source.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := source.Resume(context.Background()); err != nil {
		t.Fatalf("Resume after Close: %v", err)
	}
	if err := source.Suspend(context.Background()); err != nil {
		t.Fatalf("Suspend after Close: %v", err)
	}
}

func TestSessionYieldReportingToleratesAnAbsentLoggerAndReason(t *testing.T) {
	var absent *failureReporter
	absent.reportSessionYield("withdraw", "disk reserve", 0)
	(&failureReporter{}).reportSessionYield("withdraw", "disk reserve", 0)

	reporter, logged := silenceReporter()
	reporter.reportSessionYield("rejoin", "", 0)
	if line := logged.String(); !strings.Contains(line, "admission refused") {
		t.Fatalf("a reasonless transition logged as %q", line)
	}
}

func TestSessionYieldControllerSkipsWhatItCannotWithdraw(t *testing.T) {
	// A source that cannot suspend is not counted as a binding this node could
	// have withdrawn, and a nil entry is not counted at all.
	if controller := newSessionYieldController(newSessionYieldState(yieldPolicy()),
		[]scaleSetSource{&fakeSource{}}, nil); controller != nil {
		t.Fatal("a source that cannot be withdrawn was given to the controller")
	}
	controller := &sessionYieldController{state: newSessionYieldState(yieldPolicy()),
		sources: []yieldableSource{nil, &fakeYieldSource{}}}
	total, withdrawn := controller.Bindings()
	if total != 1 || withdrawn != 0 {
		t.Fatalf("counted %d/%d bindings across a nil entry", withdrawn, total)
	}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	controller.Apply(context.Background(), sessionYieldFacts{At: start}, "")
	controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(10 * time.Minute)}, "")
	if _, withdrawn = controller.Bindings(); withdrawn != 1 {
		t.Fatal("the withdrawable binding was skipped along with the nil one")
	}
}

func TestConstructedControllerLogsThroughTheReporterItWasGiven(t *testing.T) {
	source, err := newRecoveringScaleSetSource(recoveringScaleSetConfig{source: &wedgedSessionSource{},
		open:    func(context.Context) (scaleSetSource, error) { return &wedgedSessionSource{}, nil },
		limiter: make(chan struct{}, 1), now: time.Now})
	if err != nil {
		t.Fatalf("newRecoveringScaleSetSource: %v", err)
	}
	reporter, logged := silenceReporter()
	controller := newSessionYieldController(newSessionYieldState(yieldPolicy()), []scaleSetSource{source}, reporter)
	if controller == nil {
		t.Fatal("a node owning a real session got no controller")
	}
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	controller.Apply(context.Background(), sessionYieldFacts{At: start, AdmissionAllowed: false}, "load average")
	controller.Apply(context.Background(), sessionYieldFacts{At: start.Add(10 * time.Minute), AdmissionAllowed: false}, "load average")
	if !source.Suspended() {
		t.Fatal("the constructed controller never withdrew the session")
	}
	if line := logged.String(); !strings.Contains(line, "load average") {
		t.Fatalf("the constructed controller logged nothing an operator can act on: %q", line)
	}
}
