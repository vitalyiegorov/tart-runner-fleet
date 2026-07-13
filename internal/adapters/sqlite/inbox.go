package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func (s *Store) ApplyDemandBatch(ctx context.Context, scaleSetID, messageID int64, events []operations.DemandEvent) (bool, error) {
	if scaleSetID <= 0 || messageID <= 0 {
		return false, operations.ErrInvalid
	}
	for _, event := range events {
		if !event.Valid() {
			return false, operations.ErrInvalid
		}
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return false, fmt.Errorf("encode demand batch: %w", err)
	}
	digest := sha256.Sum256(encoded)
	now := time.Now().UTC()
	tx, err := s.beginTx(ctx, "inbox.begin")
	if err != nil {
		return false, fmt.Errorf("begin demand batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing []byte
	err = s.txRow(ctx, tx, "inbox.load", `SELECT digest FROM scale_set_inbox WHERE scale_set_id=? AND message_id=?`, scaleSetID, messageID).Scan(&existing)
	if err == nil {
		if string(existing) != string(digest[:]) {
			return false, operations.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("load demand batch: %w", err)
	}
	for _, event := range events {
		if err := s.applyDemandEvent(ctx, tx, scaleSetID, event, now); err != nil {
			return false, err
		}
	}
	if _, err := s.txExec(ctx, tx, "inbox.record", `INSERT INTO scale_set_inbox(scale_set_id,message_id,digest,events,created_at) VALUES(?,?,?,?,?)`,
		scaleSetID, messageID, digest[:], encoded, now.UnixNano()); err != nil {
		return false, fmt.Errorf("record demand batch: %w", err)
	}
	if _, err := s.txExec(ctx, tx, "inbox.cursor", `INSERT INTO scale_set_cursors(scale_set_id,message_id,updated_at) VALUES(?,?,?)
		ON CONFLICT(scale_set_id) DO UPDATE SET message_id=MAX(message_id,excluded.message_id),updated_at=excluded.updated_at`,
		scaleSetID, messageID, now.UnixNano()); err != nil {
		return false, fmt.Errorf("advance demand cursor: %w", err)
	}
	if err := s.commit(tx, "inbox.commit"); err != nil {
		return false, fmt.Errorf("commit demand batch: %w", err)
	}
	return true, nil
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
	labels, _ := json.Marshal(record.Labels)
	_, err = s.txExec(ctx, tx, "inbox.project", `INSERT INTO runner_demands(scale_set_id,runner_request_id,status,status_rank,owner,repository,workflow_run_id,job_id,event_name,labels,queue_time,runner_id,runner_name,result,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(scale_set_id,runner_request_id) DO UPDATE SET status=excluded.status,status_rank=excluded.status_rank,owner=excluded.owner,
		repository=excluded.repository,workflow_run_id=excluded.workflow_run_id,job_id=excluded.job_id,event_name=excluded.event_name,labels=excluded.labels,
		queue_time=excluded.queue_time,runner_id=excluded.runner_id,runner_name=excluded.runner_name,result=excluded.result,updated_at=excluded.updated_at`,
		record.ScaleSetID, record.RunnerRequestID, record.Status, demandRank(record.Status), record.Owner, record.Repository,
		record.WorkflowRunID, record.JobID, record.EventName, labels, toNanos(record.QueueTime), record.RunnerID,
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

func (s *Store) ActiveDemands(ctx context.Context, scaleSetID int64) ([]operations.DemandRecord, error) {
	if scaleSetID <= 0 {
		return nil, operations.ErrInvalid
	}
	rows, err := s.dbQuery(ctx, "inbox.active.query", `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,event_name,labels,queue_time,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND status_rank<? ORDER BY runner_request_id`, scaleSetID, demandRank(operations.DemandJobCompleted))
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
	return scanDemand(s.txRow(ctx, tx, "inbox.demand.load", `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,event_name,labels,queue_time,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND runner_request_id=?`, scaleSetID, requestID))
}

func (s *Store) DemandRecord(ctx context.Context, scaleSetID, requestID int64) (operations.DemandRecord, error) {
	if scaleSetID <= 0 || requestID <= 0 {
		return operations.DemandRecord{}, operations.ErrInvalid
	}
	return scanDemand(s.db.QueryRowContext(ctx, `SELECT scale_set_id,runner_request_id,status,owner,repository,workflow_run_id,job_id,event_name,labels,queue_time,runner_id,runner_name,result,updated_at
		FROM runner_demands WHERE scale_set_id=? AND runner_request_id=?`, scaleSetID, requestID))
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
	for _, candidate := range instances {
		if candidate.Repo == repo && candidate.Demand.JobID == record.RunnerRequestID {
			if instance.ID != "" {
				return operations.ErrConflict
			}
			instance = candidate
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
			case operations.StateRegistering:
				if err := advance(operations.StateOnlineIdle); err != nil {
					return err
				}
			case operations.StateOnlineIdle, operations.StateAssigned, operations.StateRunning:
				now := time.Now().UTC()
				id := "event-drain-" + instance.ID
				_, _, err := s.Transition(ctx, operations.Transition{InstanceID: instance.ID, ExpectedState: instance.State,
					ExpectedVersion: instance.Version, NextState: operations.StateDraining, DrainPhase: 1,
					Operation: operations.Operation{ID: id, IdempotencyKey: id, EffectKey: "deregister:" + instance.ID,
						Kind: "deregister", ResourceID: instance.ID, Payload: json.RawMessage(`{}`), AvailableAt: now}})
				return err
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

func scanDemand(row rowScanner) (operations.DemandRecord, error) {
	var record operations.DemandRecord
	var status string
	var labels []byte
	var queueTime, updatedAt int64
	err := row.Scan(&record.ScaleSetID, &record.RunnerRequestID, &status, &record.Owner, &record.Repository, &record.WorkflowRunID,
		&record.JobID, &record.EventName, &labels, &queueTime, &record.RunnerID, &record.RunnerName, &record.Result, &updatedAt)
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
	record.UpdatedAt = fromNanos(updatedAt)
	return record, nil
}
