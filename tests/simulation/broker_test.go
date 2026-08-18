package simulation_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// simJobTicks is how long a brokered job executes. Six ticks is three virtual
// minutes, comfortably below the fifteen-minute assignment deadline so a healthy
// job never trips a recovery.
const simJobTicks = 6

// simOverrunTicks is one whole profile occupancy budget expressed in ticks. It
// is what eventOverrunJob adds per Count, so the shortest overrun the generator
// can draw already outlives the ceiling every simulated profile declares and no
// deadline shorter than the occupancy reclaim can end it (issue #223).
const simOverrunTicks = int(simOccupancyBudget / simTick)

// simSnapshotEvery is the REST scope poll interval in ticks. Production polls at
// most every thirty seconds; one tick is thirty virtual seconds.
const simSnapshotEvery = 2

// simSource adapts a batch of broker messages to the real MessageSource contract
// the DemandCoordinator ingests through.
type simSource struct{ messages []githubscaleset.Demand }

func (s *simSource) Handle(ctx context.Context, handle func(context.Context, githubscaleset.Demand) error) error {
	for _, message := range s.messages {
		if err := handle(ctx, message); err != nil {
			return err
		}
	}
	return nil
}

// simSnapshotView adapts a captured REST scope observation to the coordinator's
// GitHubQueueSnapshot contract.
type simSnapshotView struct{ snapshot *simSnapshot }

func (v simSnapshotView) ObservedAt() time.Time { return v.snapshot.at }

func (v simSnapshotView) QueuedJobs() []githubscaleset.WorkflowJob {
	return append([]githubscaleset.WorkflowJob(nil), v.snapshot.jobs...)
}

// ---------------------------------------------------------------------------
// Physics: what GitHub and the hypervisor do between two reconciliations.
// ---------------------------------------------------------------------------

// advancePhysics decays fault windows, lets the broker hand queued jobs to
// registered runners, and advances executing jobs toward completion.
func (w *world) advancePhysics() {
	w.statisticsGap = decay(w.statisticsGap)
	w.restLag = decay(w.restLag)
	w.hostProbeStale = decay(w.hostProbeStale)
	w.tartUnavailable = decay(w.tartUnavailable)
	for id, remaining := range w.wedgedDrain {
		w.wedgedDrain[id] = decay(remaining)
	}
	for id, remaining := range w.powerMisreport {
		w.powerMisreport[id] = decay(remaining)
	}
	w.armLatchedGuestFaults()
	w.crossSiblingAssignments()
	w.substituteQueuedSibling()
	w.progressJobs()
}

func decay(remaining int) int {
	if remaining > 0 {
		return remaining - 1
	}
	return 0
}

// acquireJob is the fleet taking a job from the scale set, which production
// performs exactly once per VM at the reachable -> registering edge
// (lifecycle.ProvisionExecutor calls AcquireAndGenerateJIT with the instance's
// OWN demand). Until it happens the job is still Available to GitHub, which is
// the window every re-planning question in this package turns on.
//
// A run GitHub has already retired cannot be acquired: production answers
// ErrJobNotAcquired, the VM registers anyway, and the runner waits for work that
// never comes -- the ghost runner of the 2026-08-01 incident.
func (w *world) acquireJob(instance operations.Instance) {
	job := w.jobByRequest(instance.Demand.JobID)
	if job == nil || job.status != jobQueued || job.silentCancel {
		return
	}
	job.status = jobAcquired
	job.runner = instance.ID
}

// crossSiblingAssignments is GitHub matching two ACQUIRED requests to the
// opposite registered runners. Neither request returns to the queue -- both were
// legitimately acquired by their own VM -- but the runner each one executes on is
// swapped, which is exactly the disagreement sqlite.alignRunnerDemand exists to
// resolve and the decoupling ADR 0028 was written about.
func (w *world) crossSiblingAssignments() {
	if !w.reassignSiblings {
		return
	}
	var crossable []*simJob
	for _, job := range w.jobs {
		if job.status == jobAcquired && job.runner != "" && !job.announced[operations.DemandJobAssigned] {
			crossable = append(crossable, job)
		}
	}
	for i := 0; i < len(crossable); i++ {
		for j := i + 1; j < len(crossable); j++ {
			left, right := crossable[i], crossable[j]
			if left.binding != right.binding {
				continue
			}
			left.runner, right.runner = right.runner, left.runner
			w.reassignSiblings = false
			return
		}
	}
}

