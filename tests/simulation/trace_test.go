package simulation_test

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// A simTrace is the complete, explicit schedule of everything the world does
// that the control plane does not decide. Generation consumes the seed; EXECUTION
// consumes only the trace, so a run is a pure function of (trace, config) and a
// failing trace can be shrunk by deleting events rather than by re-rolling dice.
type simTrace struct {
	Seed   int64
	Ticks  int
	Config string
	Events []simEvent
}

type simEvent struct {
	Tick    int
	Kind    eventKind
	Repo    string
	Profile domain.ProfileID
	Event   string
	// Count is the event's magnitude: ticks of delay, cores of tenant load, or a
	// selector resolved modulo the live population.
	Count int
}

func (e simEvent) String() string {
	out := fmt.Sprintf("t%03d %s", e.Tick, e.Kind)
	if e.Repo != "" {
		out += " repo=" + e.Repo
	}
	if e.Profile != "" {
		out += " profile=" + string(e.Profile)
	}
	if e.Event != "" {
		out += " event=" + e.Event
	}
	if e.Count != 0 {
		out += fmt.Sprintf(" n=%d", e.Count)
	}
	return out
}

type eventKind string

const (
	eventArrive          eventKind = "arrive"
	eventSilentCancel    eventKind = "silent_cancel"
	eventLoudCancel      eventKind = "loud_cancel"
	eventBrokerDelay     eventKind = "broker_delay"
	eventBrokerDuplicate eventKind = "broker_duplicate"
	eventBrokerDrop      eventKind = "broker_drop"
	eventBrokerReorder   eventKind = "broker_reorder"
	eventStatisticsGap   eventKind = "statistics_gap"
	eventRESTLag         eventKind = "rest_lag"
	eventHostTenant      eventKind = "host_tenant"
	eventHostProbeStale  eventKind = "host_probe_stale"
	eventTartUnavailable eventKind = "tart_unavailable"
	eventSlowBoot        eventKind = "slow_boot"
	// eventLongJob makes the next workflow job to start run for longer than a
	// recovery deadline, which is what a real test suite does.
	eventLongJob eventKind = "long_job"
	// eventOverrunJob is issue #223: the next workflow job to start never ends.
	//
	// It is deliberately a SECOND job-duration event rather than a larger long_job,
	// because the two model different worlds. A long_job is a legitimately long
	// suite: it is doing work, it will finish, and every deadline it outlives is a
	// deadline that must not reap it. An overrun_job has stopped making progress
	// and will hold its runner until something takes it away -- the 2026-08-09
	// incident, where an `if: always()` cleanup step waited on `adb` for
	// seventy-three minutes on a builder nobody else could use.
	//
	// Its duration is scaled to the PROFILE BUDGET rather than to a recovery
	// deadline, so even Count 1 runs past every simulated profile's ceiling and
	// nothing smaller than the occupancy reclaim can end it. That is the whole
	// point: before the budget existed no generated job could outlive any sane
	// ceiling, which is exactly why this defect class was invisible to the sweep.
	eventOverrunJob    eventKind = "overrun_job"
	eventStalledRunner eventKind = "stalled_runner"
	eventWedgedDrain   eventKind = "wedged_drain"
	// eventUnstoppableGuest is issue #233: a guest that will not power itself
	// down when the fleet asks it to, for as long as the fleet keeps asking.
	//
	// It is deliberately a THIRD drain-failure event rather than a longer
	// wedged_drain, because the two model different worlds and different steps.
	// A wedged_drain is GitHub refusing to deregister a runner it considers busy:
	// it happens BEFORE deregistration, it decays, and it is a refusal the fleet
	// must retry through. An unstoppable guest happens AFTER deregistration, when
	// the runner is gone and the job is provably over, and it NEVER decays --
	// nothing about a wedged macOS guest gets better because time passed. On
	// 2026-08-10 one of them absorbed 67 identical stop attempts over 90 minutes.
	//
	// This is the fault class the harness could not previously express at all: a
	// lifecycle step that fails indefinitely. Everything it had was bounded by
	// construction, which is exactly why the defect shipped.
	eventUnstoppableGuest eventKind = "unstoppable_guest"
	// eventSilentGuest is issue #236: a running guest whose KERNEL stops. On
	// 2026-08-16 a job's `--privileged` container wrote to the guest's
	// /proc/sysrq-trigger and panicked it; with kernel.panic=0 the panicked kernel
	// hung forever. No userspace ran again, so the runner agent's socket was never
	// closed, `tart list` went on reporting the VM running, and GitHub went on
	// reporting the job in_progress until its own grace timer expired sixteen to
	// eighteen minutes later.
	//
	// It is deliberately a FOURTH failure event rather than a variant of any of
	// the three above, because it fails at a place none of them can reach. A
	// stalled_runner never starts its job; a wedged_drain and an unstoppable_guest
	// both happen after the fleet has already decided to drain. This one happens
	// while the instance is healthy, Running, and executing work, and the only
	// signal is that the guest stops answering. Like unstoppable_guest it NEVER
	// decays: a panicked kernel does not recover.
	eventSilentGuest eventKind = "silent_guest"
	// eventSaturatedGuest is the false positive the mechanism above must never
	// produce: a guest doing so much legitimate work that its liveness probe
	// cannot complete inside its own deadline. A monorepo Gradle build using every
	// core is exactly this.
	//
	// It exists as its own event because the whole risk of a guest-liveness
	// reclaim is confusing it with a dead one, and a sweep that only ever
	// generated dead guests would prove nothing about the discrimination. Its
	// probes are INCONCLUSIVE rather than refused, its job progresses normally,
	// and no instance carrying it may ever be reclaimed for guest death.
	eventSaturatedGuest  eventKind = "saturated_guest"
	eventSiblingReassign eventKind = "sibling_reassign"
	// eventSiblingSubstitute is issue #123: a registered runner is given a QUEUED
	// sibling instead of the request its own VM acquired, and that request goes
	// back to the queue with nobody to run it.
	eventSiblingSubstitute eventKind = "sibling_substitute"
	eventStopArrivals      eventKind = "stop_arrivals"
)

