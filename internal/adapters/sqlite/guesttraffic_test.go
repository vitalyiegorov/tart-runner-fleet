package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func trafficSample(id string, at time.Time, rx int64) operations.TrafficSample {
	return operations.TrafficSample{InstanceID: id, Repo: "budgie-at/budgie", Platform: domain.PlatformLinux,
		Profile: "linux-4x8", StartedAt: at.Add(-10 * time.Minute), SampledAt: at, Measured: true,
		ReceiveBytes: rx, TransmitBytes: rx / 10}
}

// TestGuestTrafficRecordsOncePerInstance keeps a drain retry from double
// counting, while letting a later real reading replace an earlier failed one.
func TestGuestTrafficRecordsOncePerInstance(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	at := time.Date(2026, 10, 24, 15, 2, 0, 0, time.UTC)
	unmeasured := operations.TrafficSample{InstanceID: "trf-a", Repo: "o/r", Platform: domain.PlatformLinux,
		Profile: "p", StartedAt: at, SampledAt: at, Unmeasured: operations.TrafficExecTimeout}
	for step, want := range []struct {
		sample   operations.TrafficSample
		recorded bool
	}{
		{unmeasured, true},
		{unmeasured, false},
		{trafficSample("trf-a", at.Add(time.Second), 1345000000), true},
		{trafficSample("trf-a", at.Add(2*time.Second), 99), false},
		{unmeasured, false},
	} {
		recorded, err := store.RecordGuestTraffic(ctx, want.sample)
		if err != nil || recorded != want.recorded {
			t.Fatalf("step %d: RecordGuestTraffic() = %t, %v; want %t", step, recorded, err, want.recorded)
		}
	}
	records, err := store.GuestTraffic(ctx, at.Add(-time.Hour), 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("GuestTraffic() = %+v, %v", records, err)
	}
	if got := records[0]; !got.Measured || got.ReceiveBytes != 1345000000 || got.Unmeasured != "" {
		t.Fatalf("the first real reading must stand: %+v", got)
	}
	if _, err := store.RecordGuestTraffic(ctx, operations.TrafficSample{InstanceID: "x"}); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("an invalid sample must be refused, got %v", err)
	}
}

// TestGuestTrafficJoinsTheJobThatRanOnTheRunner attributes bytes to the job
// GitHub brokered to this runner, not the demand it was spawned for.
func TestGuestTrafficJoinsTheJobThatRanOnTheRunner(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	at := time.Date(2026, 10, 24, 15, 2, 0, 0, time.UTC)
	if _, err := store.db.Exec(`INSERT INTO runner_demands(scale_set_id,runner_request_id,status,status_rank,owner,
		repository,workflow_run_id,job_id,display_name,workflow_ref,event_name,labels,queue_time,runner_id,runner_name,
		result,updated_at) VALUES(9,1,'completed',3,'budgie-at','budgie',4242,'j','Build and publish',
		'budgie-at/budgie/.github/workflows/ota.yml@refs/pull/1650/merge','pull_request','[]',0,7,'trf-ota','succeeded',?)`,
		at.UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []operations.TrafficSample{trafficSample("trf-ota", at, 1345000000),
		trafficSample("trf-idle", at.Add(time.Minute), 5), trafficSample("trf-old", at.Add(-48*time.Hour), 7)} {
		if _, err := store.RecordGuestTraffic(ctx, sample); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.GuestTraffic(ctx, at.Add(-time.Hour), 10)
	if err != nil || len(records) != 2 {
		t.Fatalf("GuestTraffic() = %+v, %v; want the two samples inside the window", records, err)
	}
	if records[0].InstanceID != "trf-idle" || records[0].WorkflowRef != "" {
		t.Fatalf("newest first, and a runner with no job carries no workflow: %+v", records[0])
	}
	ota := records[1]
	if ota.ScaleSetID != 9 || ota.RunID != 4242 || ota.JobName != "Build and publish" ||
		ota.WorkflowRef != "budgie-at/budgie/.github/workflows/ota.yml@refs/pull/1650/merge" ||
		ota.Repo != "budgie-at/budgie" || ota.Profile != "linux-4x8" || !ota.StartedAt.Equal(at.Add(-10*time.Minute)) {
		t.Fatalf("job identity = %+v", ota)
	}
	limited, err := store.GuestTraffic(ctx, at.Add(-time.Hour), 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit = %+v, %v", limited, err)
	}
	if _, err := store.GuestTraffic(ctx, at, 0); !errors.Is(err, operations.ErrInvalid) {
		t.Fatalf("a non-positive limit is invalid, got %v", err)
	}
}

func TestGuestTrafficFailsClosedWhenTheDatabaseIsClosed(t *testing.T) {
	store := testStore(t)
	_ = store.Close()
	if _, err := store.RecordGuestTraffic(context.Background(), trafficSample("a", time.Now(), 1)); err == nil {
		t.Fatal("want an error from a closed store")
	}
	if records, err := store.GuestTraffic(context.Background(), time.Now(), 1); err == nil || records != nil {
		t.Fatalf("an unreadable store must not answer an empty list: %+v %v", records, err)
	}
}