// substituteQueuedSibling is GitHub giving a registered runner a job OTHER than
// the one its own VM acquired, where that other job is still QUEUED — nobody
// spawned a VM for it.
//
// This is the shape of the 2026-08-02 incident (issue #123) and it is materially
// different from crossSiblingAssignments above. A cross swaps two acquired
// requests, so every request still has exactly one instance and
// sqlite.alignRunnerDemand can swap the two bindings back into agreement. A
// substitution has no counterpart: the acquired request returns to the queue
// unassigned, the runner executes a sibling no instance incarnates, and the
// fleet's binding is left naming work that will never run on it.
//
// Only a job whose JobAssigned has not been announced can be substituted, for
// the same reason a cross can only happen there: once the broker has told the
// fleet who runs what, it does not take it back.
func (w *world) substituteQueuedSibling() {
	if !w.substituteSiblings {
		return
	}
	for _, bound := range w.jobs {
		if bound.status != jobAcquired || bound.runner == "" || bound.announced[operations.DemandJobAssigned] {
			continue
		}
		sibling := w.queuedSiblingOf(bound)
		if sibling == nil {
			continue
		}
		sibling.runner, sibling.status = bound.runner, jobAcquired
		// The acquired request goes back to being an ordinary queued job that
		// GitHub still advertises, exactly as "Maestro (expo)" stayed JobAvailable
		// for the whole incident.
		bound.runner, bound.status = "", jobQueued
		bound.announced = map[operations.DemandEventKind]bool{operations.DemandJobAvailable: true}
		w.substituteSiblings = false
		w.substitutions++
		return
	}
}

// queuedSiblingOf is the oldest queued job of the same scale set. The scale set
// is GitHub's own brokering boundary and the only one that applies: a scale set
// serves every repository configured against it, so a sibling handoff is not
// confined to the workflow run, or even to the repository, that the runner's own
// request came from.
func (w *world) queuedSiblingOf(bound *simJob) *simJob {
	for _, candidate := range w.jobs {
		if candidate == bound || candidate.binding != bound.binding {
			continue
		}
		if candidate.status == jobQueued && !candidate.silentCancel && candidate.runner == "" {
			return candidate
		}
	}
	return nil
}

// progressJobs starts acquired work and retires finished work. A stalled runner
// never starts: its assignment ages in place, which is the 84-minute stalled
// assignment the recovery deadline exists for.
func (w *world) progressJobs() {
	for _, job := range w.jobs {
		switch job.status {
		case jobAcquired:
			if w.stalledRunner[job.runner] {
				continue
			}
			job.status = jobRunning
			job.startedAt = w.now
			// Job duration is a property of the workflow, not of the fleet, and the
			// harness modelled exactly one of them -- three virtual minutes, five
			// times shorter than the assignment deadline. No job could therefore
			// outlive a recovery deadline, which made the whole abort class of
			// ADR 0028 unreachable and hid the churn of issue #123. A long job is
			// the ordinary case for the maestro suites this fleet runs.
			// An overrun adds a whole profile budget on top, because the job it
			// models is not long -- it has stopped making progress and will hold the
			// runner until the occupancy reclaim takes it away (issue #223).
			job.remaining = simJobTicks + w.longJobNext + w.overrunJobNext
			w.longJobNext, w.overrunJobNext = 0, 0
		case jobRunning:
			if w.silentGuest[job.runner] {
				// The machine executing this job stopped. Nothing advances, and nothing
				// inside the guest will ever report that: GitHub goes on calling the
				// job in_progress until its own grace timer expires, which is the whole
				// sixteen-to-eighteen-minute hold of issue #236.
				continue
			}
			job.remaining--
			if job.remaining <= 0 {
				job.status, job.finishedAt = jobDone, w.now
			}
		case jobQueued, jobDone, jobCancelled:
		}
	}
}

// ---------------------------------------------------------------------------
// Broker message production and delivery
// ---------------------------------------------------------------------------