func (t simTrace) String() string {
	lines := make([]string, 0, len(t.Events)+1)
	lines = append(lines, fmt.Sprintf("trace seed=%d ticks=%d config=%s events=%d", t.Seed, t.Ticks, t.Config, len(t.Events)))
	for _, event := range t.Events {
		lines = append(lines, "  "+event.String())
	}
	return strings.Join(lines, "\n")
}

// generateTrace draws one world history from a seed. Every draw happens here and
// nowhere else, which is what makes the executor deterministic and shrinkable.
func generateTrace(seed int64, ticks int, cfg worldConfig) simTrace {
	// A reproducible schedule, not a security decision: the whole point is that
	// this stream is derivable from the seed printed beside a failure.
	rng := rand.New(rand.NewSource(seed))
	trace := simTrace{Seed: seed, Ticks: ticks, Config: cfg.Name}
	// Arrivals stop three quarters of the way through so property (f) has a
	// quiet tail to observe.
	quietAt := ticks * 3 / 4
	for tick := 1; tick <= ticks; tick++ {
		if tick == quietAt {
			trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventStopArrivals})
		}
		if tick < quietAt {
			for range arrivalsThisTick(rng) {
				trace.Events = append(trace.Events, simEvent{Tick: tick, Kind: eventArrive,
					Repo: pick(rng, cfg.Repos), Profile: pickProfile(rng, cfg.Profiles), Event: pick(rng, workflowEvents())})
			}
		}
		if fault, ok := faultThisTick(rng, tick); ok {
			trace.Events = append(trace.Events, fault)
		}
	}
	return trace
}

// arrivalsThisTick is a bursty arrival process: usually nothing, sometimes a
// wave large enough to oversubscribe the host several times over.
func arrivalsThisTick(rng *rand.Rand) int {
	switch roll := rng.Intn(100); {
	case roll < 55:
		return 0
	case roll < 85:
		return 1
	case roll < 96:
		return 2
	default:
		return 4
	}
}

// faultThisTick draws at most one environmental fault per tick. The mix is
// deliberately weighted toward the failure modes that produced real incidents:
// broker delivery anomalies, stale statistics, and drains that will not finish.
func faultThisTick(rng *rand.Rand, tick int) (simEvent, bool) {
	if rng.Intn(100) >= 22 {
		return simEvent{}, false
	}
	kinds := []eventKind{eventBrokerDelay, eventBrokerDuplicate, eventBrokerDrop, eventBrokerReorder,
		eventStatisticsGap, eventRESTLag, eventHostTenant, eventHostProbeStale, eventTartUnavailable,
		eventSlowBoot, eventLongJob, eventOverrunJob, eventStalledRunner, eventWedgedDrain, eventUnstoppableGuest,
		eventSilentGuest, eventSaturatedGuest,
		eventSiblingReassign, eventSiblingSubstitute, eventSilentCancel, eventLoudCancel}
	kind := kinds[rng.Intn(len(kinds))]
	return simEvent{Tick: tick, Kind: kind, Count: 1 + rng.Intn(6)}, true
}

