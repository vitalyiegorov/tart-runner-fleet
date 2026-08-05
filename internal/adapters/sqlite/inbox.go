package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// ApplyDemandBatch commits one broker message's events and records the message
// in the inbox that makes redelivery idempotent.
//
// The idempotency key is (scale_set_id, generation, message_id), not the message
// id alone. A message id is unique only within one broker sequence, and GitHub
// restarts that sequence: on 2026-08-01T18:32Z it restarted the sequence for
// scale set 8077185082566234948 at 100000001 while this ledger held
// 100000004..100000086 from July, so every redelivered id collided with a row
// whose content differed, the commit was refused, ScaleSet.Handle nacked, and
// the binding never ingested another message until an operator deleted the rows
// by hand three days later (issue #165).
//
// The evidence that a sequence restarted is one thing and it is unambiguous:
// the ledger already holds this id, in this generation, under different content.
// A redelivery of the same message is byte-identical by construction, so a
// divergence cannot be one -- and it is the exact moment the old key started
// refusing a job forever. The message is then applied under the next
// generation. Nothing is dropped and nothing is refused: a delivered job is
// fresh broker evidence, and refusing it forever is the failure this contract
// exists to make impossible.
//
// A message id merely LOWER than the cursor is deliberately not evidence. Inside
// one sequence a delayed message legitimately arrives below the high-water mark,
// and treating that as a restart would rewind the cursor and retire a ledger
// whose messages are still live -- weakening the dedupe that makes at-least-once
// delivery safe, to detect a restart that the divergence check catches anyway on
// the first message that could have done harm.
//
// Retiring a generation is bounded -- the live generation and the one it
// replaced are kept, the rest are deleted -- so the ledger cannot grow without
// limit across restarts while the evidence that diagnoses one stays readable.
func (s *Store) ApplyDemandBatch(ctx context.Context, scaleSetID, messageID int64, events []operations.DemandEvent) (operations.DemandBatchResult, error) {
	var result operations.DemandBatchResult
	if scaleSetID <= 0 || messageID <= 0 {
		return result, operations.ErrInvalid
	}
	for _, event := range events {
		if !event.Valid() {
			return result, operations.ErrInvalid
		}
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return result, fmt.Errorf("encode demand batch: %w", err)
	}
	digest := sha256.Sum256(encoded)
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, "inbox.begin")
	if err != nil {
		return result, fmt.Errorf("begin demand batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	generation, reset, duplicate, err := s.inboxGeneration(ctx, tx, scaleSetID, messageID, digest[:])
	if err != nil {
		return result, err
	}
	if duplicate {
		return result, nil
	}
	if reset.Detected {
		if _, err := s.txExec(ctx, tx, "inbox.retire",
			`DELETE FROM scale_set_inbox WHERE scale_set_id=? AND generation<?`, scaleSetID, generation-1); err != nil {
			return result, fmt.Errorf("retire demand generations: %w", err)
		}
	}
	for _, event := range events {
		if err := s.applyDemandEvent(ctx, tx, scaleSetID, event, now); err != nil {
			return result, err
		}
	}
	if _, err := s.txExec(ctx, tx, "inbox.record", `INSERT INTO scale_set_inbox(scale_set_id,generation,message_id,digest,events,created_at) VALUES(?,?,?,?,?,?)`,
		scaleSetID, generation, messageID, digest[:], encoded, now.UnixNano()); err != nil {
		return result, fmt.Errorf("record demand batch: %w", err)
	}
	// The cursor is the lastMessageId every future long poll opens with, so it
	// must follow the live sequence. MAX is right inside one generation -- a
	// delayed message must not rewind it -- and wrong across a restart, where the
	// retired high-water mark asks the broker for ids the new sequence will not
	// reach for thousands of jobs.
	if _, err := s.txExec(ctx, tx, "inbox.cursor", `INSERT INTO scale_set_cursors(scale_set_id,generation,message_id,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(scale_set_id) DO UPDATE SET
		message_id=CASE WHEN excluded.generation>scale_set_cursors.generation THEN excluded.message_id ELSE MAX(scale_set_cursors.message_id,excluded.message_id) END,
		generation=MAX(scale_set_cursors.generation,excluded.generation),updated_at=excluded.updated_at`,
		scaleSetID, generation, messageID, now.UnixNano()); err != nil {
		return result, fmt.Errorf("advance demand cursor: %w", err)
	}
	if err := s.commit(tx, "inbox.commit"); err != nil {
		return result, fmt.Errorf("commit demand batch: %w", err)
	}
	return operations.DemandBatchResult{Applied: true, Reset: reset}, nil
}

// inboxGeneration reports the generation this message must be recorded under,
// the evidence if that generation is newly adopted, and whether the ledger has
// already committed this exact message. It is a pure reading of durable state:
// the same message against the same ledger always yields the same answer, so a
// redelivery can never oscillate between generations.
func (s *Store) inboxGeneration(ctx context.Context, tx *sql.Tx, scaleSetID, messageID int64, digest []byte) (int64, operations.DemandSequenceReset, bool, error) {
	var generation, cursor int64
	err := s.txRow(ctx, tx, "inbox.cursor.generation", `SELECT generation,message_id FROM scale_set_cursors WHERE scale_set_id=?`, scaleSetID).
		Scan(&generation, &cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, operations.DemandSequenceReset{}, false, fmt.Errorf("load demand generation: %w", err)
	}
	var existing []byte
	err = s.txRow(ctx, tx, "inbox.load", `SELECT digest FROM scale_set_inbox WHERE scale_set_id=? AND generation=? AND message_id=?`,
		scaleSetID, generation, messageID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return generation, operations.DemandSequenceReset{}, false, nil
	case err != nil:
		return 0, operations.DemandSequenceReset{}, false, fmt.Errorf("load demand batch: %w", err)
	case string(existing) == string(digest):
		return generation, operations.DemandSequenceReset{}, true, nil
	default:
		return generation + 1, operations.DemandSequenceReset{Detected: true, Generation: generation + 1,
			RetiredMessageID: cursor, AdoptedMessageID: messageID}, false, nil
	}
}

func (s *Store) applyDemandEvent(ctx context.Context, tx *sql.Tx, scaleSetID int64, event operations.DemandEvent, now time.Time) error {
	record, err := s.demandRecord(ctx, tx, scaleSetID, event.RunnerRequestID)
	if errors.Is(err, operations.ErrNotFound) {
		record = operations.DemandRecord{ScaleSetID: scaleSetID, RunnerRequestID: event.RunnerRequestID}
	} else if err != nil {
		return err
	} else if record.JobID != "" && event.JobID != "" && record.JobID != event.JobID {
		// A deterministic synthetic-ID collision must never merge two jobs.
		return operations.ErrConflict
	}
	mergeDemandEvent(&record, event, now)
	record.LogicalKey = demandLogicalKey(record.Owner, record.Repository, record.WorkflowRunID, record.DisplayName, record.WorkflowRef, record.Labels, record.JobID)
	if record.LogicalKey != "" {
		queue := toNanos(record.QueueTime)
		if _, err := s.txExec(ctx, tx, "inbox.group", `INSERT INTO demand_groups(scale_set_id,logical_key,first_queue_time,workflow_job_id,run_attempt,updated_at)
			VALUES(?,?,?,0,0,?) ON CONFLICT(scale_set_id,logical_key) DO UPDATE SET
			first_queue_time=CASE WHEN demand_groups.run_attempt>0 THEN demand_groups.first_queue_time WHEN demand_groups.first_queue_time=0 THEN excluded.first_queue_time WHEN excluded.first_queue_time=0 THEN demand_groups.first_queue_time ELSE MIN(demand_groups.first_queue_time,excluded.first_queue_time) END,
			updated_at=excluded.updated_at`, scaleSetID, record.LogicalKey, queue, now.UnixNano()); err != nil {
			return fmt.Errorf("project demand group: %w", err)
		}
		if err := s.txRow(ctx, tx, "inbox.group.load", `SELECT first_queue_time,workflow_job_id,run_attempt FROM demand_groups WHERE scale_set_id=? AND logical_key=?`, scaleSetID, record.LogicalKey).
			Scan((*nanosTime)(&record.FirstQueueTime), &record.WorkflowJobID, &record.RunAttempt); err != nil {
			return fmt.Errorf("load demand group: %w", err)
		}
	}
	labels, _ := json.Marshal(record.Labels)
	// Fresh broker evidence retracts every REST-derived absence conclusion for
	// this request: a re-advertised job is by definition not a ghost, and an
	// expiry that the broker contradicts must never survive it.
	_, err = s.txExec(ctx, tx, "inbox.project", `INSERT INTO runner_demands(scale_set_id,runner_request_id,status,status_rank,owner,repository,workflow_run_id,job_id,display_name,workflow_ref,logical_key,event_name,labels,queue_time,first_queue_time,workflow_job_id,run_attempt,runner_id,runner_name,result,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(scale_set_id,runner_request_id) DO UPDATE SET status=excluded.status,status_rank=excluded.status_rank,owner=excluded.owner,
		repository=excluded.repository,workflow_run_id=excluded.workflow_run_id,job_id=excluded.job_id,display_name=excluded.display_name,
		workflow_ref=excluded.workflow_ref,logical_key=excluded.logical_key,event_name=excluded.event_name,labels=excluded.labels,
		queue_time=excluded.queue_time,first_queue_time=excluded.first_queue_time,workflow_job_id=excluded.workflow_job_id,run_attempt=excluded.run_attempt,
		runner_id=excluded.runner_id,runner_name=excluded.runner_name,result=excluded.result,updated_at=excluded.updated_at,
		absent_since=0,absent_observations=0,expired_at=0`,
		record.ScaleSetID, record.RunnerRequestID, record.Status, demandRank(record.Status), record.Owner, record.Repository,
		record.WorkflowRunID, record.JobID, record.DisplayName, record.WorkflowRef, record.LogicalKey, record.EventName, labels,
		toNanos(record.QueueTime), toNanos(record.FirstQueueTime), record.WorkflowJobID, record.RunAttempt, record.RunnerID,
		record.RunnerName, record.Result, record.UpdatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("project demand event: %w", err)
	}
	return nil
}

func toNanos(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func mergeDemandEvent(record *operations.DemandRecord, event operations.DemandEvent, now time.Time) {
	if demandRank(event.Kind) >= demandRank(record.Status) {
		record.Status = event.Kind
	}
	if event.Owner != "" {
		record.Owner = event.Owner
	}
	if event.Repository != "" {
		record.Repository = event.Repository
	}
	if event.WorkflowRunID != 0 {
		record.WorkflowRunID = event.WorkflowRunID
	}
	if event.JobID != "" {
		record.JobID = event.JobID
	}
	if event.DisplayName != "" {
		record.DisplayName = event.DisplayName
	}
	if event.WorkflowRef != "" {
		record.WorkflowRef = event.WorkflowRef
	}
	if event.EventName != "" {
		record.EventName = event.EventName
	}
	if event.Labels != nil {
		record.Labels = append(record.Labels[:0], event.Labels...)
	}
	if !event.QueueTime.IsZero() {
		record.QueueTime = event.QueueTime.UTC()
	}
	if event.RunnerID != 0 {
		record.RunnerID = event.RunnerID
	}
	if event.RunnerName != "" {
		record.RunnerName = event.RunnerName
	}
	if event.Result != "" {
		record.Result = event.Result
	}
	record.UpdatedAt = now
}

func demandRank(kind operations.DemandEventKind) int {
	switch kind {
	case operations.DemandJobAvailable:
		return 1
	case operations.DemandJobAssigned:
		return 2
	case operations.DemandJobStarted:
		return 3
	case operations.DemandJobCompleted:
		return 4
	default:
		return 0
	}
}

// ActiveDemands lists demand the broker has not completed and REST has not
// proven absent. An expired ghost is excluded rather than deleted, so the row
// stays available as durable evidence and is revived unchanged the moment
// either source contradicts the expiry.
func (s *Store) ActiveDemands(ctx context.Context, scaleSetID int64) ([]operations.DemandRecord, error) {
	if scaleSetID <= 0 {
		return nil, operations.ErrInvalid
	}
	rows, err := s.dbQuery(ctx, "inbox.active.query", `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,display_name,workflow_ref,logical_key,event_name,labels,queue_time,first_queue_time,workflow_job_id,run_attempt,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND status_rank<? AND expired_at=0 ORDER BY COALESCE(NULLIF(first_queue_time,0),queue_time),runner_request_id`, scaleSetID, demandRank(operations.DemandJobCompleted))
	if err != nil {
		return nil, fmt.Errorf("list active demands: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []operations.DemandRecord
	for rows.Next() {
		record, err := scanDemand(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active demands: %w", err)
	}
	return result, nil
}

func (s *Store) DemandCursor(ctx context.Context, scaleSetID int64) (int64, error) {
	if scaleSetID <= 0 {
		return 0, operations.ErrInvalid
	}
	var cursor int64
	err := s.dbRow(ctx, "inbox.cursor.load", `SELECT message_id FROM scale_set_cursors WHERE scale_set_id=?`, scaleSetID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load demand cursor: %w", err)
	}
	return cursor, nil
}

func (s *Store) demandRecord(ctx context.Context, tx *sql.Tx, scaleSetID, requestID int64) (operations.DemandRecord, error) {
	return scanDemand(s.txRow(ctx, tx, "inbox.demand.load", `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,display_name,workflow_ref,logical_key,event_name,labels,queue_time,first_queue_time,workflow_job_id,run_attempt,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND runner_request_id=?`, scaleSetID, requestID))
}

func (s *Store) DemandRecord(ctx context.Context, scaleSetID, requestID int64) (operations.DemandRecord, error) {
	if scaleSetID <= 0 || requestID <= 0 {
		return operations.DemandRecord{}, operations.ErrInvalid
	}
	return scanDemand(s.dbRow(ctx, "inbox.demand.lookup", `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,display_name,workflow_ref,logical_key,event_name,labels,queue_time,first_queue_time,workflow_job_id,run_attempt,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND runner_request_id=?`, scaleSetID, requestID))
}

// RunnerActiveJob reports whether any durable demand in the scale set shows a
// workflow job currently executing on the named runner. GitHub records the
// runner it brokered a job to on that job's own demand row, so this is the only
// evidence that answers "is THIS runner busy" independently of which demand the
// fleet spawned the VM for. Only an exactly-JobStarted status counts as
// executing: an assigned-but-unstarted job is re-queued by GitHub when its
// runner goes away, which is what makes stalled-assignment reclaim safe.
func (s *Store) RunnerActiveJob(ctx context.Context, scaleSetID int64, runnerName string) (bool, error) {
	if scaleSetID <= 0 || runnerName == "" {
		return false, operations.ErrInvalid
	}
	var active int
	if err := s.dbRow(ctx, "inbox.runner.active", `SELECT COUNT(*) FROM runner_demands
		WHERE scale_set_id=? AND runner_name=? AND status_rank=?`, scaleSetID, runnerName,
		demandRank(operations.DemandJobStarted)).Scan(&active); err != nil {
		return false, fmt.Errorf("count active runner jobs: %w", err)
	}
	return active > 0, nil
}

// ProjectDemandEvent advances the owned VM from the highest durable demand
// rank, not merely from the delivered event. This makes redelivery monotonic.
func (s *Store) ProjectDemandEvent(ctx context.Context, scaleSetID int64, event operations.DemandEvent) error {
	if scaleSetID <= 0 || !event.Valid() {
		return operations.ErrInvalid
	}
	if event.Kind == operations.DemandJobAvailable {
		return nil
	}
	record, err := s.DemandRecord(ctx, scaleSetID, event.RunnerRequestID)
	if err != nil {
		return err
	}
	repo := record.Owner + "/" + record.Repository
	instances, err := s.LiveInstances(ctx)
	if err != nil {
		return err
	}
	var instance operations.Instance
	if record.RunnerName != "" {
		for _, candidate := range instances {
			if candidate.ID == record.RunnerName {
				instance = candidate
				break
			}
		}
		if instance.ID != "" {
			instance, err = s.alignRunnerDemand(ctx, instance, instances, record)
			if err != nil {
				return err
			}
		}
	} else {
		for _, candidate := range instances {
			if candidate.Repo == repo && candidate.Demand.JobID == record.RunnerRequestID {
				if instance.ID != "" {
					return operations.ErrConflict
				}
				instance = candidate
			}
		}
	}
	if instance.ID == "" {
		if record.Status == operations.DemandJobCompleted {
			return nil
		}
		return operations.ErrUncertain
	}
	return s.projectDemandRank(ctx, instance, record.Status)
}

// alignRunnerDemand reconciles the reservation used to provision a JIT runner
// with GitHub's authoritative RunnerName assignment. GitHub decides which of a
// scale set's queued requests a registered runner executes; the fleet only
// decides which request justified spawning the VM. When those two disagree,
// GitHub wins — it is the party that actually dispatched the work — and the
// durable binding is moved to what it dispatched.
//
// Two disagreements are reachable, and they need different repairs:
//
//   - Both requests were acquired, by one VM each, and the broker matched them
//     to the opposite runners. Updating only the named runner would duplicate one
//     request and leave the displaced runner with an unsafe stale identity, so
//     both scheduling identities are swapped atomically (ADR 0016).
//   - Only the target's own request was acquired, and the runner was handed a
//     SIBLING that no VM incarnates. There is nothing to swap with, so the target
//     is rebound to the sibling and its former demand returns to the queue
//     (ADR 0033, issue #123).
//
// Ownership signatures stay immutable in both, so nothing about the VM's
// provenance, its name, or its spawn identity is rewritten.
func (s *Store) alignRunnerDemand(ctx context.Context, target operations.Instance, instances []operations.Instance, record operations.DemandRecord) (operations.Instance, error) {
	repo, requestID := record.Repo(), record.RunnerRequestID
	if target.ID == "" || record.Owner == "" || record.Repository == "" || requestID <= 0 {
		return operations.Instance{}, operations.ErrInvalid
	}
	var source operations.Instance
	for _, candidate := range instances {
		if candidate.Repo == repo && candidate.Demand.JobID == requestID {
			if source.ID != "" {
				return operations.Instance{}, operations.ErrConflict
			}
			source = candidate
		}
	}
	if source.ID == target.ID && source.ID != "" {
		return target, nil
	}
	if source.ID == "" {
		return s.rebindRunnerDemand(ctx, target, record)
	}
	if source.Platform != target.Platform || source.Profile != target.Profile || source.Route != target.Route || source.Resources != target.Resources {
		return operations.Instance{}, operations.ErrConflict
	}

	sourceNext, targetNext := source, target
	sourceNext.Repo, targetNext.Repo = target.Repo, source.Repo
	sourceNext.Demand, targetNext.Demand = target.Demand, source.Demand
	if !sourceNext.SchedulingMetadataValid() || !targetNext.SchedulingMetadataValid() {
		return operations.Instance{}, operations.ErrConflict
	}
	sourceBefore, _ := encodeSchedulingMetadata(source)
	targetBefore, _ := encodeSchedulingMetadata(target)
	sourceAfter, _ := encodeSchedulingMetadata(sourceNext)
	targetAfter, _ := encodeSchedulingMetadata(targetNext)

	tx, err := s.beginTx(ctx, "runner-demand.begin")
	if err != nil {
		return operations.Instance{}, fmt.Errorf("begin runner demand alignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	update := func(point string, current operations.Instance, before, after []byte) error {
		result, updateErr := s.txExec(ctx, tx, point, `UPDATE instances SET scheduling_metadata=?,updated_at=?
			WHERE id=? AND state=? AND version=? AND scheduling_metadata=?`, after, now.UnixNano(), current.ID, current.State, current.Version, before)
		if updateErr != nil {
			return updateErr
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return operations.ErrConflict
		}
		return nil
	}
	if err := update("runner-demand.source", source, sourceBefore, sourceAfter); err != nil {
		return operations.Instance{}, fmt.Errorf("align source runner demand: %w", err)
	}
	if err := update("runner-demand.target", target, targetBefore, targetAfter); err != nil {
		return operations.Instance{}, fmt.Errorf("align target runner demand: %w", err)
	}
	if err := s.commit(tx, "runner-demand.commit"); err != nil {
		return operations.Instance{}, fmt.Errorf("commit runner demand alignment: %w", err)
	}
	targetNext.UpdatedAt = now
	return targetNext, nil
}

// rebindRunnerDemand moves a registered runner's durable binding onto the
// SIBLING request GitHub actually dispatched to it, and releases the request the
// fleet spawned it for back to the queue.
//
// This is the 2026-08-02 shape of issue #123. The broker handed
// `trf-xl-25a374b60f46dafe` "Maestro (bare)" while the fleet had bound it to
// "Maestro (expo)" — two jobs of one workflow run, dispatched 41 ms apart — and
// no VM had acquired the sibling, so there was no second instance to swap with.
// The binding then said one thing and GitHub said another, permanently:
// `JobInactive` reads the BOUND demand and reported an idle runner, `RunnerBusy`
// reads the RUNNER and reported a live job, and the fleet planned a recovery
// drain every idle-runner deadline and aborted it every time (ADR 0028). The
// released demand meanwhile could never be respawned, because its own instance
// still incarnated it.
//
// Rebinding makes the binding say what GitHub says, so both predicates read one
// fact again. Three guards keep it fail-closed, and each one leaves the previous
// ErrUncertain behaviour in place rather than guessing:
//
//   - The runner must exist in GitHub's world. Only a registered, working
//     instance can have been dispatched to; a VM still cloning has no runner, and
//     one already tearing down must not have work re-attached to it.
//   - The dispatched row must be able to name a demand at all, so no rebind can
//     write scheduling metadata the durable layer would refuse.
//   - The released demand must show no work of its own ON THIS RUNNER. A bound
//     demand already JobStarted or JobCompleted with this runner's name means the
//     runner really did serve it, and releasing it would lose real work. The same
//     statuses under a DIFFERENT runner's name are the opposite evidence — GitHub
//     is running that job elsewhere — and releasing it is exactly right.
//
// The repository may change with the binding, and must: a scale set can serve
// several repositories, and GitHub brokers across all of them. That is safe for
// control routing — which is keyed on (repo, profile), lifecycle.SourceKey —
// because the dispatched row arrived through this instance's own scale set, and
// every repository whose demand a scale set delivers is one the fleet would have
// spawned an instance into directly (reconcile.Controller takes the instance's
// repository straight from the demand). A rebind can therefore only move an
// instance to a repository it could already have been born in. The crossed
// assignment above has swapped repositories since ADR 0016 for the same reason.
//
// The released demand's queue time is deliberately untouched: it is GitHub's
// fact about a job that has genuinely never run, and ADR 0033 explains why
// resetting it would both lie to the queue SLO and invert the aging guard of
// ADR 0004.
func (s *Store) rebindRunnerDemand(ctx context.Context, target operations.Instance, record operations.DemandRecord) (operations.Instance, error) {
	if !rebindableRunnerState(target.State) {
		return operations.Instance{}, operations.ErrUncertain
	}
	next := target
	next.Repo, next.Demand = record.Repo(), record.DemandKey()
	if next.Demand == target.Demand || !next.SchedulingMetadataValid() {
		return operations.Instance{}, operations.ErrUncertain
	}
	released, err := s.DemandRecord(ctx, record.ScaleSetID, target.Demand.JobID)
	if err != nil && !errors.Is(err, operations.ErrNotFound) {
		return operations.Instance{}, err
	}
	if err == nil && released.RunnerName == target.ID &&
		(released.Status == operations.DemandJobStarted || released.Status == operations.DemandJobCompleted) {
		return operations.Instance{}, operations.ErrUncertain
	}
	// Both encodings are already known valid: the target came from the durable
	// row and the rebound metadata was validated above, exactly as the swap
	// relies on.
	before, _ := encodeSchedulingMetadata(target)
	after, _ := encodeSchedulingMetadata(next)
	now := time.Now().UTC()
	// The same compare-and-set the swap uses: state, version, and the exact prior
	// metadata. A row that moved between the read and this write keeps its old
	// binding and the next redelivery retries, which is what at-least-once
	// delivery is for.
	result, err := s.dbExec(ctx, "runner-demand.rebind", `UPDATE instances SET scheduling_metadata=?,updated_at=?
		WHERE id=? AND state=? AND version=? AND scheduling_metadata=?`, after, now.UnixNano(), target.ID, target.State, target.Version, before)
	if err != nil {
		return operations.Instance{}, fmt.Errorf("rebind runner demand: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return operations.Instance{}, operations.ErrConflict
	}
	next.UpdatedAt = now
	return next, nil
}

// rebindableRunnerState is the set of states in which an instance has a runner
// GitHub can dispatch to, and no cleanup in flight.
//
// It starts at REACHABLE, not at registering, and that boundary is load-bearing.
// ProvisionExecutor calls AcquireAndGenerateJIT on the reachable -> registering
// edge, so the runner exists from the moment the acquisition returns, while the
// durable row only advances afterwards. A broker message naming that runner can
// and does arrive inside that window: the deterministic simulation reached it on
// the very tick the runner came up, and a rebind refused there was never retried,
// stranding the demand for the rest of the run.
//
// Everything earlier stays fail-closed because there is no runner to name yet —
// a VM that has not booted cannot have been dispatched to. So does the teardown
// chain, and so do the terminal states: work is never re-attached to an instance
// that is being reclaimed.
//
// A rebind does not bump the row's version, exactly as the crossed-assignment
// swap does not, so it cannot interfere with the provisioning executor's own
// compare-and-set mid-flight.
func rebindableRunnerState(state operations.State) bool {
	switch state {
	case operations.StateReachable, operations.StateRegistering, operations.StateOnlineIdle,
		operations.StateAssigned, operations.StateRunning:
		return true
	default:
		return false
	}
}

func (s *Store) projectDemandRank(ctx context.Context, instance operations.Instance, status operations.DemandEventKind) error {
	advance := func(next operations.State) error {
		updated, err := s.Advance(ctx, lifecycle.StateChange{InstanceID: instance.ID, ExpectedState: instance.State,
			ExpectedVersion: instance.Version, NextState: next})
		if err == nil {
			instance = updated
		}
		return err
	}
	for range 5 {
		switch status {
		case operations.DemandJobAssigned:
			switch instance.State {
			case operations.StateRegistering:
				if err := advance(operations.StateOnlineIdle); err != nil {
					return err
				}
			case operations.StateOnlineIdle:
				return advance(operations.StateAssigned)
			case operations.StateAssigned, operations.StateRunning, operations.StateDraining, operations.StateDeregistering, operations.StateStopping, operations.StateDeleted:
				return nil
			default:
				return operations.ErrUncertain
			}
		case operations.DemandJobStarted:
			switch instance.State {
			case operations.StateRegistering:
				if err := advance(operations.StateOnlineIdle); err != nil {
					return err
				}
			case operations.StateOnlineIdle:
				if err := advance(operations.StateAssigned); err != nil {
					return err
				}
			case operations.StateAssigned:
				return advance(operations.StateRunning)
			case operations.StateRunning, operations.StateDraining, operations.StateDeregistering, operations.StateStopping, operations.StateDeleted:
				return nil
			default:
				return operations.ErrUncertain
			}
		case operations.DemandJobCompleted:
			switch instance.State {
			case operations.StatePlanned, operations.StateCloning, operations.StateBooting, operations.StateReachable:
				return s.enqueueDemandDrain(ctx, instance, []string{instance.Ownership.OperationID})
			case operations.StateRegistering:
				if err := advance(operations.StateOnlineIdle); err != nil {
					return err
				}
			case operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning:
				return s.enqueueDemandDrain(ctx, instance, nil)
			case operations.StateDraining, operations.StateDeregistering, operations.StateStopping, operations.StateDeleted:
				return nil
			default:
				return operations.ErrUncertain
			}
		default:
			return operations.ErrInvalid
		}
	}
	return operations.ErrUncertain
}

func (s *Store) enqueueDemandDrain(ctx context.Context, instance operations.Instance, dependencies []string) error {
	now := time.Now().UTC()
	id := "event-drain-" + instance.ID
	_, _, err := s.Transition(ctx, operations.Transition{InstanceID: instance.ID, ExpectedState: instance.State,
		ExpectedVersion: instance.Version, NextState: operations.StateDraining, DrainPhase: 1,
		Operation: operations.Operation{ID: id, IdempotencyKey: id, EffectKey: "deregister:" + instance.ID,
			Kind: lifecycle.OperationDrain, ResourceID: instance.ID, Payload: json.RawMessage(`{}`), AvailableAt: now,
			DependsOn: append([]string(nil), dependencies...)}})
	return err
}

func scanDemand(row rowScanner) (operations.DemandRecord, error) {
	var record operations.DemandRecord
	var status string
	var labels []byte
	var queueTime, firstQueueTime, updatedAt int64
	err := row.Scan(&record.ScaleSetID, &record.RunnerRequestID, &status, &record.Owner, &record.Repository, &record.WorkflowRunID,
		&record.JobID, &record.DisplayName, &record.WorkflowRef, &record.LogicalKey, &record.EventName, &labels, &queueTime,
		&firstQueueTime, &record.WorkflowJobID, &record.RunAttempt, &record.RunnerID, &record.RunnerName, &record.Result, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.DemandRecord{}, operations.ErrNotFound
	}
	if err != nil {
		return operations.DemandRecord{}, fmt.Errorf("scan demand: %w", err)
	}
	if err := json.Unmarshal(labels, &record.Labels); err != nil {
		return operations.DemandRecord{}, fmt.Errorf("decode demand labels: %w", err)
	}
	record.Status = operations.DemandEventKind(status)
	record.QueueTime = fromNanos(queueTime)
	record.FirstQueueTime = fromNanos(firstQueueTime)
	if record.FirstQueueTime.IsZero() {
		record.FirstQueueTime = record.QueueTime
	}
	record.UpdatedAt = fromNanos(updatedAt)
	return record, nil
}

type nanosTime time.Time

func (t *nanosTime) Scan(src any) error {
	value, ok := src.(int64)
	if !ok {
		return fmt.Errorf("scan nanosecond timestamp from %T", src)
	}
	*t = nanosTime(fromNanos(value))
	return nil
}

func demandLogicalKey(owner, repository string, runID int64, displayName, workflowRef string, labels []string, fallbackJobID string) string {
	if owner == "" || repository == "" || runID <= 0 {
		return ""
	}
	if displayName == "" {
		displayName = "job:" + fallbackJobID
	}
	normalized := append([]string(nil), labels...)
	for i := range normalized {
		normalized[i] = strings.ToLower(strings.TrimSpace(normalized[i]))
	}
	sort.Strings(normalized)
	// workflowRef is intentionally not part of the join key: the workflow-jobs
	// REST endpoint does not expose it. It remains durable evidence on protocol
	// records, while owner/repo/run/name/labels provide the safe join class.
	_ = workflowRef
	identity := strings.ToLower(owner) + "\x00" + strings.ToLower(repository) + "\x00" + fmt.Sprint(runID) + "\x00" +
		displayName + "\x00" + strings.Join(normalized, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", digest[:])
}

func (s *Store) PutDemandStatistics(ctx context.Context, scaleSetID int64, statistics operations.DemandStatistics) (bool, error) {
	if scaleSetID <= 0 || !statistics.Valid() {
		return false, operations.ErrInvalid
	}
	observed := statistics.ObservedAt.UTC()
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	result, err := s.dbExec(ctx, "inbox.statistics", `INSERT INTO scale_set_statistics(
		scale_set_id,message_id,available,acquired,assigned,running,registered,busy,idle,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scale_set_id) DO UPDATE SET
		message_id=excluded.message_id,available=excluded.available,acquired=excluded.acquired,assigned=excluded.assigned,
		running=excluded.running,registered=excluded.registered,busy=excluded.busy,idle=excluded.idle,observed_at=excluded.observed_at
		WHERE excluded.message_id>=scale_set_statistics.message_id`, scaleSetID, statistics.MessageID, statistics.Available,
		statistics.Acquired, statistics.Assigned, statistics.Running, statistics.Registered, statistics.Busy,
		statistics.Idle, observed.UnixNano())
	if err != nil {
		return false, fmt.Errorf("store scale-set statistics: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (s *Store) DemandStatistics(ctx context.Context, scaleSetID int64) (operations.DemandStatistics, error) {
	if scaleSetID <= 0 {
		return operations.DemandStatistics{}, operations.ErrInvalid
	}
	var statistics operations.DemandStatistics
	var observed int64
	err := s.dbRow(ctx, "inbox.statistics.load", `SELECT message_id,available,acquired,assigned,running,registered,busy,idle,observed_at
		FROM scale_set_statistics WHERE scale_set_id=?`, scaleSetID).Scan(&statistics.MessageID, &statistics.Available,
		&statistics.Acquired, &statistics.Assigned, &statistics.Running, &statistics.Registered, &statistics.Busy,
		&statistics.Idle, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.DemandStatistics{}, operations.ErrNotFound
	}
	if err != nil {
		return operations.DemandStatistics{}, fmt.Errorf("load scale-set statistics: %w", err)
	}
	statistics.ObservedAt = fromNanos(observed)
	return statistics, nil
}

// ReconcileGitHubJobs preserves the single-scale-set API used by focused store
// tests and maintenance tools. Production reconciliation commits the complete
// scope through ReconcileGitHubJobSnapshot below.
func (s *Store) ReconcileGitHubJobs(ctx context.Context, scaleSetID int64, observedAt time.Time, jobs []operations.GitHubJobObservation) (bool, error) {
	return s.ReconcileGitHubJobSnapshot(ctx, observedAt, map[int64][]operations.GitHubJobObservation{scaleSetID: jobs})
}

// ReconcileGitHubJobSnapshot enriches broker demand with REST's stable numeric
// job identity and original creation time. Every profile in one REST scope is
// replaced in one transaction, so an injected write failure retains the entire
// previous scope snapshot. Ambiguous same-name matrix jobs share age and
// attempt but never receive a guessed numeric identity.
func (s *Store) ReconcileGitHubJobSnapshot(ctx context.Context, observedAt time.Time,
	snapshot map[int64][]operations.GitHubJobObservation,
) (bool, error) {
	if observedAt.IsZero() || len(snapshot) == 0 {
		return false, operations.ErrInvalid
	}
	keys := make([]int64, 0, len(snapshot))
	for scaleSetID, jobs := range snapshot {
		if scaleSetID <= 0 {
			return false, operations.ErrInvalid
		}
		for _, job := range jobs {
			if !job.Valid() {
				return false, operations.ErrInvalid
			}
		}
		keys = append(keys, scaleSetID)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	tx, err := s.beginTx(ctx, "githubjobs.begin")
	if err != nil {
		return false, fmt.Errorf("begin GitHub job reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, scaleSetID := range keys {
		applied, err := s.reconcileGitHubJobsTx(ctx, tx, scaleSetID, observedAt, snapshot[scaleSetID])
		if err != nil {
			return false, err
		}
		changed = changed || applied
	}
	if err := s.commit(tx, "githubjobs.commit"); err != nil {
		return false, fmt.Errorf("commit GitHub job reconciliation: %w", err)
	}
	return changed, nil
}

func (s *Store) reconcileGitHubJobsTx(ctx context.Context, tx *sql.Tx, scaleSetID int64, observedAt time.Time,
	jobs []operations.GitHubJobObservation,
) (bool, error) {
	var previousCount int
	if err := s.txRow(ctx, tx, "githubjobs.count", `SELECT COUNT(*) FROM github_job_observations WHERE scale_set_id=?`, scaleSetID).Scan(&previousCount); err != nil {
		return false, fmt.Errorf("count prior GitHub jobs: %w", err)
	}
	var previousObserved int64
	if err := s.txRow(ctx, tx, "githubjobs.mark.load", `SELECT observed_at FROM github_job_snapshots WHERE scale_set_id=?`,
		scaleSetID).Scan(&previousObserved); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("load GitHub job snapshot mark: %w", err)
	}
	if _, err := s.txExec(ctx, tx, "githubjobs.replace", `DELETE FROM github_job_observations WHERE scale_set_id=?`, scaleSetID); err != nil {
		return false, fmt.Errorf("replace GitHub job snapshot: %w", err)
	}
	type group struct {
		first   time.Time
		attempt int
		ids     []int64
	}
	groups := make(map[string]group)
	for _, job := range jobs {
		labels, _ := json.Marshal(job.Labels)
		if _, err := s.txExec(ctx, tx, "githubjobs.upsert", `INSERT INTO github_job_observations(
			scale_set_id,workflow_job_id,owner,repository,workflow_run_id,run_attempt,display_name,workflow_ref,labels,status,created_at,queue_time_exact,observed_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(scale_set_id,workflow_job_id) DO UPDATE SET
			run_attempt=excluded.run_attempt,display_name=excluded.display_name,workflow_ref=excluded.workflow_ref,
			labels=excluded.labels,status=excluded.status,created_at=excluded.created_at,queue_time_exact=excluded.queue_time_exact,
			observed_at=excluded.observed_at`,
			scaleSetID, job.WorkflowJobID, job.Owner, job.Repository, job.WorkflowRunID, job.RunAttempt, job.DisplayName,
			job.WorkflowRef, labels, job.Status, job.CreatedAt.UTC().UnixNano(), job.QueueTimeExact, observedAt.UTC().UnixNano()); err != nil {
			return false, fmt.Errorf("store GitHub job observation: %w", err)
		}
		key := demandLogicalKey(job.Owner, job.Repository, job.WorkflowRunID, job.DisplayName, job.WorkflowRef, job.Labels, "")
		candidate := groups[key]
		if job.QueueTimeExact && (candidate.first.IsZero() || job.CreatedAt.Before(candidate.first)) {
			candidate.first = job.CreatedAt.UTC()
		}
		if candidate.attempt == 0 {
			candidate.attempt = job.RunAttempt
		} else if candidate.attempt != job.RunAttempt {
			candidate.attempt = -1
		}
		candidate.ids = append(candidate.ids, job.WorkflowJobID)
		groups[key] = candidate
	}
	for key, candidate := range groups {
		jobID := int64(0)
		if len(candidate.ids) == 1 {
			jobID = candidate.ids[0]
		}
		attempt := candidate.attempt
		if attempt < 0 {
			attempt = 0
		}
		if _, err := s.txExec(ctx, tx, "githubjobs.group", `INSERT INTO demand_groups(
			scale_set_id,logical_key,first_queue_time,workflow_job_id,run_attempt,updated_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(scale_set_id,logical_key) DO UPDATE SET
			first_queue_time=CASE
				WHEN excluded.run_attempt>0 AND demand_groups.run_attempt>0 AND excluded.run_attempt<>demand_groups.run_attempt THEN excluded.first_queue_time
				WHEN demand_groups.first_queue_time=0 THEN excluded.first_queue_time
				WHEN excluded.first_queue_time=0 THEN demand_groups.first_queue_time
				ELSE MIN(demand_groups.first_queue_time,excluded.first_queue_time) END,
			workflow_job_id=excluded.workflow_job_id,run_attempt=excluded.run_attempt,updated_at=excluded.updated_at`,
			scaleSetID, key, toNanos(candidate.first), jobID, attempt, observedAt.UTC().UnixNano()); err != nil {
			return false, fmt.Errorf("reconcile demand group: %w", err)
		}
		// Seeing the job queued is positive REST evidence: it records the join
		// that makes a later absence meaningful and revokes any absence or
		// expiry a previous snapshot concluded.
		if _, err := s.txExec(ctx, tx, "githubjobs.project", `UPDATE runner_demands SET
			first_queue_time=(SELECT first_queue_time FROM demand_groups WHERE scale_set_id=? AND logical_key=?),
			workflow_job_id=(SELECT workflow_job_id FROM demand_groups WHERE scale_set_id=? AND logical_key=?),
			run_attempt=(SELECT run_attempt FROM demand_groups WHERE scale_set_id=? AND logical_key=?),updated_at=?,
			corroborated_at=?,absent_since=0,absent_observations=0,expired_at=0
			WHERE scale_set_id=? AND logical_key=?`, scaleSetID, key, scaleSetID, key, scaleSetID, key,
			observedAt.UTC().UnixNano(), observedAt.UTC().UnixNano(), scaleSetID, key); err != nil {
			return false, fmt.Errorf("project GitHub job correlation: %w", err)
		}
	}
	if err := s.accrueAbsentDemand(ctx, tx, scaleSetID, observedAt, previousObserved, len(jobs)); err != nil {
		return false, err
	}
	return previousCount > 0 || len(jobs) > 0, nil
}

// accrueAbsentDemand records what this complete snapshot did not contain. Only
// demand REST has corroborated at least once can accrue absence, so a broker
// job the REST scope never covers (a repository outside the observer, or a
// label join that never matched) is left alone forever instead of being
// expired on evidence that was never capable of seeing it. A snapshot that is
// not strictly newer than the last recorded one adds no evidence at all, which
// keeps a replayed or clock-skewed observation from counting twice.
func (s *Store) accrueAbsentDemand(ctx context.Context, tx *sql.Tx, scaleSetID int64, observedAt time.Time,
	previousObserved int64, count int,
) error {
	observed := observedAt.UTC().UnixNano()
	if observed <= previousObserved {
		return nil
	}
	if _, err := s.txExec(ctx, tx, "githubjobs.absent", `UPDATE runner_demands SET
		absent_since=CASE WHEN absent_since=0 THEN ? ELSE absent_since END,
		absent_observations=absent_observations+1
		WHERE scale_set_id=? AND status_rank=? AND expired_at=0 AND corroborated_at>0 AND corroborated_at<?`,
		observed, scaleSetID, demandRank(operations.DemandJobAvailable), observed); err != nil {
		return fmt.Errorf("accrue absent demand: %w", err)
	}
	if _, err := s.txExec(ctx, tx, "githubjobs.mark", `INSERT INTO github_job_snapshots(scale_set_id,observed_at,job_count)
		VALUES(?,?,?) ON CONFLICT(scale_set_id) DO UPDATE SET observed_at=excluded.observed_at,job_count=excluded.job_count`,
		scaleSetID, observed, count); err != nil {
		return fmt.Errorf("record GitHub job snapshot mark: %w", err)
	}
	return nil
}

// ExpireGhostDemands retires queued demand that complete REST snapshots have
// proven absent. The predicate is the whole guard: the row must still be
// exactly JobAvailable at commit time, must never have been expired before,
// must carry positive REST corroboration, and must have been missing from
// enough snapshots for long enough. A job that starts, completes, or is
// re-advertised between planning and this statement fails the WHERE clause
// instead of losing its demand.
func (s *Store) ExpireGhostDemands(ctx context.Context, scaleSetID int64, criteria operations.GhostDemandCriteria) (int64, error) {
	if scaleSetID <= 0 || !criteria.Valid() {
		return 0, operations.ErrInvalid
	}
	observed := criteria.ObservedAt.UTC().UnixNano()
	result, err := s.dbExec(ctx, "inbox.ghost.expire", `UPDATE runner_demands SET expired_at=?,updated_at=?
		WHERE scale_set_id=? AND status_rank=? AND expired_at=0 AND corroborated_at>0 AND absent_since>0
		AND absent_since<=? AND absent_observations>=?`, observed, observed, scaleSetID,
		demandRank(operations.DemandJobAvailable), criteria.AbsentBefore.UTC().UnixNano(), criteria.MinObservations)
	if err != nil {
		return 0, fmt.Errorf("expire ghost demands: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) QueuedGitHubJobs(ctx context.Context, scaleSetID int64) ([]operations.GitHubJobObservation, error) {
	if scaleSetID <= 0 {
		return nil, operations.ErrInvalid
	}
	rows, err := s.dbQuery(ctx, "githubjobs.queued", `SELECT workflow_job_id,owner,repository,workflow_run_id,run_attempt,
		display_name,workflow_ref,labels,status,created_at,queue_time_exact FROM github_job_observations
		WHERE scale_set_id=? AND status='queued' ORDER BY created_at,workflow_job_id`, scaleSetID)
	if err != nil {
		return nil, fmt.Errorf("list queued GitHub jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var jobs []operations.GitHubJobObservation
	for rows.Next() {
		var job operations.GitHubJobObservation
		var labels []byte
		var created int64
		if err := rows.Scan(&job.WorkflowJobID, &job.Owner, &job.Repository, &job.WorkflowRunID, &job.RunAttempt,
			&job.DisplayName, &job.WorkflowRef, &labels, &job.Status, &created, &job.QueueTimeExact); err != nil {
			return nil, fmt.Errorf("scan queued GitHub job: %w", err)
		}
		if err := json.Unmarshal(labels, &job.Labels); err != nil {
			return nil, fmt.Errorf("decode queued GitHub job labels: %w", err)
		}
		job.CreatedAt = fromNanos(created)
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queued GitHub jobs: %w", err)
	}
	return jobs, nil
}
