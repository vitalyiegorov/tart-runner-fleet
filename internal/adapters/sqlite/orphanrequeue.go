package sqlite

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func (s *Store) RecentOrphanRequeues(ctx context.Context, scope string) ([]operations.OrphanRequeue, error) {
	rows, err := s.dbQuery(ctx, "orphan.list", `SELECT scope,repository,run_id,at,outcome FROM orphan_requeues WHERE scope=? ORDER BY at DESC LIMIT 10`, scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []operations.OrphanRequeue
	for rows.Next() {
		var r operations.OrphanRequeue
		var at int64
		if err := rows.Scan(&r.Scope, &r.Repository, &r.RunID, &at, &r.Outcome); err != nil {
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
	result, err := s.dbExec(ctx, "orphan.claim", `INSERT INTO orphan_requeues(scope,repository,run_id,at,outcome)
 SELECT ?,?,?,?,'reserved' WHERE NOT EXISTS (SELECT 1 FROM orphan_requeues WHERE repository=? AND run_id=? AND at>?)
 AND (SELECT COUNT(*) FROM orphan_requeues WHERE at>?)<?
 AND NOT EXISTS (SELECT 1 FROM orphan_requeues WHERE scope=? AND outcome='permission_denied' AND at>?)`, r.Scope, r.Repository, r.RunID, r.At.UnixNano(), r.Repository, r.RunID, r.At.Add(-cooldown).UnixNano(), r.At.Add(-time.Hour).UnixNano(), limit, r.Scope, r.At.Add(-time.Hour).UnixNano())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (s *Store) FinishOrphanRequeue(ctx context.Context, r operations.OrphanRequeue) error {
	_, err := s.dbExec(ctx, "orphan.finish", `UPDATE orphan_requeues SET outcome=? WHERE repository=? AND run_id=? AND at=?`, r.Outcome, r.Repository, r.RunID, r.At.UnixNano())
	return err
}
