package simulation_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/membership"
)

// ---------------------------------------------------------------------------
// ADR 0063 -- an absent node's work is adopted, a failing node excludes itself
// ---------------------------------------------------------------------------
//
// Every other world in this package is ONE node, because ADR 0034 made one node
// one whole fleet and the properties checked there have no cross-node half.
// ADR 0063 is the first rule whose subject is the space BETWEEN nodes -- whose
// session a scale set has, and who may run the work bound to it -- so this file
// models exactly that and nothing else: GitHub's session arbiter, the jobs bound
// to one owner's scale set, the owner, and the siblings that advertise the same
// labels. Admission, Tart, and the durable store are not modelled here; the
// worlds beside this one already check them per node.
//
// The world model's one assumption is stated as a parameter rather than
// hidden: how long GitHub keeps a session alive after its listener stopped
// polling (`expiry`). ADR 0063's spike could not settle it from code, so every
// scenario is swept across several values and the liveness bound is written in
// terms of it.
//
// The 2026-10-16 incident is scenario `owner vanishes with bound queued jobs`:
// the MacBook's knee-doctor sets held two jobs (assigned 1, busy 1, registered
// 0) for two hours while Omarchy and the mini advertised the same labels.

const (
	membershipTick    = time.Minute
	membershipCadence = 15 * time.Minute
	membershipHorizon = 10 * time.Hour
	membershipSeeds   = 16
	// runnerLost is how long GitHub takes to fail a job whose runner stopped
	// answering. The fleet does not control it; it only has to never restart
	// such a job itself.
	runnerLost = 10 * time.Minute
)

var (
	ownedSet    = membership.Key{Scope: "knee", ID: 19}
	modelEpoch  = time.Date(2026, 10, 16, 6, 0, 0, 0, time.UTC)
	modelExpiry = []time.Duration{2 * time.Minute, 15 * time.Minute, time.Hour}
)

// arbiterModel is GitHub's half: one session per set, granted to whoever asks
// when no live session holds it, and expired once its holder stops polling.
type arbiterModel struct {
	expiry      time.Duration
	holder      string
	polled      time.Time
	unavailable bool
}

func (a *arbiterModel) live(now time.Time) bool {
	return a.holder != "" && now.Sub(a.polled) < a.expiry
}

func (a *arbiterModel) create(node string, now time.Time) membership.Arbiter {
	if a.unavailable {
		return membership.Unanswered
	}
	if a.live(now) && a.holder != node {
		return membership.Held
	}
	a.holder, a.polled = node, now
	return membership.Obtained
}

// poll is a long poll on a session the node believes it holds; false is the
// 404 of a session GitHub expired or another node now holds.
func (a *arbiterModel) poll(node string, now time.Time) bool {
	if a.holder != node || !a.live(now) {
		return false
	}
	a.polled = now
	return true
}

func (a *arbiterModel) release(node string) {
	if a.holder == node {
		a.holder = ""
	}
}

type modelJob struct {
	id        int
	duration  time.Duration
	runner    string
	startedAt time.Time
	done      bool
	lost      bool
	starts    int
}

type modelNode struct {
	name     string
	present  bool
	wedged   bool
	holding  bool
	capacity int
	running  []*modelJob
	// owner-side
	ladder map[membership.Key]membership.Binding
	out    bool
	// sibling-side
	auditOffset   time.Duration
	auditedAt     time.Time
	strandedSince time.Time
	adoption      *membership.Adoption
	held, asked   int
	unanswered    int
}

type membershipScenario struct {
	name     string
	siblings int
	jobs     int
	// absent reports whether the owner is away at `elapsed`.
	absent func(elapsed time.Duration) bool
	// ownerStartsFirstJob runs job 0 on the owner before it vanishes.
	ownerStartsFirstJob bool
	wedged              bool
	watchdog            membership.Mode
	auditsUnavailable   bool
	raceAligned         bool
	check               func(t *testing.T, w *membershipWorld)
}

