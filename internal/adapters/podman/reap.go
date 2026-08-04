package podman

import (
	"context"
	"errors"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Reap removes an owned container under explicit operator authority, for the one
// case Delete cannot serve: a GitHub runner registration that can never be
// confirmed released, so no fresh runner/job confirmation will ever be Safe. See
// docs/OPERATIONS.md for the incident this exists for.
//
// It is not a relaxed Delete. It keeps every check that does not depend on
// GitHub:
//
//   - the name must be a valid controller instance name;
//   - durable ownership must still match this controller's record, so the
//     command can never remove a container the fleet does not own;
//   - a fresh podman observation must show the container is not running. Reap
//     never stops one, because a running container may be executing a real job —
//     the caller's operator authority covers an abandoned registration, not live
//     work. This is also why the removal is a plain `podman rm` and not the
//     `--force` of Delete: the force flag would stop the container, which is
//     exactly the judgement Reap is not entitled to make;
//   - an already-absent container is success, so a partially applied discharge
//     can be retried;
//   - a failing removal is re-observed, and an unreadable observation stays
//     uncertain rather than reporting success.
//
// What it deliberately drops is Confirmation.ConfirmDeletion: the
// runner-inactive half of that evidence is exactly what a leaked registration
// withholds forever. The operator supplies that judgement instead, with a
// recorded reason.
func (a *Adapter) Reap(ctx context.Context, name string, ownership operations.Ownership) error {
	if err := validateName(name); err != nil {
		return err
	}
	instance, err := a.ownedContainer(ctx, name, ownership)
	if errors.Is(err, operations.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if instance.Running {
		return failure("reap", ErrorUncertain, operations.ErrConflict)
	}
	return a.remove(ctx, "reap", name, "rm", name)
}
