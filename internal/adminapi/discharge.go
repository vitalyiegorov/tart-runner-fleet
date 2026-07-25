package adminapi

import (
	"errors"
	"net/http"
	"strings"
)

const (
	// DischargePath is the only mutating route in fleet.v1. It is served solely on
	// the private Unix socket, never on the loopback health listener.
	DischargePath = "/v1/operations/discharge"
	// DischargeConfirmation is the exact --confirm token the operator must repeat,
	// matching the guarded-command convention of `update adopt` and
	// `scale-sets provision`.
	DischargeConfirmation = "discharge-dead-letter"
	// DischargeKind names the response document.
	DischargeKind = "DischargeResult"
	// RefusalKind names the bounded refusal document.
	RefusalKind = "Refusal"
)

// DeadLetter names one parked durable operation an operator can discharge. Code
// is the closed lifecycle vocabulary the failure aggregate already publishes;
// stored failure text never reaches this DTO.
type DeadLetter struct {
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Code        string `json:"code"`
	ResourceID  string `json:"resourceId"`
	Attempts    int    `json:"attempts"`
	// Parked reports that nothing will advance this resource without an operator:
	// no operation for it is pending or claimed, and its VM is observed stopped.
	// `fleet update` treats a parked resource as capacity a release cannot
	// interrupt; anything it cannot prove parked keeps deferring activation.
	Parked bool `json:"parked"`
}

// DischargeRequest is the guarded mutation body. Confirm and Reason are part of
// the wire contract, not CLI decoration: the daemon is the component that must
// refuse an unconfirmed or unexplained mutation, whatever client sent it.
type DischargeRequest struct {
	OperationID  string `json:"operationId"`
	InstanceID   string `json:"instanceId"`
	ReapInstance bool   `json:"reapInstance"`
	Confirm      string `json:"confirm"`
	Reason       string `json:"reason"`
}

// DischargeResult reports exactly which durable and host effects were applied.
// A retry of an already-applied discharge reports false for the steps that were
// already done, so an operator can always re-run the same command safely.
type DischargeResult struct {
	APIVersion          string `json:"apiVersion"`
	Kind                string `json:"kind"`
	OperationID         string `json:"operationId"`
	InstanceID          string `json:"instanceId"`
	OperationDischarged bool   `json:"operationDischarged"`
	InstanceReaped      bool   `json:"instanceReaped"`
	VMDeleted           bool   `json:"vmDeleted"`
}

// Refusal codes are a closed vocabulary. Nothing an upstream produced, and no
// stored error text, may ever reach this field.
const (
	RefusalNotAuthority     = "not_authority"
	RefusalUnconfirmed      = "unconfirmed"
	RefusalReasonRequired   = "reason_required"
	RefusalInvalidRequest   = "invalid_request"
	RefusalUnknownOperation = "unknown_operation"
	RefusalResourceMismatch = "resource_mismatch"
	RefusalOperationLive    = "operation_not_dead"
	RefusalResourceBusy     = "resource_not_parked"
	RefusalInstanceState    = "instance_not_reapable"
	RefusalVMRunning        = "vm_running"
	RefusalVMUnobserved     = "vm_state_unknown"
	RefusalStoreUnavailable = "store_unavailable"
	RefusalVMDeleteFailed   = "vm_delete_failed"
)

// ErrRefused marks every bounded refusal so callers can branch with errors.Is.
var ErrRefused = errors.New("adminapi: mutation refused")

// Refusal is a credential-free refusal carrying one closed-vocabulary code.
type Refusal struct{ Code string }

func (r Refusal) Error() string { return ErrRefused.Error() + ": " + r.Code }

func (r Refusal) Unwrap() error { return ErrRefused }

func ValidRefusalCode(code string) bool {
	switch code {
	case RefusalNotAuthority, RefusalUnconfirmed, RefusalReasonRequired, RefusalInvalidRequest,
		RefusalUnknownOperation, RefusalResourceMismatch, RefusalOperationLive, RefusalResourceBusy,
		RefusalInstanceState, RefusalVMRunning, RefusalVMUnobserved, RefusalStoreUnavailable,
		RefusalVMDeleteFailed:
		return true
	default:
		return false
	}
}

// RefusalStatus maps a refusal to its HTTP status. Both the daemon handler and
// the client use it, so a status can never disagree with a code.
func RefusalStatus(code string) int {
	switch code {
	case RefusalUnknownOperation:
		return http.StatusNotFound
	case RefusalNotAuthority:
		return http.StatusForbidden
	case RefusalInvalidRequest, RefusalUnconfirmed, RefusalReasonRequired:
		return http.StatusBadRequest
	case RefusalStoreUnavailable, RefusalVMUnobserved, RefusalVMDeleteFailed:
		return http.StatusServiceUnavailable
	default:
		return http.StatusConflict
	}
}

// Valid rejects a request the daemon must not even attempt: it carries the exact
// confirmation token, names both identities, and records a non-empty reason.
// Confirmation and reason are checked here rather than only in the CLI so a
// direct socket caller cannot skip them.
func (r DischargeRequest) Valid() bool {
	return r.OperationID != "" && r.InstanceID != "" && r.Confirm == DischargeConfirmation &&
		strings.TrimSpace(r.Reason) != "" && len(r.Reason) <= MaxReasonBytes &&
		len(r.OperationID) <= MaxIdentifierBytes && len(r.InstanceID) <= MaxIdentifierBytes
}

const (
	// MaxReasonBytes bounds the audited operator reason. It is logged, never
	// persisted in durable state and never echoed to GitHub.
	MaxReasonBytes = 512
	// MaxIdentifierBytes bounds both identities so a malformed body cannot drive
	// an unbounded query.
	MaxIdentifierBytes = 128
	// MaxRequestBytes bounds the mutation body the daemon will read.
	MaxRequestBytes = 8 << 10
)
