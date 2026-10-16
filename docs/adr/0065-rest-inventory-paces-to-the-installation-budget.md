# ADR 0065: The REST inventory paces itself to the installation's rate budget

## Status

Accepted for issue #377. Amends [ADR 0015](0015-canonical-inventory-and-truthful-capacity.md)'s
canonical queue observer with a budget rule; no change to routing, admission,
authority, or the `fleet.v1` API.

## Context

The canonical REST inventory lists five workflow-run statuses per repository and
the jobs of every active run on every refresh. With five repositories and a
dozen active runs that is about 38 billed requests every 30 seconds per node.
Two nodes share one GitHub App installation whose primary limit is 5,000
requests an hour, so the window was spent about 45 minutes in, and every refresh
for the rest of the hour failed. The Mini and the MacBook showed the same
failure slot in every hour of 2026-10-16 while the separately installed
`budgie-org` scope stayed fresh beside them.

GitHub answers the exhausted primary limit with 403, no `Retry-After`,
`X-RateLimit-Remaining: 0`, and the epoch reset in `X-RateLimit-Reset`.
`ClassifyResponse` read that 403 as an authorization failure with no retry
horizon; the REST ingester returned the raw error, so the failure counter said
`unclassified` and health said only `queue_observation_stale`. The node then
kept refreshing every 30 seconds against a window GitHub had already refused.

## Decision

1. A 403 whose remaining budget is zero is `RateLimited`. When `Retry-After`
   is absent the retry horizon is the window reset. A 403 with budget left stays
   an authorization failure.
2. The observer revalidates every page it fetched last time with
   `If-None-Match`. GitHub does not bill a 304 against the primary limit, and
   most pages are unchanged between two polls: the three rarely populated status
   lists and every run whose jobs have settled. A page absent from a refresh is
   forgotten. The latest `X-RateLimit-*` headers travel on the `Observation`,
   on success and on failure alike; without the headers the budget is
   unobserved, never empty.
3. The REST ingester names a failed refresh in the existing bounded vocabulary
   (`rate_limited`, `server_error`, else the existing stale/failed detail) for
   both the failure counter and the observation detail, and defers the next
   refresh to the window reset when the budget is scarce (one tenth of the
   limit is reserved for session creation, JIT credentials, the parked-set
   audit, and sibling nodes) or GitHub has already refused. The deferral is
   never below the poll interval and never above one hour.

The inventory stays stale, never empty, while it waits. No new configuration
knob: the reserve and the bound are constants of GitHub's own window.

## Consequences

- Two nodes on one installation stop spending the window out; a comfortable
  budget keeps today's cadence exactly.
- The failure counter and `detail` say `rate_limited` where they said
  `unclassified` and `queue_observation_stale`.
- A node that is told the shared window is scarce by another node's spending
  backs off too, because the headers report the installation, not the node.
- An elapsed reset, or a rate limit with no horizon, defers nothing: the next
  poll is the only way to learn the new window.