type membershipWorld struct {
	arb      *arbiterModel
	owner    *modelNode
	siblings []*modelNode
	jobs     []*modelJob
	adopted  int
	// ownerHeldWhileAdopted counts owner session attempts refused because a
	// sibling held the set: the returning-owner path.
	ownerHeldWhileAdopted int
	lastJobDone           time.Duration
}

func newMembershipWorld(s membershipScenario, expiry time.Duration, rng *rand.Rand) *membershipWorld {
	w := &membershipWorld{arb: &arbiterModel{expiry: expiry},
		owner: &modelNode{name: "macbook", capacity: 1, ladder: map[membership.Key]membership.Binding{}}}
	for i := range s.jobs {
		w.jobs = append(w.jobs, &modelJob{id: i, duration: time.Duration(5+rng.Intn(30)) * time.Minute})
	}
	for i := range s.siblings {
		offset := time.Duration(rng.Intn(int(membershipCadence/time.Minute))) * time.Minute
		if s.raceAligned {
			offset = 0
		}
		w.siblings = append(w.siblings, &modelNode{name: fmt.Sprintf("sibling-%d", i), present: true,
			capacity: 1 + rng.Intn(2), auditOffset: offset})
	}
	return w
}

// sighting is what a sibling's audit reads from GitHub's counters for the set.
func (w *membershipWorld) sighting(now time.Time) membership.Sighting {
	s := membership.Sighting{Key: ownedSet, Equivalent: true, AuditedAt: now}
	for _, job := range w.jobs {
		if job.done {
			continue
		}
		s.Assigned++
		s.Busy++
		if job.runner != "" {
			s.Registered++
		}
	}
	return s
}

func (w *membershipWorld) nextUnstarted() *modelJob {
	for _, job := range w.jobs {
		if job.runner == "" && !job.done {
			return job
		}
	}
	return nil
}

func (w *membershipWorld) start(t *testing.T, node *modelNode, job *modelJob, now time.Time) {
	t.Helper()
	if w.arb.holder != node.name || !w.arb.live(now) {
		t.Fatalf("%s started job %d without holding the set's session (holder %q)", node.name, job.id, w.arb.holder)
	}
	if len(node.running) >= node.capacity {
		t.Fatalf("%s started job %d beyond its own capacity %d", node.name, job.id, node.capacity)
	}
	job.runner, job.startedAt = node.name, now
	job.starts++
	node.running = append(node.running, job)
}

// advanceJobs completes the work of present nodes and lets GitHub fail the
// work of a runner that went dark.
func (w *membershipWorld) advanceJobs(node *modelNode, now time.Time, elapsed time.Duration) {
	kept := node.running[:0]
	for _, job := range node.running {
		switch {
		case node.present && now.Sub(job.startedAt) >= job.duration:
			job.done = true
			w.lastJobDone = elapsed
		case !node.present && now.Sub(job.startedAt) >= runnerLost:
			job.done, job.lost = true, true
		default:
			kept = append(kept, job)
			continue
		}
	}
	node.running = kept
}

func (w *membershipWorld) ownerTick(t *testing.T, s membershipScenario, now time.Time, elapsed time.Duration) {
	o := w.owner
	wasPresent := o.present
	o.present = !s.absent(elapsed)
	w.advanceJobs(o, now, elapsed)
	if !o.present {
		return
	}
	if !wasPresent && elapsed > 0 {
		// A node that wakes has no idea how long it slept; its next poll tells it.
		o.holding = o.holding && w.arb.poll(o.name, now)
	}
	if o.holding && !w.arb.poll(o.name, now) {
		o.holding = false
	}
	if s.watchdog.Enabled() && elapsed%membershipCadence == 0 {
		w.ownerWatchdog(o, s, now)
	}
	if !o.holding && !o.out {
		switch w.arb.create(o.name, now) {
		case membership.Obtained:
			o.holding = true
		case membership.Held:
			// ADR 0063 (d): a held session at startup or on rejoin is not fatal;
			// the binding stays pending and is retried on the next poll.
			if w.adoptionActive() {
				w.ownerHeldWhileAdopted++
			}
		}
	}
	if o.holding && !o.wedged {
		for len(o.running) < o.capacity {
			job := w.nextUnstarted()
			if job == nil {
				break
			}
			w.start(t, o, job, now)
		}
	}
}

