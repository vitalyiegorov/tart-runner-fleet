# ADR 0008: Keep runner VMs independent from controller restarts

## Status

Accepted

## Context

`fleetd` starts each long-lived Tart VM as a child process. The daemon itself
runs as a launchd job and is intentionally restarted during upgrades and
operational recovery. With launchd's default process-group behavior and no
independent session on the Tart child, restarting the controller can terminate
an active VM while GitHub still reports its runner and job as busy. GitHub can
then retain the impossible job for minutes, and durable cleanup must remain
fail-closed.

## Decision

Tart run processes start in a new Unix session, and every shipped launchd mode
sets `AbandonProcessGroup=true`. Controller shutdown therefore stops only the
controller. Durable state and fresh Tart/GitHub observations reconnect the new
controller generation to VMs that continue executing.

A stopped VM already in a teardown state remains durably live for ownership,
deregistration, and deletion, but no longer counts against CPU, memory, slots,
or host-mode exclusivity. Unknown power, running teardown instances, and
stopped assigned/running instances continue to consume capacity until the
recovery transition is durably committed.

## Consequences

Normal controller restart and atomic upgrade no longer interrupt active CI.
Unexpected VM loss cannot globally starve unrelated queues while GitHub
terminalizes an offline busy runner. Cleanup remains idempotent, retried, and
fully ownership-guarded; no database row or VM is deleted merely to restore
capacity.
