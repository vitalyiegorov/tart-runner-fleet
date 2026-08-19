package scheduler

import (
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
)

// The 2026-08-16 production incident (issue #236), reproduced host-side on a
// dedicated probe VM before any of this was written.
//
// A job's `--privileged` redroid container could not open `/dev/binder`, Android
// init shut the "device" down, and five minutes later init's reboot watchdog
// escalated through the guest's `/proc/sysrq-trigger` to `c` — a deliberate
// kernel crash. `kernel.panic` was 0, so the panicked kernel hung forever. From
// the host: `tart list` still said `running`, the `tart run` process idled at
// 0.0% CPU, the runner agent's TCP connection was never closed, and `tart exec`
// was refused within seconds. Eight production runners died this way, each
// holding its whole vector until GitHub's grace timer failed the job sixteen to
// eighteen minutes later, and not one produced a single daemon log line.
//
// Nothing in `assignmentRecoveries` could express it: the power was `running`,
// GitHub reported the job `in_progress`, and every existing cause is a statement
// about whether work is happening rather than about whether the guest is alive.
//
// These tests pin the missing cause in both directions. The one that matters
// most is the second: a guest under legitimate heavy load must never be probed
// into a drain.

// guestProfiles is the incident's shape: the 6 CPU / 12288 MiB `trf-xl` profile
// the eight dead runners were spawned on.
func guestProfiles() map[domain.ProfileID]domain.Profile {
	profiles := testConfig().Profiles
	profiles["xl"] = domain.Profile{ID: "xl", Platform: domain.PlatformLinux, Route: "tiered",
		Resources: domain.Resources{CPU: 6, MemoryMB: 12_288, Slots: 1}}
	return profiles
}

// silentGuest is the incident's instance: powered on, running, bound to a job
// GitHub still calls in_progress, and refusing every probe since `silence` ago.
func silentGuest(refusals int, silence time.Duration) domain.Instance {
	config := guestProfiles()["xl"]
	instance := domain.Instance{
		ID: "trf-xl-0aacdbcc6653bd8a", Repo: "rnw-community/rnw-community",
		Demand:    domain.DemandKey{Repo: "rnw-community/rnw-community", RunID: 31_939_037_119, Attempt: 1, JobID: 93_540_000_001},
		Platform:  config.Platform,
		Profile:   "xl",
		Route:     config.Route,
		Resources: config.Resources,
		State:     domain.InstanceRunning,
		Power:     domain.InstancePowerRunning,
		// GitHub reported the job in_progress the whole time, so the
		// lingering-runner gate cannot fire. This is a "busy" runner.
		JobInactive:   false,
		RunningSince:  testNow.Add(-20 * time.Minute),
		OccupiedSince: testNow.Add(-20 * time.Minute),
	}
	if refusals > 0 {
		instance.Guest = domain.GuestLivenessState{Refusals: refusals, RefusedSince: testNow.Add(-silence),
			LastAlive: testNow.Add(-silence - time.Minute), LastProbe: testNow}
	}
	return instance
}

func guestLivenessInput(instances []domain.Instance, demands []domain.Demand, policy domain.GuestLivenessPolicy) Input {
	in := input(demands, instances, State{})
	in.Config.Profiles = guestProfiles()
	in.Config.GuestLiveness = policy
	return in
}

func guestPolicy() domain.GuestLivenessPolicy {
	return domain.GuestLivenessPolicy{ConsecutiveRefusals: 5, Window: 90 * time.Second}
}