// ownerWatchdog is rung (a) and (b): the owner's own audit reads its set
// wedged when GitHub holds work for it that it is not running.
func (w *membershipWorld) ownerWatchdog(o *modelNode, s membershipScenario, now time.Time) {
	var wedged []membership.Key
	if sight := w.sighting(now); sight.Assigned > 0 && sight.Registered == 0 && len(o.running) == 0 {
		wedged = []membership.Key{ownedSet}
	}
	policy := membership.Policy{Mode: s.watchdog, Cadence: membershipCadence}
	var actions map[membership.Key]membership.Action
	o.ladder, actions = membership.Step(policy, o.ladder,
		membership.Evidence{At: now, AuditedAt: now, Wedged: wedged, Bound: []membership.Key{ownedSet}})
	switch actions[ownedSet] {
	case membership.Heal:
		// Recycling the session cannot unwedge a wedged node in this model;
		// it must cost nothing and change nothing.
		w.arb.release(o.name)
		o.holding = w.arb.create(o.name, now) == membership.Obtained
	case membership.Exclude:
		w.arb.release(o.name)
		o.holding, o.out = false, true
	case membership.Rejoin:
		o.out = false
	}
}

func (w *membershipWorld) adoptionActive() bool {
	for _, sib := range w.siblings {
		if sib.adoption != nil {
			return true
		}
	}
	return false
}

func (w *membershipWorld) siblingTick(t *testing.T, s membershipScenario, sib *modelNode, now time.Time, elapsed time.Duration) {
	w.advanceJobs(sib, now, elapsed)
	policy := membership.AdoptionPolicy{Cadence: membershipCadence}
	// An unavailable audit lets the sibling see the set stranded once and then
	// never again: the shape in which a stale reading could be mistaken for a
	// standing one.
	auditable := !s.auditsUnavailable || elapsed < membershipCadence+time.Minute
	if (elapsed-sib.auditOffset)%membershipCadence == 0 && auditable {
		sight := w.sighting(now)
		if sight.Assigned > 0 && sight.Registered == 0 {
			if sib.strandedSince.IsZero() {
				sib.strandedSince = now
			}
		} else {
			sib.strandedSince = time.Time{}
		}
		sib.auditedAt = now
	}
	if sib.adoption != nil {
		if !w.arb.poll(sib.name, now) {
			t.Fatalf("%s lost an adopted session it never released", sib.name)
		}
		for len(sib.running) < sib.capacity {
			job := w.nextUnstarted()
			if job == nil {
				break
			}
			w.start(t, sib, job, now)
		}
		if sib.adoption.Release(now, w.nextUnstarted() == nil && len(sib.running) == 0) {
			w.arb.release(sib.name)
			sib.adoption = nil
		}
		return
	}
	sight := w.sighting(sib.auditedAt)
	sight.StrandedSince = sib.strandedSince
	if !policy.MayAsk(sight, now, len(sib.running) < sib.capacity) {
		return
	}
	if s.auditsUnavailable {
		t.Fatalf("%s asked for a session on one audit followed by silence", sib.name)
	}
	sib.asked++
	answer := w.arb.create(sib.name, now)
	if adoption, ok := policy.Arbitrate(ownedSet, answer, now); ok {
		sib.adoption = &adoption
		w.adopted++
		return
	}
	if answer == membership.Held {
		sib.held++
	} else {
		sib.unanswered++
	}
}

// invariants are checked on every tick of every seed.
func (w *membershipWorld) invariants(t *testing.T, now time.Time) {
	t.Helper()
	believers := 0
	for _, node := range append([]*modelNode{w.owner}, w.siblings...) {
		holds := node.holding || node.adoption != nil
		if !holds || !node.present {
			continue
		}
		believers++
		if w.arb.holder != node.name {
			t.Fatalf("%s believes it holds the set's session but GitHub granted it to %q", node.name, w.arb.holder)
		}
	}
	if believers > 1 {
		t.Fatalf("%d present nodes believe they hold one set's session", believers)
	}
	for _, job := range w.jobs {
		if job.starts > 1 {
			t.Fatalf("job %d was started %d times; a bound job runs once or is GitHub's to fail", job.id, job.starts)
		}
	}
}