// produceMessages packages the events the broker owes the fleet into one
// message per scale set and schedules its delivery.
func (w *world) produceMessages() {
	for index := range w.cfg.Bindings {
		events := w.pendingEvents(index)
		if len(events) == 0 && w.statisticsGap > 0 {
			continue
		}
		if len(events) == 0 && w.tick%2 != 0 {
			// A quiet queue only refreshes statistics periodically; a broker that
			// never speaks is what ages statistics past their freshness budget.
			continue
		}
		available, assigned, running := w.statisticsFor(index)
		w.outbound = append(w.outbound, &simMessage{binding: index, messageID: w.nextMessageID(),
			events: events, available: available, assigned: assigned, running: running,
			deliverAt: w.tick + w.messageDelay, withStats: w.statisticsGap == 0})
	}
}

func (w *world) nextMessageID() int64 {
	w.nextMsgID++
	return w.nextMsgID
}

// restartSequence is GitHub restarting the broker's message-id sequence for this
// scale set, which it did for scale set 8077185082566234948 at
// 2026-08-01T18:32Z. Nothing else changes: the same jobs are advertised, on the
// same cadence, under ids the fleet has already seen. The fleet's inbox is the
// only party that believes those ids are unique forever (issue #165).
func (w *world) restartSequence() {
	if w.cfg.SequenceResetAt <= 0 || w.tick != w.cfg.SequenceResetAt {
		return
	}
	w.nextMsgID = 0
}

// redeliverAfter is how long the broker waits before handing back a message the
// fleet declined to commit. Production observed almost exactly five minutes,
// which is ten simulated ticks; two keeps a bounded run informative while
// preserving the shape -- an uncommitted message always comes back.
const redeliverAfter = 2

// pendingEvents is the difference between what the broker knows and what it has
// already told this scale set.
func (w *world) pendingEvents(binding int) []operations.DemandEvent {
	var events []operations.DemandEvent
	for _, job := range w.jobs {
		if job.binding != binding {
			continue
		}
		for _, kind := range demandEventProgression(job) {
			if job.announced[kind] {
				continue
			}
			job.announced[kind] = true
			events = append(events, w.demandEvent(job, kind))
		}
	}
	return events
}

// demandEventProgression is the ordered set of broker events a job's current
// status implies. A silently cancelled run deliberately never reaches a terminal
// event: GitHub cancelled it server side and kept advertising it, which is the
// ghost of ADR 0026.
func demandEventProgression(job *simJob) []operations.DemandEventKind {
	kinds := []operations.DemandEventKind{operations.DemandJobAvailable}
	switch job.status {
	case jobAcquired:
		kinds = append(kinds, operations.DemandJobAssigned)
	case jobRunning:
		kinds = append(kinds, operations.DemandJobAssigned, operations.DemandJobStarted)
	case jobDone:
		kinds = append(kinds, operations.DemandJobAssigned, operations.DemandJobStarted, operations.DemandJobCompleted)
	case jobCancelled:
		if job.silentCancel {
			return kinds
		}
		kinds = append(kinds, operations.DemandJobCompleted)
	case jobQueued:
	}
	return kinds
}

func (w *world) demandEvent(job *simJob, kind operations.DemandEventKind) operations.DemandEvent {
	event := operations.DemandEvent{Kind: kind, RunnerRequestID: job.requestID, Owner: job.owner,
		Repository: job.name, WorkflowRunID: job.runID, JobID: fmt.Sprintf("job-%d", job.requestID),
		DisplayName: fmt.Sprintf("build-%d", job.requestID), WorkflowRef: "refs/heads/main",
		EventName: job.event, Labels: w.jobLabels(job), QueueTime: job.queuedAt}
	if kind != operations.DemandJobAvailable {
		event.RunnerName = job.runner
		event.RunnerID = int(job.requestID % 10_000)
	}
	if kind == operations.DemandJobCompleted {
		event.Result = "succeeded"
	}
	return event
}

func (w *world) jobLabels(job *simJob) []string {
	return []string{"self-hosted", string(w.cfg.Bindings[job.binding].Profile.Route)}
}

// statisticsFor is GitHub's point-in-time view of one scale set, computed when
// the message is produced. A delayed message therefore carries a genuinely stale
// count, which is what bounds admission in app.DemandCoordinator.QueuedDemands.
func (w *world) statisticsFor(binding int) (available, assigned, running int) {
	for _, job := range w.jobs {
		if job.binding != binding {
			continue
		}
		switch job.status {
		case jobQueued:
			available++
		case jobAcquired:
			assigned++
		case jobRunning:
			assigned++
			running++
		case jobDone, jobCancelled:
		}
	}
	return available, assigned, running
}

