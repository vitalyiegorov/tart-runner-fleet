package discharge

import (
	"context"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// RecoverStoppedCleanup revives a safely idle parked cleanup as a new durable
// attempt. The ordinary worker, not this observer, owns every external effect.
// Thus a failed delete remains retryable and never loses its owning row.
func (s Service) RecoverStoppedCleanup(ctx context.Context, letter operations.DeadLetter, instance domain.Instance,
	confirmation interface {
		ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
	}) error {
	store, ok := s.Store.(interface {
		ResumeStoppedCleanup(context.Context, string, string, time.Time) error
	})
	if !s.Authority || !ok || s.VM == nil || confirmation == nil || !recoverableCleanup(letter, instance) {
		return nil
	}
	power, err := s.VM.Power(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !power.ProvenIdle() {
		return nil
	}
	observed, err := confirmation.ConfirmDeletion(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !observed.Safe(s.now(), 30*time.Second) {
		return nil
	}
	err = store.ResumeStoppedCleanup(ctx, letter.OperationID, instance.ID, s.now())
	if err == nil && s.Logger != nil {
		s.Logger.Warn("automatic cleanup resumed", "operation", letter.OperationID, "instance", instance.ID, "code", letter.Code)
	}
	return err
}

func recoverableCleanup(letter operations.DeadLetter, instance domain.Instance) bool {
	return letter.Kind == "deregister" && (letter.Code == "stop" || letter.Code == "delete") && !letter.ResourceProgressing &&
		instance.ID == letter.ResourceID && instance.State.TearingDown() && instance.Power.ProvenIdle()
}
