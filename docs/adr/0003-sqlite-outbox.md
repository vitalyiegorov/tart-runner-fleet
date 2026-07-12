# ADR 0003: SQLite WAL and operation outbox

## Status

Accepted.

## Decision

SQLite is the single-host durable authority. WAL mode, foreign keys, a busy
timeout, schema migrations, and process ownership fencing are mandatory. Each
plan and its idempotent operations are committed in one transaction. Executors
lease operations for bounded periods and may retry them after crashes.

External operations are at least once. Their effects are exactly once where the
backend supports idempotency, otherwise reconciliation observes the effect
before retry. Instance names and operation keys are deterministic and include
controller ownership.

## Consequences

Process restart cannot forget a reservation, create a second VM for one job, or
delete an unowned VM. The database remains local because the Mac mini is one
failure domain; backups and exported metrics cover disaster recovery.

