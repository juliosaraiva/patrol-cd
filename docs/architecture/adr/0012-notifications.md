# ADR-0012: Notifications engine

Status: accepted — 2026-10-12

## Decision
Patrol implements its own notifications engine with the Argo CD configuration format
(`trigger.*`, `template.*`, `service.*`, `subscriptions`, `defaultTriggers`, `context`;
`$secret-key` references into `patrol-notifications-secret`), `expr` for trigger conditions
and Go `text/template` + sprig for templates. Delivery state is recorded per
(trigger, condition hash, recipient) in an Application annotation so deliveries are
deduplicated across restarts; failed deliveries emit Events and metrics and are retried with
backoff within a bounded window.
