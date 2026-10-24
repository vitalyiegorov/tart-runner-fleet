package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// RecordGuestTraffic stores an instance's end-of-job counters and reports
// whether the row changed. The first measured sample wins: a drain retry
// cannot double count, and a later real reading replaces an earlier
// unmeasured one, but never the reverse.
func (s *Store) RecordGuestTraffic(ctx context.Context, sample operations.TrafficSample) (bool, error) {
	if !sample.Valid() {
		return false, operations.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO guest_traffic(instance_id,repo,platform,profile,started_at,
		sampled_at,measured,receive_bytes,transmit_bytes,unmeasured) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(instance_id) DO UPDATE SET sampled_at=excluded.sampled_at,measured=excluded.measured,
		receive_bytes=excluded.receive_bytes,transmit_bytes=excluded.transmit_bytes,unmeasured=excluded.unmeasured
		WHERE guest_traffic.measured=0 AND excluded.measured=1`,
		sample.InstanceID, sample.Repo, string(sample.Platform), string(sample.Profile), sample.StartedAt.UTC().UnixNano(),
		sample.SampledAt.UTC().UnixNano(), sample.Measured, sample.ReceiveBytes, sample.TransmitBytes, sample.Unmeasured)
	if err != nil {
		return false, fmt.Errorf("record guest traffic: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

// GuestTraffic is every sample taken at or after since, newest first, at most
// limit rows, each joined with the most recent job GitHub assigned to that
// runner name.
func (s *Store) GuestTraffic(ctx context.Context, since time.Time, limit int) ([]operations.TrafficRecord, error) {
	if limit <= 0 {
		return nil, operations.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.instance_id,t.repo,t.platform,t.profile,t.started_at,t.sampled_at,
		t.measured,t.receive_bytes,t.transmit_bytes,t.unmeasured,COALESCE(d.scale_set_id,0),COALESCE(d.workflow_run_id,0),
		COALESCE(d.workflow_ref,''),COALESCE(d.display_name,'')
		FROM guest_traffic t LEFT JOIN runner_demands d ON d.rowid=(SELECT rowid FROM runner_demands
			WHERE runner_name=t.instance_id ORDER BY updated_at DESC LIMIT 1)
		WHERE t.sampled_at>=? ORDER BY t.sampled_at DESC LIMIT ?`, since.UTC().UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("query guest traffic: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []operations.TrafficRecord{}
	for rows.Next() {
		var record operations.TrafficRecord
		var platform, profile string
		var started, sampled int64
		if err := rows.Scan(&record.InstanceID, &record.Repo, &platform, &profile, &started, &sampled, &record.Measured,
			&record.ReceiveBytes, &record.TransmitBytes, &record.Unmeasured, &record.ScaleSetID, &record.RunID,
			&record.WorkflowRef, &record.JobName); err != nil {
			return nil, fmt.Errorf("scan guest traffic: %w", err)
		}
		record.Platform, record.Profile = domain.Platform(platform), domain.ProfileID(profile)
		record.StartedAt, record.SampledAt = fromNanos(started), fromNanos(sampled)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read guest traffic: %w", err)
	}
	return records, nil
}
