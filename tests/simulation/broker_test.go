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
	w.crossSiblingAssignments()
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
			job.remaining = simJobTicks
		case jobRunning:
			job.remaining--
			if job.remaining <= 0 {
				job.status = jobDone
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
// real DemandCoordinator, in delivery order.
func (w *world) deliverMessages() {
	ready := make(map[int][]githubscaleset.Demand, len(w.cfg.Bindings))
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
		w.delivered[message.binding] = message
	}
	w.outbound = remaining
	for index := range w.cfg.Bindings {
		if len(ready[index]) == 0 {
			continue
		}
		source := &simSource{messages: ready[index]}
		if _, err := w.demand.IngestOnceResult(w.ctx, w.cfg.Bindings[index], source); err != nil {
			w.noteIngestFailure(err)
		}
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

func (w *world) bindingFor(profile domain.ProfileID) int {
	for index, binding := range w.cfg.Bindings {
		if binding.Profile.ID == profile {
			return index
		}
	}
	return -1
}

func splitRepo(repo string) (owner, name string) {
	for index := range repo {
		if repo[index] == '/' {
			return repo[:index], repo[index+1:]
		}
	}
	return repo, repo
}