// deliverMessages hands every message whose delivery instant has arrived to the
// real DemandCoordinator, in delivery order. A batch the fleet declines to
// commit is not lost: the broker keeps it and hands it back, which is the
// at-least-once contract ADR 0009 and rule 6 are written about, and the loop
// that turned one refused message into a three-day outage (issue #165).
func (w *world) deliverMessages() {
	ready := make(map[int][]githubscaleset.Demand, len(w.cfg.Bindings))
	inFlight := make(map[int][]*simMessage, len(w.cfg.Bindings))
	var remaining []*simMessage
	for _, message := range w.outbound {
		if message.deliverAt > w.tick {
			remaining = append(remaining, message)
			continue
		}
		ready[message.binding] = append(ready[message.binding], githubscaleset.Demand{
			MessageID: int(message.messageID), Assigned: message.assigned, Running: message.running,
			Statistics: w.statisticsMessage(message), Events: convertEvents(message.events),
		})
		inFlight[message.binding] = append(inFlight[message.binding], message)
		w.delivered[message.binding] = message
	}
	w.outbound = remaining
	for index := range w.cfg.Bindings {
		if len(ready[index]) == 0 {
			continue
		}
		w.noteAdvertised(inFlight[index])
		source := &simSource{messages: ready[index]}
		if _, err := w.demand.IngestOnceResult(w.ctx, w.cfg.Bindings[index], source); err != nil {
			w.noteIngestFailure(err)
			w.redeliver(inFlight[index])
		}
	}
}

// noteAdvertised records the tick at which the broker actually handed a job's
// availability to the fleet. Property (j) is measured from this instant and not
// from the job's creation, so a delayed message is never mistaken for a binding
// that refused what it was given.
func (w *world) noteAdvertised(messages []*simMessage) {
	for _, message := range messages {
		for _, event := range message.events {
			if event.Kind != operations.DemandJobAvailable {
				continue
			}
			if job := w.jobByRequest(event.RunnerRequestID); job != nil && job.advertisedAt == 0 {
				job.advertisedAt = w.tick
			}
		}
	}
}

// redeliver returns an uncommitted batch to the broker's outbound queue. The
// message keeps its id: a redelivery is the SAME message, which is exactly why
// the inbox must recognize it and why recognizing it by id alone was unsound.
func (w *world) redeliver(messages []*simMessage) {
	for _, message := range messages {
		message.deliverAt = w.tick + redeliverAfter
		w.outbound = append(w.outbound, message)
	}
}

// statisticsMessage carries the broker's counters. MessageID zero means the
// message carries no statistics at all, which is how a statistics gap ages the
// binding past its freshness budget without silencing demand.
func (w *world) statisticsMessage(message *simMessage) githubscaleset.DemandStatistics {
	if !message.withStats {
		return githubscaleset.DemandStatistics{}
	}
	return githubscaleset.DemandStatistics{MessageID: int(message.messageID), Available: message.available,
		Acquired: message.assigned, Assigned: message.assigned, Running: message.running,
		Registered: message.assigned, Busy: message.running, Idle: 0}
}

func convertEvents(events []operations.DemandEvent) []githubscaleset.JobEvent {
	converted := make([]githubscaleset.JobEvent, 0, len(events))
	for _, event := range events {
		converted = append(converted, githubscaleset.JobEvent{Kind: githubscaleset.JobEventKind(event.Kind),
			RunnerRequestID: event.RunnerRequestID, Owner: event.Owner, Repository: event.Repository,
			WorkflowRunID: event.WorkflowRunID, JobID: event.JobID, DisplayName: event.DisplayName,
			WorkflowRef: event.WorkflowRef, EventName: event.EventName, Labels: event.Labels,
			QueueTime: event.QueueTime, RunnerID: event.RunnerID, RunnerName: event.RunnerName,
			Result: event.Result})
	}
	return converted
}

// noteIngestFailure classifies a rejected broker batch. Uncertainty is the
// documented at-least-once answer -- the message is redelivered -- but a durable
// refusal of well-formed evidence is a defect the simulation must surface.
func (w *world) noteIngestFailure(err error) {
	if errors.Is(err, operations.ErrUncertain) || errors.Is(err, operations.ErrConflict) {
		return
	}
	w.record(findingStoreError, fmt.Sprintf("ingest broker batch: %v", err))
}

// ---------------------------------------------------------------------------
// REST scope snapshots
// ---------------------------------------------------------------------------