func runMembershipScenario(t *testing.T, s membershipScenario) {
	t.Helper()
	for _, expiry := range modelExpiry {
		for seed := range membershipSeeds {
			t.Run(fmt.Sprintf("expiry=%s/seed=%d", expiry, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(int64(seed)))
				w := newMembershipWorld(s, expiry, rng)
				w.arb.unavailable = false
				// The owner holds its session before anything happens, as a node
				// serving its sets does.
				w.owner.present = true
				w.owner.wedged = s.wedged
				w.owner.holding = w.arb.create(w.owner.name, modelEpoch) == membership.Obtained
				if s.ownerStartsFirstJob {
					w.start(t, w.owner, w.jobs[0], modelEpoch)
				}
				for elapsed := membershipTick; elapsed <= membershipHorizon; elapsed += membershipTick {
					now := modelEpoch.Add(elapsed)
					w.ownerTick(t, s, now, elapsed)
					for _, sib := range w.siblings {
						w.siblingTick(t, s, sib, now, elapsed)
					}
					w.invariants(t, now)
				}
				s.check(t, w)
			})
		}
	}
}

func awayFor(from, until time.Duration) func(time.Duration) bool {
	return func(elapsed time.Duration) bool { return elapsed >= from && elapsed < until }
}

// adoptionBound is the liveness bound on an absent owner's bound work: the
// sibling needs the grace, plus up to one audit cadence to see it start and one
// to see it pass, plus however long GitHub keeps the dead session alive -- the
// unknown the live experiment E2 must measure.
func adoptionBound(expiry time.Duration) time.Duration {
	return membership.AdoptionPolicy{Cadence: membershipCadence}.Grace() + 2*membershipCadence + expiry
}

func TestMembershipOwnerVanishesWithBoundQueuedJobs(t *testing.T) {
	t.Parallel()
	runMembershipScenario(t, membershipScenario{name: "vanish-queued", siblings: 2, jobs: 2,
		absent: awayFor(0, 6*time.Hour),
		check: func(t *testing.T, w *membershipWorld) {
			for _, job := range w.jobs {
				if !job.done || job.lost || job.runner == w.owner.name {
					t.Fatalf("job %d was not adopted and run: %+v", job.id, *job)
				}
			}
			// Every job is at most 34 minutes and each sibling has a slot.
			if limit := adoptionBound(w.arb.expiry) + 2*35*time.Minute; w.lastJobDone > limit {
				t.Fatalf("bound work finished at %s, beyond the adoption bound %s", w.lastJobDone, limit)
			}
			if w.adopted == 0 {
				t.Fatal("nothing was adopted")
			}
		}})
}

func TestMembershipOwnerVanishesMidJob(t *testing.T) {
	t.Parallel()
	runMembershipScenario(t, membershipScenario{name: "vanish-mid-job", siblings: 1, jobs: 3,
		ownerStartsFirstJob: true, absent: awayFor(time.Minute, 6*time.Hour),
		check: func(t *testing.T, w *membershipWorld) {
			if first := w.jobs[0]; !first.lost || first.starts != 1 {
				t.Fatalf("the job running on the vanished owner is GitHub's to fail, never the fleet's to restart: %+v", *first)
			}
			for _, job := range w.jobs[1:] {
				if !job.done || job.lost || job.runner != w.siblings[0].name {
					t.Fatalf("queued job %d behind the lost one was not adopted: %+v", job.id, *job)
				}
			}
		}})
}

func TestMembershipOwnerReturnsWhileAdopted(t *testing.T) {
	t.Parallel()
	runMembershipScenario(t, membershipScenario{name: "return-while-adopted", siblings: 1, jobs: 6,
		// Back after the grace and the longest expiry, so an adoption is live.
		absent: awayFor(0, 2*time.Hour),
		check: func(t *testing.T, w *membershipWorld) {
			for _, job := range w.jobs {
				if !job.done || job.lost {
					t.Fatalf("job %d did not complete: %+v", job.id, *job)
				}
			}
			if w.adopted == 0 {
				t.Fatal("the scenario never reached an adoption")
			}
			if !w.owner.holding || w.arb.holder != w.owner.name {
				t.Fatalf("the returned owner must hold its set again once the adoption is released (holder %q)", w.arb.holder)
			}
		}})
}

