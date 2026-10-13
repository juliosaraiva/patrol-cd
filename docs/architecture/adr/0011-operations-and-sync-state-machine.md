# ADR-0011: Operation resource and sync state machine

Status: accepted — 2026-10-12

## Decision
Sync and rollback requests are `Operation` objects. See `docs/architecture/sync-state-machine.md`.

- Concurrency: at most one non-terminal Operation per Application. Arbitration is done by
  the controller (oldest wins); the API server pre-checks to return 409 early.
- Idempotency: every mutation is derived from (operation UID, resolved revisions, task key);
  hook object names are deterministic; re-executing a step converges.
- Retry: `spec.retry` (limit, backoff) applies to Failed attempts; `Error` (infrastructure)
  attempts retry with controller backoff without consuming user retry budget only for
  transient API errors (timeouts, 429, 5xx, conflicts).
- Cancellation: `spec.cancel=true` → `Terminating` (running hooks are deleted per policy,
  no new tasks start) → `Cancelled`.
- Retention: keep the newest `revisionHistoryLimit` (default 10) terminal Operations per
  Application plus any non-terminal one; older ones are deleted by the controller.
- No exactly-once claim: resource application is at-least-once and idempotent.
