# ADR-0011: Operation resource and sync state machine

Status: accepted — 2026-10-12

## Decision
Sync and rollback requests are `Operation` objects. See `docs/architecture/sync-state-machine.md`.

- Concurrency: at most one non-terminal Operation per Application. Arbitration is done by
  the controller (oldest wins); the API server pre-checks to return 409 early.
- Idempotency: every mutation is derived from (operation UID, resolved revisions, task key);
  hook object names are deterministic; re-executing a step converges.
- Retry: `spec.retry` (limit, backoff) applies to `Failed` and `Error` attempts. A transient
  Kubernetes API error inside a step (timeouts, 429, 503, connection errors) consumes no
  retry budget: the step is requeued after a fixed poll interval (5s), without rate-limited
  backoff. A 500 fails the task; a 409 on a resource is retried inside the step, and one on
  the Operation abandons the step until the next reconcile
  (`docs/architecture/sync-state-machine.md`, "Retry semantics").
- Cancellation: `spec.cancel=true` → `Terminating` (running hooks are deleted per policy,
  no new tasks start) → `Cancelled`.
- Retention: keep the newest `revisionHistoryLimit` (default 10) terminal Operations per
  Application plus any non-terminal one; older ones are deleted by the controller.
- No exactly-once claim: resource application is at-least-once and idempotent.

Amended 2026-10-29: the retry item described rate-limited controller backoff for timeouts,
429, every 5xx and conflicts, which the controller never implemented.
