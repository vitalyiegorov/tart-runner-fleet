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
//	Create    tart clone <image> <name>    podman create --name <name> <image>
//	Start     tart run <name>              podman start <name>
//	Power     tart list                    podman inspect
//	Stop      tart stop <name>             podman stop <name>
//	Terminate tart stop <name> -t 0        podman stop --time 0 <name>
//	Destroy   tart stop -t 0 && delete     podman rm --force <name>
//	Delete    tart delete <name>           podman rm <name>
//	Reap      tart delete <name>           podman rm <name>       (operator authority)
//	List      tart list --format json      podman ps --format json
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

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
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
	Name string
	// Power is what the backend established about this instance's power state,
	// in three values rather than two.
	//
	// It was a bool until issue #252. Both backends compute that bool from a
	// reading that can fail — `tart`'s `running()` swallows every error opening a
	// VM's `config.json` and answers false (ADR 0042); podman's container state is
	// a string this adapter may not recognise — so "the backend could not tell"
	// arrived at the fleet as "the VM is off", which is the premise a destructive
	// recovery rests on. AGENTS.md rule 4 already forbade exactly this: an
	// unavailable observation may not be represented as an empty one. A bool has
	// nowhere to put the third answer, so it went where the type had room.
	//
	// InstancePowerUnknown is not a default. Every consumer names it.
	Power domain.InstancePower
	// Unreadable is why the backend could not determine Power, and how long the
	// attempt took. It is set only alongside InstancePowerUnknown, and is the
	// whole of what the fleet can say about a misreport it has never reproduced.
	Unreadable domain.PowerReadFailure
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
	// Stop asks the guest to power itself off and waits for it, under a deadline
	// the backend owns. An absent instance is success. It is the polite verb: a
	// guest that has stopped answering can refuse it indefinitely, which is why
	// the two below exist (ADR 0039).
	Stop(context.Context, string, operations.Ownership) error
	// Terminate powers the instance off without asking the guest, and without
	// waiting for a guest-initiated shutdown that may never come. An absent
	// instance is success. It ends the guest, never the fleet's own processes or
	// the message sessions it owns.
	Terminate(context.Context, string, operations.Ownership) error
	// Destroy terminates the instance and removes it in one step, for a drain
	// whose guest will not stop. It requires exactly the evidence Delete requires
	// — fresh ownership and fresh deletion confirmation — and differs from Delete
	// only in the force it applies to a guest that has already finished its work.
	// An absent instance is success.
	Destroy(context.Context, string, operations.Ownership) error
	// Delete removes the instance after fresh deletion confirmation. An absent
	// instance is success.
	Delete(context.Context, string, operations.Ownership) error
	// Power reports the instance's current power state; an absent instance is
	// proven not running. Recovery drains re-verify their premise against this
	// before every destructive step, so it must distinguish a VM the backend
	// found powered off from one whose power it could not read at all: the second
	// is not permission to act, and answering it as the first is what let thirty
	// drains of a live runner past their own guard on 2026-08-19 (issue #252).
	Power(context.Context, string) (domain.InstancePower, error)
	// Reap removes a stopped, owned instance under explicit operator authority,
	// without the runner-inactive confirmation Delete requires. It must refuse a
	// running instance.
	Reap(context.Context, string, operations.Ownership) error
	// List enumerates every instance the backend can see, so the fleet can
	// detect an owned instance that vanished and an untracked one it never
	// created.
	List(context.Context) ([]Instance, error)
}