// captureSnapshot records the complete queued-job view at this instant and
// schedules it for delivery, which may be several ticks later.
func (w *world) captureSnapshot() {
	snapshot := &simSnapshot{at: w.now, deliverAt: w.tick + w.restLag}
	for _, job := range w.jobs {
		switch job.status {
		case jobQueued, jobAcquired, jobRunning:
			snapshot.outstanding++
		case jobDone, jobCancelled:
		}
		if job.status != jobQueued || job.silentCancel {
			// A silently cancelled run is absent from REST while the broker still
			// advertises it. That disagreement is the only evidence ADR 0026 accepts.
			continue
		}
		snapshot.jobs = append(snapshot.jobs, githubscaleset.WorkflowJob{
			ID: job.requestID, RunID: job.runID, Name: fmt.Sprintf("build-%d", job.requestID),
			Repository: githubscaleset.Repository{Owner: job.owner, Name: job.name},
			Status:     "queued", Labels: w.jobLabels(job), RunAttempt: 1,
			CreatedAt: job.queuedAt, QueueTimeExact: true,
		})
	}
	w.restQueue = append(w.restQueue, snapshot)
}

// deliverSnapshots commits every due REST observation through the real
// coordinator, which is what proves absence and expires ghosts.
func (w *world) deliverSnapshots() {
	if w.tick%simSnapshotEvery == 0 {
		w.captureSnapshot()
	}
	var remaining []*simSnapshot
	for _, snapshot := range w.restQueue {
		if snapshot.deliverAt > w.tick {
			remaining = append(remaining, snapshot)
			continue
		}
		// The observation the fleet has just committed. In a federated scope
		// EVERY one of its jobs matches BOTH scale sets, so it is also the
		// observation two unbounded attributions would double (#153).
		w.restCommitted = snapshot
		if _, err := w.demand.ReconcileQueuedJobs(w.ctx, w.cfg.Bindings, simSnapshotView{snapshot: snapshot}); err != nil {
			w.record(findingStoreError, fmt.Sprintf("reconcile REST snapshot: %v", err))
		}
	}
	w.restQueue = remaining
}

// ---------------------------------------------------------------------------
// Job creation
// ---------------------------------------------------------------------------

// arrive creates one workflow job in GitHub's world. It is the only way demand
// enters the simulation, so a trace with no arrivals has an empty fleet.
func (w *world) arrive(repo string, profile domain.ProfileID, event string) *simJob {
	if w.arrivalsStopped {
		return nil
	}
	binding := w.bindingFor(profile)
	if binding < 0 {
		return nil
	}
	owner, name := splitRepo(repo)
	w.nextRunID++
	w.nextReq++
	job := &simJob{requestID: w.nextReq, runID: w.nextRunID, repo: repo, owner: owner, name: name,
		binding: binding, event: event, queuedAt: w.now, status: jobQueued,
		announced: map[operations.DemandEventKind]bool{}}
	w.jobs = append(w.jobs, job)
	return job
}

// bindingFor is GitHub's placement decision, not the fleet's. A profile served
// by one scale set has one answer. A profile served by two identically-labelled
// sets in one scope has the answer the #144 spike measured: the job goes to the
// set with the most room, so each set fills to the capacity it last advertised
// and the remainder stays queued. Ties break on the lower index so a (seed,
// world) pair still names one exact history.
func (w *world) bindingFor(profile domain.ProfileID) int {
	chosen, room := -1, 0
	for index, binding := range w.cfg.Bindings {
		if binding.Profile.ID != profile {
			continue
		}
		free := w.cfg.Scheduler.Profiles[profile].MaxActive - w.outstandingJobs(index)
		if chosen < 0 || free > room {
			chosen, room = index, free
		}
	}
	return chosen
}

// outstandingJobs is what one scale set is already holding: everything GitHub
// has given it that has not finished.
func (w *world) outstandingJobs(binding int) int {
	held := 0
	for _, job := range w.jobs {
		if job.binding != binding {
			continue
		}
		switch job.status {
		case jobQueued, jobAcquired, jobRunning:
			held++
		case jobDone, jobCancelled:
		}
	}
	return held
}

func splitRepo(repo string) (owner, name string) {
	for index := range repo {
		if repo[index] == '/' {
			return repo[:index], repo[index+1:]
		}
	}
	return repo, repo
}
