// Package noexecutor is the backend of a node that has no execution technology.
//
// ADR 0034 gives each node exactly one backend, and docs/MULTI_NODE_PLAN.md
// brings the second node up in two stages: the daemon runs first, in observe
// mode with no scale sets (Phase 1 Part A), and the container adapter that can
// actually start a runner arrives afterwards (Phase 2, issue #139). Between
// those two points the node is a real fleet daemon on a real machine with
// nothing to provision onto, and something has to say so.
//
// Saying so is the whole package. Every verb that would bring an instance into
// existence, or act on one, fails with ErrNoBackend, so a misconfigured node
// produces a loud, retrying, eventually parked operation instead of a runner
// that silently never appears. `internal/daemon` refuses to start in any mode
// but observe while this is the backend, so those verbs are unreachable in
// practice and the failure is a second line of defence, not the design.
//
// List and Running are the two verbs that observe rather than act, and they
// answer truthfully rather than fail. This is not the empty-collection-for-an
// -unavailable-observation that AGENTS.md §4 forbids: nothing is unread here.
// A node with no execution technology cannot hold an instance — Create is
// incapable of producing one — so the empty set is the measurement, and
// reporting it as unavailable would fail-close a node whose only job in this
// stage is to observe.
package noexecutor

import (
	"context"
	"errors"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/executor"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// ErrNoBackend is why a mutation was refused. It names the deployment stage
// rather than a fault, because on a node in Phase 1 Part A this is the correct
// answer and not a broken one.
var ErrNoBackend = errors.New("noexecutor: this node has no execution technology, so it can only observe")

// Backend implements executor.Backend, and with it lifecycle.VMControl,
// app.ExecutorInventory and discharge.VM.
type Backend struct{}

func (Backend) Create(context.Context, executor.InstanceSpec) error { return ErrNoBackend }

func (Backend) Start(context.Context, string, operations.Ownership) error { return ErrNoBackend }

func (Backend) Stop(context.Context, string, operations.Ownership) error { return ErrNoBackend }

func (Backend) Delete(context.Context, string, operations.Ownership) error { return ErrNoBackend }

func (Backend) Reap(context.Context, string, operations.Ownership) error { return ErrNoBackend }

// Running reports the power state of an instance that cannot exist.
func (Backend) Running(context.Context, string) (bool, error) { return false, nil }

// List enumerates the instances this node can see. The slice is empty and never
// nil, so a caller ranging over it cannot tell an empty answer from a missing
// one by accident.
func (Backend) List(context.Context) ([]executor.Instance, error) {
	return []executor.Instance{}, nil
}
