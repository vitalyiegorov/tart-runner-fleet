// Package discharge implements the one guarded operator mutation the fleet
// exposes: closing a dead-lettered cleanup operation and, when the operator asks
// for it, retiring the phantom instance row and VM left behind.
//
// It exists because a dead letter used to have no sanctioned remedy at all. The
// CLI is read-only apart from provisioning and release updates, and AGENTS.md
// forbids opening fleet.db while the daemon runs, so an operator facing a
// permanently un-completable cleanup had no permitted action — only database
// surgery the contributor contract prohibits.
//
// The package owns the ordering that makes the remedy safe. It is a policy
// decision, not an implementation detail: see Service.DischargeDeadLetter.
package discharge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// Store is the durable half of the mutation.
type Store interface {
	DischargeDeadLetter(context.Context, operations.Discharge) (operations.DischargeOutcome, error)
}

// VM is the host half: a fresh power observation and an owner-checked removal.
type VM interface {
	Running(context.Context, string) (bool, error)
	Reap(context.Context, string, operations.Ownership) error
}

// Service applies one discharge. Authority is the controller's real mode gate:
// this mutation can terminate a VM, so it is refused outright unless the daemon
// holds fleet authority, exactly like every other destructive path.
type Service struct {
	Store     Store
	VM        VM
	Authority bool
	Now       func() time.Time
	Logger    *slog.Logger
}

// DischargeDeadLetter closes one dead letter and reports precisely what changed.
//
// The ordering is the safety property, and it is not interchangeable. Removing
// the VM first would leave a live instance row with no owned VM, and the reverse
// order is never better: for a row outside the cleanup states that observation
// still turns the ENTIRE instance inventory Unavailable ("owned VM %s missing
// from Tart"), blocking planning for the whole host with nothing left to prove
// anything, and even for a cleanup row it degrades the row to absent power for no
// benefit. A cleared row whose VM still exists also blocks planning ("untracked
// controller VM requires reconciliation"), but that wedge is trivially repairable:
// the VM is still there and re-running this command removes it. So the durable row
// is always retired FIRST and the VM removed SECOND, and a failure of the second
// step is reported as a refusal with the durable half already applied, for the
// operator to retry.
//
// Refusals never reach the destructive half: an unconfirmed request, a
// non-authority daemon, a running VM, an unreadable Tart observation, an
// operation that is not dead, or a resource that some other operation is still
// progressing all stop before anything is written.
func (s Service) DischargeDeadLetter(ctx context.Context, request adminapi.DischargeRequest) (adminapi.DischargeResult, error) {
	if !s.Authority {
		return s.refuse(request, adminapi.RefusalNotAuthority)
	}
	if !request.Valid() {
		return s.refuse(request, invalidCode(request))
	}
	if request.ReapInstance {
		running, err := s.VM.Running(ctx, request.InstanceID)
		if err != nil {
			return s.refuse(request, adminapi.RefusalVMUnobserved)
		}
		if running {
			return s.refuse(request, adminapi.RefusalVMRunning)
		}
	}
	outcome, err := s.Store.DischargeDeadLetter(ctx, operations.Discharge{OperationID: request.OperationID,
		InstanceID: request.InstanceID, ReapInstance: request.ReapInstance, Reason: request.Reason, At: s.now()})
	if err != nil {
		return s.refuse(request, storeCode(err))
	}
	result := adminapi.DischargeResult{APIVersion: adminapi.APIVersion, Kind: adminapi.DischargeKind,
		OperationID: request.OperationID, InstanceID: request.InstanceID,
		OperationDischarged: outcome.OperationDischarged, InstanceReaped: outcome.InstanceReaped}
	if !request.ReapInstance {
		s.audit(request, result, "")
		return result, nil
	}
	if err := s.VM.Reap(ctx, request.InstanceID, outcome.Ownership); err != nil {
		s.audit(request, result, adminapi.RefusalVMDeleteFailed)
		return result, adminapi.Refusal{Code: adminapi.RefusalVMDeleteFailed}
	}
	result.VMDeleted = true
	s.audit(request, result, "")
	return result, nil
}

// invalidCode names the specific guard a malformed request failed, so an operator
// sees "unconfirmed" rather than a generic rejection.
func invalidCode(request adminapi.DischargeRequest) string {
	switch {
	case request.Confirm != adminapi.DischargeConfirmation:
		return adminapi.RefusalUnconfirmed
	case strings.TrimSpace(request.Reason) == "" || len(request.Reason) > adminapi.MaxReasonBytes:
		return adminapi.RefusalReasonRequired
	default:
		return adminapi.RefusalInvalidRequest
	}
}

// storeCode maps a durable refusal to its bounded code. Unrecognised failures
// report the store as unavailable rather than guessing a cause.
func storeCode(err error) string {
	switch {
	case errors.Is(err, operations.ErrNotFound):
		return adminapi.RefusalUnknownOperation
	case errors.Is(err, operations.ErrInvalid):
		return adminapi.RefusalInvalidRequest
	case errors.Is(err, operations.ErrResourceMismatch):
		return adminapi.RefusalResourceMismatch
	case errors.Is(err, operations.ErrNotDeadLettered):
		return adminapi.RefusalOperationLive
	case errors.Is(err, operations.ErrResourceProgressing):
		return adminapi.RefusalResourceBusy
	case errors.Is(err, operations.ErrInstanceNotReapable):
		return adminapi.RefusalInstanceState
	case errors.Is(err, operations.ErrConflict):
		return adminapi.RefusalResourceBusy
	default:
		return adminapi.RefusalStoreUnavailable
	}
}

func (s Service) refuse(request adminapi.DischargeRequest, code string) (adminapi.DischargeResult, error) {
	s.audit(request, adminapi.DischargeResult{OperationID: request.OperationID, InstanceID: request.InstanceID}, code)
	return adminapi.DischargeResult{}, adminapi.Refusal{Code: code}
}

// audit records every attempt, applied or refused, with the operator's reason.
// It logs the two identities, the requested scope, what was applied, and the
// bounded refusal code — never an operation payload, and never upstream text.
func (s Service) audit(request adminapi.DischargeRequest, result adminapi.DischargeResult, refusal string) {
	if s.Logger == nil {
		return
	}
	outcome := "applied"
	if refusal != "" {
		outcome = "refused"
	}
	s.Logger.Warn("operator discharge", "outcome", outcome, "refusal", refusal,
		"operation", request.OperationID, "instance", request.InstanceID, "reap", request.ReapInstance,
		"reason", request.Reason, "operationDischarged", result.OperationDischarged,
		"instanceReaped", result.InstanceReaped, "vmDeleted", result.VMDeleted)
}

func (s Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}
