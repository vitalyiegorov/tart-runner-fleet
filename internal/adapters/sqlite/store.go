package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	driver "modernc.org/sqlite"
)

// sqliteConstraintCode is SQLite's primary result code for every constraint
// violation. The extended code names the specific constraint in its high bits
// (UNIQUE 2067, PRIMARY KEY 1555, FOREIGN KEY 787, NOT NULL 1299, CHECK 275), so
// the low byte is the stable classification and new extended codes cannot break
// it.
const sqliteConstraintCode = 19

// refusedByConstraint reports whether the durable layer refused a write because
// the plan violated the schema rather than because the store was unavailable.
//
// The distinction is the operator's whole triage. A refused constraint repeats
// identically for as long as the inputs do — the planner is pure, so the same
// tick inputs rebuild the same rejected plan forever — while an unavailable
// store clears on its own. Before this predicate both reported plan_commit_failed
// ("check the database"), and during the 2026-08-02 wedge that sent triage to a
// database whose integrity_check was clean.
func refusedByConstraint(err error) bool {
	var refusal *driver.Error
	return errors.As(err, &refusal) && refusal.Code()&0xFF == sqliteConstraintCode
}

// refuseMalformed marks a constraint violation as a plan the durable layer will
// not accept, leaving every other failure exactly as the driver reported it. The
// original error is preserved alongside the sentinel so the constraint that was
// violated stays readable in the wrapped chain.
func refuseMalformed(err error) error {
	if !refusedByConstraint(err) {
		return err
	}
	return fmt.Errorf("%w: %w", operations.ErrInvalid, err)
}

type Store struct {
	db          *sql.DB
	clock       func() time.Time
	injectFault func(string) error
	injectRows  func(string) rowsScanner
}

// Option configures a Store at open time.
type Option func(*Store)

// WithClock replaces the clock every durable timestamp this package writes on
// its own initiative is stamped from.
//
// AGENTS.md rule 3 -- "Time, I/O, randomness, and process execution enter
// through interfaces" -- and this store is the last durable writer that did not
// meet it. Production passes nothing and keeps time.Now, so the two clocks are
// the same one there and always were; the deterministic simulation (ADR 0031)
// passes its virtual clock, and until it could, an operation this store stamped
// was thirteen real days in that world's future and therefore unclaimable in it
// forever (issue #249). A nil clock is not a clock and leaves the wall clock in
// place.
func WithClock(now func() time.Time) Option {
	return func(store *Store) {
		if now != nil {
			store.clock = now
		}
	}
}

// instant is the one place this package reads the time. Every timestamp it
// writes without being handed one comes from here.
func (s *Store) instant() time.Time { return s.clock().UTC() }

// legacyStageDeregisterError is the exact redacted value persisted by
// releases before migration 6. Keep it immutable: deriving this predicate
// from future error formatting could broaden a one-shot data repair.
const (
	legacyStageDeregisterError = "runner lifecycle failed at deregister"
	legacyStageAcquireJITError = "runner lifecycle failed at acquire_jit"
)

// seedSchedulerState idempotently restores the scheduler_state singleton to its
// cold-start values. INSERT OR IGNORE leaves any existing row untouched, so the
// same statement is safe both on every open and as a runtime repair after an
// operator deleted the row. Version 0 with empty reservation/DRR is optimization
// state the scheduler rebuilds from durable demand — never authoritative data.
const seedSchedulerState = `INSERT OR IGNORE INTO scheduler_state(singleton,version,data,reservations,drr_state,observation_cursor,updated_at) VALUES(1,0,'{}','[]','{}','',0)`

func Open(ctx context.Context, path string, options ...Option) (*Store, error) {
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
	store := &Store{db: db, clock: time.Now}
	for _, option := range options {
		option(store)
	}
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
	if errors.Is(err, sql.ErrNoRows) {
		return operations.SchedulerState{}, operations.ErrSchedulerStateMissing
	}
	if err != nil {
		return operations.SchedulerState{}, fmt.Errorf("load scheduler state: %w", err)
	}
	state.Data = append(state.Data, data...)
	state.Reservations = append(state.Reservations, reservations...)
	state.DeficitRoundRobin = append(state.DeficitRoundRobin, drr...)
	return state, nil
}

