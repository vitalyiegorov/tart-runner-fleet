package reconcile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/scheduler"
)

type PlanStore interface {
	SchedulerState(context.Context) (operations.SchedulerState, error)
	Instance(context.Context, string) (operations.Instance, error)
	ApplyPlan(context.Context, operations.Plan) (bool, error)
}

type Controller struct {
	Store        PlanStore
	ControllerID string
	Mode         Mode
	Profiles     map[domain.ProfileID]domain.Profile
}

func (c Controller) Commit(ctx context.Context, plan scheduler.Plan, observationCursor string, now time.Time) (bool, error) {
	if c.Store == nil || strings.TrimSpace(c.ControllerID) == "" || !c.Mode.Valid() || now.IsZero() || plan.ID == "" {
		return false, operations.ErrInvalid
	}
	if plan.Status == scheduler.PlanBlockedObservation {
		return false, nil
	}
	if plan.Status != scheduler.PlanReady {
		return false, fmt.Errorf("planner status %s: %w", plan.Status, operations.ErrUncertain)
	}
	if c.Mode == Observe {
		return false, nil
	}
	prior, err := c.Store.SchedulerState(ctx)
	if err != nil {
		return false, err
	}
	if len(plan.Operations) == 0 {
		current, err := schedulerStateMatches(prior, plan.Next, observationCursor)
		if err != nil {
			return false, err
		}
		if current {
			return false, nil
		}
	}
	durable, err := c.translate(ctx, plan, prior, observationCursor, now)
	if err != nil {
		return false, err
	}
	return c.Store.ApplyPlan(ctx, durable)
}

func schedulerStateMatches(prior operations.SchedulerState, next scheduler.State, observationCursor string) (bool, error) {
	nextState, _ := json.Marshal(next)
	nextReservations, _ := json.Marshal(next.Reservation)
	nextDRR, _ := json.Marshal(map[string]string{"cursor": next.DRRCursor})
	for _, comparison := range []struct {
		name     string
		current  json.RawMessage
		fallback string
		expected json.RawMessage
	}{
		{name: "state", current: prior.Data, fallback: `{}`, expected: nextState},
		{name: "reservations", current: prior.Reservations, fallback: `[]`, expected: nextReservations},
		{name: "deficit round robin", current: prior.DeficitRoundRobin, fallback: `{}`, expected: nextDRR},
	} {
		equal, err := jsonEquivalent(comparison.current, comparison.fallback, comparison.expected)
		if err != nil {
			return false, fmt.Errorf("decode scheduler %s: %w", comparison.name, err)
		}
		if !equal {
			return false, nil
		}
	}
	return prior.ObservationCursor == observationCursor, nil
}

