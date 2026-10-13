# CRD reference

Group `cd.patrol.example`, version `v1alpha1` (ADR-0002). Go types: `api/v1alpha1`. Generated
manifests: `deploy/crds` (`make generate`; `make verify-generated` fails when they are stale).

Every kind is namespaced, has the `status` subresource, belongs to category `patrol`
(`kubectl get patrol`), and reports `status.conditions` (`[]metav1.Condition`, list-map keyed by
`type`) with `status.observedGeneration`. Condition types and reasons are constants in
`api/v1alpha1/conditions.go`; annotation, label, finalizer and Secret keys are in
`api/v1alpha1/keys.go`.

Install the CRDs with server-side apply (`kubectl apply --server-side -f deploy/crds`): the
ApplicationSet CRD (about 356 KB as compact JSON) exceeds the 256 KiB annotation limit that
client-side apply's `last-applied-configuration` is subject to.

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

Objects never contain credentials. Referenced Secrets live in the referencing object's namespace;
the recognized keys are the `RepositorySecretKey*`, `ClusterSecretKey*` and `SCMProviderSecretKey*`
constants.

### Application

- Spec mirrors Argo CD `ApplicationSpec` (same JSON field names): `source` | `sources` |
  `sourceHydrator`, `destination`, `project`, `syncPolicy`, `ignoreDifferences`, `info`,
  `revisionHistoryLimit`.
- Admission (CEL on `spec`): exactly one of `source`, `sources` (non-empty), `sourceHydrator`;
  exactly one of `destination.server`, `destination.name`. The refresh annotation is not validated.
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
- Spec: `application` (required), `sync`, `rollback` (`id` of a history entry), `initiatedBy`,
  `info`, `retry`, `cancel`.
- Admission: one of `sync` or `rollback`; `rollback` excludes `sync.source(s)`; `sync.source` and
  `sync.sources` are exclusive; each `sync.manifests` document ≤ 1 MiB and the total ≤ 1 MiB.
- The spec is immutable except `cancel`, which may only go from false to true. Immutability is a
  CEL comparison of the stored spec, so change `cancel` (or metadata) with a patch (merge or
  server-side apply); a full update that drops explicitly-set zero values is rejected.
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
  only), `depth`, plus the shared access options: `enableOCI`, `insecureOCIForceHTTP`,
  `forceHTTPBasicAuth`, `proxy`, `noProxy`, `useAzureWorkloadIdentity`, `azureServicePrincipal`,
  `githubApp`, `credentialsRef`, `scmProviderRef`.
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

### SCMProvider

- Spec: `type` (`github`, `gitlab`, `gitea`, `bitbucketCloud`, `bitbucketServer`, `azureDevOps`,
  `awsCodeCommit`, `fake`), `apiURL`, `webURL`, `insecure`, `caSecretRef`, `auth` (at most one of
  `githubApp`, `tokenSecretRef`, `basicAuthSecretRef`), `webhook.secretRef`, `statusReporting`
  (`mode` `statuses` default or `checks`, which requires `type: github` with `auth.githubApp`;
  `contextPrefix` default `patrol-cd`), `rateLimit`.
- Status: `capabilities`, `conditions` (`Ready`, `ConnectionFailed`), `rateLimit`, `lastCheckedAt`.

## Annotation and label mapping

Argo CD keys map mechanically (full list with semantics in `api/v1alpha1/keys.go`):

| Argo CD | Patrol CD |
|---------|-----------|
| `argocd.argoproj.io/<key>` | `cd.patrol.example/<key>` |
| `link.argocd.argoproj.io/<name>`, `notice.…`, `pref.…` | `link.cd.patrol.example/<name>`, `notice.…`, `pref.…` |
| `notifications.argoproj.io/subscribe.<trigger>.<service>` | `notifications.cd.patrol.example/subscribe.<trigger>.<service>` |
| `notified.notifications.argoproj.io` | `notified.notifications.cd.patrol.example` |
| `resources-finalizer.argocd.argoproj.io[/background\|/foreground]` | `resources-finalizer.cd.patrol.example[/…]` |
| `pre-/post-delete-finalizer.argocd.argoproj.io[/cleanup]` | `pre-/post-delete-finalizer.cd.patrol.example[/cleanup]` |

Not mapped: `applications.argoproj.io/app-name` (legacy), legacy argocd-notifications
`recipients` annotations, `gitops-agent.argoproj.io/gc-mark`.