func TestADeadGuestReleasesItsVectorWithoutWaitingForGitHub(t *testing.T) {
	dead := silentGuest(5, 2*time.Minute)
	// The queue the eight dead runners starved: other repositories' work that
	// fits inside the vector a panicked kernel is holding.
	queued := []domain.Demand{demand("sudoku-repo/builder", 2, 30*time.Minute, "medium")}

	plan := PlanTick(guestLivenessInput([]domain.Instance{dead}, queued, guestPolicy()))

	operation, ok := drainOf(plan, dead.ID)
	if !ok {
		t.Fatalf("a running instance whose guest has refused %d consecutive probes over %s must be drained; plan operations: %#v",
			dead.Guest.Refusals, 2*time.Minute, plan.Operations)
	}
	if !operation.Recovery || !operation.GuestUnresponsive {
		t.Fatalf("the drain must name the unresponsive guest as its cause; got %#v", operation)
	}
	if operation.Demand != dead.Demand {
		t.Fatalf("the drain must name the job that died with the guest (%v); got %v", dead.Demand, operation.Demand)
	}
	// The cause is part of the content-addressed identity, so a guest-death drain
	// and a lingering-runner drain of the same instance are distinct attempts.
	lingering := Operation{Kind: OperationDrain, Instance: dead.ID, Profile: dead.Profile, Route: dead.Route,
		Demand: dead.Demand, Recovery: true, LingeringRunner: true}
	if operation.ID == stableID("op", lingering) {
		t.Fatalf("the guest-liveness cause must change the operation identity; got %s", operation.ID)
	}
}

// The design risk of this whole mechanism, pinned. A guest running a monorepo
// Gradle build at full tilt is slow; a probe that times out against it, or one
// that succeeds after several seconds, establishes nothing. Neither may ever
// accumulate toward a verdict, at any number of ticks and any elapsed time.
func TestASaturatedButAliveGuestIsNeverProbedIntoADrain(t *testing.T) {
	for _, test := range []struct {
		name  string
		guest domain.GuestLivenessState
	}{
		{name: "answers every probe, slowly", guest: domain.GuestLivenessState{
			LastAlive: testNow, LastProbe: testNow}},
		{name: "inconclusive for an hour", guest: domain.GuestLivenessState{
			LastAlive: testNow.Add(-time.Hour), LastProbe: testNow}},
		{name: "one refusal between answers", guest: domain.GuestLivenessState{
			Refusals: 1, RefusedSince: testNow.Add(-time.Hour), LastAlive: testNow.Add(-time.Hour), LastProbe: testNow}},
		{name: "one short of the count, far past the window", guest: domain.GuestLivenessState{
			Refusals: 4, RefusedSince: testNow.Add(-time.Hour), LastAlive: testNow.Add(-time.Hour), LastProbe: testNow}},
		{name: "past the count, inside the window", guest: domain.GuestLivenessState{
			Refusals: 9, RefusedSince: testNow.Add(-89 * time.Second), LastAlive: testNow.Add(-2 * time.Minute), LastProbe: testNow}},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := silentGuest(0, 0)
			instance.Guest = test.guest
			plan := PlanTick(guestLivenessInput([]domain.Instance{instance},
				[]domain.Demand{demand("b/repo", 3, time.Hour, "medium")}, guestPolicy()))
			if _, drained := drainOf(plan, instance.ID); drained {
				t.Fatalf("a guest that has not hard-failed both bounds must not be drained; plan operations: %#v", plan.Operations)
			}
		})
	}
}

// TestGuestLivenessIsFailClosedWithoutABoundOrEvidence covers every input to the
// bound that establishes nothing. Power is deliberately not among them any more:
// a reading the backend could not take neither authorizes this verdict nor
// disables it, which is the reversal the test at the bottom of this file states.
func TestGuestLivenessIsFailClosedWithoutABoundOrEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy domain.GuestLivenessPolicy
		mutate func(*domain.Instance)
	}{
		{name: "no configured bound", policy: domain.GuestLivenessPolicy{}, mutate: func(*domain.Instance) {}},
		{name: "count stated without a window", policy: domain.GuestLivenessPolicy{ConsecutiveRefusals: 5},
			mutate: func(*domain.Instance) {}},
		{name: "window stated without a count", policy: domain.GuestLivenessPolicy{Window: 90 * time.Second},
			mutate: func(*domain.Instance) {}},
		{name: "unknown silence start", policy: guestPolicy(),
			mutate: func(i *domain.Instance) { i.Guest.RefusedSince = time.Time{} }},
		{name: "silence start in the future", policy: guestPolicy(),
			mutate: func(i *domain.Instance) { i.Guest.RefusedSince = testNow.Add(time.Hour) }},
		{name: "still only assigned", policy: guestPolicy(),
			mutate: func(i *domain.Instance) { i.State = domain.InstanceAssigned }},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := silentGuest(5, 2*time.Minute)
			test.mutate(&instance)
			plan := PlanTick(guestLivenessInput([]domain.Instance{instance}, nil, test.policy))
			if _, drained := drainOf(plan, instance.ID); drained {
				t.Fatalf("%s must not authorize a reclaim; plan operations: %#v", test.name, plan.Operations)
			}
		})
	}
}

