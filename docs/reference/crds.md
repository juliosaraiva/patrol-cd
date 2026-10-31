# CRD reference

Group `cd.patrol.example`, version `v1alpha1` (ADR-0002). Go types: `api/v1alpha1`. Generated
manifests: `deploy/crds` (`make generate`; `make verify-generated` fails when they are stale).
`deploy/crds/operations-admission-policy.yaml` is hand-written and ships with the CRDs (see
[Operation](#operation)).

Every kind is namespaced, has the `status` subresource, belongs to category `patrol`
(`kubectl get patrol`), and reports `status.conditions` (`[]metav1.Condition`, list-map keyed by
`type`) with `status.observedGeneration`. Condition types and reasons are constants in
`api/v1alpha1/conditions.go`; annotation, label, finalizer and Secret keys are in
`api/v1alpha1/keys.go`.

Install with server-side apply (`kubectl apply --server-side -f deploy/crds`, Kubernetes >= 1.30
for the admission policy): the ApplicationSet CRD (about 356 KB as compact JSON) exceeds the
256 KiB annotation limit that client-side apply's `last-applied-configuration` is subject to.

## Kinds

| Kind | Short names | Replaces in Argo CD | Owned by / owns |
|------|-------------|---------------------|-----------------|
| `Application` | `papp`, `papps` | `Application` (minus `operation`) | Owned by its ApplicationSet when generated. Owns its Operations. |
| `Operation` | `pop`, `pops` | `Application.operation` + `status.operationState.syncResult` | Owned (controller ownerReference) by its Application; garbage-collected with it. |
| `AppProject` | `pproj`, `pprojs` | `AppProject` | None. |
| `ApplicationSet` | `pappset`, `pappsets` | `ApplicationSet` | Owns generated Applications. |
| `Repository` | `prepo` | Secret `secret-type=repository` / `repository-write` | References a Secret (`spec.credentialsRef`) and optionally an SCMProvider. |
| `RepositoryCredential` | `prepocred` | Secret `secret-type=repo-creds` / `repo-write-creds` | Same references as Repository. |
| `Cluster` | `pcluster` | Secret `secret-type=cluster` | References a Secret (`spec.config.credentialsRef`). |
| `SCMProvider` | `pscm` | Inline SCM tokens in generators, notification services and GitHub App repo credentials | References Secrets (auth, webhook, CA). |

The short names do not collide with Argo CD's, but the plural names `applications`,
`applicationsets` and `appprojects` are the same as Argo CD's (`argoproj.io`), and `clusters`,
`repositories` and `operations` are common elsewhere (e.g. `clusters.cluster.x-k8s.io`). When Argo
CD or Cluster API is installed next to Patrol CD (the migration scenario), a bare
`kubectl get applications` resolves to whichever group discovery lists first, most likely
`argoproj.io`. Use the short names, `kubectl get patrol`, or fully qualified names such as
`applications.cd.patrol.example`.

Objects never contain credentials. Referenced Secrets live in the referencing object's namespace;
the recognized keys are the `RepositorySecretKey*`, `ClusterSecretKey*` and `SCMProviderSecretKey*`
constants.

### Application

- Spec mirrors Argo CD `ApplicationSpec` (same JSON field names): `source` | `sources` |
  `sourceHydrator`, `destination`, `project`, `syncPolicy`, `ignoreDifferences`, `info`,
  `revisionHistoryLimit`.
- Admission (CEL on `spec`): exactly one of `source`, `sources` (non-empty), `sourceHydrator`;
  exactly one of `destination.server`, `destination.name`; every `syncPolicy.syncOptions` entry has
  the form `Key=Value` (at most 64 entries of 256 characters). `sourceHydrator.syncSource.path` must
  contain a character other than `.` and `/`, so `.`, `./`, `//` and `..` are rejected. These rules
  are attached to `Application.spec`, not to the spec type, so ApplicationSet templates (which may
  hold template expressions) are not subject to them. The refresh annotation is not validated.
- Status: `sync` (`shortRevision` is display-only, used by the Revision column), `health`,
  `resources`, bounded `history` (each entry names the Operation that produced it), `conditions`,
  `reconciledAt`, `operationState` (summary of the latest Operation; no per-resource results),
  `sourceType(s)`, `summary`, `sourceHydrator`.
- Printer columns: Sync, Health, Revision, Project, Age.
- Finalizers: `resources-finalizer.cd.patrol.example` (cascade, foreground) and its `/foreground`
  and `/background` variants; `pre-delete-finalizer.cd.patrol.example[/cleanup]`;
  `post-delete-finalizer.cd.patrol.example[/cleanup]`. Hook objects carry
  `cd.patrol.example/hook-finalizer`.
- Conditions: Argo CD types `InvalidSpecError`, `ComparisonError`, `SyncError`, `UnknownError`,
  `DeletionError`, `SharedResourceWarning`, `RepeatedResourceWarning`, `ExcludedResourceWarning`,
  `OrphanedResourceWarning` (status `True` while active), plus aggregates `Reconciled`, `Synced`,
  `Healthy`.

### Operation

- One sync or rollback request and its execution record (ADR-0011). Labels
  `cd.patrol.example/application` and `cd.patrol.example/operation-phase` support selection.
  Decisions that must be correct (one non-terminal Operation per Application, retention) use
  `spec.application`, never the labels.
- Spec: `application` (required), `sync`, `rollback` (`id` of a history entry), `initiatedBy`,
  `info`, `retry`, `cancel`.
- Admission (CEL): one of `sync` or `rollback`; `rollback` excludes `sync.source(s)`; `sync.source`
  and `sync.sources` are exclusive; each `sync.manifests` document ≤ 1 MiB and the total ≤ 1 MiB;
  `sync.syncOptions` entries have the form `Key=Value`.
- Admission (ValidatingAdmissionPolicy `operations.cd.patrol.example`, because CRD rules cannot see
  the requester or labels): on create, `initiatedBy.username` must be empty or the requesting
  Kubernetes user and `initiatedBy.automated` must be false, unless the requester has the
  `impersonate` verb on `operations.cd.patrol.example` (`v1alpha1.OperationInitiatorVerb`). Grant
  that verb only to patrol-server, which sets the user from verified token claims, and to the
  application controller, which creates automated syncs; cluster admins have it through `*`. The
  `cd.patrol.example/application` label, when present, must equal `spec.application`.
- The spec is immutable except `cancel`, which may only go from false to true. Immutability is a
  CEL comparison against the stored spec, and Go clients omit empty values on a full update. The
  scalars of `sync` (`revision`, `prune`, `dryRun`, `autoHealAttemptsCount`, `syncStrategy.*.force`)
  are defaulted and `initiatedBy` is compared field by field, so explicit `false`/`""`/`0` in YAML
  does not break a typed `Update`. An explicit empty list or object (e.g. `syncOptions: []`) does:
  the update is rejected with "spec.sync is immutable; change metadata and spec.cancel with a patch,
  not a full update". Controllers and clients change Operations with a merge patch or server-side
  apply.
- Status: `phase` (`Pending`, `Running`, `Terminating`, `Succeeded`, `Failed`, `Error`,
  `Cancelled`, `Rejected`; terminal = the last five), `message`, timestamps, `retryCount`,
  `syncResult` (per-resource results), `progress` (phase, wave, task counts), `plan` (resolved
  revisions persisted before mutation, rendered-input digest, resolved destination, warnings).
- Printer columns: Application, Phase, Initiator, Automated, Revision, Age.
- Conditions: `Admitted`, `Planned`, `Completed`, `Retrying`.
- Retention: the newest `revisionHistoryLimit` (default 10) terminal Operations per Application plus
  any non-terminal one; at most one non-terminal Operation per Application (others are `Rejected`
  with reason `OperationInProgress`).

### AppProject

- Spec mirrors Argo CD `AppProjectSpec`, including `sourceIntegrity` (v3.5) and the deprecated
  `signatureKeys`. Admission: sync window `kind` is `allow`/`deny`, `schedule` is a 5-field cron
  expression or descriptor, GPG mode is `none`/`head`/`strict`, description ≤ 255 characters.
- Status: `jwtTokensByRole`, `conditions`.

### ApplicationSet

- Spec mirrors Argo CD `ApplicationSetSpec`. Matrix/merge generators nested inside matrix/merge are
  stored as raw JSON (`NestedMatrixGenerator` / `NestedMergeGenerator`, decoded with
  `ToNestedMatrixGenerator` / `ToNestedMergeGenerator`) because CRD schemas cannot recurse.
- Admission: at most 128 generators; a matrix has exactly 2 generators and a merge 2 to 64 (Argo CD
  fails these at reconcile time; for nested levels the decoders return the same errors);
  `strategy.deletionOrder` is `AllAtOnce` or `Reverse` (exact case) and `Reverse` requires
  `strategy.type: RollingSync`; rolling sync `matchExpressions[].operator` is `In` or `NotIn`.
- Every SCM provider and pull request generator block accepts `providerRef` (an SCMProvider name)
  as an alternative to inline token references; CEL rejects mixing both.
- The CRD carries no field descriptions (as upstream): every generator embeds the Application spec,
  and descriptions would push the object toward the etcd size limit. `make manifests` generates it
  with `crd:maxDescLen=0`.
- Status: `conditions` (`ErrorOccurred`, `ParametersGenerated`, `ResourcesUpToDate`,
  `RolloutProgressing`, reasons as upstream), `applicationStatus` (progressive sync), `resources`,
  `resourcesCount`, `health`.
- Finalizer: `cd.patrol.example/applicationset-finalizer` (generated Applications are deleted in
  `strategy.deletionOrder`, or released when `syncPolicy.preserveResourcesOnDeletion` is true).
- Annotations: `cd.patrol.example/application-set-refresh` on the ApplicationSet;
  `cd.patrol.example/application-set-progressive-status` on generated Applications.

### Repository and RepositoryCredential

- Repository: `url` (required), `type` (`git` default, `helm`, `oci`), `purpose` (`read` default,
  `write` for git only), `name`, `project`, `insecure`, `insecureIgnoreHostKey`, `enableLFS` (git
  only), `depth`, `webhookManifestCacheWarmDisabled`, plus the shared access options: `enableOCI`,
  `insecureOCIForceHTTP`, `forceHTTPBasicAuth`, `proxy`, `noProxy`, `useAzureWorkloadIdentity`,
  `azureServicePrincipal`, `githubApp`, `credentialsRef`, `scmProviderRef`.
- `githubApp.installationID` is optional: when unset, the installation is discovered from the
  organization in the repository URL, so one RepositoryCredential can serve every organization the
  App is installed in (Argo CD: `githubAppInstallationID` 0).
- RepositoryCredential: `urlPrefix` (required, longest prefix wins), `type`, `purpose` and the same
  access options.
- Status: `connectionState` (`Successful`, `Failed`, `Unknown`), `conditions` (`Ready`,
  `ConnectionFailed`).

### Cluster

- Spec: `server` (http(s) URL, required), `name` (required), `namespaces`, `clusterResources`,
  `project`, `shard`, `config` (`tlsClientConfig`, `awsAuthConfig`, `execProviderConfig`,
  `disableCompression`, `proxyURL`, `credentialsRef`). Labels/annotations are the object's metadata.
- Status: `connectionState`, `serverVersion`, `info` (cache info, applications count, API
  versions), `assignedShard`, `conditions` (`Ready`, `ConnectionFailed`).
- `cd.patrol.example/skip-reconcile` on a Cluster skips every Application targeting it.
- `cd.patrol.example/refresh` on a Cluster, set to an RFC 3339 timestamp, requests invalidation of
  the controller's cache for that cluster (Argo CD's `refreshRequestedAt`, stored upstream as the
  same annotation on the cluster Secret). The cache is rebuilt when
  `status.info.cacheInfo.lastCacheSyncTime` is older than the timestamp.

### SCMProvider

- Spec: `type` (`github`, `gitlab`, `gitea`, `bitbucketCloud`, `bitbucketServer`, `azureDevOps`,
  `awsCodeCommit`, `fake`), `apiURL`, `webURL`, `insecure`, `caSecretRef`, `auth` (at most one of
  `githubApp`, `tokenSecretRef`, `basicAuthSecretRef`), `webhook.secretRef`, `statusReporting`
  (`mode` `statuses` default or `checks`, which requires `type: github` with `auth.githubApp`;
  `contextPrefix` default `patrol-cd`), `rateLimit`. `statusReporting` and `rateLimit` are
  validated but no component reads them yet (#235, #236).
- Status: `capabilities`, `conditions` (`Ready`, `ConnectionFailed`), `rateLimit`, `lastCheckedAt`.
  No component writes the status yet (#236).

## Annotation and label mapping

Argo CD keys map mechanically (full list with semantics in `api/v1alpha1/keys.go`):

| Argo CD | Patrol CD |
|---------|-----------|
| `argocd.argoproj.io/<key>` | `cd.patrol.example/<key>` |
| `link.argocd.argoproj.io/<name>`, `notice.…`, `pref.…` | `link.cd.patrol.example/<name>`, `notice.…`, `pref.…` |
| `notifications.argoproj.io/subscribe.<trigger>.<service>` | `notifications.cd.patrol.example/subscribe.<trigger>.<service>` |
| `notifications.argoproj.io/subscribe.<service>` (default triggers) | `notifications.cd.patrol.example/subscribe.<service>` |
| `notifications.argoproj.io/subscriptions` (YAML list) | `notifications.cd.patrol.example/subscriptions` |
| `notified.notifications.argoproj.io` | `notified.notifications.cd.patrol.example` |
| `resources-finalizer.argocd.argoproj.io[/background\|/foreground]` | `resources-finalizer.cd.patrol.example[/…]` |
| `pre-/post-delete-finalizer.argocd.argoproj.io[/cleanup]` | `pre-/post-delete-finalizer.cd.patrol.example[/cleanup]` |
| `managed-by: argocd.argoproj.io` (exception to the rules) | `cd.patrol.example/managed-by: patrol-cd` |

Values of the tracking method setting are the `TrackingMethod*` constants (`annotation`, `label`,
`annotation+label`).

Not mapped: `applications.argoproj.io/app-name` (legacy), legacy argocd-notifications
`recipients` annotations, `gitops-agent.argoproj.io/gc-mark`.

## Migrating from Argo CD

### Repository and cluster Secrets

Argo CD's repository and cluster Secrets become typed objects. Credentials stay in a Secret named by
`credentialsRef` with the same key names; the other keys become spec fields. Field names follow
Kubernetes casing for acronyms, so four keys are renamed:

| Argo CD Secret key | Patrol CD field |
|--------------------|-----------------|
| `enableLfs` | `Repository.spec.enableLFS` |
| `forceHttpBasicAuth` | `spec.forceHTTPBasicAuth` |
| `insecureOCIForceHttp` | `spec.insecureOCIForceHTTP` |
| `config.proxyUrl` (cluster) | `Cluster.spec.config.proxyURL` |

Restructured keys: `url` → `spec.urlPrefix` on RepositoryCredential; `githubAppID`,
`githubAppInstallationID` (omit when 0), `githubAppEnterpriseBaseUrl` → `spec.githubApp.appID`,
`.installationID`, `.enterpriseBaseURL`; `azureServicePrincipalClientId`,
`azureServicePrincipalTenantId`, `azureActiveDirectoryEndpoint` → `spec.azureServicePrincipal`
`.clientID`, `.tenantID`, `.activeDirectoryEndpoint`; secret types `repository-write` /
`repo-write-creds` → `spec.purpose: write`; cluster `namespaces` (comma-separated) → a list;
cluster `config.bearerToken`, `username`, `password` and `tlsClientConfig.caData`, `certData`,
`keyData` → keys of the `spec.config.credentialsRef` Secret. All other keys keep their names.

### Deprecated fields that are dropped

The structural schemas prune these fields, all marked unused upstream: Application
`status.observedAt`, Application `status.health.message`, and `lastTransitionTime` of per-resource
health (`status.resources[].health`) and of ApplicationSet `status.health`.

### Stricter admission

Argo CD accepts these objects (ignoring or failing them at reconcile time); Patrol CD rejects them,
so migration tooling must normalize them: malformed `syncOptions` entries (not `Key=Value`); a
hydrator `syncSource.path` made only of `.` and `/`; `strategy.deletionOrder` in another case
(`reverse`) or `Reverse` without `RollingSync`; rolling sync operators other than `In`/`NotIn`;
matrix generators without exactly 2 children and merge generators with fewer than 2; more than
128 top-level generators.

### Go types

JSON is identical, but code ported from Argo CD must nil-check fields that are pointers here and
values upstream: `ComparedTo.Source`, `RevisionHistory.Source`, `SyncOperationResult.Source` and
`SyncResultSummary.Source` (`*ApplicationSource`), `HydrateOperation.StartedAt` (`*metav1.Time`),
`HydrateOperation.SourceHydrator` and `SuccessfulHydrateOperation.SourceHydrator`
(`*SourceHydrator`), and `OperationSpec.Retry` (`*RetryStrategy`). They distinguish unset from
empty. Upstream slices of pointers (`Env`, `SyncWindows`, `SourceIntegrityGit.Policies`,
`TagFilters`, operation `Info`) are slices of values.
