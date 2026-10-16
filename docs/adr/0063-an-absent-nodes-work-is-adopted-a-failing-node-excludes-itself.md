# ADR 0063: An absent node's work is adopted, a failing node excludes itself

## Status

Accepted, **partly built**. Implements the first half of
[issue #373](https://github.com/vitalyiegorov/tart-runner-fleet/issues/373).
The number skips 0061 and 0062 because two open pull requests (#367 and #372)
already claim 0061.

| Rung | What it is | State |
| --- | --- | --- |
| (a) self-heal | a wedged set's session is recycled, a bounded number of times | **built**, opt-in |
| (b) self-exclusion | a set that healing did not fix has its session released | **built**, opt-in |
| (c) sibling adoption | a sibling opens the session of a set whose owner is gone | **designed and simulated, not wired**; gated on live experiments E1–E4 below |
| (d) rejoin with probation | an excluded set comes back on probation; a held session is waited out, not fatal | **built** (the held-session half for every node, the probation half opt-in) |
| (e) roaming | a node that comes and goes declares it | **mode built** (shorter ladder); proactive yield on sleep **deferred** |

It amends, precisely:

- [ADR 0009](0009-recover-github-message-sessions.md): a session create GitHub
  refuses with `409` is a reason to wait, never to stop the node. Its reason is
  `session_held`.
- [ADR 0034](0034-a-node-serves-the-scale-sets-it-owns.md) §9: "no lost-node
  reclaim" stays true of what is built. Rung (c) is the first design in this
  repository in which a node acts on a set it does not own. It is recorded here
  so it is decided once, and not built until GitHub's behaviour is measured. No
  node reads another node's state under any rung. The only cross-node signal is
  GitHub's own session exclusivity, which §"What GitHub actually enforces"
  already names as the arbiter.
- [ADR 0047](0047-a-node-that-cannot-admit-yields-its-sessions.md): withdrawal
  can now be one binding wide. A node that can admit, but whose set GitHub has
  stopped delivering to, releases that set's session and keeps the rest.
- [ADR 0054](0054-a-parked-scale-set-is-audited-not-trusted.md)'s amendment
  ("the verdict is the hub's") stands for everything built. Rung (c) designs
  the one exception: a parked set's reading becomes the *premise* for asking
  GitHub's arbiter. It is never a verdict on its own. The verdict is GitHub's
  `409` or `200` (ADR 0042).
- [ADR 0056](0056-a-stranded-bound-scale-set-is-recreated.md): its Wedged
  verdict now drives an automatic **session** repair (rung a) and a **session**
  release (rung b). Automatic **recreation** stays refused, exactly as ADR 0056
  decided. Deleting a GitHub object on the daemon's own judgement is still a
  larger authority than any node holds.

## Context

On 2026-10-16 two `knee-doctor` jobs sat queued for about two hours. GitHub had
bound them to the MacBook's `trf-knee-linux-arm64-{2x4,4x8}-macbook` sets 19
and 20, which read `assigned=1 busy=1 registered=0 acquired=0`. Omarchy and the
mac mini advertised the same labels (`linux-portable-2x4/4x8`, ADR 0060) and
had free capacity, but held nothing. The MacBook is a laptop. It had gone to
work with its owner. The only remedy was to cancel and re-run the jobs by hand.

Every piece that could have helped requires the node to be present. ADR 0047
yields a node's sessions, but only a node that is running. ADR 0056 detects a
stranded bound set, but only from the node that serves it. ADR 0054 lets a
sibling see the stranding, but only as evidence, because one node cannot tell a
stranding from a sibling's backlog. And ADR 0047's own measurement says a job
already bound does not migrate.

## The spike: what code and documentation settle, and what they do not

Every finding below comes from reading code and documents. Nothing was run
against GitHub.

1. **A new session is told the set's assigned work as a count, not as jobs.**
   `RunnerScaleSetSession` carries `Statistics`, including `TotalAssignedJobs`
   (`github.com/actions/scaleset@v0.4.0/types.go`). Both reference listeners
   size their runner count from that initial count: the SDK's
   `listener.Run` ("Handling initial session statistics") and
   actions-runner-controller's `ghalistener` at `gha-runner-scale-set-0.10.1`
   (`cmd/ghalistener/listener/listener.go`, `Listen`, commit `3bda9bb`). They
   then register JIT runners against the **set**
   (`GenerateJitRunnerConfig(..., scaleSetID)`). GitHub's own design consumes
   already-assigned work this way: the job is bound to the set, the runner is
   registered to the set, and the broker gives the job to a runner of that set.
   Per-job messages are not redelivered.
2. **A job message is redelivered only if its earlier delivery was never
   acknowledged.** The SDK documents `GetMessage` with: "Unless a message is
   deleted after being processed (using DeleteMessage), it will be returned
   again". ADR 0009 observed that a replacement session, reading the durable
   cursor, receives an unacknowledged message again. So a `JobAssigned` that
   the owner never received or never acknowledged is still in the queue.
   Whether that queue belongs to the set or to the session, and whether a
   session under a *different owner name* reads it, is not stated anywhere.
   ADR 0009's observation was always the same owner.
3. **This fleet cannot admit a count.** Our daemon admits per job: ADR 0033
   binds a runner to the job GitHub gave it, and repository caps, targets, and
   priority all need the job's repository. A sibling holding only
   `TotalAssignedJobs` has nothing its admission can judge. If the owner
   acknowledged a job's message before it vanished (it commits, then
   acknowledges), a sibling would see no message for that job at all.
4. **Session expiry is not documented.** The SDK has no expiry concept.
   actions-runner-controller answers a `409` on session create by retrying
   every 30 seconds, up to 10 times (`sessionCreationMaxRetries`). That design
   expects GitHub to release a dead listener's session within minutes, but no
   bound is published. The incident cannot settle it either: two hours of work
   bound to the MacBook's sets fits a session that outlives its listener by
   hours, and equally fits jobs bound before the laptop slept.
5. **A `409` was invisible to this adapter, and fatal at startup.** The pinned
   client renders every non-2xx response as a string; it exposes no status.
   `SessionTerminal`'s `Conflict` branch is reachable only for this package's
   own `APIError`, never for the official client. A session create refused
   with `409` therefore reached `transientStartupFailure` as an unknown error,
   and the daemon exited. A laptop restarted by launchd while its previous
   session was still held would crash-loop until GitHub let go.
   `githubscaleset.SessionHeld` now recognises the client's rendering, anchored
   to the session endpoint. `sessionheld_test.go` drives the real pinned client
   against a fake actions service, so an SDK upgrade that changes the rendering
   fails a test.
6. **JIT registration under adoption** would use the adopted set's id with the
   adopter's own App installation. That works only where both nodes'
   installations cover the scope. They do today, because every node is
   configured from `config/nodes/shared/scopes.json`. It is a precondition of
   rung (c), not a given.

**Verdict.** It is *not* settled that a sibling's new session receives work
already assigned to the set in a form this fleet can admit. Nor is it settled
how long GitHub keeps an unpolled session. Rung (c) is therefore designed,
proven against a model, and **not wired**. No configuration enables it. A flag
that enabled an unbuilt path would be a knob that does nothing, which is the
overengineering the owner's first question refuses.

### Live experiments the wiring PR must run first (none were run)

All in a throwaway repository scope with two `scaleset` SDK programs, owner
names `a` and `b`, on one scale set `S`:

- **E1, redelivery.** `a` opens a session on `S`. Queue one job. `a` receives
  the `JobAssigned` and is killed (`kill -9`, no `DELETE`) *before* deleting
  the message. `b` creates a session on `S`, once immediately and then every
  30 s. Record: the create status; whether `GetMessage(lastMessageId=0)`
  returns that `JobAssigned` to `b`; the session's `Statistics`.
- **E2, expiry.** Repeat E1 and record the time from `a`'s last poll to `b`'s
  first successful create. Repeat once with `a` on a laptop whose lid is closed
  instead of killed.
- **E3, arbitration.** While `a` is actively polling, `b` creates a session.
  Record whether `b` gets `409` (the premise of rung c), or whether `a` is
  evicted (the "evict each other in a loop" ADR 0034 describes for two daemons
  sharing a name).
- **E4, acknowledged then absent.** `a` receives and *acknowledges* the
  `JobAssigned`, then is killed before generating JIT. `b` obtains the session.
  Record whether the job is ever offered to `b` as a message, or only as
  `TotalAssignedJobs`. Then record whether a JIT runner `b` registers on `S`
  without `AcquireJobs` receives it.

Rung (c) is wired only after E1–E4 are recorded in an amendment to this ADR.
If E4 says "count only", rung (c) also needs a statistics-driven admission path
that ADR 0033 does not allow today, and that is a separate decision.

## Decision

### Names

- **`membership`** is the configuration key: the node's declared place in the
  fleet. It is omitted by default, and omitted means no watchdog. That is the
  node this code was before, byte for byte in the file
  (`internal/config/membership_test.go`).
- **`stationary`** and **`roaming`** are the two modes. The owner suggested
  "not stationary runner". *Roaming* is that idea as one positive word: it is
  the established term for a device that leaves its home network, and it pairs
  with *stationary* without a negation.
- The four stages of one bound set are **serving**, **healing**, **excluded**
  and **probation**. *Excluded* was chosen over the issue's *quarantined*.
  Quarantine implies the node is suspect and isolated as a whole. What happens
  here is narrower and is the node's own act: it stops competing for one set's
  work. The issue calls that self-exclusion.

### The ladder (rungs a, b, d), one rule stated once

`membership.Step` is the rule. `internal/daemon/membership.go` adds no
mechanism of its own; it only carries the rule's decisions out.

- **Evidence** is the scale-set audit's ADR 0056 verdict. `Wedged` is the
  `strandedScaleSets` the authority already publishes, read only for sets this
  node serves. `membership.Policy.Fresh` is contributor rule 4 on this path:
  evidence counts only if an audit completed, at most two audit cadences ago,
  and not in the future. Stale or missing evidence moves the ladder in neither
  direction.
- **Every transition is judged by an audit taken after the action it judges.**
  A binding remembers the audit that caused its last transition (`JudgedAt`),
  and nothing moves until a strictly newer one arrives. A heal can never be
  judged by the reading that triggered it.
- **serving → healing (heal).** A fresh audit reads the set wedged. The heal
  is what a daemon restart does to one binding: close its session and open a
  new one through the factory ADR 0009's recovery already uses. A heal whose
  release GitHub refuses still counts as an attempt.
- **healing → healing (heal)**, up to `HealAttempts`: 2 for stationary, 1 for
  roaming. **healing → serving** when an audit taken after the heal reads the
  set clean.
- **healing → excluded (exclude).** The attempts are spent and the set is
  still wedged. The set's session is released by the session-yield
  controller's existing level-triggered loop, so GitHub binds new jobs to a
  sibling that advertises the same labels. That is ADR 0047's measurement
  applied to one binding. The binding reports `stale` / `session_yielded`
  rather than polling into a failure it caused itself.
- **excluded → probation (rejoin)** by **time alone**: 4 audit cadences for
  stationary, 2 for roaming, doubling on each consecutive exclusion, capped at
  16. Rejoining is the direction back to configured behaviour, so an audit
  outage must never hold a set out. The bound job keeps the set reading wedged
  while it is excluded, so a verdict could never end the exclusion anyway.
- **probation → serving** on the first clean audit taken after the rejoin.
  **probation → excluded** if that audit reads wedged.

Every bound is derived from the mode and the audit cadence the node already
declares (`github.parkedScaleSetAuditMinutes`). No new knob was added: the
evidence arrives once a cadence, so no bound could usefully be shorter.

A node that has withdrawn every session under ADR 0047 has nothing to heal, so
its ladder holds still until it rejoins.

Only the authority runs the ladder, because only the authority audits. A
membership declared on a node that cannot be handed evidence is refused by
`config.Validate`: the audit disabled, or no `github.scopes`. A watchdog that
can never act would be the "green no-op" the third question warns about.

### Held sessions are waited out (rung d, every node)

A session create refused with `409` defers the binding instead of ending the
run (startup), and is retried every poll instead of being abandoned (a pending
binding). The reason is `session_held`. This changes behaviour on every node,
not only on opted-in ones. It is the one part of this ADR that is not opt-in,
and it is the conservative direction: before, the same condition killed the
daemon.

### Adoption (rung c), designed and not built

`membership.AdoptionPolicy` is the rule, with no production caller:

- **`MayAsk`.** A sibling may ask GitHub for a set's session only if all of
  these hold: the set is *equivalent* to one it serves (same scope, identical
  labels, which ADR 0034 amendment c makes a promise of equivalent capability);
  its own audit is fresh; the set has read stranded (`assigned|busy > 0`,
  `registered = 0`) across audits that span the grace (two cadences, so at
  least three audits agree and one reading followed by an outage never
  qualifies); and its own admission would admit now. Every term is a fact the
  sibling holds. The claim about the owner is left to the arbiter.
- **`Arbitrate`.** `409`: the owner is alive, possibly wedged and possibly
  behind its own capacity. Do nothing. Obtained: no other session holds the
  set, so serve its work within the sibling's own admission, under a lease of 4
  cadences. Anything else (timeout, 5xx, refused credential) proves nothing and
  adopts nothing.
- **`Release`.** Hand the session back when nothing of the set's is left to
  run, and in any case when the lease ends, so a returning owner is locked out
  for at most one lease. A returning owner gets `409` meanwhile and waits it out
  as `session_held` (rung d).
- A sibling cannot read its owner's declared mode, so a roaming owner cannot
  shorten a sibling's grace. A roaming owner shortens its own exposure instead
  (rung e).
- No foreign set is ever deleted, and no configuration is ever written.

### Roaming (rung e)

`membership: roaming` gives a node's sets the shorter ladder above. The second
half of the issue's rung (e) is to yield every session *before* the host goes
dark, on a sleep or network-change notification. It is deferred, and here is
why. A notification that arrives before sleep needs IOKit on macOS. The release
is built with `CGO_ENABLED=0`, so that means cgo or a helper process, and
neither belongs in this change. A network-loss notification arrives too late to
`DELETE` anything. The follow-up needs a `HostEvents` port in
`internal/executor` (implemented by the macOS and Linux host probes, with
logind's `PrepareForSleep` as the Linux source) whose events feed the existing
session-yield controller as a forced withdrawal.

## Consequences

- **The incident is not yet fixed by this ADR.** An absent node runs no code.
  What ships makes a *present* wedged node repair itself and then stop
  attracting work it cannot deliver. It also makes a returning or restarted
  node wait out its own lingering session instead of crash-looping. The
  incident needs rung (c) or rung (e)'s proactive yield, and both are gated as
  stated above.
- **A false Wedged verdict costs capacity, and the cost is bounded.** ADR
  0054's amendment recorded a healthy set whose counters read
  `assigned=4 busy=4 registered=0` with an empty queue. The ladder would heal
  and then exclude such a set. Where a sibling advertises the same labels,
  GitHub routes the new work there. Where no sibling does, new jobs for those
  labels wait for the next probation: at most one exclusion (1 h stationary,
  30 min roaming at the default cadence, doubling to at most 4 h for a set that
  keeps reading wedged). This is why the ladder is opt-in and an exclusion
  always ends on time. A node that is the only one advertising its labels
  should declare `membership` only after reading this.
- **No behaviour changes on an existing node** until its configuration declares
  `membership`, apart from `session_held`. That converts an exit into a retry.
- **API.** Additive `fleet.v1` fields: `membership` (`mode`, `state`,
  `bindings[]`) and `membershipCheck`. One doctor row, `membership`, fails only
  for an excluded set. One gauge, `fleet_membership_scale_sets{stage}`. One
  ingest reason, `session_held`. The declared policy (ADR 0053) gains
  `membership`, so its digest changes once for every node.
- **No migration and no durable rows.** The ladder is level-triggered from
  GitHub's own evidence and lives in memory. A restarted daemon starts every
  set at serving, and a restart is itself the heal rung (a) would perform. A
  set that is still wedged climbs the ladder again within a few cadences. A
  durable row would add a migration and a store port, and the only thing it
  would buy is skipping one heal after a restart.
- **Authority limits.** Observe and shadow are untouched: they run no ladder.
  Nothing here enables authority mode.

## The three questions

1. **Overengineering removed?** No new pass, store, or knob. Heal reuses ADR
   0009's recovery and exclusion reuses ADR 0047's controller, one binding
   wide. Every bound is derived from the existing audit cadence. Adoption ships
   no flag, because it is not wired.
2. **Complexity reduced?** One new axis, per-binding membership, expressed as
   one pure function (`membership.Step`). The node-wide yield could not express
   it: withdrawing a whole node because one set is wedged would take every
   healthy set's capacity with it.
3. **Testable?** `internal/membership` is pure and fully covered. The ladder's
   wiring is tested against fake sessions (`internal/daemon/membership_test.go`).
   `tests/simulation/membership_test.go` is the first cross-node world. It
   models GitHub's session arbiter, an owner, and its siblings, and sweeps
   session expiry over 2 min, 15 min, and 1 h, since the spike could not settle
   it. It checks these scenarios on every seed:
   - the owner vanishes with bound queued jobs;
   - the owner vanishes mid-job (that job is GitHub's to fail and is never
     restarted);
   - the owner returns while its set is adopted;
   - two siblings race to adopt (GitHub picks exactly one);
   - a wedged but alive owner (`409`, never adopted from);
   - the same owner with its watchdog on (it excludes itself and is then
     adopted from);
   - an unavailable audit (never read as an absent owner).

   Two invariants hold on every tick: at most one present node believes it
   holds a set's session, and that node is the one GitHub granted it to; and no
   job starts twice. The world model's one assumption is the expiry parameter.
   If E2 measures a value outside the swept range, that is a world-model gap to
   close before rung (c) is wired.
