# ADR-0001: Independent implementation and reuse policy

Status: accepted — 2026-10-12

## Context
Patrol CD must reach functional parity with Argo CD v3.5.5 without rebranding, embedding or
proxying Argo CD. Argo CD is Apache-2.0 licensed.

## Decision
- All control-plane logic (reconciliation, diff, health evaluation engine, sync engine,
  API server, auth, RBAC, ApplicationSet, notifications, CLI, UI) is written for Patrol CD.
  `gitops-engine`, `notifications-engine` and Argo CD packages are **not** dependencies.
- Upstream source is read to determine behavior. Code is not copied verbatim.
- Exception — behavioral data: the built-in Lua health checks and resource actions under
  upstream `resource_customizations/` are reused as data files so that third-party CRD health
  and actions behave identically. They retain their license, are listed in `NOTICE`, and
  run inside Patrol's own sandboxed Lua runtime.
- General-purpose OSS libraries are acceptable (client-go, controller-runtime,
  structured-merge-diff, gopher-lua, expr, go-jsonnet, gojq, sprig, go-oidc, go-github,
  cobra, prometheus). Each new direct dependency is recorded in `docs/architecture/dependencies.md`
  with license and justification.
- Dex may be deployed as an independent upstream component for SSO brokering.

## Consequences
Parity must be demonstrated by tests rather than inherited. Behavioral drift risk is managed
by the parity matrix and side-by-side fixtures against upstream where practical.
