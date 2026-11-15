package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/lifecycle"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

func assertDemandConflict(t *testing.T, err error, check, instance string) {
	t.Helper()
	var detail *operations.DemandConflict
	if !errors.Is(err, operations.ErrConflict) || !errors.As(err, &detail) {
		t.Fatalf("missing conflict diagnostic for %s: %v", check, err)
	}
	if string(detail.Check) != check || detail.Instance != instance {
		t.Fatalf("diagnostic = %+v", detail)
	}
	if !strings.Contains(err.Error(), check) || !strings.Contains(err.Error(), instance) {
		t.Fatalf("error lacks target: %v", err)
	}
}

func TestDemandConflictChecks(t *testing.T) {
	ctx := context.Background()
	source, target := runnerDemandInstance("runner-alpha", 101), runnerDemandInstance("runner-beta", 102)
	record := runnerDemandRecord(101)
	for _, check := range []string{"reservation_duplicate", "binding_shape_mismatch", "binding_metadata_invalid", "binding_source_cas", "binding_target_cas", "binding_rebind_cas"} {
		t.Run(check, func(t *testing.T) {
			store := testStore(t)
			a, b := source, target
			instances := []operations.Instance{a, b}
			switch check {
			case "reservation_duplicate":
				duplicate := a
				duplicate.ID = "runner-other"
				instances = append(instances, duplicate)
			case "binding_shape_mismatch":
				b.Profile = "builder"
			case "binding_metadata_invalid":
				a.Resources.CPU = 0
				b.Resources.CPU = 0
				instances[0] = a
			case "binding_source_cas", "binding_target_cas":
				if check == "binding_target_cas" {
					if err := store.CreateInstance(ctx, a); err != nil {
						t.Fatal(err)
					}
				}
			case "binding_rebind_cas":
				instances = nil
			}
			_, err := store.alignRunnerDemand(ctx, b, instances, record)
			id := b.ID
			if check == "binding_source_cas" || check == "reservation_duplicate" {
				id = a.ID
			}
			assertDemandConflict(t, err, check, id)
			var detail *operations.DemandConflict
			if !errors.As(err, &detail) || detail.From != "" || detail.To != "" {
				t.Fatalf("binding contains filler states: %v", err)
			}
			if check == "reservation_duplicate" && !strings.Contains(err.Error(), "runner-other") {
				t.Fatalf("missing other duplicate: %v", err)
			}
		})
	}
	t.Run("demand_job_identity", func(t *testing.T) {
		store := testStore(t)
		event := demandEvent(operations.DemandJobStarted, 101)
		event.RunnerName = target.ID
		if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
			t.Fatal(err)
		}
		event.JobID = "another-job"
		_, err := store.ApplyDemandBatch(ctx, 1, 2, []operations.DemandEvent{event})
		assertDemandConflict(t, err, "demand_job_identity", target.ID)
	})
	for _, check := range []string{"instance_state_transition", "transition_instance_cas", "transition_idempotent_state"} {
		t.Run(check, func(t *testing.T) {
			store := testStore(t)
			instance := target
			instance.State = operations.StateOnlineIdle
			if err := store.CreateInstance(ctx, instance); err != nil {
				t.Fatal(err)
			}
			var err error
			switch check {
			case "instance_state_transition":
				_, err = store.Advance(ctx, lifecycle.StateChange{InstanceID: instance.ID, ExpectedState: instance.State, ExpectedVersion: 99, NextState: operations.StateAssigned})
			case "transition_instance_cas":
				instance.Version = 99
				err = store.enqueueDemandDrain(ctx, instance, nil)
			case "transition_idempotent_state":
				if err = store.enqueueDemandDrain(ctx, instance, nil); err != nil {
					t.Fatal(err)
				}
				// An already recorded operation cannot prove a later state is still its target.
				if _, err = store.db.ExecContext(ctx, `UPDATE instances SET state=? WHERE id=?`, operations.StateDeregistering, instance.ID); err != nil {
					t.Fatal(err)
				}
				err = store.enqueueDemandDrain(ctx, instance, nil)
			}
			assertDemandConflict(t, err, check, instance.ID)
		})
	}
}

func TestProjectDemandConflictCarriesDeliveredIdentifiers(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprint(named), func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			event := demandEvent(operations.DemandJobStarted, 101)
			event.DisplayName = "secret-display"
			event.WorkflowRef = "secret-ref"
			event.Labels = []string{"secret-label"}
			if named {
				event.RunnerName = "runner-beta"
			}
			if _, err := store.ApplyDemandBatch(ctx, 1, 1, []operations.DemandEvent{event}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"runner-alpha", "runner-beta"} {
				if err := store.CreateInstance(ctx, runnerDemandInstance(id, 101)); err != nil {
					t.Fatal(err)
				}
			}
			err := store.ProjectDemandEvent(ctx, 1, event)
			assertDemandConflict(t, err, "reservation_duplicate", "runner-alpha")
			var detail *operations.DemandConflict
			if !errors.As(err, &detail) || detail.MessageType != event.Kind || detail.RequestID != 101 || detail.JobID != event.JobID || detail.Runner != event.RunnerName {
				t.Fatalf("missing delivered identifiers: %v", err)
			}
			if !strings.Contains(err.Error(), "other=runner-beta") {
				t.Fatalf("missing duplicate: %v", err)
			}
			for _, secret := range []string{"secret-display", "secret-ref", "secret-label"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("payload leaked: %v", err)
				}
			}
		})
	}
}