func pick(rng *rand.Rand, values []string) string { return values[rng.Intn(len(values))] }

func pickProfile(rng *rand.Rand, values []domain.ProfileID) domain.ProfileID {
	return values[rng.Intn(len(values))]
}

func workflowEvents() []string { return []string{"pull_request", "push", "schedule"} }

// ---------------------------------------------------------------------------
// Trace execution
// ---------------------------------------------------------------------------

// applyTraceEvents performs every scheduled event for the current tick. Selector
// events resolve their target modulo the live population, so deleting an earlier
// arrival remains a legal (if differently-targeted) trace during shrinking.
func (w *world) applyTraceEvents() {
	for _, event := range w.trace.Events {
		if event.Tick != w.tick {
			continue
		}
		w.applyTraceEvent(event)
	}
	if w.delayWindow > 0 {
		w.delayWindow--
		if w.delayWindow == 0 {
			w.messageDelay = 0
		}
	}
}

func (w *world) applyTraceEvent(event simEvent) {
	switch event.Kind {
	case eventArrive:
		w.arrive(event.Repo, event.Profile, event.Event)
	case eventStopArrivals:
		w.arrivalsStopped = true
	case eventSilentCancel:
		w.cancelJob(event.Count, true)
	case eventLoudCancel:
		w.cancelJob(event.Count, false)
	case eventBrokerDelay:
		w.messageDelay, w.delayWindow = event.Count, event.Count+1
	case eventBrokerDuplicate:
		w.duplicateLastMessage()
	case eventBrokerDrop:
		w.dropNewestMessage()
	case eventBrokerReorder:
		w.reorderNewestMessages()
	case eventStatisticsGap:
		w.statisticsGap = event.Count
	case eventRESTLag:
		w.restLag = event.Count
	case eventHostTenant:
		w.tenantCPU, w.tenantMemoryMB = event.Count, event.Count*1_024
	case eventHostProbeStale:
		w.hostProbeStale = event.Count
	case eventTartUnavailable:
		w.tartUnavailable = event.Count
	case eventSlowBoot:
		w.slowBootNext = event.Count
	case eventLongJob:
		// Scaled to deadlines: Count of 6 is roughly one assignment deadline, so
		// the generator reaches jobs that outlive one and jobs that do not.
		w.longJobNext = event.Count * 6
	case eventOverrunJob:
		// Scaled to the profile budget, not to a deadline: one whole budget per
		// Count, so the smallest overrun the generator can draw still holds its
		// vector past the ceiling and only the reclaim can end it.
		w.overrunJobNext = event.Count * simOverrunTicks
	case eventStalledRunner:
		w.stallRunner(event.Count)
	case eventWedgedDrain:
		w.wedgeDrain(event.Count)
	case eventUnstoppableGuest:
		w.wedgeGuest(event.Count)
	case eventSilentGuest:
		w.silenceGuest()
	case eventSaturatedGuest:
		w.saturateGuest()
	case eventSiblingReassign:
		w.reassignSiblings = true
	case eventSiblingSubstitute:
		w.substituteSiblings = true
	}
}

// cancelJob is GitHub retiring a queued run. A silent cancellation never emits a
// terminal broker message -- the ghost of ADR 0026 -- while a loud one does.
func (w *world) cancelJob(selector int, silent bool) {
	var queued []*simJob
	for _, job := range w.jobs {
		if job.status == jobQueued && !job.silentCancel {
			queued = append(queued, job)
		}
	}
	if len(queued) == 0 {
		return
	}
	job := queued[selector%len(queued)]
	job.silentCancel = silent
	if !silent {
		job.status = jobCancelled
	}
}

// duplicateLastMessage redelivers a message the fleet has already committed.
// ApplyDemandBatch must recognize it by (scale set, message id, digest) and
// treat it as a no-op; anything else is a durable idempotency defect.
func (w *world) duplicateLastMessage() {
	for binding := range w.cfg.Bindings {
		message, delivered := w.delivered[binding]
		if !delivered {
			continue
		}
		copied := *message
		copied.deliverAt = w.tick + 1
		w.outbound = append(w.outbound, &copied)
	}
}

