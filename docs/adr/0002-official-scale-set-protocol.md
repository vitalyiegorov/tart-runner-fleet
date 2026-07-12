# ADR 0002: Official GitHub Actions Scale Set protocol

## Status

Accepted with preview isolation.

## Decision

Demand and just-in-time registration use `github.com/actions/scaleset`, pinned to
an reviewed version. The dependency is isolated behind local ports because the
upstream module is a public preview. REST discovery exists only as a bounded,
paginated compatibility adapter and never converts an API error into an empty
queue.

Scale-set messages are processed at least once: the cursor is durable, work is
committed before acknowledgement, and duplicate delivery is harmless. JIT
configuration is an in-memory secret and is never written to SQLite or logs.

## Consequences

The daemon follows GitHub's runner assignment protocol instead of continuously
scraping every repository. A future upstream API change affects one adapter,
not the scheduler or lifecycle engine.

