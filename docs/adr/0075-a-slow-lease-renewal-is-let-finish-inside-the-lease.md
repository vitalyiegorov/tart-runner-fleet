# ADR 0075: A slow lease renewal is let finish inside the lease

## Status

Accepted. Amends [ADR 0025](0025-saturation-is-not-durable-failure.md) (the
per-attempt deadline of the authority lease renewal).

## Context

Issue #295, reopened on 2026-10-22. The Mac Studio ran at load 384, falling to
135. The load came from the owner's interactive work: Android NDK and Xcode
compiles, the Android emulator, iOS Simulators, and Chrome. The authority daemon
exited with

```
fleet daemon failed: controller authority lost: renew lease: context deadline exceeded
```

launchd relaunched it, and each relaunch re-created every scale-set session on
the starved host. It crash-looped and never completed a tick
(`successful_tick_missing`). The node's guests kept running, but the node
stopped serving its scale sets.

ADR 0025 already retries a transient renewal failure inside the lease's
validity. It also capped each attempt at five seconds, so that a wedged attempt
would leave room for retries. Under this load that cap is the defect. When a
renewal needs longer than five seconds to commit, every attempt is cut off,
every retry starts the same work again, and the guard runs out of retries while
the durable row still names this owner. The lease already renews on a dedicated
connection (#302), so a retry queues behind nothing the abandoned attempt was
not also waiting on. Cancelling a slow attempt only throws away its progress.

## Decision

**One renewal attempt may use the lease's whole remaining validity.** Its
deadline is the `ExpiresAt` of the lease it is renewing, not a fixed short
budget. The five-second per-attempt timeout is removed. A failure that comes
back quickly and is transient (SQLite busy after `busy_timeout`, an I/O error)
is still retried every two seconds while a retry fits before expiry. A fencing
loss (`ErrLeaseLost`) or a malformed request (`ErrInvalid`) still surrenders on
the first attempt.

**A renewal is stamped from when it began.** The expiry it commits is
`began + TTL`, however long the commit took. The next renewal is scheduled for
when the remaining validity falls to the ordinary margin (`TTL − interval`,
twenty seconds). A slow success is therefore followed sooner, rather than one
full interval later with only seconds of validity left.

### Invariant

1. Every renewal attempt is cancelled no later than the `ExpiresAt` of the last
   lease the guard durably holds. The guard never starts an attempt on a lease
   that has already expired.
2. If an attempt fails and no retry fits before that `ExpiresAt`, the guard
   reports authority lost at once. The daemon then cancels its work, so it
   stops acting at expiry plus the time that cancellation takes. That is the
   same bound as before this change.
3. A committed renewal never extends past `began + TTL`.
4. The successor's acquisition rule is unchanged: it needs an absent or expired
   row, and the `token` fences the old owner out. No two controllers can
   overlap.

This change does not lengthen the TTL, does not let the guard act past
`ExpiresAt`, and does not count an expired lease as held. It only stops the
guard from abandoning a renewal that could still have landed in time.

## Consequences

A host whose renewal commit takes longer than five seconds, but less than the
remaining twenty, no longer loses authority or crash-loops. A host too starved
to commit within the lease's validity still surrenders at expiry, and the
successor waits out the abandoned row as before (acquire window 1.5 × TTL).

What is not addressed: a relaunched daemon on a host that stays starved still
pays to re-create its sessions. That is no longer triggered by a slow renewal
that would have succeeded.

Tests: `TestAuthorityLeaseSurvivesARenewalSlowerThanAShortDeadline` (red on the
five-second budget) and
`TestAuthorityLeaseStillSurrendersWhenTheRenewalCannotLandBeforeExpiry` in
`internal/daemon/runtime_test.go`. Their fake store also fails if any attempt is
given a deadline past the lease's remaining validity.
