package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	_ "modernc.org/sqlite"
)

type Store struct {
	db          *sql.DB
	injectFault func(string) error
	injectRows  func(string) rowsScanner
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, operations.ErrInvalid
	}
	filesystemPath := !strings.HasPrefix(path, ":") && !strings.HasPrefix(path, "file:")
	if filesystemPath {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refuse symlink database path: %w", operations.ErrInvalid)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect sqlite path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if filesystemPath {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("secure sqlite permissions: %w", err)
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) SchedulerState(ctx context.Context) (operations.SchedulerState, error) {
	var state operations.SchedulerState
	var data, reservations, drr []byte
	err := s.db.QueryRowContext(ctx, `SELECT version,data,reservations,drr_state,observation_cursor FROM scheduler_state WHERE singleton=1`).Scan(
		&state.Version, &data, &reservations, &drr, &state.ObservationCursor)
	if err != nil {
		return operations.SchedulerState{}, fmt.Errorf("load scheduler state: %w", err)
	}
	state.Data = append(state.Data, data...)
	state.Reservations = append(state.Reservations, reservations...)
	state.DeficitRoundRobin = append(state.DeficitRoundRobin, drr...)
	return state, nil
}

func (s *Store) ApplyPlan(ctx context.Context, plan operations.Plan) (bool, error) {
	if !plan.Valid() {
		return false, operations.ErrInvalid
	}
	digest, err := plan.Digest()
	if err != nil {
		return false, fmt.Errorf("digest plan: %w", err)
	}
	tx, err := s.beginTx(ctx, "apply.begin")
	if err != nil {
		return false, fmt.Errorf("begin plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingDigest []byte
	err = s.txRow(ctx, tx, "apply.load", `SELECT digest FROM plans WHERE id=?`, plan.ID).Scan(&existingDigest)
	if err == nil {
		if string(existingDigest) != string(digest[:]) {
			return false, operations.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("load plan: %w", err)
	}
	var currentVersion int64
	if err := s.txRow(ctx, tx, "apply.version", `SELECT version FROM scheduler_state WHERE singleton=1`).Scan(&currentVersion); err != nil {
		return false, fmt.Errorf("load scheduler version: %w", err)
	}
	if currentVersion != plan.ExpectedSchedulerVersion {
		return false, operations.ErrConflict
	}
	for _, intent := range plan.Instances {
		ownershipJSON, _ := json.Marshal(intent.Instance.Ownership)
		schedulingJSON, _ := encodeSchedulingMetadata(intent.Instance)
		if intent.ExpectedVersion < 0 {
			_, err = s.txExec(ctx, tx, "apply.instance.insert", `INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				intent.Instance.ID, intent.Instance.State, intent.Instance.Version, intent.Instance.DrainPhase, ownershipJSON, schedulingJSON, intent.Instance.LastError,
				plan.CreatedAt.UTC().UnixNano(), plan.CreatedAt.UTC().UnixNano())
		} else {
			result, updateErr := s.txExec(ctx, tx, "apply.instance.update", `UPDATE instances SET state=?,version=?,drain_phase=?,ownership=?,last_error=?,updated_at=? WHERE id=? AND version=? AND state=? AND scheduling_metadata=?`,
				intent.Instance.State, intent.Instance.Version, intent.Instance.DrainPhase, ownershipJSON, intent.Instance.LastError,
				plan.CreatedAt.UTC().UnixNano(), intent.Instance.ID, intent.ExpectedVersion, intent.ExpectedState, schedulingJSON)
			err = updateErr
			if err == nil {
				changed, _ := result.RowsAffected()
				if changed != 1 {
					return false, operations.ErrConflict
				}
			}
		}
		if err != nil {
			return false, fmt.Errorf("apply instance intent: %w", err)
		}
	}
	for _, operation := range plan.Operations {
		if err := insertOperation(ctx, tx, operation, plan.CreatedAt); err != nil {
			return false, err
		}
	}
	for _, operation := range plan.Operations {
		for _, dependency := range operation.DependsOn {
			var present int
			if err := s.txRow(ctx, tx, "apply.dependency.check", `SELECT COUNT(*) FROM operations WHERE id=?`, dependency).Scan(&present); err != nil {
				return false, fmt.Errorf("validate operation dependency: %w", err)
			}
			if present != 1 {
				return false, operations.ErrInvalid
			}
			if _, err := s.txExec(ctx, tx, "apply.dependency.record", `INSERT INTO operation_dependencies(operation_id,depends_on) VALUES(?,?)`, operation.ID, dependency); err != nil {
				return false, fmt.Errorf("record operation dependency: %w", err)
			}
		}
	}
	result, err := s.txExec(ctx, tx, "apply.scheduler", `UPDATE scheduler_state SET version=?,data=?,reservations=?,drr_state=?,observation_cursor=?,updated_at=? WHERE singleton=1 AND version=?`,
		plan.Scheduler.Version, jsonOrDefault(plan.Scheduler.Data, `{}`), jsonOrDefault(plan.Scheduler.Reservations, `[]`), jsonOrDefault(plan.Scheduler.DeficitRoundRobin, `{}`),
		plan.Scheduler.ObservationCursor, plan.CreatedAt.UTC().UnixNano(), plan.ExpectedSchedulerVersion)
	if err != nil {
		return false, fmt.Errorf("persist scheduler state: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, operations.ErrConflict
	}
	if _, err := s.txExec(ctx, tx, "apply.record", `INSERT INTO plans(id,digest,scheduler_version,created_at) VALUES(?,?,?,?)`,
		plan.ID, digest[:], plan.Scheduler.Version, plan.CreatedAt.UTC().UnixNano()); err != nil {
		return false, fmt.Errorf("record plan: %w", err)
	}
	if err := s.commit(tx, "apply.commit"); err != nil {
		return false, fmt.Errorf("commit plan: %w", err)
	}
	return true, nil
}

func jsonOrDefault(value json.RawMessage, fallback string) []byte {
	if len(value) == 0 {
		return []byte(fallback)
	}
	return []byte(value)
}

type schedulingMetadata struct {
	SchemaVersion int              `json:"schema_version"`
	Repo          string           `json:"repo,omitempty"`
	Platform      domain.Platform  `json:"platform,omitempty"`
	Profile       domain.ProfileID `json:"profile,omitempty"`
	Route         domain.Route     `json:"route,omitempty"`
	Resources     domain.Resources `json:"resources,omitempty"`
	Demand        domain.DemandKey `json:"demand,omitempty"`
}

func encodeSchedulingMetadata(instance operations.Instance) ([]byte, error) {
	if !instance.SchedulingMetadataValid() {
		return nil, operations.ErrInvalid
	}
	version := 0
	if instance.Repo != "" {
		version = 1
	}
	encoded, _ := json.Marshal(schedulingMetadata{SchemaVersion: version, Repo: instance.Repo, Platform: instance.Platform,
		Profile: instance.Profile, Route: instance.Route, Resources: instance.Resources, Demand: instance.Demand})
	return encoded, nil
}

func decodeSchedulingMetadata(encoded []byte, instance *operations.Instance) error {
	var metadata schedulingMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return fmt.Errorf("decode scheduling metadata: %w", err)
	}
	if metadata.SchemaVersion != 0 && metadata.SchemaVersion != 1 {
		return fmt.Errorf("unsupported scheduling metadata version %d: %w", metadata.SchemaVersion, operations.ErrInvalid)
	}
	instance.Repo, instance.Platform, instance.Profile, instance.Route = metadata.Repo, metadata.Platform, metadata.Profile, metadata.Route
	instance.Resources, instance.Demand = metadata.Resources, metadata.Demand
	if !instance.SchedulingMetadataValid() {
		return operations.ErrInvalid
	}
	return nil
}

func insertOperation(ctx context.Context, tx *sql.Tx, operation operations.Operation, now time.Time) error {
	payload := []byte(operation.Payload)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		operation.ID, operation.IdempotencyKey, operation.EffectKey, operation.Kind, operation.ResourceID, payload, operations.OperationPending,
		operation.Attempts, operation.AvailableAt.UTC().UnixNano(), "", int64(0), operation.LastError, now.UTC().UnixNano(), now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("enqueue plan operation: %w", err)
	}
	return nil
}

func (s *Store) Migrate(ctx context.Context) error {
	for _, pragma := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA synchronous=FULL`, `PRAGMA foreign_keys=ON`, `PRAGMA busy_timeout=5000`} {
		if _, err := s.dbExec(ctx, "migrate.pragma", pragma); err != nil {
			return fmt.Errorf("configure sqlite: %w", err)
		}
	}
	tx, err := s.beginTx(ctx, "migrate.begin")
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.txExec(ctx, tx, "migrate.table", `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var version int
	if err := s.txRow(ctx, tx, "migrate.version", `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	versionOne := []string{
		`CREATE TABLE IF NOT EXISTS instances (
			id TEXT PRIMARY KEY,
			state TEXT NOT NULL,
			version INTEGER NOT NULL,
			drain_phase INTEGER NOT NULL DEFAULT 0,
			ownership BLOB NOT NULL,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS operations (
			id TEXT PRIMARY KEY,
			idempotency_key TEXT NOT NULL UNIQUE,
			effect_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			available_at INTEGER NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS operations_claimable ON operations(status, available_at, lease_until, created_at)`,
		`CREATE TABLE IF NOT EXISTS operation_effects (
			effect_key TEXT PRIMARY KEY,
			operation_id TEXT NOT NULL,
			recorded_at INTEGER NOT NULL,
			FOREIGN KEY(operation_id) REFERENCES operations(id)
		)`,
		`CREATE TABLE IF NOT EXISTS transition_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			version INTEGER NOT NULL,
			operation_id TEXT,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS leases (
			name TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			token INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ownership (
			resource_name TEXT PRIMARY KEY,
			metadata BLOB NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	}
	if version < 1 {
		for _, statement := range versionOne {
			if _, err := s.txExec(ctx, tx, "migrate.v1", statement); err != nil {
				return fmt.Errorf("migration 1: %w", err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v1.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(1, ?)`, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration 1: %w", err)
		}
	}
	if version < 2 {
		for _, statement := range []string{
			`CREATE TABLE plans (id TEXT PRIMARY KEY, digest BLOB NOT NULL, scheduler_version INTEGER NOT NULL, created_at INTEGER NOT NULL)`,
			`CREATE TABLE scheduler_state (singleton INTEGER PRIMARY KEY CHECK(singleton=1), version INTEGER NOT NULL, data BLOB NOT NULL, reservations BLOB NOT NULL, drr_state BLOB NOT NULL, observation_cursor TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
			`INSERT INTO scheduler_state(singleton,version,data,reservations,drr_state,observation_cursor,updated_at) VALUES(1,0,'{}','[]','{}','',0)`,
		} {
			if _, err := s.txExec(ctx, tx, "migrate.v2", statement); err != nil {
				return fmt.Errorf("migration 2: %w", err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v2.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(2, ?)`, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration 2: %w", err)
		}
	}
	if version < 3 {
		if _, err := s.txExec(ctx, tx, "migrate.v3", `CREATE TABLE operation_dependencies (
			operation_id TEXT NOT NULL,
			depends_on TEXT NOT NULL,
			PRIMARY KEY(operation_id, depends_on),
			FOREIGN KEY(operation_id) REFERENCES operations(id),
			FOREIGN KEY(depends_on) REFERENCES operations(id)
		)`); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v3.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(3, ?)`, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration 3: %w", err)
		}
	}
	if version < 4 {
		for _, statement := range []string{
			`CREATE TABLE scale_set_inbox (
				scale_set_id INTEGER NOT NULL,
				message_id INTEGER NOT NULL,
				digest BLOB NOT NULL,
				events BLOB NOT NULL,
				created_at INTEGER NOT NULL,
				PRIMARY KEY(scale_set_id,message_id)
			)`,
			`CREATE TABLE scale_set_cursors (
				scale_set_id INTEGER PRIMARY KEY,
				message_id INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE runner_demands (
				scale_set_id INTEGER NOT NULL,
				runner_request_id INTEGER NOT NULL,
				status TEXT NOT NULL,
				status_rank INTEGER NOT NULL,
				owner TEXT NOT NULL,
				repository TEXT NOT NULL,
				workflow_run_id INTEGER NOT NULL,
				job_id TEXT NOT NULL,
				event_name TEXT NOT NULL,
				labels BLOB NOT NULL,
				queue_time INTEGER NOT NULL,
				runner_id INTEGER NOT NULL,
				runner_name TEXT NOT NULL,
				result TEXT NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY(scale_set_id,runner_request_id)
			)`,
			`CREATE INDEX runner_demands_active ON runner_demands(scale_set_id,status_rank,runner_request_id)`,
		} {
			if _, err := s.txExec(ctx, tx, "migrate.v4", statement); err != nil {
				return fmt.Errorf("migration 4: %w", err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v4.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(4, ?)`, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration 4: %w", err)
		}
	}
	if version < 5 {
		if _, err := s.txExec(ctx, tx, "migrate.v5", `ALTER TABLE instances ADD COLUMN scheduling_metadata BLOB NOT NULL DEFAULT '{}'`); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v5.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(5, ?)`, time.Now().UTC().UnixNano()); err != nil {
			return fmt.Errorf("record migration 5: %w", err)
		}
	}
	if err := s.commit(tx, "migrate.commit"); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	var integrity string
	if err := s.dbRow(ctx, "migrate.quick", `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("sqlite quick_check: %s: %w", integrity, err)
	}
	return nil
}

func (s *Store) CreateInstance(ctx context.Context, instance operations.Instance) error {
	if instance.ID == "" || !operations.ValidState(instance.State) || !instance.Ownership.Valid() || !instance.SchedulingMetadataValid() {
		return operations.ErrInvalid
	}
	now := instance.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	metadata, _ := json.Marshal(instance.Ownership)
	schedulingJSON, _ := encodeSchedulingMetadata(instance)
	_, err := s.db.ExecContext(ctx, `INSERT INTO instances(id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		instance.ID, instance.State, instance.Version, instance.DrainPhase, metadata, schedulingJSON, instance.LastError, now.UnixNano(), now.UnixNano())
	if err != nil {
		return fmt.Errorf("create instance: %w", err)
	}
	return nil
}

func (s *Store) Instance(ctx context.Context, id string) (operations.Instance, error) {
	return scanInstance(s.db.QueryRowContext(ctx, `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, id))
}

func (s *Store) LiveInstances(ctx context.Context) ([]operations.Instance, error) {
	rows, err := s.dbQuery(ctx, "instances.live.query", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE state<>? ORDER BY id`, operations.StateDeleted)
	if err != nil {
		return nil, fmt.Errorf("list live instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []operations.Instance
	for rows.Next() {
		instance, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, instance)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live instances: %w", err)
	}
	return result, nil
}

// OperationCounts returns bounded aggregate telemetry without exposing payloads
// or coupling callers to the operations table schema.
func (s *Store) OperationCounts(ctx context.Context) (retrying, dead int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN attempts>0 AND status NOT IN (?,?) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0)
		FROM operations`, operations.OperationCompleted, operations.OperationDead, operations.OperationDead).Scan(&retrying, &dead)
	if err != nil {
		return 0, 0, fmt.Errorf("summarize operations: %w", err)
	}
	return retrying, dead, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanInstance(row rowScanner) (operations.Instance, error) {
	var instance operations.Instance
	var state string
	var metadata, schedulingJSON []byte
	var createdAt, updatedAt int64
	err := row.Scan(&instance.ID, &state, &instance.Version, &instance.DrainPhase, &metadata, &schedulingJSON, &instance.LastError, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Instance{}, operations.ErrNotFound
	}
	if err != nil {
		return operations.Instance{}, fmt.Errorf("scan instance: %w", err)
	}
	if err := json.Unmarshal(metadata, &instance.Ownership); err != nil {
		return operations.Instance{}, fmt.Errorf("decode ownership: %w", err)
	}
	if err := decodeSchedulingMetadata(schedulingJSON, &instance); err != nil {
		return operations.Instance{}, err
	}
	instance.State = operations.State(state)
	instance.CreatedAt = fromNanos(createdAt)
	instance.UpdatedAt = fromNanos(updatedAt)
	return instance, nil
}

func (s *Store) Transition(ctx context.Context, transition operations.Transition) (operations.Instance, operations.Operation, error) {
	if transition.InstanceID == "" || !operations.ValidState(transition.ExpectedState) ||
		!operations.ValidState(transition.NextState) || !transition.ExpectedState.CanTransitionTo(transition.NextState) || !transition.Operation.Valid() {
		return operations.Instance{}, operations.Operation{}, operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "transition.begin")
	if err != nil {
		return operations.Instance{}, operations.Operation{}, fmt.Errorf("begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existing, found, err := s.operationByKey(ctx, tx, transition.Operation.IdempotencyKey); err != nil {
		return operations.Instance{}, operations.Operation{}, err
	} else if found {
		instance, err := scanInstance(s.txRow(ctx, tx, "transition.idempotent.instance", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, transition.InstanceID))
		if err != nil {
			return operations.Instance{}, operations.Operation{}, err
		}
		if instance.State != transition.NextState || instance.Version < transition.ExpectedVersion+1 {
			return operations.Instance{}, operations.Operation{}, operations.ErrConflict
		}
		return instance, existing, nil
	}

	now := transition.Operation.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := s.txExec(ctx, tx, "transition.instance", `UPDATE instances SET state=?,version=version+1,drain_phase=?,last_error=?,updated_at=? WHERE id=? AND state=? AND version=?`,
		transition.NextState, transition.DrainPhase, transition.LastError, now.UnixNano(), transition.InstanceID, transition.ExpectedState, transition.ExpectedVersion)
	if err != nil {
		return operations.Instance{}, operations.Operation{}, fmt.Errorf("transition instance: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return operations.Instance{}, operations.Operation{}, operations.ErrConflict
	}
	operation := transition.Operation
	operation.Status = operations.OperationPending
	operation.CreatedAt = now
	operation.UpdatedAt = now
	payload := []byte(operation.Payload)
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	_, err = s.txExec(ctx, tx, "transition.operation", `INSERT INTO operations(id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		operation.ID, operation.IdempotencyKey, operation.EffectKey, operation.Kind, operation.ResourceID, payload, operation.Status,
		operation.Attempts, operation.AvailableAt.UTC().UnixNano(), "", int64(0), "", now.UnixNano(), now.UnixNano())
	if err != nil {
		return operations.Instance{}, operations.Operation{}, fmt.Errorf("enqueue operation: %w", err)
	}
	for _, dependency := range operation.DependsOn {
		var present int
		if err := s.txRow(ctx, tx, "transition.dependency.check", `SELECT COUNT(*) FROM operations WHERE id=?`, dependency).Scan(&present); err != nil {
			return operations.Instance{}, operations.Operation{}, fmt.Errorf("validate operation dependency: %w", err)
		}
		if present != 1 {
			return operations.Instance{}, operations.Operation{}, operations.ErrInvalid
		}
		if _, err := s.txExec(ctx, tx, "transition.dependency.record", `INSERT INTO operation_dependencies(operation_id,depends_on) VALUES(?,?)`, operation.ID, dependency); err != nil {
			return operations.Instance{}, operations.Operation{}, fmt.Errorf("record operation dependency: %w", err)
		}
	}
	_, err = s.txExec(ctx, tx, "transition.history", `INSERT INTO transition_history(instance_id,from_state,to_state,version,operation_id,created_at) VALUES(?,?,?,?,?,?)`,
		transition.InstanceID, transition.ExpectedState, transition.NextState, transition.ExpectedVersion+1, operation.ID, now.UnixNano())
	if err != nil {
		return operations.Instance{}, operations.Operation{}, fmt.Errorf("record transition: %w", err)
	}
	instance, err := scanInstance(s.txRow(ctx, tx, "transition.result", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, transition.InstanceID))
	if err != nil {
		return operations.Instance{}, operations.Operation{}, err
	}
	if err := s.commit(tx, "transition.commit"); err != nil {
		return operations.Instance{}, operations.Operation{}, fmt.Errorf("commit transition: %w", err)
	}
	return instance, operation, nil
}

func (s *Store) operationByKey(ctx context.Context, tx *sql.Tx, key string) (operations.Operation, bool, error) {
	operation, err := scanOperation(s.txRow(ctx, tx, "transition.operation.load", `SELECT id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at FROM operations WHERE idempotency_key=?`, key))
	if errors.Is(err, operations.ErrNotFound) {
		return operations.Operation{}, false, nil
	}
	return operation, err == nil, err
}

func (s *Store) Claim(ctx context.Context, owner string, limit int, now time.Time, leaseFor time.Duration) ([]operations.Operation, error) {
	if owner == "" || limit <= 0 || leaseFor <= 0 {
		return nil, operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "claim.begin")
	if err != nil {
		return nil, fmt.Errorf("begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := s.txExec(ctx, tx, "claim.propagate", `WITH RECURSIVE doomed(id) AS (
			SELECT dependency.operation_id FROM operation_dependencies dependency
			JOIN operations prerequisite ON prerequisite.id=dependency.depends_on
			WHERE prerequisite.status=?
			UNION
			SELECT dependency.operation_id FROM operation_dependencies dependency
			JOIN doomed ON doomed.id=dependency.depends_on
		)
		UPDATE operations SET status=?,last_error='dependency dead',lease_owner='',lease_until=0,updated_at=?
		WHERE status=? AND id IN (SELECT id FROM doomed)`, operations.OperationDead, operations.OperationDead, now.UTC().UnixNano(), operations.OperationPending); err != nil {
		return nil, fmt.Errorf("propagate dead dependency: %w", err)
	}
	rows, err := s.txQuery(ctx, tx, "claim.query", `SELECT operation.id FROM operations operation
		WHERE operation.available_at<=? AND (operation.status=? OR (operation.status=? AND operation.lease_until<=?))
		AND NOT EXISTS (
			SELECT 1 FROM operation_dependencies dependency
			JOIN operations prerequisite ON prerequisite.id=dependency.depends_on
			WHERE dependency.operation_id=operation.id AND prerequisite.status<>?
		)
		ORDER BY operation.created_at,operation.id LIMIT ?`,
		now.UTC().UnixNano(), operations.OperationPending, operations.OperationClaimed, now.UTC().UnixNano(), operations.OperationCompleted, limit)
	if err != nil {
		return nil, fmt.Errorf("select claimable: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan claimable: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close claim rows: %w", err)
	}
	claimed := make([]operations.Operation, 0, len(ids))
	for _, id := range ids {
		result, err := s.txExec(ctx, tx, "claim.update", `UPDATE operations SET status=?,lease_owner=?,lease_until=?,updated_at=? WHERE id=? AND (status=? OR (status=? AND lease_until<=?))`,
			operations.OperationClaimed, owner, now.Add(leaseFor).UTC().UnixNano(), now.UTC().UnixNano(), id,
			operations.OperationPending, operations.OperationClaimed, now.UTC().UnixNano())
		if err != nil {
			return nil, fmt.Errorf("claim operation: %w", err)
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			continue
		}
		operation, err := scanOperation(s.txRow(ctx, tx, "claim.load", `SELECT id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,lease_owner,lease_until,last_error,created_at,updated_at FROM operations WHERE id=?`, id))
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, operation)
	}
	if err := s.commit(tx, "claim.commit"); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return claimed, nil
}

func (s *Store) RenewOperation(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) error {
	if id == "" || owner == "" || leaseFor <= 0 {
		return operations.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET lease_until=?,updated_at=? WHERE id=? AND status=? AND lease_owner=? AND lease_until>?`,
		now.Add(leaseFor).UTC().UnixNano(), now.UTC().UnixNano(), id, operations.OperationClaimed, owner, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("renew operation: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return operations.ErrLeaseLost
	}
	return nil
}

func (s *Store) Complete(ctx context.Context, id, owner, effectKey string, now time.Time) (bool, error) {
	tx, err := s.beginTx(ctx, "complete.begin")
	if err != nil {
		return false, fmt.Errorf("begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status, leaseOwner, storedEffect string
	err = s.txRow(ctx, tx, "complete.load", `SELECT status,lease_owner,effect_key FROM operations WHERE id=?`, id).Scan(&status, &leaseOwner, &storedEffect)
	if errors.Is(err, sql.ErrNoRows) {
		return false, operations.ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("load completion: %w", err)
	}
	if storedEffect != effectKey {
		return false, operations.ErrConflict
	}
	if status == string(operations.OperationCompleted) {
		return false, nil
	}
	if status != string(operations.OperationClaimed) || leaseOwner != owner {
		return false, operations.ErrLeaseLost
	}
	result, err := s.txExec(ctx, tx, "complete.effect", `INSERT OR IGNORE INTO operation_effects(effect_key,operation_id,recorded_at) VALUES(?,?,?)`, effectKey, id, now.UTC().UnixNano())
	if err != nil {
		return false, fmt.Errorf("record effect: %w", err)
	}
	inserted, _ := result.RowsAffected()
	if _, err := s.txExec(ctx, tx, "complete.update", `UPDATE operations SET status=?,lease_owner='',lease_until=0,updated_at=? WHERE id=?`, operations.OperationCompleted, now.UTC().UnixNano(), id); err != nil {
		return false, fmt.Errorf("complete operation: %w", err)
	}
	if err := s.commit(tx, "complete.commit"); err != nil {
		return false, fmt.Errorf("commit completion: %w", err)
	}
	return inserted == 1, nil
}

func (s *Store) Retry(ctx context.Context, id, owner, message string, availableAt time.Time, dead bool) error {
	status := operations.OperationPending
	if dead {
		status = operations.OperationDead
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?,attempts=attempts+1,available_at=?,lease_owner='',lease_until=0,last_error=?,updated_at=? WHERE id=? AND status=? AND lease_owner=?`,
		status, availableAt.UTC().UnixNano(), message, time.Now().UTC().UnixNano(), id, operations.OperationClaimed, owner)
	if err != nil {
		return fmt.Errorf("retry operation: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return operations.ErrLeaseLost
	}
	return nil
}

func (s *Store) RecoverExpired(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?,lease_owner='',lease_until=0,updated_at=? WHERE status=? AND lease_until<=?`,
		operations.OperationPending, now.UTC().UnixNano(), operations.OperationClaimed, now.UTC().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("recover operations: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) AcquireLease(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (operations.Lease, error) {
	if name == "" || owner == "" || ttl <= 0 {
		return operations.Lease{}, operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "lease.begin")
	if err != nil {
		return operations.Lease{}, fmt.Errorf("begin lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var currentOwner string
	var token, expires int64
	err = s.txRow(ctx, tx, "lease.load", `SELECT owner,token,expires_at FROM leases WHERE name=?`, name).Scan(&currentOwner, &token, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		token = 1
		_, err = s.txExec(ctx, tx, "lease.insert", `INSERT INTO leases(name,owner,token,expires_at) VALUES(?,?,?,?)`, name, owner, token, now.Add(ttl).UTC().UnixNano())
	} else if err == nil && (expires <= now.UTC().UnixNano() || currentOwner == owner) {
		token++
		_, err = s.txExec(ctx, tx, "lease.update", `UPDATE leases SET owner=?,token=?,expires_at=? WHERE name=? AND token=?`, owner, token, now.Add(ttl).UTC().UnixNano(), name, token-1)
	} else if err == nil {
		return operations.Lease{}, operations.ErrLeaseHeld
	}
	if err != nil {
		return operations.Lease{}, fmt.Errorf("acquire lease: %w", err)
	}
	if err := s.commit(tx, "lease.commit"); err != nil {
		return operations.Lease{}, fmt.Errorf("commit lease: %w", err)
	}
	return operations.Lease{Name: name, Owner: owner, Token: token, ExpiresAt: now.Add(ttl).UTC()}, nil
}

func (s *Store) RenewLease(ctx context.Context, lease operations.Lease, now time.Time, ttl time.Duration) (operations.Lease, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE leases SET expires_at=? WHERE name=? AND owner=? AND token=? AND expires_at>?`,
		now.Add(ttl).UTC().UnixNano(), lease.Name, lease.Owner, lease.Token, now.UTC().UnixNano())
	if err != nil {
		return operations.Lease{}, fmt.Errorf("renew lease: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return operations.Lease{}, operations.ErrLeaseLost
	}
	lease.ExpiresAt = now.Add(ttl).UTC()
	return lease, nil
}

func (s *Store) ReleaseLease(ctx context.Context, lease operations.Lease) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE name=? AND owner=? AND token=?`, lease.Name, lease.Owner, lease.Token)
	if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return operations.ErrLeaseLost
	}
	return nil
}

func (s *Store) PutOwnership(ctx context.Context, resource string, ownership operations.Ownership) error {
	if resource == "" || !ownership.Valid() {
		return operations.ErrInvalid
	}
	metadata, _ := json.Marshal(ownership)
	var err error
	var existing []byte
	err = s.db.QueryRowContext(ctx, `SELECT metadata FROM ownership WHERE resource_name=?`, resource).Scan(&existing)
	if err == nil {
		var current operations.Ownership
		if json.Unmarshal(existing, &current) != nil || current != ownership {
			return operations.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load ownership: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO ownership(resource_name,metadata,updated_at) VALUES(?,?,?)`, resource, metadata, time.Now().UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("put ownership: %w", err)
	}
	return nil
}

func (s *Store) Ownership(ctx context.Context, resource string) (operations.Ownership, error) {
	var metadata []byte
	if err := s.db.QueryRowContext(ctx, `SELECT metadata FROM ownership WHERE resource_name=?`, resource).Scan(&metadata); errors.Is(err, sql.ErrNoRows) {
		return operations.Ownership{}, operations.ErrNotFound
	} else if err != nil {
		return operations.Ownership{}, fmt.Errorf("load ownership: %w", err)
	}
	var ownership operations.Ownership
	if err := json.Unmarshal(metadata, &ownership); err != nil {
		return operations.Ownership{}, fmt.Errorf("decode ownership: %w", err)
	}
	return ownership, nil
}

func scanOperation(row rowScanner) (operations.Operation, error) {
	var operation operations.Operation
	var status string
	var payload []byte
	var availableAt, leaseUntil, createdAt, updatedAt int64
	err := row.Scan(&operation.ID, &operation.IdempotencyKey, &operation.EffectKey, &operation.Kind, &operation.ResourceID,
		&payload, &status, &operation.Attempts, &availableAt, &operation.LeaseOwner, &leaseUntil, &operation.LastError, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operations.Operation{}, operations.ErrNotFound
	}
	if err != nil {
		return operations.Operation{}, fmt.Errorf("scan operation: %w", err)
	}
	operation.Payload = append(operation.Payload[:0], payload...)
	operation.Status = operations.OperationStatus(status)
	operation.AvailableAt = fromNanos(availableAt)
	operation.LeaseUntil = fromNanos(leaseUntil)
	operation.CreatedAt = fromNanos(createdAt)
	operation.UpdatedAt = fromNanos(updatedAt)
	return operation, nil
}

func fromNanos(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}
