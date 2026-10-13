# Patrol CD architecture

Status: accepted (living document). Decisions with alternatives are recorded under
`docs/architecture/adr/`.

## Goals

1. Continuously reconcile declaratively specified application state from version-controlled
   sources into Kubernetes clusters, with functional parity to Argo CD v3.5.5.
2. Kubernetes is the only authoritative store. Every cache is disposable.
3. Tenant isolation and least privilege between components, projects and plugins.
4. Explainable behavior: every decision is visible as a condition, event, operation record
   or audit entry.

## Component topology

```
                       ┌───────────────────────────── control-plane namespace ─────────────────────────────┐
  browser / CLI ──TLS──▶ patrol-server ──────────────┐                                                     │
  SCM webhooks  ──TLS──▶  (API, UI, auth, RBAC,      │ kube API (CRDs, Secrets, ConfigMaps, Events, Leases)│
                       │   webhooks, logs, terminal,  ▼                                                     │
                       │   extensions proxy) ───▶ kube-apiserver ◀──── patrol-controller (StatefulSet,      │
                       │        │   │                 ▲   ▲            sharded by cluster: refresh, diff,    │
                       │        │   └── internal tree API ──────────▶  health, sync operations, hydrator,  │
                       │        │                     │   │            live-state cache per cluster)        │
                       │        ▼                     │   │                 │                               │
                       │  patrol-repo-server ◀────────┼───┼─────────────────┘   (render requests carry       │
                       │   (git/helm/oci fetch,       │   │                      resolved credentials)       │
                       │    render, no kube token)    │   │                                                  │
                       │     └─ cmp sidecars (unix socket, per-plugin container, no kube token)            │
                       │  patrol-appset-controller ───┘   │  (generators, SCM providers, progressive sync)   │
                       │  patrol-notifications-controller ┘  (triggers, templates, delivery)               │
                       │  patrol-commit-server (hydrator writes; holds write credentials only)             │
                       │  dex (optional, upstream image)                                                   │
                       └────────────────────────────────────────────────────────────────────────────────────┘
                                   │ cluster credentials (from Cluster CRD → Secret)
                                   ▼
                        destination clusters (in-cluster and remote)
```

All control-plane components ship in one image (`patrol-cd`) as subcommands of the
`patrolcd` multi-call binary; the CLI is a separate `patrol` binary.

| Component | Workload | Kubernetes permissions | Holds secrets |
|-----------|----------|------------------------|---------------|
| patrol-server | Deployment, horizontally scalable | read/write Patrol CRDs; read Secrets in control-plane namespace (repo/cluster/settings); create Events; pods/log + pods/exec on destination clusters via cluster credentials | session signing key, OIDC client secret, webhook secrets |
| patrol-controller | StatefulSet, one shard per lease | read/write Patrol CRDs + status; read Secrets (repo/cluster creds); full access to destination clusters via cluster credentials (or impersonation) | cluster credentials, repo credentials (forwarded per request) |
| patrol-repo-server | Deployment, scalable | **none** (`automountServiceAccountToken: false`) | only credentials passed per request, kept in memory / tmpfs for one operation |
| cmp sidecar | container in repo-server pod | none | none (receives source tree only) |
| patrol-appset-controller | Deployment, leader-elected | read/write Applications/ApplicationSets; read SCM provider Secrets | SCM tokens / GitHub App keys |
| patrol-notifications-controller | Deployment, leader-elected | read Applications/AppProjects; read notification ConfigMap/Secret; write delivery-state annotations | notification service credentials |
| patrol-commit-server | Deployment | none | write credentials passed per request |

## State ownership

| State | Owner (writer) | Store | Rebuildable |
|-------|---------------|-------|-------------|
| Application spec | users/API/appset controller | `Application` CRD | — (authoritative) |
| Application status (sync, health, conditions, resources summary, bounded history) | controller (status subresource) | `Application.status` | yes, from live state + repo |
| Sync operation request + progress + per-resource results | API/controller (spec), controller (status) | `Operation` CRD (owned by Application, retention-bounded) | progress is authoritative for in-flight recovery |
| Project policy | users/API | `AppProject` CRD | — |
| Repositories, credential templates, clusters, SCM providers | users/API | `Repository`, `RepositoryCredential`, `Cluster`, `SCMProvider` CRDs + referenced Secrets | — |
| Settings, RBAC, notifications, TLS/SSH/GPG trust | users | ConfigMaps / Secrets with Argo-compatible key names | — |
| Rendered manifests | repo-server | in-memory + tmpfs LRU | yes |
| Git clones | repo-server | emptyDir | yes |
| Live object graph per cluster | controller | in-memory informers | yes (relist) |
| Resource tree served to UI | controller (internal API) | in-memory | yes |
| Session revocations | server | Secret `patrol-session-revocations` (pruned at expiry) | no (authoritative for logout) |
| Shard ownership | controller | `coordination.k8s.io/Lease` per shard | yes |
| Webhook delivery dedup | server | in-memory LRU per replica | yes (effects are idempotent) |
| Notification delivery state | notifications controller | annotation on Application | yes (may re-send once after loss) |

Redis is not used (ADR-0004).

## Request and reconciliation flows

### Refresh (comparison)

1. Triggers: periodic (`timeout.reconciliation` ± jitter), `cd.patrol.example/refresh`
   annotation (set by API/CLI/webhooks), spec change (generation), live-state change of a
   managed resource or its descendants, project/cluster/repo change.
