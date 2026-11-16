package sqlite

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func (s *Store) RecentOrphanRequeues(ctx context.Context, scope string) ([]operations.OrphanRequeue, error) {
	rows, err := s.dbQuery(ctx, "orphan.list", `SELECT scope,repository,run_id,at,CASE WHEN outcome='rerun' AND (SELECT COUNT(*) FROM orphan_requeues x WHERE x.repository=o.repository AND x.run_id=o.run_id AND x.outcome IN ('rerun','reserved','cancelled_not_rerun'))>=2 THEN 'gave_up' ELSE outcome END,run_attempt FROM orphan_requeues o WHERE scope=? AND (outcome IN ('reserved','cancelled_not_rerun') OR o.rowid IN (SELECT rowid FROM orphan_requeues WHERE scope=? ORDER BY at DESC LIMIT 10)) ORDER BY at DESC`, scope, scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []operations.OrphanRequeue
	for rows.Next() {
		var r operations.OrphanRequeue
		var at int64
		if err := rows.Scan(&r.Scope, &r.Repository, &r.RunID, &at, &r.Outcome, &r.RunAttempt); err != nil {
			return nil, err
		}
		r.At = time.Unix(0, at).UTC()
		result = append(result, r)
	}
	return result, rows.Err()
}

// Reservation commits before HTTP, including failed/uncertain attempts in both
// limits. A crash cannot erase a potentially successful external mutation.
func (s *Store) ClaimOrphanRequeue(ctx context.Context, r operations.OrphanRequeue, cooldown time.Duration, limit int) (bool, error) {
	if !r.RetryAt.IsZero() {
		return s.claimOrphanResume(ctx, r, cooldown, limit)
	}
	result, err := s.dbExec(ctx, "orphan.claim", `INSERT INTO orphan_requeues(scope,repository,run_id,at,outcome,run_attempt)
 SELECT ?,?,?,?,'reserved',? WHERE NOT EXISTS (SELECT 1 FROM orphan_requeues WHERE repository=? AND run_id=? AND at>?)
 AND (SELECT COUNT(*) FROM orphan_requeues WHERE repository=? AND run_id=? AND outcome IN ('rerun','reserved','cancelled_not_rerun'))<2
 AND (SELECT COUNT(*) FROM orphan_requeues WHERE at>? AND outcome NOT IN ('held','excluded'))<?
 AND NOT EXISTS (SELECT 1 FROM orphan_requeues WHERE scope=? AND outcome='permission_denied' AND at>?)`, r.Scope, r.Repository, r.RunID, r.At.UnixNano(), r.RunAttempt, r.Repository, r.RunID, r.At.Add(-cooldown).UnixNano(), r.Repository, r.RunID, r.At.Add(-time.Hour).UnixNano(), limit, r.Scope, r.At.Add(-time.Hour).UnixNano())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (s *Store) FinishOrphanRequeue(ctx context.Context, r operations.OrphanRequeue) error {
	_, err := s.dbExec(ctx, "orphan.finish", `UPDATE orphan_requeues SET outcome=?,at=? WHERE repository=? AND run_id=? AND at=?`, r.Outcome, max(r.At.UnixNano(), r.RetryAt.UnixNano()), r.Repository, r.RunID, r.At.UnixNano())
	return err
}

// Each retry keeps a separate hourly charge. Transfer the unfinished lifetime
// reservation to the new row atomically, leaving the old charge as a noop.
func (s *Store) claimOrphanResume(ctx context.Context, r operations.OrphanRequeue, cooldown time.Duration, limit int) (bool, error) {
	tx, err := s.beginTx(ctx, "orphan.resume.begin")
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.txExec(ctx, tx, "orphan.resume.claim", `INSERT INTO orphan_requeues(scope,repository,run_id,at,outcome,run_attempt)
 SELECT scope,repository,run_id,?,'cancelled_not_rerun',run_attempt FROM orphan_requeues
 WHERE repository=? AND run_id=? AND at=? AND outcome='cancelled_not_rerun' AND at<=?
 AND (SELECT COUNT(*) FROM orphan_requeues WHERE at>? AND outcome NOT IN ('held','excluded'))<?
 AND NOT EXISTS (SELECT 1 FROM orphan_requeues WHERE scope=? AND outcome='permission_denied' AND at>?)`,
		r.RetryAt.UnixNano(), r.Repository, r.RunID, r.At.UnixNano(), r.RetryAt.Add(-cooldown).UnixNano(), r.RetryAt.Add(-time.Hour).UnixNano(), limit, r.Scope, r.RetryAt.Add(-time.Hour).UnixNano())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if _, err := s.txExec(ctx, tx, "orphan.resume.transfer", `UPDATE orphan_requeues SET outcome='noop' WHERE repository=? AND run_id=? AND at=?`, r.Repository, r.RunID, r.At.UnixNano()); err != nil {
		return false, err
	}
	if err := s.commit(tx, "orphan.resume.commit"); err != nil {
		return false, err
	}
	return true, nil
}
