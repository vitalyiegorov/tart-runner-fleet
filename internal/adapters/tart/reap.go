package tart

import (
	"context"
	"errors"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Reap removes an owned VM under explicit operator authority, for the one case
// Delete cannot serve: a GitHub runner registration that can never be confirmed
// released, so no fresh runner/job confirmation will ever be Safe. See
// docs/OPERATIONS.md for the incident this exists for.
//
// It is not a relaxed Delete. It keeps every check that does not depend on
// GitHub:
//
//   - the name must be a valid controller VM name;
//   - durable ownership must still match this controller's record, so the command
//     can never remove a VM the fleet does not own;
//   - a fresh Tart observation must show the VM is not running. Reap never stops
//     a VM, because a running guest may be executing a real job — the caller's
//     operator authority covers an abandoned registration, not live work;
//   - an already-absent VM is success, so a partially applied discharge can be
//     retried;
//   - a failing delete is re-observed, and an unreadable observation stays
//     uncertain rather than reporting success.
//
// What it deliberately drops is Confirmation.ConfirmDeletion: the runner-inactive
// half of that evidence is exactly what a leaked registration withholds forever.
// The operator supplies that judgement instead, with a recorded reason.
func (a *Adapter) Reap(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	vm, err := a.ownedVM(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if vm.Running {
		return &Error{Op: "reap", Kind: ErrorUncertain, ExitCode: -1, Err: operations.ErrConflict}
	}
	commandCtx, cancel := context.WithTimeout(ctx, a.timeout())
	_, commandErr := a.runner().Run(commandCtx, "delete", name)
	cancel()
	if commandErr == nil {
		return nil
	}
	_, observeErr := a.find(ctx, name)
	if errors.Is(observeErr, operations.ErrNotFound) {
		return nil
	}
	if observeErr != nil {
		return &Error{Op: "reap", Kind: ErrorUncertain, ExitCode: -1, Err: errors.Join(commandErr, observeErr)}
	}
	return commandErr
}