// Every safer cause acts first. A dead guest whose runner is also provably idle
// is reclaimed as an idle runner, because that path re-verifies its premise at
// execution time and this one cannot.
func TestASaferReclaimCauseOutranksADeadGuest(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*domain.Instance)
		want   func(Operation) bool
	}{
		{name: "confirmed inactive", want: func(o Operation) bool { return o.ConfirmedInactive },
			mutate: func(i *domain.Instance) { i.RecoveryReady = true }},
		{name: "lingering runner", want: func(o Operation) bool { return o.LingeringRunner },
			mutate: func(i *domain.Instance) { i.JobInactive = true; i.RunningSince = testNow.Add(-time.Hour) }},
		{name: "stopped power", want: func(o Operation) bool { return !o.GuestUnresponsive },
			mutate: func(i *domain.Instance) { i.Power = domain.InstancePowerStopped }},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := silentGuest(5, 2*time.Minute)
			test.mutate(&instance)
			plan := PlanTick(guestLivenessInput([]domain.Instance{instance}, nil, guestPolicy()))
			operation, drained := drainOf(plan, instance.ID)
			if !drained {
				t.Fatalf("the instance must still be reclaimed; plan operations: %#v", plan.Operations)
			}
			if operation.GuestUnresponsive || !test.want(operation) {
				t.Fatalf("%s must act instead of the guest-liveness cause; got %#v", test.name, operation)
			}
		})
	}
}

// The occupancy budget is the one cause the guest-liveness verdict outranks. Both
// end a job GitHub still believes is running, but a budget breach claims only
// that a hold is long, while a refused transport is fresh evidence that the guest
// executing that job is gone.
func TestADeadGuestOutranksAMerelyLongHold(t *testing.T) {
	instance := silentGuest(5, 2*time.Minute)
	instance.OccupiedSince = testNow.Add(-3 * time.Hour)
	in := guestLivenessInput([]domain.Instance{instance}, nil, guestPolicy())
	profiles := guestProfiles()
	profile := profiles["xl"]
	profile.OccupancyBudget = time.Hour
	profiles["xl"] = profile
	in.Config.Profiles = profiles

	operation, drained := drainOf(PlanTick(in), instance.ID)
	if !drained || !operation.GuestUnresponsive || operation.OccupancyExceeded {
		t.Fatalf("a dead guest must be reclaimed as a dead guest, not as a long hold; got %#v, drained=%v", operation, drained)
	}
}