func TestMembershipTwoSiblingsRaceToAdopt(t *testing.T) {
	t.Parallel()
	runMembershipScenario(t, membershipScenario{name: "race", siblings: 2, jobs: 4, raceAligned: true,
		absent: awayFor(0, 8*time.Hour),
		check: func(t *testing.T, w *membershipWorld) {
			winners, losers := 0, 0
			for _, sib := range w.siblings {
				if sib.asked > sib.held {
					winners++
				}
				losers += sib.held
			}
			if winners == 0 || losers == 0 {
				t.Fatalf("aligned siblings must contend and GitHub must pick exactly one: winners=%d held=%d", winners, losers)
			}
		}})
}

func TestMembershipWedgedButAliveOwnerIsNeverAdoptedFrom(t *testing.T) {
	t.Parallel()
	// Without the owner's watchdog the 409 is the whole story: a sibling asks,
	// is refused, and never runs the owner's work. The jobs stay -- that is ADR
	// 0056's finding on the owner, not a sibling's to override.
	runMembershipScenario(t, membershipScenario{name: "wedged-off", siblings: 2, jobs: 2, wedged: true,
		absent: func(time.Duration) bool { return false },
		check: func(t *testing.T, w *membershipWorld) {
			if w.adopted != 0 {
				t.Fatalf("a sibling adopted from a live owner %d time(s)", w.adopted)
			}
			held := 0
			for _, sib := range w.siblings {
				held += sib.held
			}
			if held == 0 {
				t.Fatal("siblings never asked, so the 409 path was never exercised")
			}
		}})
}

func TestMembershipWedgedOwnerExcludesItselfAndIsThenAdoptedFrom(t *testing.T) {
	t.Parallel()
	// The whole ladder: the wedged owner heals twice, excludes the set, and the
	// sibling's next ask is granted.
	runMembershipScenario(t, membershipScenario{name: "wedged-watchdog", siblings: 1, jobs: 2, wedged: true,
		watchdog: membership.Stationary, absent: func(time.Duration) bool { return false },
		check: func(t *testing.T, w *membershipWorld) {
			if w.adopted == 0 {
				t.Fatal("the excluded set was never adopted")
			}
			for _, job := range w.jobs {
				if !job.done || job.runner == w.owner.name {
					t.Fatalf("job %d: %+v", job.id, *job)
				}
			}
		}})
}

func TestMembershipUnavailableAuditIsNeverReadAsAnAbsentOwner(t *testing.T) {
	t.Parallel()
	runMembershipScenario(t, membershipScenario{name: "audit-unavailable", siblings: 2, jobs: 2,
		auditsUnavailable: true, absent: awayFor(0, 6*time.Hour),
		check: func(t *testing.T, w *membershipWorld) {
			seen := false
			for _, sib := range w.siblings {
				if sib.asked != 0 {
					t.Fatalf("%s asked GitHub on stale evidence", sib.name)
				}
				seen = seen || !sib.strandedSince.IsZero()
			}
			if !seen {
				t.Fatal("no sibling ever saw the set stranded, so the stale path was never exercised")
			}
		}})
}

func TestMembershipArbiterThatDoesNotAnswerAdoptsNothing(t *testing.T) {
	t.Parallel()
	w := &membershipWorld{arb: &arbiterModel{expiry: time.Minute, unavailable: true}}
	if answer := w.arb.create("sibling", modelEpoch); answer != membership.Unanswered {
		t.Fatalf("an unavailable arbiter answered %q", answer)
	}
	if _, ok := (membership.AdoptionPolicy{Cadence: membershipCadence}).Arbitrate(ownedSet, membership.Unanswered, modelEpoch); ok {
		t.Fatal("an unanswered request must not adopt")
	}
}
