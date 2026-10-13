# ADR-0003: Resource model

Status: accepted — 2026-10-12

## Decision
CRDs (all namespaced, status subresource, conditions with `observedGeneration`):

| Kind | Purpose | Notes |
|------|---------|-------|
| `Application` | desired source(s) → destination | Spec mirrors Argo CD; no top-level `operation` field |
| `Operation` | one sync/rollback request and its execution record | Owned by its Application; retention-bounded; replaces Argo's `Application.operation` + `status.operationState.syncResult` |
| `AppProject` | tenancy policy | Mirrors Argo CD |
| `ApplicationSet` | templated Application generation | Mirrors Argo CD |
| `Repository` | one repository (git/helm/oci), read or write purpose | Credentials only via `secretRef`; replaces labeled repository Secrets |
| `RepositoryCredential` | URL-prefix credential template | Replaces `repo-creds` Secrets |
| `Cluster` | destination cluster | Credentials via `secretRef`; replaces cluster Secrets |
| `SCMProvider` | SCM API endpoint + auth (GitHub App, token) + webhook secret + status reporting | New; referenced by repositories, generators, notifications |

Settings, RBAC policy, notification catalog, TLS certs, SSH known hosts and GPG keys remain in
ConfigMaps/Secrets whose key names follow Argo CD (`patrol-cm`, `patrol-rbac-cm`,
`patrol-cmd-params-cm`, `patrol-secret`, `patrol-notifications-cm`,
`patrol-notifications-secret`, `patrol-tls-certs-cm`, `patrol-ssh-known-hosts-cm`,
`patrol-gpg-keys-cm`). They are free-form maps where CRD typing adds little, and identical
keys make migration a copy operation. All components share one parser
(`internal/settings`) that reports invalid keys as Kubernetes Events and through
`patrol admin settings validate`.

## Alternatives considered
- Keep Argo's `operation` field on Application: rejected; mixing request/progress into the
  Application object makes per-resource results unbounded in Application status and makes
  operation history non-inspectable.
- Typed `Settings` CRD: deferred; would block migration-by-copy and the settings surface is
  ~150 free-form keys.