2. Controller resolves every source revision (branch/tag/semver → commit SHA or chart
   version/digest) through repo-server; credentials resolved by the controller from
   `Repository`/`RepositoryCredential` and forwarded.
3. Repo-server renders (cache key = repo URL, resolved revision, path, source spec digest,
   tool versions, referenced `$values` revisions, kube version, API versions, app name/namespace).
4. Controller loads live objects from the cluster cache, applies tracking to determine
   managed resources, normalizes and diffs, assesses health, evaluates project policy.
5. Status patch: `sync.status`, `sync.revisions`, `health`, `resources` (bounded),
   `conditions` (with `observedGeneration`), `reconciledAt`, `summary`.
6. If automated sync is enabled and the app is OutOfSync at a revision that has not already
   been attempted (or self-heal is enabled and backoff permits), the controller creates an
   `Operation` (`initiatedBy.automated=true`).

### Sync operation

`Operation` objects are processed by an explicit state machine (see `sync-state-machine.md`):
`Pending → Running → (Succeeded | Failed | Error | Cancelled)`, with `Terminating` as an
intermediate state for cancellation and `Running` re-entered after controller restarts.

1. Admission: only one non-terminal Operation per Application; extra ones are marked
   `Rejected` with reason `OperationInProgress` (oldest `creationTimestamp`, then name wins).
2. Plan: resolve revisions (persisted to `status.syncResult.revisions` before any mutation),
   render, compute tasks (prune/apply/hooks) ordered by phase → wave → kind order → name,
   check project permissions per task, compute destructive warnings.
3. Execute wave by wave: dry-run the wave, apply/prune, wait for health/hook completion,
   persist per-resource results and `status.progress` after every wave.
4. Recovery: on restart, a `Running` operation re-plans from the persisted resolved
   revisions; completed tasks are recognized from live state + persisted results, hooks are
   identified by deterministic names derived from the operation UID, so re-execution is
   idempotent.

### Webhook

SCM → `patrol-server /api/webhook` (and `/api/webhook/<provider>`): raw body read with a size
cap → provider adapter verifies signature over raw bytes → event normalized → delivery ID
dedup → matching Applications get `cd.patrol.example/refresh=normal`, matching
ApplicationSets get `cd.patrol.example/application-set-refresh=true`. Polling remains the
recovery path.

## Trust boundaries

1. Internet ↔ patrol-server (TLS, auth, CSRF protection, rate limits, webhook signatures).
2. patrol-server/controller ↔ repo-server (shared-token auth + optional mTLS; repo-server
   never receives kube credentials).
3. repo-server ↔ cmp sidecars (unix socket in a shared emptyDir; sidecar has no secrets).
4. Rendering subprocesses (helm, kustomize, git, jsonnet evaluator) run with sanitized
   environment, per-call timeout, process-group kill, output size caps, resource limits and
   a per-request scratch directory; symlinks escaping the repo root are rejected.
5. Controller ↔ destination clusters (per-cluster credentials, optional impersonation of
   project-defined service accounts).
6. Tenant boundary: AppProject restrictions are enforced in the API server (admission of user
   intent) **and** in the controller (before every sync task), so a bypass of the API cannot
   escalate.

## Package map (Go)

| Package | Responsibility |
|---------|---------------|
| `api/v1alpha1` | CRD types, constants, condition reasons, annotation keys |
| `internal/settings` | Parse/validate settings ConfigMaps/Secrets; watch & reload |
| `internal/rbac` | Policy model (`p`/`g` CSV), glob/regex matching, enforcement |
| `internal/auth` | Local accounts, bcrypt, JWT sessions & API tokens, OIDC/Dex, revocation |
| `internal/project` | Project permission evaluation (sources, destinations, resources, windows) |
| `internal/git`, `internal/helm`, `internal/oci` | Source transports |
| `internal/repository` | Credential resolution, repo access facade |
| `internal/render` | Directory/Helm/Kustomize/Jsonnet/CMP rendering + detection |
| `internal/reposerver` | Repo-server HTTP service + typed client |
| `internal/cmp` | CMP sidecar server and client |
| `internal/sandbox` | Subprocess execution limits |
| `internal/clusters` | Cluster registry, `rest.Config` construction, auth providers |
| `internal/cache` | Per-cluster live object graph |
| `internal/diff` | Normalization, legacy 3-way, server-side diff, ignore rules |
| `internal/health`, `internal/lua` | Built-in health, Lua health/actions, customizations |
| `internal/tracking` | Resource tracking methods |
| `internal/sync` | Sync planning & execution engine |
| `internal/controller` | Application + Operation reconcilers, sharding, hydrator |
| `internal/appset` | ApplicationSet controller, generators, templating, progressive sync |
| `pkg/scm` | Versioned SCM provider contract, registry, conformance suite; providers under `pkg/scm/<provider>` |
| `internal/webhook` | Webhook ingestion → refresh annotations |
| `internal/notifications` | Triggers, templates, services, delivery |
| `internal/apiserver` | REST API, OpenAPI, SSE, terminal, logs, extensions proxy, badge |
| `pkg/apiclient` | Go client for the public API |
| `internal/cli` | `patrol` CLI commands |
| `internal/metrics`, `internal/audit`, `internal/tracing` | Observability |
