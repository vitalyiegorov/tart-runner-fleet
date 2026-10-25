package discharge

import (
	"context"
	"errors"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// RecoverCleanup revives a dead cleanup as a new durable attempt. The ordinary
// worker, not this observer, owns every external effect -- including the stop
// of a VM that is still running. Thus a failed delete remains retryable and
// never loses its owning row.
//
// A powered-off VM needs only fresh power and GitHub proof (ADR 0061). A VM
// still running after its stop died additionally needs a fresh, healthy host:
// the stop budget is spent in minutes by a host-wide fault, and retrying it
// while the fault persists would only spend it again (ADR 0081).
func (s Service) RecoverCleanup(ctx context.Context, letter operations.DeadLetter, instance domain.Instance, hostHealthy bool,
	confirmation interface {
		ConfirmDeletion(context.Context, string) (operations.DeletionConfirmation, error)
	}) error {
	store, ok := s.Store.(interface {
		ResumeCleanup(context.Context, string, string, bool, time.Time) error
	})
	if !s.Authority || !ok || s.VM == nil || confirmation == nil || !recoverableCleanup(letter, instance, hostHealthy) ||
		!operations.CleanupResumptionDue(letter.Resumptions, letter.DeadAt, s.now()) {
		return nil
	}
	power, err := s.VM.Power(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !samePower(power, instance.Power) {
		// The tick's observation and the fresh read disagree: neither is proof.
		return nil
	}
	observed, err := confirmation.ConfirmDeletion(ctx, instance.ID)
	if err != nil {
		return err
	}
	if !observed.Safe(s.now(), 30*time.Second) {
		return nil
	}
	err = store.ResumeCleanup(ctx, letter.OperationID, instance.ID, power.ProvenIdle(), s.now())
	if errors.Is(err, operations.ErrResumptionNotDue) {
		return nil
	}
	if err == nil && s.Logger != nil {
		s.Logger.Warn("automatic cleanup resumed", "operation", letter.OperationID, "instance", instance.ID, "code", letter.Code,
			"power", string(power), "resumption", letter.Resumptions+1)
	}
	return err
}

// recoverableCleanup is the cheap half of the rule, decided from the tick's
// own observations before any fresh probe is spent.
func recoverableCleanup(letter operations.DeadLetter, instance domain.Instance, hostHealthy bool) bool {
	if letter.Kind != "deregister" || letter.ResourceProgressing || instance.ID != letter.ResourceID || !instance.State.TearingDown() {
		return false
	}
	switch {
	case instance.Power.ProvenIdle():
		return letter.Code == "stop" || letter.Code == "delete"
	case instance.Power == domain.InstancePowerRunning:
		return letter.Code == "stop" && hostHealthy
	default:
		return false
	}
}

// samePower reports two power reads that prove the same thing. Stopped and
// absent are both a proven idle VM.
func samePower(fresh, observed domain.InstancePower) bool {
	return fresh == observed || (fresh.ProvenIdle() && observed.ProvenIdle())
}