func jsonEquivalent(current json.RawMessage, fallback string, expected json.RawMessage) (bool, error) {
	if len(current) == 0 {
		current = json.RawMessage(fallback)
	}
	normalize := func(value json.RawMessage) ([]byte, error) {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, err
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, errors.New("multiple JSON values")
			}
			return nil, err
		}
		return json.Marshal(decoded)
	}
	left, err := normalize(current)
	if err != nil {
		return false, err
	}
	right, err := normalize(expected)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func (c Controller) translate(ctx context.Context, plan scheduler.Plan, prior operations.SchedulerState, cursor string, now time.Time) (operations.Plan, error) {
	// These values contain only JSON-native scalar structs; marshaling cannot
	// fail, and keeping them typed prevents arbitrary state from entering here.
	stateJSON, _ := json.Marshal(plan.Next)
	reservationJSON, _ := json.Marshal(plan.Next.Reservation)
	drrJSON, _ := json.Marshal(map[string]string{"cursor": plan.Next.DRRCursor})
	durable := operations.Plan{
		ID: durablePlanID(c.Mode, plan.ID, prior.Version), ExpectedSchedulerVersion: prior.Version, CreatedAt: now.UTC(),
		Scheduler: operations.SchedulerState{Version: prior.Version + 1, Data: stateJSON, Reservations: reservationJSON, DeficitRoundRobin: drrJSON, ObservationCursor: cursor},
	}
	if c.Mode == Shadow {
		return durable, nil
	}
	for _, operation := range plan.Operations {
		switch operation.Kind {
		case scheduler.OperationDrain:
			instance, err := c.Store.Instance(ctx, operation.Instance)
			if err != nil {
				return operations.Plan{}, err
			}
			drainPhase := 1
			validState := instance.State == operations.StateOnlineIdle
			if operation.Recovery {
				drainPhase = operations.DrainPhaseStoppedRecovery
				validState = instance.State == operations.StateAssigned || instance.State == operations.StateRunning
			}
			if !validState {
				return operations.Plan{}, fmt.Errorf("drain %s in state %s: %w", instance.ID, instance.State, operations.ErrConflict)
			}
			next := instance
			next.State, next.Version, next.DrainPhase, next.UpdatedAt = operations.StateDraining, instance.Version+1, drainPhase, now.UTC()
			durable.Instances = append(durable.Instances, operations.InstanceIntent{ExpectedVersion: instance.Version, ExpectedState: instance.State, Instance: next})
			durable.Operations = append(durable.Operations, outbox(operation, "deregister", instance.ID, instance.Ownership, now))
		case scheduler.OperationSpawn:
			profile, ok := c.Profiles[operation.Profile]
			if !ok || profile.Route != operation.Route || profile.Resources.CPU <= 0 || profile.Resources.MemoryMB <= 0 || profile.Resources.Slots <= 0 {
				return operations.Plan{}, fmt.Errorf("spawn profile %q: %w", operation.Profile, operations.ErrInvalid)
			}
			name := instanceName(operation)
			ownership := operations.Ownership{ControllerID: c.ControllerID, ResourceID: operation.Demand.String(), OperationID: operation.ID}
			instance := operations.Instance{ID: name, Repo: operation.Demand.Repo, Platform: profile.Platform, Profile: profile.ID, Route: profile.Route,
				Resources: profile.Resources, Demand: operation.Demand, State: operations.StatePlanned, Version: 0, Ownership: ownership, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
			durable.Instances = append(durable.Instances, operations.InstanceIntent{ExpectedVersion: -1, Instance: instance})
			durable.Operations = append(durable.Operations, outbox(operation, "clone", name, ownership, now))
		default:
			return operations.Plan{}, fmt.Errorf("operation %q: %w", operation.Kind, operations.ErrInvalid)
		}
	}
	return durable, nil
}

func outbox(source scheduler.Operation, kind, resource string, ownership operations.Ownership, now time.Time) operations.Operation {
	payload, _ := json.Marshal(struct {
		Repo      string               `json:"repo"`
		Profile   string               `json:"profile"`
		Route     string               `json:"route"`
		RunID     int64                `json:"run_id"`
		JobID     int64                `json:"job_id"`
		Attempt   int                  `json:"attempt"`
		Ownership operations.Ownership `json:"ownership"`
	}{source.Demand.Repo, string(source.Profile), string(source.Route), source.Demand.RunID, source.Demand.JobID, source.Demand.Attempt, ownership})
	return operations.Operation{ID: source.ID, IdempotencyKey: source.ID, EffectKey: kind + ":" + resource, Kind: kind,
		ResourceID: resource, Payload: payload, AvailableAt: now.UTC(), DependsOn: append([]string(nil), source.DependsOn...)}
}

func instanceName(operation scheduler.Operation) string {
	sum := sha256.Sum256([]byte(operation.ID + "\x00" + operation.Demand.String()))
	return "trf-" + strings.ToLower(string(operation.Profile)) + "-" + hex.EncodeToString(sum[:8])
}

func durablePlanID(mode Mode, schedulerID string, schedulerVersion int64) string {
	// The deterministic scheduler may return to an earlier logical plan after
	// one or more committed transitions. Scope durable idempotency to the
	// expected generation so retrying the same transition remains a no-op while
	// a later recurrence receives a distinct plan record.
	sum := sha256.Sum256([]byte(string(mode) + "\x00" + strconv.FormatInt(schedulerVersion, 10) + "\x00" + schedulerID))
	return "commit-" + hex.EncodeToString(sum[:12])
}