// dropNewestMessage loses a message in flight. Scale-set delivery is
// at-least-once, so the events it carried are un-announced and the broker will
// offer them again -- possibly out of order relative to newer facts.
func (w *world) dropNewestMessage() {
	if len(w.outbound) == 0 {
		return
	}
	dropped := w.outbound[len(w.outbound)-1]
	w.outbound = w.outbound[:len(w.outbound)-1]
	for _, event := range dropped.events {
		if job := w.jobByRequest(event.RunnerRequestID); job != nil {
			delete(job.announced, event.Kind)
		}
	}
}

// reorderNewestMessages delivers two in-flight messages in the wrong order.
func (w *world) reorderNewestMessages() {
	if len(w.outbound) < 2 {
		return
	}
	last, previous := w.outbound[len(w.outbound)-1], w.outbound[len(w.outbound)-2]
	last.deliverAt, previous.deliverAt = previous.deliverAt, last.deliverAt+1
}

// stallRunner picks a registered runner GitHub will never hand work to, which is
// how an instance ages past the assignment deadline.
func (w *world) stallRunner(selector int) {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil || len(instances) == 0 {
		return
	}
	var candidates []string
	for _, instance := range instances {
		if instance.State == operations.StateRegistering || instance.State == operations.StateOnlineIdle {
			candidates = append(candidates, instance.ID)
		}
	}
	if len(candidates) == 0 {
		return
	}
	w.stalledRunner[candidates[selector%len(candidates)]] = true
}

// wedgeDrain holds a drain in the draining state, modelling GitHub refusing to
// deregister a runner it has brokered another job to.
func (w *world) wedgeDrain(ticks int) {
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return
	}
	for _, instance := range instances {
		if instance.State == operations.StateDraining {
			w.wedgedDrain[instance.ID] = ticks
			return
		}
	}
	w.wedgeNextDrain = ticks
}

// wedgeGuest makes one guest refuse to power itself down, and keeps it refusing.
//
// The level is how much force the guest finally yields to: an odd Count models a
// guest that ignores every polite request but dies when the hypervisor is told
// to end it, and an even Count one that has to be removed outright. There is
// deliberately no level that survives removal — that is a broken host rather
// than a wedged guest, and the fleet's answer to it is a dead letter and a named
// `fleet doctor` finding, not a released vector.
func (w *world) wedgeGuest(count int) {
	level := lifecycle.StopForced
	if count%2 == 0 {
		level = lifecycle.StopDestructive
	}
	instances, err := w.store.LiveInstances(w.ctx)
	if err != nil {
		return
	}
	for _, instance := range instances {
		if instance.State == operations.StateDeregistering || instance.State == operations.StateDraining {
			w.unstoppableGuest[instance.ID] = level
			return
		}
	}
	w.wedgeNextGuest = level
}

// ---------------------------------------------------------------------------
// Shrinking
// ---------------------------------------------------------------------------

// shrink performs delta debugging over the event trace: it repeatedly removes
// the largest prefix, suffix, or single event whose removal still reproduces a
// finding of the same kind. Naive by design -- a minimal-ish trace an operator
// can read beats a clever algorithm nobody trusts.
func shrink(t testingT, cfg worldConfig, trace simTrace, want findingKind) (simTrace, []finding) {
	best := trace
	bestFindings := runTrace(t, cfg, best)
	if !containsKind(bestFindings, want) {
		return best, bestFindings
	}
	granularity := 2
	for len(best.Events) > 1 {
		reduced := false
		chunk := max(len(best.Events)/granularity, 1)
		for start := 0; start < len(best.Events); start += chunk {
			candidate := best
			candidate.Events = withoutRange(best.Events, start, min(start+chunk, len(best.Events)))
			findings := runTrace(t, cfg, candidate)
			if !containsKind(findings, want) {
				continue
			}
			best, bestFindings, reduced = candidate, findings, true
			break
		}
		if reduced {
			granularity = 2
			continue
		}
		if granularity >= len(best.Events) {
			break
		}
		granularity = min(granularity*2, len(best.Events))
	}
	return best, bestFindings
}

// testingT is the narrow slice of *testing.T the harness needs, so shrinking can
// run many candidate worlds without each one owning the test's lifetime.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

func withoutRange(events []simEvent, from, to int) []simEvent {
	reduced := make([]simEvent, 0, len(events)-(to-from))
	reduced = append(reduced, events[:from]...)
	return append(reduced, events[to:]...)
}

func containsKind(findings []finding, kind findingKind) bool {
	for _, item := range findings {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func firstKind(findings []finding) findingKind {
	if len(findings) == 0 {
		return ""
	}
	return findings[0].Kind
}