// ReseedSchedulerState performs a bounded, idempotent cold-start repair of the
// scheduler_state singleton after it was lost (e.g. an operator DELETE). It
// deliberately repairs nothing else: instance and operation rows are
// authoritative and are never synthesized here.
func (s *Store) ReseedSchedulerState(ctx context.Context) error {
	if _, err := s.dbExec(ctx, "scheduler.reseed", seedSchedulerState); err != nil {
		return fmt.Errorf("reseed scheduler state: %w", err)
	}
	return nil
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
			return false, fmt.Errorf("apply instance intent: %w", refuseMalformed(err))
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
				return false, fmt.Errorf("record operation dependency: %w", refuseMalformed(err))
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
		return false, fmt.Errorf("record plan: %w", refuseMalformed(err))
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
		return fmt.Errorf("enqueue plan operation: %w", refuseMalformed(err))
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
		if _, err := s.txExec(ctx, tx, "migrate.v1.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(1, ?)`, s.instant().UnixNano()); err != nil {
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
		if _, err := s.txExec(ctx, tx, "migrate.v2.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(2, ?)`, s.instant().UnixNano()); err != nil {
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
		if _, err := s.txExec(ctx, tx, "migrate.v3.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(3, ?)`, s.instant().UnixNano()); err != nil {
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
		if _, err := s.txExec(ctx, tx, "migrate.v4.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(4, ?)`, s.instant().UnixNano()); err != nil {
			return fmt.Errorf("record migration 4: %w", err)
		}
	}
	if version < 5 {
		if _, err := s.txExec(ctx, tx, "migrate.v5", `ALTER TABLE instances ADD COLUMN scheduling_metadata BLOB NOT NULL DEFAULT '{}'`); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v5.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(5, ?)`, s.instant().UnixNano()); err != nil {
			return fmt.Errorf("record migration 5: %w", err)
		}
	}
	if version < 6 {
		now := s.instant().UnixNano()
		// v0.1.69 could dead-letter a drain when an ephemeral JIT runner
		// disappeared between the runner lookup and GitHub's DELETE. Revive
		// only that bounded, redacted failure class and only while its owned
		// instance is still in the exact draining state. Recording migration 6
		// in the same transaction makes this recovery one-shot across restarts.
		if _, err := s.txExec(ctx, tx, "migrate.v6", `UPDATE operations
			SET status=?,attempts=0,available_at=?,lease_owner='',lease_until=0,last_error='',updated_at=?
			WHERE status=? AND kind=? AND last_error=?
			AND effect_key=kind||':'||resource_id
			AND NOT EXISTS (SELECT 1 FROM operation_effects WHERE operation_effects.operation_id=operations.id)
			AND EXISTS (
				SELECT 1 FROM instances WHERE instances.id=operations.resource_id AND instances.state=? AND instances.drain_phase=1
			)`, operations.OperationPending, now, now, operations.OperationDead, lifecycle.OperationDrain,
			legacyStageDeregisterError, operations.StateDraining); err != nil {
			return fmt.Errorf("migration 6: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v6.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(6, ?)`, now); err != nil {
			return fmt.Errorf("record migration 6: %w", err)
		}
	}
	if version < 7 {
		now := s.instant().UnixNano()
		// v0.1.71 still used the generic five-attempt budget for drain
		// operations. A transient scale-set observation could therefore
		// exhaust cleanup before GitHub converged even though the ephemeral
		// runner was already absent. Requeue only that exact, effect-free,
		// owned draining state; migration 7 is transactional and one-shot.
		if _, err := s.txExec(ctx, tx, "migrate.v7", `UPDATE operations
			SET status=?,attempts=0,available_at=?,lease_owner='',lease_until=0,last_error='',updated_at=?
			WHERE status=? AND kind=? AND last_error=?
			AND effect_key=kind||':'||resource_id
			AND NOT EXISTS (SELECT 1 FROM operation_effects WHERE operation_effects.operation_id=operations.id)
			AND EXISTS (
				SELECT 1 FROM instances WHERE instances.id=operations.resource_id AND instances.state=? AND instances.drain_phase=1
			)`, operations.OperationPending, now, now, operations.OperationDead, lifecycle.OperationDrain,
			legacyStageDeregisterError, operations.StateDraining); err != nil {
			return fmt.Errorf("migration 7: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v7.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(7, ?)`, now); err != nil {
			return fmt.Errorf("record migration 7: %w", err)
		}
	}
	if version < 8 {
		now := s.instant().UnixNano()
		if _, err := s.txExec(ctx, tx, "migrate.v8", `UPDATE operations
			SET status=?,attempts=0,available_at=?,lease_owner='',lease_until=0,last_error='',updated_at=?
			WHERE status=? AND kind=? AND last_error=?
			AND effect_key=kind||':'||resource_id
			AND NOT EXISTS (SELECT 1 FROM operation_effects WHERE operation_effects.operation_id=operations.id)
			AND EXISTS (
				SELECT 1 FROM instances WHERE instances.id=operations.resource_id AND instances.state=? AND instances.drain_phase=1
			)`, operations.OperationPending, now, now, operations.OperationDead, lifecycle.OperationDrain,
			legacyStageDeregisterError, operations.StateDraining); err != nil {
			return fmt.Errorf("migration 8: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v8.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(8, ?)`, now); err != nil {
			return fmt.Errorf("record migration 8: %w", err)
		}
	}
	if version < 9 {
		now := s.instant().UnixNano()
		// Releases through v0.1.104 could exhaust the generic five-attempt
		// provision budget while acquiring a JIT configuration. No external
		// effect had been recorded, but the owned Tart VM was already reachable.
		// Repair only that exact redacted signature. The revived provision effect
		// gates cleanup, so the worker first settles any ambiguous GitHub state and
		// then removes the orphan without creating a replacement.
		match := `o.status=? AND o.kind=? AND o.last_error=?
			AND o.effect_key=o.kind||':'||o.resource_id
			AND NOT EXISTS (SELECT 1 FROM operation_effects e WHERE e.operation_id=o.id)
			AND i.id=o.resource_id AND i.state=?
			AND json_extract(i.ownership,'$.controller_id')='tart-runner-fleet'
			AND json_extract(i.ownership,'$.operation_id')=o.id`
		steps := []struct {
			point string
			query string
			args  []any
		}{
			{point: "drain", query: `INSERT INTO operations(
				id,idempotency_key,effect_key,kind,resource_id,payload,status,attempts,available_at,
				lease_owner,lease_until,last_error,created_at,updated_at)
				SELECT 'recovery-drain-'||i.id,'recovery-drain-'||i.id,'deregister:'||i.id,?,i.id,'{}',?,0,?,'',0,'',?,?
				FROM operations o JOIN instances i ON i.id=o.resource_id WHERE ` + match,
				args: []any{lifecycle.OperationDrain, operations.OperationPending, now, now, now,
					operations.OperationDead, lifecycle.OperationProvision, legacyStageAcquireJITError, operations.StateReachable}},
			{point: "dependency", query: `INSERT INTO operation_dependencies(operation_id,depends_on)
				SELECT 'recovery-drain-'||i.id,o.id FROM operations o JOIN instances i ON i.id=o.resource_id WHERE ` + match,
				args: []any{operations.OperationDead, lifecycle.OperationProvision, legacyStageAcquireJITError, operations.StateReachable}},
			{point: "history", query: `INSERT INTO transition_history(instance_id,from_state,to_state,version,operation_id,created_at)
				SELECT i.id,i.state,?,i.version+1,'recovery-drain-'||i.id,? FROM operations o JOIN instances i ON i.id=o.resource_id WHERE ` + match,
				args: []any{operations.StateDraining, now, operations.OperationDead, lifecycle.OperationProvision,
					legacyStageAcquireJITError, operations.StateReachable}},
			{point: "instance", query: `UPDATE instances SET state=?,version=version+1,drain_phase=1,updated_at=?
				WHERE state=? AND json_extract(ownership,'$.controller_id')='tart-runner-fleet'
				AND EXISTS (SELECT 1 FROM operations o WHERE o.id=json_extract(instances.ownership,'$.operation_id')
					AND o.resource_id=instances.id AND o.status=? AND o.kind=? AND o.last_error=?
					AND o.effect_key=o.kind||':'||o.resource_id
					AND NOT EXISTS (SELECT 1 FROM operation_effects e WHERE e.operation_id=o.id))`,
				args: []any{operations.StateDraining, now, operations.StateReachable, operations.OperationDead,
					lifecycle.OperationProvision, legacyStageAcquireJITError}},
			{point: "provision", query: `UPDATE operations
				SET status=?,attempts=0,available_at=?,lease_owner='',lease_until=0,last_error='',updated_at=?
				WHERE status=? AND kind=? AND last_error=? AND effect_key=kind||':'||resource_id
				AND NOT EXISTS (SELECT 1 FROM operation_effects e WHERE e.operation_id=operations.id)
				AND EXISTS (SELECT 1 FROM operation_dependencies d
					WHERE d.operation_id='recovery-drain-'||operations.resource_id AND d.depends_on=operations.id)`,
				args: []any{operations.OperationPending, now, now, operations.OperationDead, lifecycle.OperationProvision,
					legacyStageAcquireJITError}},
			{point: "record", query: `INSERT INTO schema_migrations(version, applied_at) VALUES(9, ?)`, args: []any{now}},
		}
		for _, step := range steps {
			if _, err := s.txExec(ctx, tx, "migrate.v9."+step.point, step.query, step.args...); err != nil {
				return fmt.Errorf("migration 9 %s: %w", step.point, err)
			}
		}
	}
	if version < 10 {
		now := s.instant().UnixNano()
		for _, column := range []struct {
			name  string
			query string
		}{
			{"display_name", `ALTER TABLE runner_demands ADD COLUMN display_name TEXT NOT NULL DEFAULT ''`},
			{"workflow_ref", `ALTER TABLE runner_demands ADD COLUMN workflow_ref TEXT NOT NULL DEFAULT ''`},
			{"logical_key", `ALTER TABLE runner_demands ADD COLUMN logical_key TEXT NOT NULL DEFAULT ''`},
			{"first_queue_time", `ALTER TABLE runner_demands ADD COLUMN first_queue_time INTEGER NOT NULL DEFAULT 0`},
			{"workflow_job_id", `ALTER TABLE runner_demands ADD COLUMN workflow_job_id INTEGER NOT NULL DEFAULT 0`},
			{"run_attempt", `ALTER TABLE runner_demands ADD COLUMN run_attempt INTEGER NOT NULL DEFAULT 0`},
		} {
			var present int
			if err := s.txRow(ctx, tx, "migrate.v10.column."+column.name,
				`SELECT COUNT(*) FROM pragma_table_info('runner_demands') WHERE name=?`, column.name).Scan(&present); err != nil {
				return fmt.Errorf("inspect migration 10 column %s: %w", column.name, err)
			}
			if present == 0 {
				if _, err := s.txExec(ctx, tx, "migrate.v10."+column.name, column.query); err != nil {
					return fmt.Errorf("migration 10 column %s: %w", column.name, err)
				}
			}
		}
		for _, step := range []struct {
			point string
			query string
		}{
			{"groups", `CREATE TABLE IF NOT EXISTS demand_groups (
				scale_set_id INTEGER NOT NULL, logical_key TEXT NOT NULL, first_queue_time INTEGER NOT NULL,
				workflow_job_id INTEGER NOT NULL DEFAULT 0, run_attempt INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
				PRIMARY KEY(scale_set_id,logical_key)
			)`},
			{"statistics", `CREATE TABLE IF NOT EXISTS scale_set_statistics (
				scale_set_id INTEGER PRIMARY KEY, message_id INTEGER NOT NULL, available INTEGER NOT NULL, acquired INTEGER NOT NULL,
				assigned INTEGER NOT NULL, running INTEGER NOT NULL, registered INTEGER NOT NULL, busy INTEGER NOT NULL,
				idle INTEGER NOT NULL, observed_at INTEGER NOT NULL
			)`},
			{"github-jobs", `CREATE TABLE IF NOT EXISTS github_job_observations (
				scale_set_id INTEGER NOT NULL, workflow_job_id INTEGER NOT NULL, owner TEXT NOT NULL, repository TEXT NOT NULL,
				workflow_run_id INTEGER NOT NULL, run_attempt INTEGER NOT NULL, display_name TEXT NOT NULL, workflow_ref TEXT NOT NULL,
				labels BLOB NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, queue_time_exact INTEGER NOT NULL DEFAULT 0,
				observed_at INTEGER NOT NULL,
				PRIMARY KEY(scale_set_id,workflow_job_id)
			)`},
			{"active-order", `CREATE INDEX IF NOT EXISTS runner_demands_logical ON runner_demands(scale_set_id,logical_key,status_rank,first_queue_time)`},
		} {
			if _, err := s.txExec(ctx, tx, "migrate.v10."+step.point, step.query); err != nil {
				return fmt.Errorf("migration 10 %s: %w", step.point, err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v10.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(10, ?)`, now); err != nil {
			return fmt.Errorf("record migration 10: %w", err)
		}
	}
	if version < 11 {
		now := s.instant().UnixNano()
		var present int
		if err := s.txRow(ctx, tx, "migrate.v11.queue-time-exact",
			`SELECT COUNT(*) FROM pragma_table_info('github_job_observations') WHERE name='queue_time_exact'`).Scan(&present); err != nil {
			return fmt.Errorf("inspect migration 11 queue_time_exact: %w", err)
		}
		if present == 0 {
			if _, err := s.txExec(ctx, tx, "migrate.v11.add-queue-time-exact",
				`ALTER TABLE github_job_observations ADD COLUMN queue_time_exact INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("migration 11 queue_time_exact: %w", err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v11.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(11, ?)`, now); err != nil {
			return fmt.Errorf("record migration 11: %w", err)
		}
	}
	if version < 12 {
		now := s.instant().UnixNano()
		// Ghost demand evidence (issue #113). GitHub kept advertising one
		// acquirable job for 11 hours after the backing job was cancelled by a
		// force-push, so the broker alone can never retract queued demand.
		// corroborated_at/absent_since/absent_observations record what complete
		// REST snapshots saw, and expired_at is the revocable conclusion drawn
		// from them; all four default to zero, which means "no evidence" and
		// therefore "never expire".
		for _, column := range []struct {
			name  string
			query string
		}{
			{"corroborated_at", `ALTER TABLE runner_demands ADD COLUMN corroborated_at INTEGER NOT NULL DEFAULT 0`},
			{"absent_since", `ALTER TABLE runner_demands ADD COLUMN absent_since INTEGER NOT NULL DEFAULT 0`},
			{"absent_observations", `ALTER TABLE runner_demands ADD COLUMN absent_observations INTEGER NOT NULL DEFAULT 0`},
			{"expired_at", `ALTER TABLE runner_demands ADD COLUMN expired_at INTEGER NOT NULL DEFAULT 0`},
		} {
			var present int
			if err := s.txRow(ctx, tx, "migrate.v12.column."+column.name,
				`SELECT COUNT(*) FROM pragma_table_info('runner_demands') WHERE name=?`, column.name).Scan(&present); err != nil {
				return fmt.Errorf("inspect migration 12 column %s: %w", column.name, err)
			}
			if present == 0 {
				if _, err := s.txExec(ctx, tx, "migrate.v12."+column.name, column.query); err != nil {
					return fmt.Errorf("migration 12 column %s: %w", column.name, err)
				}
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v12.snapshots", `CREATE TABLE IF NOT EXISTS github_job_snapshots (
			scale_set_id INTEGER PRIMARY KEY, observed_at INTEGER NOT NULL, job_count INTEGER NOT NULL
		)`); err != nil {
			return fmt.Errorf("migration 12 snapshots: %w", err)
		}
		if _, err := s.txExec(ctx, tx, "migrate.v12.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(12, ?)`, now); err != nil {
			return fmt.Errorf("record migration 12: %w", err)
		}
	}
	if version < 13 {
		now := s.instant().UnixNano()
		// Broker message-id sequences are not unique for the life of a database
		// (issue #165). GitHub restarted the sequence for scale set
		// 8077185082566234948 on 2026-08-01T18:32Z, every redelivered id collided
		// with a July row, and the binding refused every message for three days.
		// The idempotency key gains the generation the row was ingested under, so
		// a retired sequence can never collide with the live one again. Existing
		// rows are generation zero: they were all ingested under one sequence.
		var generation int
		if err := s.txRow(ctx, tx, "migrate.v13.inbox-generation",
			`SELECT COUNT(*) FROM pragma_table_info('scale_set_inbox') WHERE name='generation'`).Scan(&generation); err != nil {
			return fmt.Errorf("inspect migration 13 inbox generation: %w", err)
		}
		if generation == 0 {
			// The primary key itself changes, which SQLite expresses as a rebuild.
			for _, step := range []struct{ point, query string }{
				{"create", `CREATE TABLE scale_set_inbox_v13 (
					scale_set_id INTEGER NOT NULL,
					generation INTEGER NOT NULL DEFAULT 0,
					message_id INTEGER NOT NULL,
					digest BLOB NOT NULL,
					events BLOB NOT NULL,
					created_at INTEGER NOT NULL,
					PRIMARY KEY(scale_set_id,generation,message_id)
				)`},
				{"copy", `INSERT INTO scale_set_inbox_v13(scale_set_id,generation,message_id,digest,events,created_at)
					SELECT scale_set_id,0,message_id,digest,events,created_at FROM scale_set_inbox`},
				{"drop", `DROP TABLE scale_set_inbox`},
				{"rename", `ALTER TABLE scale_set_inbox_v13 RENAME TO scale_set_inbox`},
			} {
				if _, err := s.txExec(ctx, tx, "migrate.v13."+step.point, step.query); err != nil {
					return fmt.Errorf("migration 13 inbox %s: %w", step.point, err)
				}
			}
		}
		var cursorGeneration int
		if err := s.txRow(ctx, tx, "migrate.v13.cursor-generation",
			`SELECT COUNT(*) FROM pragma_table_info('scale_set_cursors') WHERE name='generation'`).Scan(&cursorGeneration); err != nil {
			return fmt.Errorf("inspect migration 13 cursor generation: %w", err)
		}
		if cursorGeneration == 0 {
			if _, err := s.txExec(ctx, tx, "migrate.v13.cursor",
				`ALTER TABLE scale_set_cursors ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`); err != nil {
				return fmt.Errorf("migration 13 cursor generation: %w", err)
			}
		}
		if _, err := s.txExec(ctx, tx, "migrate.v13.record", `INSERT INTO schema_migrations(version, applied_at) VALUES(13, ?)`, now); err != nil {
			return fmt.Errorf("record migration 13: %w", err)
		}
	}
	// Self-heal the seeded scheduler_state singleton on every open. Migration 2
	// creates and first seeds the table, so it always exists by this point; an
	// operator deleting the row (incident 2026-07-22, which wedged every tick on
	// the missing singleton for ~18h) is repaired idempotently by INSERT OR
	// IGNORE without disturbing a live row.
	if _, err := s.txExec(ctx, tx, "migrate.reseed", seedSchedulerState); err != nil {
		return fmt.Errorf("reseed scheduler state: %w", err)
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
		now = s.instant()
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

// SpawnGeneration counts the demand's prior SPENT incarnations from durable
// state. A spent incarnation is one that can never execute this demand again, so
// its rows are a tombstone the next spawn's content-addressed identity must
// supersede instead of colliding with. Two things spend an incarnation, and both
// are exactly the conditions under which domain.Instance.IncarnatesDemand stops
// holding the demand — the same seam app.plannableDemands lets the demand back
// into the queue on, so identity and admission can never disagree:
//
//   - It reached a terminal state. A completed or failed attempt leaves a
//     deleted/failed instance row and its durable clone operation behind.
//   - It was REBOUND. GitHub handed its runner a different job from the same
//     scale set, the durable binding followed (ADR 0033), and the demand it was
//     spawned for went back to the queue with no incarnation at all. The row stays
//     live and busy, and its immutable ownership signature still names the demand
//     it was spawned for — which is precisely why the next spawn for that demand
//     re-derived a byte-identical instance name and wedged on the instances
//     primary key until this counted it.
//
// A still-live incarnation that still holds its demand is deliberately NOT
// counted, so a genuine double-spawn hard-fails. Neither is a live row that
// declares no binding at all — a legacy schema-0 row, or metadata json_extract
// cannot read: an undeclared binding is not evidence of a rebind, and an
// identity that is not provably free must keep colliding.
//
// The count is stable within a tick: the fleet is a single writer and there is at
// most one live incarnation per demand. The demand is addressed through
// ownership.resource_id, matching how recovery migrations already key ownership.
func (s *Store) SpawnGeneration(ctx context.Context, demand domain.DemandKey) (int, error) {
	var generation int
	err := s.dbRow(ctx, "instances.generation", `SELECT COUNT(*) FROM instances
		WHERE json_extract(ownership,'$.resource_id')=?
		  AND (state IN (?,?)
		       OR (json_extract(scheduling_metadata,'$.demand.JobID')>0
		           AND (json_extract(scheduling_metadata,'$.demand.JobID')<>?
		                OR json_extract(scheduling_metadata,'$.demand.RunID')<>?)))`,
		demand.String(), operations.StateDeleted, operations.StateFailed, demand.JobID, demand.RunID).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("count spawn generation: %w", err)
	}
	return generation, nil
}

// DrainGeneration counts the instance's prior drain attempts that can no longer
// act. It is the drain half of SpawnGeneration and exists for the same reason:
// a drain identity is content-addressed from the scheduler's recovery decision
// alone, so a recovery whose premise recurs re-derives a byte-identical
// operation. DrainExecutor.abort makes that recurrence ordinary — fresh evidence
// of a running job returns the instance to Running and completes the drain as a
// no-op, the deadline elapses again, and the identical operation is planned
// against a row that already exists.
//
// Only terminal attempts are counted, so a claimed or pending drain still
// collides and a genuine double-drain of one attempt hard-fails rather than
// enqueuing the effect twice. A discharged attempt counts: the operator declared
// it terminal, and its row is exactly the tombstone a later attempt must
// supersede. The count is stable within a tick because the fleet is a single
// writer and a live drain keeps the instance out of the recovery states the
// scheduler derives these operations from.
func (s *Store) DrainGeneration(ctx context.Context, instanceID string) (int, error) {
	var generation int
	err := s.dbRow(ctx, "operations.drain_generation", `SELECT COUNT(*) FROM operations
		WHERE kind=? AND resource_id=? AND status IN (?,?,?)`,
		lifecycle.OperationDrain, instanceID,
		operations.OperationCompleted, operations.OperationDead, operations.OperationDischarged).Scan(&generation)
	if err != nil {
		return 0, fmt.Errorf("count drain generation: %w", err)
	}
	return generation, nil
}

// Advance atomically persists one externally observed lifecycle edge without
// enqueuing another effect. The owning outbox operation remains claimed until
// its executor reaches a terminal lifecycle state and completes it.
func (s *Store) Advance(ctx context.Context, change lifecycle.StateChange) (operations.Instance, error) {
	if change.InstanceID == "" || !operations.ValidState(change.ExpectedState) || !operations.ValidState(change.NextState) ||
		!change.ExpectedState.CanTransitionTo(change.NextState) || len(change.FailureCode) > 64 ||
		(change.NextState == operations.StateFailed) != (change.FailureCode != "") {
		return operations.Instance{}, operations.ErrInvalid
	}
	tx, err := s.beginTx(ctx, "advance.begin")
	if err != nil {
		return operations.Instance{}, fmt.Errorf("begin lifecycle advance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.instant()
	result, err := s.txExec(ctx, tx, "advance.instance", `UPDATE instances SET state=?,version=version+1,last_error=?,updated_at=? WHERE id=? AND state=? AND version=?`,
		change.NextState, change.FailureCode, now.UnixNano(), change.InstanceID, change.ExpectedState, change.ExpectedVersion)
	if err != nil {
		return operations.Instance{}, fmt.Errorf("advance lifecycle instance: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return operations.Instance{}, operations.ErrConflict
	}
	if _, err := s.txExec(ctx, tx, "advance.history", `INSERT INTO transition_history(instance_id,from_state,to_state,version,operation_id,created_at) VALUES(?,?,?,?,NULL,?)`,
		change.InstanceID, change.ExpectedState, change.NextState, change.ExpectedVersion+1, now.UnixNano()); err != nil {
		return operations.Instance{}, fmt.Errorf("record lifecycle advance: %w", err)
	}
	instance, err := scanInstance(s.txRow(ctx, tx, "advance.result", `SELECT id,state,version,drain_phase,ownership,scheduling_metadata,last_error,created_at,updated_at FROM instances WHERE id=?`, change.InstanceID))
	if err != nil {
		return operations.Instance{}, err
	}
	if err := s.commit(tx, "advance.commit"); err != nil {
		return operations.Instance{}, fmt.Errorf("commit lifecycle advance: %w", err)
	}
	return instance, nil
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
	// A discharged operation is terminal: it is neither retrying (nothing will
	// claim it again) nor dead (an operator already closed it), so it must leave
	// both counts or `fleet update` would keep deferring on a closed wedge.
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN attempts>0 AND status NOT IN (?,?,?) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status=? THEN 1 ELSE 0 END),0)
		FROM operations`, operations.OperationCompleted, operations.OperationDead,
		operations.OperationDischarged, operations.OperationDead).Scan(&retrying, &dead)
	if err != nil {
		return 0, 0, fmt.Errorf("summarize operations: %w", err)
	}
	return retrying, dead, nil
}

// OperationFailures reports why the operations that are not progressing are
// failing. Persisted failure text is never returned: each stored message is
// reduced to one closed lifecycle code, and anything the executors did not
// author is withheld as unclassified. This is the diagnosability the 2026-07-25
// incident lacked, when 397 identical attempts were visible only as a count.
func (s *Store) OperationFailures(ctx context.Context) ([]operations.OperationFailure, error) {
	rows, err := s.dbQuery(ctx, "operations.failures.query", `SELECT kind,last_error,COUNT(*),MAX(attempts)
		FROM operations WHERE last_error<>'' AND status IN (?,?) GROUP BY kind,last_error`,
		operations.OperationPending, operations.OperationDead)
	if err != nil {
		return nil, fmt.Errorf("summarize operation failures: %w", err)
	}
	defer func() { _ = rows.Close() }()
	merged := map[operations.OperationFailure]operations.OperationFailure{}
	for rows.Next() {
		var kind, lastError string
		var count, attempts int
		if err := rows.Scan(&kind, &lastError, &count, &attempts); err != nil {
			return nil, fmt.Errorf("scan operation failure: %w", err)
		}
		key := operations.OperationFailure{Kind: kind, Code: lifecycle.FailureCode(lastError)}
		total := merged[key]
		total.Kind, total.Code = key.Kind, key.Code
		total.Count += count
		total.Attempts = max(total.Attempts, attempts)
		merged[key] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operation failures: %w", err)
	}
	failures := make([]operations.OperationFailure, 0, len(merged))
	for _, failure := range merged {
		failures = append(failures, failure)
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].Kind != failures[j].Kind {
			return failures[i].Kind < failures[j].Kind
		}
		return failures[i].Code < failures[j].Code
	})
	return failures, nil
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
		now = s.instant()
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
		status, availableAt.UTC().UnixNano(), message, s.instant().UnixNano(), id, operations.OperationClaimed, owner)
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO ownership(resource_name,metadata,updated_at) VALUES(?,?,?)`, resource, metadata, s.instant().UnixNano())
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