// The evidence half of issue #236: eight runners died and no daemon log line
// recorded any of it. The projection is what every surface — the warning, the
// metric, the doctor finding — reads, so they cannot disagree about a silence.
func TestGuestSilencesNameTheInstanceTheJobAndTheTimeline(t *testing.T) {
	dead := silentGuest(5, 2*time.Minute)
	partial := silentGuest(2, 40*time.Second)
	partial.ID = "trf-xl-partial"
	healthy := silentGuest(0, 0)
	healthy.ID = "trf-xl-healthy"
	healthy.Guest = domain.GuestLivenessState{LastAlive: testNow, LastProbe: testNow}

	in := guestLivenessInput([]domain.Instance{dead, partial, healthy}, nil, guestPolicy())
	reports := GuestSilences(in.Now, in.Config, in.Instances.Value)

	byID := map[string]GuestSilence{}
	for _, report := range reports {
		byID[report.Instance] = report
	}
	if len(byID) != 2 {
		t.Fatalf("only instances with an unbroken run of refusals are silent; got %#v", reports)
	}
	report := byID[dead.ID]
	if !report.Unresponsive || report.Refusals != 5 || report.Silence != 2*time.Minute ||
		report.Demand != dead.Demand || report.Repo != dead.Repo || report.Resources != dead.Resources {
		t.Fatalf("the declared-dead guest must be reported with its job, vector, and probe timeline; got %#v", report)
	}
	if report.RequiredRefusals != 5 || report.Window != 90*time.Second {
		t.Fatalf("the report must carry the bound it was judged against; got %#v", report)
	}
	if report := byID[partial.ID]; report.Unresponsive || report.Refusals != 2 || report.Silence != 40*time.Second {
		t.Fatalf("a guest inside both bounds must be reported without a verdict; got %#v", report)
	}
	if _, reported := byID[healthy.ID]; reported {
		t.Fatalf("a guest that answered its last probe is not silent at all; got %#v", reports)
	}
}

// A silence the fleet cannot judge must not be published as a judged one, and a
// disabled policy judges nothing.
func TestGuestSilencesPublishNoVerdictWithoutABound(t *testing.T) {
	dead := silentGuest(9, time.Hour)
	in := guestLivenessInput([]domain.Instance{dead}, nil, domain.GuestLivenessPolicy{})
	reports := GuestSilences(in.Now, in.Config, in.Instances.Value)
	if len(reports) != 1 || reports[0].Unresponsive || reports[0].RequiredRefusals != 0 {
		t.Fatalf("an unconfigured bound reports the silence and no verdict; got %#v", reports)
	}

	// A future start is not a measurable silence, and a deleted instance is not
	// holding anything, so neither is reported at all.
	future := silentGuest(9, 0)
	future.Guest.RefusedSince = testNow.Add(time.Hour)
	gone := silentGuest(9, time.Hour)
	gone.ID, gone.State, gone.Power = "trf-xl-gone", domain.InstanceDeleted, domain.InstancePowerAbsent
	in = guestLivenessInput([]domain.Instance{future, gone}, nil, guestPolicy())
	if reports := GuestSilences(in.Now, in.Config, in.Instances.Value); len(reports) != 0 {
		t.Fatalf("an unmeasurable or released silence must not be reported at all; got %#v", reports)
	}
}

// TestAnUnreadablePowerStateDoesNotDisableTheGuestVerdict is issue #252's second
// half, and it reverses a case this file used to assert.
//
// The verdict was narrowed to a powered-on VM because "a stopped or absent
// instance is reclaimed by a cheaper gate". With three-valued power that
// reasoning stops holding: an instance whose power the backend could not read has
// NO cheaper gate — the stopped recovery may not act on an unread premise, by
// construction — so keeping the verdict closed made an unreadable enumeration a
// way to hold a whole vector, with a dead guest on it, forever. The sweep found
// exactly that the first time `unreadable_power` was drawn: property (p),
// twenty-five ticks past a twenty-four tick release bound, on several seeds.
//
// The reclaim is not resting on the unread power. It rests on five refused guest
// probes over ninety seconds — an INDEPENDENT source, and on this fleet the only
// fast probe failure is `VM "…" is not running` (ADR 0042's measurement), so the
// verdict corroborates from the guest exactly the fact the enumeration could not
// read. Absence of evidence still authorizes nothing; evidence from somewhere
// else does.
func TestAnUnreadablePowerStateDoesNotDisableTheGuestVerdict(t *testing.T) {
	instance := silentGuest(5, 2*time.Minute)
	instance.Power = domain.InstancePowerUnknown
	plan := PlanTick(guestLivenessInput([]domain.Instance{instance}, nil, guestPolicy()))
	operation, drained := drainOf(plan, instance.ID)
	if !drained || !operation.GuestUnresponsive {
		t.Fatalf("a dead guest on an unreadable VM was never reclaimed; plan operations: %#v", plan.Operations)
	}
}
