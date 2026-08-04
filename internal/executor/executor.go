// Package executor is the seam between the fleet's lifecycle and the execution
// technology of the node it runs on.
//
// One node has exactly one backend: Tart on Apple silicon, ephemeral containers
// on Linux/amd64 (ADR 0034 §1, docs/MULTI_NODE_PLAN.md). Nothing above this
// package may name a backend's own types, because the moment a port speaks Tart
// there is no second implementation of it — which is precisely what
// `lifecycle.VMControl` taking a `tart.Request` and `app` reading a `tart.VM`
// had made true.
//
// The verbs are the ones every ephemeral-runner backend already has, and no
// more:
//
//	Create  tart clone <image> <name>      podman create --name <name> <image>
//	Start   tart run <name>                podman start <name>
//	Running tart list                      podman inspect
//	Stop    tart stop <name>               podman stop <name>
//	Delete  tart delete <name>             podman rm <name>
//	Reap    tart delete <name>             podman rm <name>       (operator authority)
//	List    tart list --format json        podman ps --format json
//
// "Clone a base image" and "create a container from an image reference" are the
// same verb over InstanceSpec.Image, so nothing VM-specific survives here: no
// disk image, no snapshot, no display, no nesting flag. A backend that cannot
// honour a dimension of the spec ignores it, exactly as a container backend
// ignores DiskGB.
//
// What this package deliberately does NOT own: readiness polling, JIT bootstrap,
// registration, and drain guards are policy, and they live in
// internal/lifecycle behind their own already-neutral ports.
package executor

import (
	"context"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
)

// CommandRunner is one argument vector of a backend's command-line interface.
// Both supported backends are CLI-shelling adapters, so this is the single
// primitive their implementations share, and a port typed on it can be wired to
// either without naming one.
//
// Implementations must never assemble a shell command: the vector is passed to
// the process directly, under the caller's context deadline.
type CommandRunner interface {
	Run(context.Context, ...string) ([]byte, error)
}

// InstanceSpec is the request one backend receives to bring an instance into
// existence. It is the backend-neutral replacement for tart.Request.
type InstanceSpec struct {
	// Name is the instance identity, already validated by
	// domain.ValidateInstanceName. It is the VM name, the container name, and
	// the GitHub runner name, which is why one grammar governs all three.
	Name string
	// Image is what the instance is made from: a Tart base VM name, or an OCI
	// image reference.
	Image string
	CPU   int
	// MemoryMB is the guest's memory allowance in mebibytes.
	MemoryMB int
	// DiskGB is the guest's minimum root disk in gibibytes. A backend with no
	// disk sizing ignores it; zero means the image's own size stands.
	DiskGB int
	// Ownership is the durable proof that this controller owns the instance.
	// Every mutating verb re-checks it, so a backend can never act on an
	// instance the fleet does not own.
	Ownership operations.Ownership
}

// Instance is what List reports: one instance the backend can see, whether or
// not the fleet owns it. It is the backend-neutral replacement for tart.VM.
type Instance struct {
	Name    string
	Running bool
	// Source is where the backend found the instance. Tart reports "local" or
	// "oci"; a container backend reports its image reference.
	Source string
}

// Backend is what a node's execution technology must provide. It is the whole
// contract: an implementation of exactly these verbs is a node, and no caller
// needs anything else from the machine's virtualization or container layer.
//
// Every mutating verb is idempotent for an instance that is already in the
// requested condition, and treats an absent instance as success where the
// fleet's durable cleanup depends on retry (Stop, Delete, Reap) — ADR 0007.
// Every verb that observes must fail rather than report an empty or default
// observation, because an unreadable fact is not a measurement (AGENTS.md §4).
type Backend interface {
	// Create brings the instance into existence from InstanceSpec.Image and
	// leaves it stopped with the requested resources. It records ownership
	// before the instance exists, so a lost response cannot orphan it, and is
	// idempotent for an instance this controller already owns.
	Create(context.Context, InstanceSpec) error
	// Start powers the instance on and returns only once the backend confirms
	// it is running, or fails.
	Start(context.Context, string, operations.Ownership) error
	// Stop powers the instance off. An absent instance is success.
	Stop(context.Context, string, operations.Ownership) error
	// Delete removes the instance after fresh deletion confirmation. An absent
	// instance is success.
	Delete(context.Context, string, operations.Ownership) error
	// Running reports the instance's current power state; an absent instance is
	// not running. Recovery drains re-verify their premise against this before
	// every destructive step.
	Running(context.Context, string) (bool, error)
	// Reap removes a stopped, owned instance under explicit operator authority,
	// without the runner-inactive confirmation Delete requires. It must refuse a
	// running instance.
	Reap(context.Context, string, operations.Ownership) error
	// List enumerates every instance the backend can see, so the fleet can
	// detect an owned instance that vanished and an untracked one it never
	// created.
	List(context.Context) ([]Instance, error)
}
