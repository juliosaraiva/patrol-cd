# ApplicationSet — parity inventory (Argo CD v3.5.5)

Scope: everything the `argocd-applicationset-controller` does, plus the ApplicationSet surfaces of the API server,
CLI and Web UI, at commit `a5038b0e2bbac1715754380ae0084df8e3d5b48c`. In scope: the `ApplicationSet` CRD and every
spec/status field; all generators (List, Cluster, Git directories/files, SCM Provider for GitHub/GitHub App, GitLab,
Gitea, Bitbucket Server, Bitbucket Cloud, Azure DevOps, AWS CodeCommit, Pull Request for GitHub/GitLab/Gitea/Bitbucket
Server/Bitbucket Cloud/Azure DevOps, Cluster Decision Resource, Plugin, Matrix, Merge) with their filters, `values` and
post-selectors; templating (fasttemplate, Go templates, Sprig allow-list, custom functions, `templatePatch`);
Application create/update/delete policies, preserved fields, `ignoreApplicationDifferences`, ownership, finalizers and
deletion; progressive syncs (RollingSync, `maxUpdate`, `deletionOrder`); status conditions/resources/health;
ApplicationSets in any namespace and the SCM security controls; the ApplicationSet webhook server; the
`ApplicationSetService` gRPC/REST API; `argocd appset` CLI; the alpha ApplicationSet Web UI; controller metrics and
controller process configuration.
Out of scope (covered by other areas): the Application CRD and its sync/health semantics, the application
controller, repo-server internals (manifest generation; only the `GetGitFiles`/`GetGitDirectories` contract used by
the Git generator is described here), the API-server Git webhook for Applications, notifications, RBAC engine
internals (only the `applicationsets` resource semantics are listed), cluster/repository credential storage formats.

Permalink base used below: `https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/`.

---

### ASET-001 ApplicationSet custom resource
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L48-L82, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/crds/applicationset-crd.yaml#L1-L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset.yaml#L1-L339, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/index.md#L1-L111
- **Maturity**: GA
- **Semantics**: Namespaced CRD `applicationsets.argoproj.io`, group `argoproj.io`, version `v1alpha1` (served+storage), kind `ApplicationSet`, list kind `ApplicationSetList`, plural `applicationsets`, short names `appset`,`appsets`, status subresource enabled. Top-level `metadata` and `spec` are required. `spec` fields: `goTemplate` (bool), `generators` ([]ApplicationSetGenerator), `template` (ApplicationSetTemplate), `syncPolicy`, `strategy`, `preservedFields`, `goTemplateOptions` ([]string), `applyNestedSelectors` (deprecated, ignored), `ignoreApplicationDifferences`, `templatePatch` (string). Status fields: `conditions`, `applicationStatus`, `resources`, `resourcesCount`, `health`. `QualifiedName()` is `<namespace>/<name>` (or `<name>` when namespace empty). Embedded generator `template` fields and nested Matrix/Merge generators are stored as `x-kubernetes-preserve-unknown-fields` JSON in the schema.
- **Edge cases**: Unknown generator keys are pruned by the schema; an entry with no recognised generator produces nothing in the controller (warning only, ASET-007) but is rejected by the API (ASET-112). The schema accepts several generator types inside one `generators[]` element (see ASET-006).
- **Configuration surface**: CRD `applicationsets.argoproj.io`; all `spec.*`/`status.*` fields listed above.
- **Exposure**: CRD; API (`ApplicationSetService`, ASET-110..ASET-117); CLI (`argocd appset`); UI (`/applicationsets`).
- **Tests required**:
  - Apply the upstream CRD-equivalent schema and round-trip every documented field without loss (including preserve-unknown nested generators and `templatePatch`).
  - `kubectl get appset`/`appsets` short names resolve.
  - Status can only be written through the status subresource.
  - Objects missing `spec` are rejected by the API server.

### ASET-002 Application template and rendered Application identity
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L180-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/template.go#L16-L101, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Template.md#L1-L50
- **Maturity**: GA
- **Semantics**: `spec.template` has `metadata` (`name`, `namespace`, `labels`, `annotations`, `finalizers`) and `spec` (a full Application `spec`). For each generator, for each parameter set, a copy of the (merged, ASET-003) template is rendered (ASET-070/ASET-071) into an Application. After rendering (and `templatePatch`, ASET-077) the Application `metadata.namespace` is unconditionally overwritten with the ApplicationSet's namespace (security boundary for appsets-in-any-namespace); `template.metadata.namespace` is therefore ignored. Applications from all generators are concatenated in generator order. Generation does not short-circuit: every generator/param set is attempted, but if any error occurred the first error (and its reason) is returned and the whole reconcile is treated as failed (ASET-096).
- **Edge cases**: Unresolved fasttemplate tags remain literally (`{{x}}`) and typically produce an invalid name that fails at Kubernetes create. An empty parameter map (e.g. a List element `{}`) returns the template unrendered and without the default finalizer (ASET-004). Name collisions across generators are validation errors (ASET-008).
- **Configuration surface**: `spec.template.metadata.*`, `spec.template.spec.*`.
- **Exposure**: CRD; API (`Generate`, `Create` dry-run) and UI Preview evaluate it.
- **Tests required**:
  - Rendered Application is created in the ApplicationSet namespace even when `template.metadata.namespace` names another namespace.
  - Labels/annotations/finalizers from the template are set on the Application.
  - Two generators produce apps in generator order; output identical to upstream for a fixed fixture.
  - Failure in one generator prevents any create/update/delete in that reconcile.

### ASET-003 Generator-level template override
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L43-L54, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L165-L173, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Template.md#L80-L128
- **Maturity**: GA
- **Semantics**: Every generator type (List, Clusters, Git, SCMProvider, ClusterDecisionResource, PullRequest, Plugin, Matrix, Merge) has an optional `template`. The effective template is `mergo.Merge(dst = copy(generator.template), src = spec.template)`: non-zero fields in the generator template win; zero/empty fields are filled from `spec.template` (maps are unioned with generator keys winning; slices are taken from `spec.template` only if empty in the generator template). The merge happens before parameters are generated (fail fast).
- **Edge cases**: Template overrides on children of Matrix/Merge are ignored (children are evaluated with an empty base template and only the parent's template is used). Setting a field to its zero value in the generator template cannot "unset" the spec-level value.
- **Configuration surface**: `spec.generators[].<type>.template`.
- **Exposure**: CRD.
- **Tests required**:
  - Generator `template.spec.source.path` overrides spec-level path while other fields come from `spec.template`.
  - Generator template labels merged with spec labels (generator wins on conflict).
  - Child template inside a Matrix is not applied.

### ASET-004 Default resources finalizer on generated Applications
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L276-L305, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Application-Deletion.md#L3-L29
- **Maturity**: GA
- **Semantics**: After rendering, if the rendered template has no finalizers and (`spec.syncPolicy` is nil or `spec.syncPolicy.preserveResourcesOnDeletion` is false), `resources-finalizer.argocd.argoproj.io` is added. If the template declares any finalizers (custom list), the resources finalizer is not added automatically. With `preserveResourcesOnDeletion: true` no finalizer is added.
- **Edge cases**: Rendering with an empty parameter map skips this step (no finalizer). Because the live finalizer list is replaced on update (ASET-084), toggling `preserveResourcesOnDeletion` to true removes the resources finalizer from existing Applications (pre/post-delete finalizers are preserved, ASET-082).
- **Configuration surface**: `spec.syncPolicy.preserveResourcesOnDeletion`, `spec.template.metadata.finalizers`.
- **Exposure**: CRD.
- **Tests required**:
  - Default: generated app carries `resources-finalizer.argocd.argoproj.io`.
  - `preserveResourcesOnDeletion: true`: no finalizer; deleting the app leaves cluster resources.
  - Custom finalizer list in template is used verbatim (no implicit resources finalizer).

### ASET-005 Application ownership and conflicting ApplicationSets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L734, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L597-L611, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L772-L805, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Application-Deletion.md#L3-L16
- **Maturity**: GA
- **Semantics**: Every created/updated Application gets a controller owner reference to the ApplicationSet (`controller: true`, `blockOwnerDeletion: true`, apiVersion `argoproj.io/v1alpha1`, kind `ApplicationSet`). "Current" Applications of an ApplicationSet are found via a field index `.metadata.controller` = owner name, restricted to the ApplicationSet namespace. Create/update targets an Application by name+namespace: if an Application with that name exists and has no controller owner it is adopted (owner ref set, spec/labels/annotations overwritten); if it is controlled by a different ApplicationSet the owner-reference step fails and the reconcile reports an update/create error.
- **Edge cases**: Under `create-only`/`create-delete` the "only create new" filter uses the owned set, so an unowned pre-existing Application with a generated name is still adopted and overwritten. Two ApplicationSets generating the same name: the second never takes ownership and keeps reporting `ErrorOccurred` with reason `UpdateApplicationError`/`CreateApplicationError`.
- **Configuration surface**: none (behavioral).
- **Exposure**: CRD (Application `metadata.ownerReferences`); UI shows owner badge (ASET-127).
- **Tests required**:
  - Generated app has exactly one controller ownerReference to the ApplicationSet.
  - Second ApplicationSet producing the same app name gets `ErrorOccurred=True` and does not modify the app.
  - Pre-existing unowned app with matching name is adopted.
  - Owned apps are only discovered in the ApplicationSet's own namespace.

### ASET-006 Multiple generators and per-entry generator selection
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/template.go#L22-L77, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L104-L124, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/utils.go#L14-L50
- **Maturity**: GA
- **Semantics**: `spec.generators` is a list; each entry is processed independently and outputs are concatenated. Within one top-level entry every non-nil generator field (excluding `selector`) is a "relevant generator"; each produces its own result with its own merged template (so one entry with both `list` and `clusters` yields both outputs — no error at top level). The registry maps: top-level `List, Clusters, Git, SCMProvider, ClusterDecisionResource, PullRequest, Plugin, Matrix, Merge`; Matrix/Merge children may use the same set where nested Matrix/Merge children only accept terminal generators.
- **Edge cases**: An entry with only `selector` set generates nothing. Inside Matrix/Merge, more than one generator in a single child entry is an error (`found more than one generator in matrix.Generators`), and an empty child entry is `child generator generated no parameters`.
- **Configuration surface**: `spec.generators[]`.
- **Exposure**: CRD.
- **Tests required**:
  - Two top-level generators: union of Applications.
  - One top-level entry with `list` and `clusters`: both outputs generated.
  - Child entry with two generators inside Matrix: error condition with upstream message.

### ASET-007 Unrecognized generators detection
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L444-L529, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L196-L197, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L532-L548
- **Maturity**: GA
- **Semantics**: A generator entry with all generator fields nil is "invalid". Names are recovered from the `kubectl.kubernetes.io/last-applied-configuration` annotation (first key of the entry at that index). Message: `ApplicationSet <name> contains unrecognized generators: <sorted, comma-separated>` or `ApplicationSet <name> contains unrecognized generators` when names cannot be recovered. The controller only logs a warning and continues; the API `Create`/`Generate` reject the request with this error.
- **Edge cases**: Malformed last-applied annotation → generic message. `selector` counts as a non-nil field, so an entry with only a selector is not flagged.
- **Configuration surface**: annotation `kubectl.kubernetes.io/last-applied-configuration` (read-only use).
- **Exposure**: CRD; API (`Create`, `Generate`).
- **Tests required**:
  - Controller: misspelled generator key → no error condition, other generators still work.
  - API: same spec rejected with the exact message including the unknown name.

### ASET-008 Generated Application validation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L217-L238, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L275-L307, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L541-L570
- **Maturity**: GA
- **Semantics**: Each generated Application is validated: (1) duplicate name within the ApplicationSet → `ApplicationSet <appset> contains applications with duplicate name: <name>` (first occurrence is valid, later ones invalid); (2) `spec.project` must exist as an AppProject in the Argo CD control-plane namespace → `application references project <p> which does not exist`; (3) destination must resolve to a known cluster → `application destination spec is invalid: <err>`. Non-NotFound API errors abort the reconcile (requeue after 3 min). Invalid apps are excluded from create/update but valid ones proceed. The error condition message is the last error (sorted by qualified app name) with ` (and N more)` appended when several apps failed; reason `ApplicationValidationError`. Deletion still uses the full generated list, so existing apps whose regenerated form is invalid are not deleted.
- **Edge cases**: When validation errors exist and no generator requests a requeue, the reconcile requeues after 3 minutes. `ResourcesUpToDate` is not set True while validation errors exist.
- **Configuration surface**: none.
- **Exposure**: CRD (status conditions).
- **Tests required**:
  - App referencing a missing project is skipped; others created; condition message matches upstream text.
  - Duplicate names: only first applied; condition lists duplicate.
  - Three invalid apps: message ends with `(and 2 more)`.
  - Existing app whose new rendering is invalid is not deleted.

### ASET-009 Reconcile requeue scheduling
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L396-L420, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L572-L589, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/interface.go#L34-L41, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L373-L374
- **Maturity**: GA
- **Semantics**: After a successful reconcile the ApplicationSet is requeued after the minimum non-zero `GetRequeueAfter` across all top-level generators (0 = no time-based requeue). Defaults: List 0, Clusters 0 (event-driven, ASET-010), Git and ClusterDecisionResource = `ARGOCD_APPLICATIONSET_CONTROLLER_REQUEUE_AFTER` (default 3m, valid range 1s..8760h, Go duration or day syntax; out-of-range/invalid → 3m), SCMProvider/PullRequest/Plugin = 30m (hard-coded default), Matrix/Merge = minimum non-zero of their children. Per-generator `requeueAfterSeconds` overrides the default; `0` means "no requeue" for that generator. Generation failure and validation errors requeue after 3 minutes (`ReconcileRequeueOnValidationError`) with a nil error; create/update/delete/status errors return an error (controller-runtime exponential backoff).
- **Edge cases**: `requeueAfterSeconds: 0` on all generators of a Git ApplicationSet disables polling (webhooks still work). Matrix child with `requeueAfterSeconds: 0` is ignored in the min computation.
- **Configuration surface**: `spec.generators[].<git|scmProvider|pullRequest|clusterDecisionResource|plugin>.requeueAfterSeconds`; env `ARGOCD_APPLICATIONSET_CONTROLLER_REQUEUE_AFTER`; cmd-params `applicationsetcontroller.requeue.after`.
- **Exposure**: CRD; controller config.
- **Tests required**:
  - Git appset without override requeues ~3m; with env `1m` requeues ~1m; with env `0s`/invalid falls back to 3m.
  - SCM/PR/Plugin default 30m unaffected by the env var.
  - Mixed generators requeue at the minimum.
  - Generator error → condition set and retry after ~3m.

### ASET-010 Watch triggers and event filtering
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L613-L634, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L1159-L1337, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/clustereventhandler.go#L53-L171
- **Maturity**: GA
- **Semantics**: The controller reconciles on: ApplicationSet create/delete/generic (always); ApplicationSet update only when spec, labels, finalizers or deletionTimestamp change, or annotations change (except when the only change is removal of the refresh annotation), or — with progressive syncs enabled — when `status.applicationStatus` changes. Owned Application events: create ignored; delete and generic always; update when spec, labels, annotations or finalizers differ (nil vs empty treated equal), and with progressive syncs enabled also when health status, sync status, or operation phase/startedAt change. Any create/update/delete of a Secret labelled `argocd.argoproj.io/secret-type=cluster` enqueues every ApplicationSet in an allowed namespace that has a Clusters generator at top level or nested in Matrix/Merge (one or two levels). All events are filtered to allowed namespaces (ASET-099). Max concurrent reconciles is `--concurrent-reconciliations` (default 10).
- **Edge cases**: ClusterDecisionResource generators are not triggered by cluster secret changes (time-based only). Status-only updates of the ApplicationSet do not retrigger (prevents loops). Malformed nested matrix JSON is logged and ignored for the cluster-secret check.
- **Configuration surface**: `--concurrent-reconciliations` / `ARGOCD_APPLICATIONSET_CONTROLLER_CONCURRENT_RECONCILIATIONS` / `applicationsetcontroller.concurrent.reconciliations.max` (default 10, min 1).
- **Exposure**: controller behavior.
- **Tests required**:
  - Adding/removing a cluster secret re-renders a Clusters ApplicationSet without waiting for a timer.
  - Manually editing a generated app's spec triggers reconcile and reverts it (under `sync` policy).
  - Changing only the app's status (reconciledAt) does not trigger reconcile when progressive syncs are disabled.
  - Cluster secret change does not enqueue ApplicationSets outside allowed namespaces.

### ASET-011 Refresh annotation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/common/common.go#L444-L445, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L945-L949, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L70, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L378-L394, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L454-L460
- **Maturity**: GA
- **Semantics**: Presence (any value) of annotation `argocd.argoproj.io/application-set-refresh` triggers a reconcile, and makes the Git generator call the repo-server with `noRevisionCache=true` (resolve refs directly, bypass revision cache). After a successful create/update/delete and resources-status update, the controller removes the annotation with a full `Update`; failure sets `ErrorOccurred` with reason `RefreshApplicationError`. The webhook sets this annotation to `"true"` (ASET-104).
- **Edge cases**: If generation fails the annotation is not removed (it stays until a successful pass). Removal of the annotation itself does not trigger a new reconcile.
- **Configuration surface**: annotation `argocd.argoproj.io/application-set-refresh`.
- **Exposure**: CRD (annotation).
- **Tests required**:
  - Annotating triggers reconcile and is removed afterwards.
  - Git generator receives `noRevisionCache=true` only while annotated.
  - Update conflict when removing → `RefreshApplicationError` condition.

### ASET-012 List generator: `elements`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/list.go#L32-L78, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L276-L282, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/list_test.go#L14-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-List.md#L1-L58
- **Maturity**: GA
- **Semantics**: Each element of `list.elements` (arbitrary JSON object) becomes one parameter set, in order. With `goTemplate: true` the element is passed as-is (any JSON types, nested objects usable with `.a.b`). With fasttemplate every top-level value must be a string, except key `values`, which must be an object of string values and is flattened to `values.<k>` keys. No time-based requeue.
- **Edge cases**: fasttemplate + non-string value → error starting `error parsing value as string`; `values` not an object → `error parsing values map`; invalid JSON element → `error unmarshling list element ...`. An empty element `{}` yields an empty param set (template rendered unchanged, no default finalizer). `elements` may be omitted (optional in the schema).
- **Configuration surface**: `spec.generators[].list.elements[]`, `.list.template`.
- **Exposure**: CRD.
- **Tests required**:
  - fasttemplate: string elements rendered; `values: {a: b}` usable as `{{values.a}}`.
  - fasttemplate: numeric element value → `ErrorOccurred` with `ApplicationGenerationFromParamsError`.
  - goTemplate: nested object and number rendered via `{{.x.y}}`.
  - Element order preserved in the generated list.

### ASET-013 List generator: `elementsYaml`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/list.go#L79-L89, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/matrix_test.go#L866-L1026, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-List.md#L60-L118
- **Maturity**: GA
- **Semantics**: `list.elementsYaml` is a string containing a YAML (or JSON) array of objects; each object is appended after the `elements` parameter sets, unmodified (no string coercion even in fasttemplate mode). Intended to be templated from a previous Matrix child, e.g. `elementsYaml: "{{ .key.components | toJson }}"`.
- **Edge cases**: Unparseable YAML or a non-array → `error unmarshling decoded ElementsYaml ...`. In fasttemplate mode only string-valued keys of these objects are substitutable.
- **Configuration surface**: `spec.generators[].list.elementsYaml`.
- **Exposure**: CRD.
- **Tests required**:
  - Matrix(git files, list elementsYaml) produces one app per array item.
  - `elements` + `elementsYaml` together: elements first, then YAML items.
  - Invalid YAML surfaces the upstream error text.

### ASET-014 Cluster generator: parameters and local cluster
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L48-L188, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/clusterUtils.go#L19-L27, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster_test.go#L32-L346, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster.md#L1-L62
- **Maturity**: GA
- **Semantics**: Lists Secrets in the Argo CD control-plane namespace labelled `argocd.argoproj.io/secret-type=cluster` (plus the user selector, ASET-015) from the controller cache, keyed by `data.name` (duplicate names collapse; last wins). Per cluster: `name`, `nameNormalized` (SanitizeName: lowercase, `[^-a-z0-9.]`→`-`, max 253, trim `-`/`.`), `server`, `project` (`data.project` or ""). fasttemplate: `metadata.labels.<k>` / `metadata.annotations.<k>` flat keys; goTemplate: `metadata` map with `labels`/`annotations` maps (each key present only if non-empty). If the selector is empty and no secret has `server == https://kubernetes.default.svc`, a synthetic local cluster is appended last: `name=in-cluster`, `nameNormalized=in-cluster`, `server=https://kubernetes.default.svc`, `project=""` (no metadata). No time-based requeue; refreshed by cluster-secret events (ASET-010).
- **Edge cases**: Order of secret-backed clusters is map order (unordered). The synthetic in-cluster has no `metadata` key, so `{{.metadata.labels...}}` with `missingkey=error` fails for it. Secret lookup errors → `error getting cluster secrets: ...`.
- **Configuration surface**: `spec.generators[].clusters` (`selector`, `values`, `flatList`, `template`); cluster Secret data `name`, `server`, `project`, labels, annotations.
- **Exposure**: CRD.
- **Tests required**:
  - `clusters: {}` with no cluster secrets → single in-cluster app.
  - With a secret for `https://kubernetes.default.svc` → no synthetic duplicate.
  - fasttemplate and goTemplate parameter shapes match upstream fixtures (labels/annotations).
  - `nameNormalized` of `My_Cluster` is `my-cluster`.
  - Deleting a cluster secret deletes its Application (policy `sync`).

### ASET-015 Cluster generator: label selector
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L58-L60, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L165-L176, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster.md#L64-L163
- **Maturity**: GA
- **Semantics**: `clusters.selector` is a standard Kubernetes `LabelSelector` (`matchLabels` + `matchExpressions` with `In/NotIn/Exists/DoesNotExist`) ANDed with `argocd.argoproj.io/secret-type=cluster`, evaluated server-side on Secret labels (standard label-value validation applies). Any non-empty selector (any matchLabels or matchExpressions entry) excludes the synthetic local cluster. Clusters can be selected by the auto label `argocd.argoproj.io/kubernetes-version` when the secret has `argocd.argoproj.io/auto-label-cluster-info: "true"` (label maintained by the application controller).
- **Edge cases**: A `NotIn` expression still excludes the synthetic local cluster. Invalid selector → `error converting label selector`.
- **Configuration surface**: `spec.generators[].clusters.selector`.
- **Exposure**: CRD.
- **Tests required**:
  - `matchLabels` selects only labelled clusters; local cluster excluded.
  - `matchExpressions` In/NotIn/Exists/DoesNotExist behave like Kubernetes.
  - Selecting on `argocd.argoproj.io/secret-type: cluster` excludes the in-cluster default.

### ASET-016 Cluster generator: `values`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L78-L101, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/value_interpolation.go#L8-L41, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster.md#L165-L255
- **Maturity**: GA
- **Semantics**: `clusters.values` (map[string]string) is templated per cluster against that cluster's params (see ASET-069 for shared semantics) and exposed as `values.<k>` (fasttemplate) or `.values.<k>` (goTemplate). Also applied to the synthetic local cluster.
- **Edge cases**: Values cannot reference other values. Template error → `error appending templated values for cluster: ...`.
- **Configuration surface**: `spec.generators[].clusters.values`.
- **Exposure**: CRD.
- **Tests required**:
  - Two Clusters generators with different selectors and `values.revision` yield different `targetRevision`.
  - `values` referencing `{{index .metadata.annotations "x"}}` resolves.

### ASET-017 Cluster generator: `flatList`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L68-L69, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/cluster.go#L110-L126, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster.md#L256-L315
- **Maturity**: GA
- **Semantics**: With `flatList: true` the generator returns exactly one parameter set `{"clusters": [<per-cluster params>...]}` (per-cluster params include their own `values`). Intended for goTemplate `range .clusters`. Multiple flat-list Clusters generators each produce one Application.
- **Edge cases**: A single param set is produced even when zero clusters match (`clusters` empty/nil) — one Application is still rendered. Not usable meaningfully with fasttemplate (slice value).
- **Configuration surface**: `spec.generators[].clusters.flatList`.
- **Exposure**: CRD.
- **Tests required**:
  - Two matching clusters → one app whose Helm values list both names.
  - Zero matching clusters → still one app.

### ASET-018 Git directory generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L61-L140, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L325-L368, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3308-L3394, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L13-L86
- **Maturity**: GA
- **Semantics**: Requires `repoURL` and `revision` (CRD-required). Calls repo-server `GetGitDirectories` (all directories of the checked-out revision, relative paths, repo root excluded, directories whose name starts with `.` skipped unless repo-server `--include-hidden-directories`). Each directory matching (Go `path.Match`, `*` does not cross `/`) at least one non-exclude `directories[].path` and no exclude pattern yields: fasttemplate `path`, `path.basename`, `path.basenameNormalized`, `path[<i>]` for each non-empty segment; goTemplate `.path.path`, `.path.basename`, `.path.basenameNormalized`, `.path.segments` ([]string). Path `'*'` selects top-level directories. If both `directories` and `files` are set, only `directories` is used; if neither → `ApplicationSet is empty`. Results are cached by repo-server per revision.
- **Edge cases**: Invalid glob in a pattern is logged and that pattern ignored. Repo-server errors → `error generating params from git: error getting directories from repo: ...`.
- **Configuration surface**: `spec.generators[].git.{repoURL,revision,directories[].path,directories[].exclude}`; repo-server `ARGOCD_REPO_SERVER_INCLUDE_HIDDEN_DIRECTORIES` / `reposerver.include.hidden.directories`.
- **Exposure**: CRD.
- **Tests required**:
  - `apps/*` produces one app per immediate subdirectory with correct basename/segments.
  - Hidden directories ignored by default.
  - `directories` + `files` both set → directories only.
  - Revision change (new directory pushed) is picked up on next poll/webhook.

### ASET-019 Git directory generator: `exclude`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L295-L323, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L87-L206
- **Maturity**: GA
- **Semantics**: `directories[].exclude: true` turns the pattern into an exclusion. A directory is included iff it matches at least one include and zero exclude patterns; exclusion always wins, independent of list order.
- **Edge cases**: An include `/d/e` plus exclude `/d/*` excludes `/d/e`. Only exclude patterns → nothing generated.
- **Configuration surface**: `spec.generators[].git.directories[].exclude`.
- **Exposure**: CRD.
- **Tests required**:
  - Include `d/*`, exclude `d/[fg]` → only `d/e`.
  - Reordering include/exclude entries does not change output.

### ASET-020 Git files generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L142-L293, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/reposerver/repository/repository.go#L3230-L3306, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L249-L353
- **Maturity**: GA
- **Semantics**: For each non-exclude `files[].path` pattern, repo-server `GetGitFiles` returns matching file contents; results are unioned, then files matched by any exclude pattern are removed, then processed in sorted path order. Each file is parsed as a single YAML/JSON object (an empty file parses as one empty object) or, failing that, as an array of objects (one param set per element). fasttemplate: object flattened with dot keys (`a.b`, arrays as `a.0`), values stringified with `%v`, plus `path` (directory of the file), `path.basename`, `path.filename`, `path.basenameNormalized`, `path.filenameNormalized`, `path[<i>]`. goTemplate: object kept as-is plus `.path` = `{path, basename, filename, basenameNormalized, filenameNormalized, segments}` (overwrites a file key named `path`).
- **Edge cases**: Neither object nor array → `unable to process file '<file>' from repository '<repo>': unable to parse file: ...`. Repo-server pattern errors are returned verbatim. Generator-provided `path` keys overwrite same-named keys from the file.
- **Configuration surface**: `spec.generators[].git.files[].path`, `.files[].exclude`.
- **Exposure**: CRD.
- **Tests required**:
  - JSON object file → flattened keys (fasttemplate) / nested (goTemplate).
  - YAML array file → N apps.
  - Empty file → one app with only path params.
  - Malformed file → error message includes file path and repo.

### ASET-021 Git files generator: `exclude`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L151-L197, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git_test.go#L1080-L1758, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L355-L392
- **Maturity**: GA
- **Semantics**: `files[].exclude: true` patterns are evaluated with the same repo-server globbing as includes (one `GetGitFiles` call per pattern) and their matches are removed from the included set.
- **Edge cases**: Exclude pattern matching nothing is a no-op. Behavior differs between old and new globbing (ASET-022).
- **Configuration surface**: `spec.generators[].git.files[].exclude`.
- **Exposure**: CRD.
- **Tests required**:
  - Include `cluster-config/**/config.json`, exclude `cluster-config/*/dev/config.json` → dev excluded (both globbing modes as in upstream tests).

### ASET-022 Git file globbing modes
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/client.go#L703-L748, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/repo_service.go#L53-L75, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L312, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git-File-Globbing.md#L1-L85
- **Maturity**: GA
- **Semantics**: Default (old) globbing runs `git ls-files --full-name -z -- <pattern>` (git pathspec: `*` matches across `/`, so `a/*/*/values.yaml` behaves like `a/**/values.yaml`). With new globbing enabled, the repo-server uses doublestar `FilepathGlob` on the checked-out tree (`*` = one segment, `**` = any depth, `/path/**/` matches only directories); matches whose symlink-resolved path is outside the repo root are skipped with a warning. The flag is sent per request (`NewGitFileGlobbingEnabled`).
- **Edge cases**: The API server's `Generate` uses its own flag (`--appset-enable-new-git-file-globbing`), so preview and controller can disagree if configured differently.
- **Configuration surface**: `--enable-new-git-file-globbing` / `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_NEW_GIT_FILE_GLOBBING` / `applicationsetcontroller.enable.new.git.file.globbing` (default false); argocd-server `--appset-enable-new-git-file-globbing`.
- **Exposure**: CRD (behavior), controller config.
- **Tests required**:
  - Repo with nested subchart `values.yaml`: old globbing returns both, new returns only the intended depth.
  - Symlink escaping repo root ignored under new globbing.

### ASET-023 Git generator: `pathParamPrefix`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L255-L281, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L339-L356, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L257-L353
- **Maturity**: GA
- **Semantics**: When set, path params are namespaced: fasttemplate keys become `<prefix>.path`, `<prefix>.path.basename`, ... `<prefix>.path[<i>]`; goTemplate stores `{"path": {...}}` under key `<prefix>` (i.e. `.<prefix>.path.basename`). Needed when two Git generators are combined in a Matrix in fasttemplate mode.
- **Edge cases**: goTemplate: a file key equal to the prefix is overwritten.
- **Configuration surface**: `spec.generators[].git.pathParamPrefix`.
- **Exposure**: CRD.
- **Tests required**:
  - Matrix of two Git file generators with prefixes `app` and `target` produces the upstream doc's 4 combinations.
  - Without prefix in fasttemplate mode the same matrix fails with a duplicate-key error.

### ASET-024 Git generator: `values`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L284-L287, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L359-L362, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L208-L247, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L394-L433
- **Maturity**: GA
- **Semantics**: `git.values` is templated against each param set (directory or file params, including file content keys) and exposed under `values` (ASET-069).
- **Edge cases**: Template failure → `failed to append templated values: ...`.
- **Configuration surface**: `spec.generators[].git.values`.
- **Exposure**: CRD.
- **Tests required**:
  - Directory generator `values.cluster: '{{.path.basename}}'` used as destination namespace.
  - Files generator `values.base_dir` built from `index .path.segments`.

### ASET-025 Git generator: polling, revision resolution and caching
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L48-L57, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/repo_service.go#L53-L97, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L435-L460
- **Maturity**: GA
- **Semantics**: Git generators requeue every `requeueAfterSeconds` or the global default (ASET-009). `revision` is resolved by the repo-server (branch, tag, commit, `HEAD`); results are cached per (repo, resolved revision[, pattern]) and the revision resolution itself honours the repo-server revision cache unless the refresh annotation is set (ASET-011). Git submodule checkout follows `ARGOCD_GIT_MODULES_ENABLED`.
- **Edge cases**: A revision cache expiry (`timeout.reconciliation`) longer than the poll interval delays detection of new commits. Webhooks bypass polling delay (ASET-105).
- **Configuration surface**: `spec.generators[].git.requeueAfterSeconds`; `ARGOCD_GIT_MODULES_ENABLED` / `applicationsetcontroller.enable.git.submodule` (default true).
- **Exposure**: CRD.
- **Tests required**:
  - Push new directory: appears after ≤ requeue interval without webhook.
  - Branch revision resolves to latest commit; pinned SHA is stable.

### ASET-026 Git generator: repository credentials, project scoping and source integrity
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L72-L97, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L370-L377, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/repo_service.go#L53-L97, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L5-L11, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L544-L549
- **Maturity**: GA
- **Semantics**: If `spec.template.spec.project` contains no `{{`, the AppProject with that name is fetched from the controller namespace (fallback: the ApplicationSet namespace if the controller namespace is empty); missing project → `error getting project <p>: ...` (generation error). Its effective source-integrity / signature policy is sent to the repo-server (`SourceIntegrity`, `VerifyCommit`), so unsigned or untrusted commits fail generation. The project name is passed to repository credential lookup, so project-scoped repositories/credentials apply. If the project is templated, signature verification is skipped (info log) and an empty project is used, so only non-project-scoped (global) repository credentials work.
- **Edge cases**: Templated project + project-scoped private repo → repo appears anonymous and generation fails. Note only `spec.template.spec.project` is inspected (generator-level template project overrides are not).
- **Configuration surface**: `spec.template.spec.project`; AppProject source-integrity/signature keys; repository Secrets (`project` field).
- **Exposure**: CRD.
- **Tests required**:
  - Private repo with matching project-scoped creds succeeds; mismatching project fails.
  - Templated project with global creds succeeds; with only scoped creds fails.
  - Project requiring GPG signatures + unsigned commit → generation error condition.
  - Missing AppProject (non-templated) → `ApplicationGenerationFromParamsError`.

### ASET-027 SCM Provider generator: common behavior and parameters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L93-L295, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L435-L473, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/types.go#L8-L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L1-L27, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L441-L525
- **Maturity**: GA
- **Semantics**: Exactly one provider is used, chosen in this precedence when several are set: `github`, `gitlab`, `gitea`, `bitbucketServer`, `azureDevOps`, `bitbucket`, `awsCodeCommit`; none → `no SCM provider implementation configured`. Order of checks: SCM providers enabled (ASET-102) → allowed API URL (ASET-101) → build provider (secret refs read from the ApplicationSet namespace) → list repos (cloneProtocol) → repo-level filters → expand branches → branch-level filters (ASET-041). Per repository/branch the params are: `organization`, `repository`, `repository_id`, `url`, `branch`, `sha`, `short_sha` (first 8), `short_sha_7` (first 7), `labels` (comma-joined), `branchNormalized` (SanitizeName), plus `values` (ASET-069). Default requeue 30 minutes (`requeueAfterSeconds` overrides).
- **Edge cases**: Provider initialisation errors are wrapped (`error fetching <Provider> token: ...`, `error initializing <Provider> service: ...`); listing errors → `error listing repos: ...`. If `tokenRef` is omitted, GitHub/GitLab/Gitea fall back to the controller env vars `GITHUB_TOKEN`/`GITLAB_TOKEN`/`GITEA_TOKEN` (undocumented test hook) before anonymous access.
- **Configuration surface**: `spec.generators[].scmProvider.{github,gitlab,gitea,bitbucketServer,azureDevOps,bitbucket,awsCodeCommit,filters,cloneProtocol,requeueAfterSeconds,template,values}`.
- **Exposure**: CRD.
- **Tests required**:
  - Fake SCM returning 2 repos → 2 apps with all documented params.
  - `short_sha`/`short_sha_7` truncation when SHA shorter than 8/7.
  - Default requeue 30m; override respected.
  - Missing referenced Secret → `ErrorOccurred` with upstream-style message.

### ASET-028 SCM Provider: GitHub (token)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/github.go#L23-L163, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L297-L320, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L491-L505, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L46-L88
- **Maturity**: GA
- **Semantics**: Fields: `organization` (required), `api` (GitHub Enterprise base URL used for both API and uploads; empty = api.github.com), `tokenRef` {secretName,key}, `appSecretName` (ASET-029), `allBranches`, `excludeArchivedRepos`. Lists `GET /orgs/{org}/repos` (100/page, all pages). `organization` param = repo owner login; `labels` = repository topics; `repository_id` = numeric repo ID. Default branch fetched with `GetBranch`; a 404 on the default branch means an empty repo (no entries). `pathsExist` uses the Contents API at the branch ref (404 → false). Requests go through the SCM HTTP client (proxy, ASET-045) and optional GitHub API metrics (ASET-129).
- **Edge cases**: `excludeArchivedRepos` skips archived repos after clone-URL selection. Unknown clone protocol → `unknown clone protocol for GitHub <p>`.
- **Configuration surface**: `scmProvider.github.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Pagination across >100 repos.
  - Archived repo excluded only when flag set.
  - Topics usable with `labelMatch`.
  - Empty repository (no default branch) produces no params and no error.

### ASET-029 SCM Provider / Pull Request: GitHub App authentication
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L307-L313, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L229-L236, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/internal/github_app/client.go#L16-L74, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/github_app/repos.go#L20-L34, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/git/creds.go#L771-L800
- **Maturity**: GA
- **Semantics**: When `appSecretName` is set (takes precedence over `tokenRef`), the named Secret in the Argo CD namespace is loaded as repository credentials (repo-creds format: `githubAppID`, `githubAppInstallationID` optional, `githubAppEnterpriseBaseUrl`, `githubAppPrivateKey`). If no installation ID is stored, it is auto-discovered for the organization/owner via the GitHub Apps API (cached per org/domain/app ID). API base URL: generator `api`, else the secret's enterprise base URL, else github.com. Used for both SCM Provider (`organization`) and Pull Request (`owner`) GitHub generators.
- **Edge cases**: Secret without a private key → `no github app found for <name>`; lookup failure → `error getting creds for <name>: ...` (wrapped as `error fetching Github app secret` / `error getting GitHub App secret`). Installation ID ≤ 0 after discovery → `installation ID is required for github`. `tokenRefStrictMode` does not apply to `appSecretName`.
- **Configuration surface**: `scmProvider.github.appSecretName`, `pullRequest.github.appSecretName`; repo-creds Secret keys listed above.
- **Exposure**: CRD.
- **Tests required**:
  - App with explicit installation ID authenticates (installation token used).
  - App without installation ID discovers it for the org.
  - Missing private key → error condition.

### ASET-030 SCM Provider: GitLab
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/gitlab.go#L29-L224, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L177-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L507-L533, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L90-L153
- **Maturity**: GA
- **Semantics**: Fields: `group` (ID or full path, required), `api`, `tokenRef`, `allBranches`, `includeSubgroups` (default false), `includeSharedProjects` (pointer, default true), `includeArchivedRepos` (default false; sends `archived=true` when set), `topic` (single topic filter), `insecure`, `caRef`. Lists group projects (100/page, retrying HTTP client). `organization` = project namespace full path, `repository` = project path, `labels` = topics (falls back to legacy tag list when topics empty), `repository_id` = project ID. Default-branch-only mode uses `GetBranch` (404 → no entries); all-branches paginates. `pathsExist` checks file then tree (404 → false).
- **Edge cases**: Unknown clone protocol → `unknown clone protocol for Gitlab <p>`. TLS via `insecure`/`caRef`/SCM root CA (ASET-044); proxy via ASET-045.
- **Configuration surface**: `scmProvider.gitlab.*`.
- **Exposure**: CRD.
- **Tests required**:
  - `includeSubgroups` true/false changes result set.
  - `includeSharedProjects: false` excludes shared projects.
  - `topic` filter passed to API.
  - Self-signed server with `caRef` succeeds; without CA fails; with `insecure` succeeds.

### ASET-031 SCM Provider: Gitea
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/gitea.go#L25-L147, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L475-L489, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L155-L193
- **Maturity**: GA
- **Semantics**: Fields: `owner` (org, required), `api` (required in schema), `tokenRef`, `allBranches`, `insecure`, `excludeArchivedRepos`. Lists org repos; for every repo also lists repo labels (label names become `labels`, so `labelMatch` works). `organization` = configured owner; `repository_id` = repo ID. `cloneProtocol` `ssh` → SSH URL, `https` → the repository HTML URL. Default-branch mode returns the default branch SHA; a 404 is an error: `got 404 while getting default branch "<b>" for repo "<r>" - check your repo config`. `pathsExist`: 404 → false; a directory path (`expect file, got directory`) → true.
- **Edge cases**: Unknown protocol error text says "for GitHub" (`unknown clone protocol for GitHub <p>`). Archived exclusion applied after label lookup.
- **Configuration surface**: `scmProvider.gitea.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Repo labels filterable via `labelMatch`.
  - `https` clone protocol yields HTML URL.
  - Directory path in `pathsExist` counts as existing.

### ASET-032 SCM Provider: Bitbucket Server
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/bitbucket_server.go#L24-L221, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L204-L232, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/util.go#L14-L24, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L531-L539, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L195-L255
- **Maturity**: GA
- **Semantics**: Fields: `project` (key, required), `api` (required; `/rest` appended if missing), `basicAuth` {username, passwordRef}, `bearerToken` {tokenRef}, `allBranches`, `insecure`, `caRef`. Auth precedence: bearerToken > basicAuth > anonymous; authenticated clients send `x-atlassian-token: no-check` and `x-requested-with: XMLHttpRequest`. Repos paginated (limit 100); default branch fetched per repo and repos without one (404 or empty) are skipped; `sha` = latest commit; `labels` empty; `https` uses the `http` clone link. `pathsExist` via content API at the branch (404 → false).
- **Edge cases**: Unknown protocol → `unknown clone protocol for Bitbucket Server <p>`. SCM root CA path also applies (ASET-044).
- **Configuration surface**: `scmProvider.bitbucketServer.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Bearer token preferred when both auths set.
  - Pagination of repos and branches.
  - Repo without default branch skipped.
  - API URL without `/rest` normalised.

### ASET-033 SCM Provider: Azure DevOps
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/azure_devops.go#L59-L220, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L565-L577, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L257-L291
- **Maturity**: GA
- **Semantics**: Fields: `organization` (required), `api` (default `https://dev.azure.com`; URL is `<api>/<organization>`), `teamProject` (required), `accessTokenRef` (required PAT), `allBranches`. Lists repositories of the team project, skipping entries missing name/default branch/remote URL/ID. `url` is always the remote (HTTPS) URL — `cloneProtocol` is ignored. Default branch looked up after stripping `refs/heads/`. `labels` empty; `organization` = configured org. `pathsExist` via GetItem (`GitItemNotFoundException` → false).
- **Edge cases**: Empty token → `no access token provided`. Disabled/missing repo (`GitRepositoryNotFoundException`) yields no branches rather than an error. Invalid URL → `got an invalid URL for the Azure SCM generator`. Not proxied (ASET-045).
- **Configuration surface**: `scmProvider.azureDevOps.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Disabled repo ignored without error.
  - `refs/heads/main` default branch resolved.
  - Missing PAT → error condition.

### ASET-034 SCM Provider: Bitbucket Cloud
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/bitbucket_cloud.go#L26-L179, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L535-L545, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L293-L327
- **Maturity**: GA
- **Semantics**: Fields: `owner` (workspace slug), `user`, `appPasswordRef`, `allBranches`. Basic auth with user/app password against api.bitbucket.org (no custom API URL, so never subject to the allow-list). Lists repositories with role `member`; `repository` = slug, `branch` = main branch, `repository_id` = UUID, `labels` empty. Clone URL taken from `links.clone[]` entry whose name equals the protocol (default `ssh`). `pathsExist` calls `/repositories/{owner}/{repo}/src/{sha}/{path}?format=meta` (200 → true, 404 → false, other → error with HTTP status).
- **Edge cases**: Protocol with no matching clone link → `unknown clone protocol for Bitbucket cloud <p>`; branch without hash → error. Not proxied.
- **Configuration surface**: `scmProvider.bitbucket.*`.
- **Exposure**: CRD.
- **Tests required**:
  - ssh vs https clone link selection.
  - `pathsExist` 404 vs 500 handling.

### ASET-035 SCM Provider: AWS CodeCommit
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/aws_codecommit.go#L57-L377, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L579-L596, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L329-L408
- **Maturity**: Alpha
- **Semantics**: Fields: `tagFilters[]` {key, value?}, `role` (IAM role ARN to assume via STS), `region` (default: controller's AWS region), `allBranches`. Uses the controller's ambient AWS credentials (IRSA, env, IMDS…). Without tag filters, `ListRepositories` (paginated); with filters, Resource Groups Tagging `GetResources` for `codecommit:repository` with filters grouped by key (values of the same key ORed; a key without value matches any value). Repos without default branch are skipped. `organization` = AWS account ID, `repository_id` = repository ID, `sha`/`short_sha`/`short_sha_7` always empty, `labels` empty. Protocols: `ssh` (default), `https`, `https-fips` (rewrites `https://git-codecommit.` to `https://git-codecommit-fips.`). `pathsExist` uses `GetFolder` on the parent folder and matches files/folders/submodules/symlinks by base name; root `/` always exists; RepositoryDoesNotExist/CommitDoesNotExist/FolderDoesNotExist → false.
- **Edge cases**: Non-`https://git-codecommit.` URL with `https-fips` → error. Unknown protocol → `unknown clone protocol for codecommit <p>`. Unparsable ARN from tagging API is skipped with a warning. Not proxied; no custom API URL (always allowed by the allow-list).
- **Configuration surface**: `scmProvider.awsCodeCommit.*`; AWS SDK environment of the controller pod.
- **Exposure**: CRD.
- **Tests required**:
  - Tag filter with key-only and key/value entries builds the correct tagging request.
  - Cross-account role assumption used when `role` set.
  - `https-fips` URL rewrite.
  - `pathsExist` for file, folder, submodule, missing folder.

### ASET-036 SCM filter: `repositoryMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L18-L24, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L52-L55, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L410-L439
- **Maturity**: GA
- **Semantics**: Go RE2 regexp (unanchored `MatchString`) against the repository name. Marks the filter as repo-level unless a branch-level condition is also present in the same filter (ASET-041).
- **Edge cases**: Invalid regexp → `error compiling RepositoryMatch regexp "<re>": ...` (generation error).
- **Configuration surface**: `scmProvider.filters[].repositoryMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `^myapp` keeps `myapp-x` and drops `other-myapp`.
  - Unanchored `myapp` keeps both `myapp-x` and `other-myapp`.
  - Bad regexp → error condition.

### ASET-037 SCM filter: `labelMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L25-L31, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L61-L66
- **Maturity**: GA
- **Semantics**: Regexp that must match at least one repository label (GitHub/GitLab topics, Gitea labels). Repo-level filter. Providers with no labels (Bitbucket Server/Cloud, Azure DevOps, CodeCommit) never match.
- **Edge cases**: Invalid regexp → `error compiling LabelMatch regexp ...`.
- **Configuration surface**: `scmProvider.filters[].labelMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - Repo with topics `[a, deploy-ok]` matches `deploy-ok`; repo without topics excluded.

### ASET-038 SCM filter: `pathsExist`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L32-L35, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L68-L79
- **Maturity**: GA
- **Semantics**: Every listed path (file or directory, trailing `/` trimmed) must exist on the evaluated branch (provider `RepoHasPath`). Branch-level filter (evaluated after branch expansion, so with `allBranches` each branch is checked).
- **Edge cases**: Provider errors other than not-found abort generation.
- **Configuration surface**: `scmProvider.filters[].pathsExist`.
- **Exposure**: CRD.
- **Tests required**:
  - Two paths, one missing → repo excluded.
  - Directory path with trailing slash accepted.

### ASET-039 SCM filter: `pathsDoNotExist`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L36-L39, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L80-L91
- **Maturity**: GA
- **Semantics**: Every listed path must be absent on the evaluated branch. Branch-level filter.
- **Edge cases**: As ASET-038.
- **Configuration surface**: `scmProvider.filters[].pathsDoNotExist`.
- **Exposure**: CRD.
- **Tests required**:
  - Repo containing `disabledrepo.txt` excluded.

### ASET-040 SCM filter: `branchMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L40-L46, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L57-L59
- **Maturity**: GA
- **Semantics**: Regexp against the branch name; branch-level filter. Mostly useful with `allBranches: true`.
- **Edge cases**: Invalid regexp → `error compiling BranchMatch regexp ...`.
- **Configuration surface**: `scmProvider.filters[].branchMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `allBranches` + `branchMatch: ^release-` → only release branches.

### ASET-041 SCM filter combination semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L13-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L96-L178, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L598-L612, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils_test.go#L200-L293
- **Maturity**: GA
- **Semantics**: Within one filter all set conditions are ANDed. Each filter is typed by the last condition kind set in the order repositoryMatch, labelMatch (repo-level) → pathsExist, pathsDoNotExist, branchMatch (branch-level); a filter with any branch-level condition is evaluated entirely (all its conditions) after branch expansion. Stage 1: if any repo-level filters exist, a repo is kept iff it matches at least one of them. Stage 2: branches are expanded for kept repos; if any branch-level filters exist, a branch is kept iff it matches at least one of them. No filters → everything. A filter with no conditions is ignored.
- **Edge cases**: Mixed lists are effectively AND across stages: `[{repositoryMatch: a}, {branchMatch: main}]` requires both, not either.
- **Configuration surface**: `scmProvider.filters[]`.
- **Exposure**: CRD.
- **Tests required**:
  - Two repo-level filters → OR.
  - Repo-level + branch-level filters in separate entries → AND.
  - Filter `{repositoryMatch, pathsExist}` evaluated per branch.

### ASET-042 SCM `cloneProtocol`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L446-L448, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/github.go#L84-L93, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/aws_codecommit.go#L96-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L20
- **Maturity**: GA
- **Semantics**: Selects the `url` param. Empty or `ssh` → SSH URL (all providers that support it); `https` → HTTPS clone URL (Gitea: HTML URL; Bitbucket Server: `http` link); `https-fips` only for CodeCommit. Azure DevOps ignores it (always remote URL).
- **Edge cases**: Unsupported value → provider-specific `unknown clone protocol ...` error.
- **Configuration surface**: `scmProvider.cloneProtocol`.
- **Exposure**: CRD.
- **Tests required**:
  - Per provider: default/ssh/https URLs match upstream fixtures; invalid value errors.

### ASET-043 SCM `allBranches`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/github.go#L130-L163, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/scm_provider/utils.go#L134-L161
- **Maturity**: GA
- **Semantics**: Per provider flag. False: one entry per repo for its default branch (with that branch's head SHA where supported). True: one entry per branch (all pages), each then subject to branch-level filters.
- **Edge cases**: Empty repositories yield no entries (GitHub/GitLab/Bitbucket Server); Gitea default-branch 404 is an error (ASET-031).
- **Configuration surface**: `scmProvider.<provider>.allBranches`.
- **Exposure**: CRD.
- **Tests required**:
  - Repo with 3 branches: false → 1 app, true → 3 apps with distinct `branch`/`sha`.

### ASET-044 SCM / PR TLS: `insecure`, `caRef`, SCM root CA path
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L589-L625, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/kubernetes.go#L46-L63, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L318, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L147-L153
- **Maturity**: GA
- **Semantics**: GitLab and Bitbucket Server (SCM and PR) build TLS config from: PEM certs from `caRef` (ConfigMap key in the ApplicationSet namespace) + the file at the SCM root CA path (appended); `insecure: true` sets InsecureSkipVerify. Gitea (SCM and PR) supports only `insecure`. GitHub, Azure DevOps, Bitbucket Cloud and CodeCommit have no TLS options.
- **Edge cases**: Missing root CA file or unreadable/invalid PEM is logged and the system pool is used. Missing ConfigMap key → `key <k> not found in ConfigMap <cm>` (wrapped `error fetching CA certificates from ConfigMap`).
- **Configuration surface**: `*.insecure`, `*.caRef{configMapName,key}`; `--scm-root-ca-path` / `ARGOCD_APPLICATIONSET_CONTROLLER_SCM_ROOT_CA_PATH` / `applicationsetcontroller.scm.root.ca.path`; argocd-server `--appset-scm-root-ca-path`.
- **Exposure**: CRD; controller config.
- **Tests required**:
  - Self-signed GitLab with CA from `caRef` works; wrong CA fails with TLS error.
  - Root CA path file honoured for Bitbucket Server.
  - `insecure: true` bypasses verification.

### ASET-045 SCM / PR outbound proxy
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L49-L84, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L322-L329, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L319-L320, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L28-L44
- **Maturity**: GA
- **Semantics**: `--scm-proxy-url` and `--scm-no-proxy` (comma-separated hosts/CIDRs) configure an HTTP(S) proxy used by GitHub (SCM+PR, also GitHub App), GitLab, Gitea and Bitbucket Server clients. Does not affect Kubernetes API traffic. Not applied to Azure DevOps, Bitbucket Cloud, AWS CodeCommit, nor to the API server's `Generate` evaluation.
- **Edge cases**: With an empty `--scm-proxy-url` the clients fall back to the standard `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment of the controller process. `--scm-no-proxy` only has effect together with `--scm-proxy-url`.
- **Configuration surface**: `--scm-proxy-url` / `ARGOCD_APPLICATIONSET_CONTROLLER_SCM_PROXY_URL`; `--scm-no-proxy` / `ARGOCD_APPLICATIONSET_CONTROLLER_SCM_NO_PROXY` (no cmd-params keys).
- **Exposure**: controller config.
- **Tests required**:
  - Requests to GitHub/GitLab/Gitea/BBS traverse the proxy; hosts in no-proxy bypass it.

### ASET-046 Pull Request generator: common behavior and parameters
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L40-L217, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L614-L656, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/interface.go#L8-L35, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L370-L448, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L497-L535
- **Maturity**: GA
- **Semantics**: One provider is used, precedence `github`, `gitlab`, `gitea`, `bitbucketServer`, `bitbucket`, `azuredevops`; none → `no Pull Request provider implementation configured`. Checks: SCM providers enabled (ASET-102), allowed API URL (ASET-101). Secret refs are read from the ApplicationSet namespace. Params per PR: `number` (decimal string), `title`, `branch`, `branch_slug` and `target_branch_slug` (gosimple/slug, `_`→`-`, max 50 chars), `target_branch`, `head_sha`, `head_short_sha` (8), `head_short_sha_7` (7), `author`, `labels` ([]string, only when `goTemplate: true`), plus `values` (ASET-069). Default requeue 30 minutes. When a PR stops matching (closed/merged/filtered) its Application is deleted on the next reconcile, subject to the sync policy.
- **Edge cases**: Provider selection errors → `failed to select pull request service provider: ...`; listing errors → `error listing repos: ...`. `labels` param absent in fasttemplate mode.
- **Configuration surface**: `spec.generators[].pullRequest.{github,gitlab,gitea,bitbucketServer,bitbucket,azuredevops,filters,requeueAfterSeconds,template,values,continueOnRepoNotFoundError}`.
- **Exposure**: CRD.
- **Tests required**:
  - Fake provider with 2 PRs → 2 apps with all params; `branch_slug` of `Feature_Branch/ABC` slugified and ≤50 chars.
  - goTemplate exposes `labels`; fasttemplate does not.
  - Closing a PR removes its app (policy `sync`) and keeps it (policy `create-update`).

### ASET-047 Pull Request: GitHub
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/github.go#L23-L123, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L219-L244, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L690-L704, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L28-L70
- **Maturity**: GA
- **Semantics**: Fields: `owner`, `repo` (required), `api` (GitHub Enterprise URL), `tokenRef` (optional, anonymous otherwise), `appSecretName` (ASET-029, takes precedence), `labels`. Lists pull requests of the repository (GitHub default: open), 100/page, all pages; keeps PRs carrying all `labels`. `branch` = head ref, `target_branch` = base ref, `head_sha` = head SHA, `author` = user login. Uses the proxied SCM HTTP client and optional GitHub API metrics.
- **Edge cases**: HTTP 404 on listing → repository-not-found error (ASET-057).
- **Configuration surface**: `pullRequest.github.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Label filter requires all labels.
  - GHE `api` used for requests.
  - 404 without `continueOnRepoNotFoundError` → error condition.

### ASET-048 Pull Request: GitLab
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/gitlab.go#L25-L109, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L706-L723, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L72-L119
- **Maturity**: GA
- **Semantics**: Fields: `project` (ID or path, required), `api` (default gitlab.com), `tokenRef`, `labels` (sent as the API `labels` filter), `pullRequestState` (`""` = all states; `opened`, `closed`, `merged`, `locked` sent as `state`), `insecure`, `caRef`. Lists project merge requests 100/page. `number` = MR IID, `head_sha` = MR SHA, `author` = username.
- **Edge cases**: Default (empty state) includes closed and merged MRs, so they generate Applications unless filtered. 404 → repository-not-found error. Webhook matching requires a numeric project ID (ASET-106).
- **Configuration surface**: `pullRequest.gitlab.*`.
- **Exposure**: CRD.
- **Tests required**:
  - `pullRequestState: opened` excludes merged MRs; empty includes them.
  - Labels passed through to the API.
  - TLS options as ASET-044.

### ASET-049 Pull Request: Gitea
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/gitea.go#L24-L107, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L658-L672, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L121-L157
- **Maturity**: GA
- **Semantics**: Fields: `owner`, `repo`, `api` (all required), `tokenRef`, `insecure`, `labels` (all must be present, client-side). Lists open pull requests. `number` = PR index, `author` = poster username.
- **Edge cases**: 404 → repository-not-found error.
- **Configuration surface**: `pullRequest.gitea.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Only open PRs; label filter applied.

### ASET-050 Pull Request: Bitbucket Server
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/bitbucket_server.go#L24-L102, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L166-L190, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L725-L741, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L159-L222
- **Maturity**: GA
- **Semantics**: Fields: `project`, `repo`, `api` (required; `/rest` appended if missing), `basicAuth`, `bearerToken` (precedence bearer > basic > anonymous), `insecure`, `caRef`. Pages pull requests (limit 100). `branch` = from-ref display ID, `target_branch` = to-ref display ID, `head_sha` = from-ref latest commit, `author` = author user name; labels unsupported (empty).
- **Edge cases**: 404 → repository-not-found error.
- **Configuration surface**: `pullRequest.bitbucketServer.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Pagination; bearer vs basic auth headers; branchMatch filtering.

### ASET-051 Pull Request: Bitbucket Cloud
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/bitbucket_cloud.go#L65-L169, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L191-L207, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L743-L767, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L224-L295
- **Maturity**: GA
- **Semantics**: Fields: `owner` (workspace), `repo` (slug), `api` (default `https://api.bitbucket.org/2.0`), `basicAuth` {username,passwordRef} or `bearerToken` {tokenRef} (bearer > basic > unauthenticated). Lists pull requests (default open). `branch`/`target_branch` from source/destination branch names, `head_sha` = source commit hash, `author` = nickname; no labels.
- **Edge cases**: An error whose text contains `404 Not Found` → repository-not-found error. Invalid `api` URL → `error parsing base url of ...`. Unexpected response shape → `unknown type returned from bitbucket pull requests`.
- **Configuration surface**: `pullRequest.bitbucket.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Default API URL used when empty; pagination; malformed response error.

### ASET-052 Pull Request: Azure DevOps
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/azure_devops.go#L48-L158, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L674-L688, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L297-L337
- **Maturity**: GA
- **Semantics**: Fields (YAML key `azuredevops`): `organization`, `project`, `repo` (required), `api` (default `https://dev.azure.com`; URL `<api>/<organization>`), `tokenRef` (PAT; anonymous connection when absent), `labels` (all required, client-side). Lists pull requests of the project with default search criteria, keeps those whose repository name equals `repo`, skipping PRs with missing repository/ID/refs/last-merge-source commit. `branch`/`target_branch` have `refs/heads/` stripped, `head_sha` = last merge source commit, `author` = part of creator unique name before `@`.
- **Edge cases**: Error containing `The following project does not exist` → repository-not-found error (only project existence is checked).
- **Configuration surface**: `pullRequest.azuredevops.*`.
- **Exposure**: CRD.
- **Tests required**:
  - PRs from other repos in the same project are excluded.
  - Labels filter; author parsing.

### ASET-053 Pull Request filter: `branchMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/utils.go#L11-L80, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L777-L784, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L339-L368
- **Maturity**: GA
- **Semantics**: Regexp (unanchored) on the PR source branch. Filters list: a PR is kept if it matches any filter; within a filter all set fields must match; no filters → all PRs; an empty filter `{}` matches everything.
- **Edge cases**: Invalid regexp → `error compiling BranchMatch regexp "<re>": ...`.
- **Configuration surface**: `pullRequest.filters[].branchMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `.*-argocd` keeps only matching branches; OR across two filters.

### ASET-054 Pull Request filter: `targetBranchMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/utils.go#L22-L27, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/utils.go#L43-L45
- **Maturity**: GA
- **Semantics**: Regexp on the PR target branch; combined as ASET-053.
- **Edge cases**: Invalid regexp → `error compiling TargetBranchMatch regexp ...`.
- **Configuration surface**: `pullRequest.filters[].targetBranchMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `{branchMatch: ".*-argocd", targetBranchMatch: "master"}` requires both.

### ASET-055 Pull Request filter: `titleMatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/utils.go#L28-L33, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/utils.go#L46-L48
- **Maturity**: GA
- **Semantics**: Regexp on the PR title; combined as ASET-053.
- **Edge cases**: Invalid regexp → `error compiling TitleMatch regexp ...`.
- **Configuration surface**: `pullRequest.filters[].titleMatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `^feat:` keeps only feature PRs.

### ASET-056 Pull Request provider-side `labels` filter
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/github.go#L96-L114, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/gitlab.go#L60-L80, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/gitea.go#L85-L98, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/azure_devops.go#L137-L146
- **Maturity**: GA
- **Semantics**: `labels` on GitHub, GitLab, Gitea and Azure DevOps providers: a PR is generated only if it has all listed labels (GitLab delegates to the API `labels` parameter; others filter client-side). Bitbucket Server/Cloud have no labels field.
- **Edge cases**: Empty list → no label filtering.
- **Configuration surface**: `pullRequest.{github,gitlab,gitea,azuredevops}.labels`.
- **Exposure**: CRD.
- **Tests required**:
  - PR with labels `[a]` excluded by `labels: [a, b]`; included once `b` is added.

### ASET-057 Pull Request `continueOnRepoNotFoundError`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L73-L82, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/pull_request/errors.go#L5-L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L631-L632, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset.yaml#L133-L144
- **Maturity**: GA
- **Semantics**: Default false. When true and the provider reports a repository-not-found error (GitHub/GitLab/Gitea/Bitbucket Server HTTP 404, Bitbucket Cloud error text `404 Not Found`, Azure DevOps "project does not exist"), the generator logs a warning and returns zero parameter sets instead of failing. Typical use: PR generator nested in a Matrix with a Git/SCM generator.
- **Edge cases**: Because zero params are returned, existing Applications for that repo are deleted under policies allowing delete; providers often return 404 for permission loss too.
- **Configuration surface**: `pullRequest.continueOnRepoNotFoundError`.
- **Exposure**: CRD.
- **Tests required**:
  - 404 + false → `ErrorOccurred`; 404 + true → no error, zero apps; 500 + true → error.

### ASET-058 Cluster Decision Resource generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type.go#L61-L231, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L396-L410, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type_test.go#L29-L360, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Cluster-Decision-Resource.md#L1-L87
- **Maturity**: GA
- **Semantics**: Reads ConfigMap `configMapRef` (required) from the Argo CD namespace: `apiVersion` (`group/version`, a `/` at index ≥1 required), `kind` (used verbatim as the resource plural for the dynamic client, e.g. `ducks`), `statusListKey` (default `clusters`), `matchKey` (required). Lists resources of that GVR in the Argo CD namespace by `name` (field selector `metadata.name`) or by `labelSelector`. Skips resources with no/empty `status`; concatenates `status.<statusListKey>` arrays. For each decision, `<matchKey>` value (stringified) must equal an Argo CD cluster **name** (cluster informer list including synthetic `in-cluster`); otherwise skipped. Params: `name`, `server` of the matched cluster plus every key of the decision stringified with `%v` (even in goTemplate mode).
- **Edge cases**: Bad apiVersion/kind → `there is a problem with the apiVersion, kind or resourceName provided`. Missing `matchKey` → no params, no error (warning). No resources → `no clusterDecisionResources found`. Empty decisions → no params. A resource whose non-empty status lacks the list key triggers a type-assertion panic recovered by the reconciler. Not re-triggered by cluster secret events.
- **Configuration surface**: `clusterDecisionResource.{configMapRef,name,labelSelector,requeueAfterSeconds,values,template}`; ConfigMap keys `apiVersion`, `kind`, `statusListKey`, `matchKey`.
- **Exposure**: CRD.
- **Tests required**:
  - Placement-like CR with two decisions → two apps; adding a cluster to status adds an app.
  - Decision referencing unknown cluster skipped.
  - Non-string decision values stringified (`score: 0` → `"0"`).
  - Missing matchKey → no apps, no error.

### ASET-059 Cluster Decision Resource: selection mode, `values`, requeue
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type.go#L47-L55, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type.go#L100-L121, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type.go#L170-L172, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/duck_type.go#L222-L231
- **Maturity**: GA
- **Semantics**: Exactly one of `name` or `labelSelector` (matchLabels/matchExpressions) must be set; both or neither → `there is a problem with the definition of the ClusterDecisionResource generator`. `values` are copied **verbatim** (not templated) as `values.<k>` (fasttemplate) or `.values.<k>` (goTemplate). Requeue default = global default (3m) unless `requeueAfterSeconds`.
- **Edge cases**: Values containing `{{...}}` are not resolved by the generator (they are rendered later only if used inside the template string).
- **Configuration surface**: `clusterDecisionResource.name`, `.labelSelector`, `.values`, `.requeueAfterSeconds`.
- **Exposure**: CRD.
- **Tests required**:
  - Name + labelSelector together → error condition.
  - labelSelector selection across multiple CRs.
  - `values.foo: bar` available as `values.foo`.

### ASET-060 Plugin generator: ConfigMap and token resolution
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/plugin.go#L86-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/plugin.go#L147-L201, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/plugin/utils.go#L10-L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/settings/settings.go#L2526-L2541, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Plugin.md#L77-L143
- **Maturity**: GA
- **Semantics**: `plugin.configMapRef.name` names a ConfigMap in the Argo CD namespace with `baseUrl` (required, non-empty), `token` (required, non-empty, must start with `$`) and optional `requestTimeout` (integer seconds; default 30). Token reference `$<key>` reads `<key>` from Secret `argocd-secret`; `$<secret>:<key>` reads `<key>` from Secret `<secret>` (both in the Argo CD namespace); the value is whitespace-trimmed.
- **Edge cases**: Missing CM → `error fetching ConfigMap: ...`; empty baseUrl → `baseUrl not found in ConfigMap`; empty token → `token not found in ConfigMap`; token without `$` → `token is empty, or does not reference a secret key starting with '$': ...`; missing Secret → `error fetching secret <ns>/<name>: ...`; key absent in an existing Secret → the literal `$...` string is sent as bearer token (warning only); non-integer `requestTimeout` → `error set requestTimeout : ...`.
- **Configuration surface**: `plugin.configMapRef.name`; ConfigMap keys `baseUrl`, `token`, `requestTimeout`; Secret keys referenced by `token`.
- **Exposure**: CRD.
- **Tests required**:
  - `$plugin.x.token` resolves from argocd-secret; `$other:plugin.x.token` from another secret.
  - Each error case above yields the upstream error.
  - `requestTimeout: "1"` aborts a slow plugin after ~1s.

### ASET-061 Plugin generator: HTTP contract
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/plugin/plugin_service.go#L12-L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/internal/http/client.go#L14-L159, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Plugin.md#L145-L233
- **Maturity**: GA
- **Semantics**: Request: `POST <baseUrl>/api/v1/getparams.execute` (a `/` is appended to baseUrl if missing), headers `Content-Type: application/json`, `Authorization: Bearer <token>`, `User-Agent: argocd-applicationset`, body `{"applicationSetName": "<appset name>", "input": {"parameters": {...}}}` (JSON, HTML escaping disabled). Response must be 2xx with body `{"output": {"parameters": [ {..}, ... ]}}`; each object is one parameter set. A request is made per evaluation (per Matrix first-child param set when nested).
- **Edge cases**: Non-2xx → `API error with status code <n>: <message|error field|raw body>`; JSON decode errors include the raw body; empty body accepted (zero params). All wrapped as `error listing params: error get api '<appset>': ...`.
- **Configuration surface**: plugin service endpoint; ConfigMap `baseUrl`.
- **Exposure**: CRD; external HTTP contract.
- **Tests required**:
  - Recorded request matches path, headers and body schema exactly.
  - 403 response with `{"message": "x"}` → error text includes `x`.
  - Response with 2 parameter objects → 2 apps.

### ASET-062 Plugin generator: input parameters, output mapping, `values`, requeue
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/plugin.go#L41-L84, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/plugin.go#L112-L145, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L786-L810, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Plugin.md#L23-L75
- **Maturity**: GA
- **Semantics**: `input.parameters` is a map of arbitrary JSON values sent to the plugin. Output params: goTemplate keeps objects as returned; fasttemplate flattens with dot keys and `%v` values. Every param set then gets `generator` = `{"input": {"parameters": <input.parameters>}}` (overwriting any plugin key `generator`) and `values` (ASET-069; generator values are not sent to the plugin). Default requeue 30 minutes. Any ApplicationSet webhook push/PR event refreshes ApplicationSets with a top-level Plugin generator (ASET-107).
- **Edge cases**: In fasttemplate mode `generator.input.parameters.*` is a nested map and cannot be substituted. In goTemplate mode a plugin-returned `values` key survives only if the generator defines no `values`.
- **Configuration surface**: `plugin.input.parameters`, `plugin.values`, `plugin.requeueAfterSeconds`, `plugin.template`.
- **Exposure**: CRD.
- **Tests required**:
  - goTemplate template reads `{{ index .generator.input.parameters.map "key1" }}`.
  - Plugin output key `generator` replaced by input echo.
  - Matrix(PR, plugin) sends interpolated `branch` per PR.

### ASET-063 Matrix generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/matrix.go#L17-L175, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/map.go#L16-L35, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L284-L325, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L1-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L175-L255
- **Maturity**: GA
- **Semantics**: Exactly two children (`found less than two generators, Matrix support only two` / `found more than two generators, Matrix support only two`). For each param set `a` of child 0 (after its selector), child 1 is evaluated (interpolated with `a`, ASET-064, then its selector applied) and each result `b` is combined: goTemplate → deep merge of `b` then `a` with override (child 0 wins on conflicting keys, nested maps merged); fasttemplate → all values stringified, union of keys, a key present in both with different values fails with `found duplicate key <k> with different value, a: <v1> ,b: <v2>`. Output order: child-0 order × child-1 order. Requeue: minimum non-zero requeue of the children. Only the Matrix's own `template` is used.
- **Edge cases**: Child 0 with zero params → zero output (child 1 never evaluated). Errors are wrapped `error failed to get params for first generator in matrix generator: ...` / `failed to get params for second generator in the matrix generator: ...` / `child generator returned an error on parameter generation: ...`.
- **Configuration surface**: `spec.generators[].matrix.generators[2]`, `.matrix.template`.
- **Exposure**: CRD.
- **Tests required**:
  - Git dirs × clusters → N×M apps with combined params.
  - goTemplate override precedence (child 0 wins) per doc example.
  - fasttemplate conflicting `path` keys without prefix → error.
  - One or three children → exact error messages.

### ASET-064 Matrix: parameter interpolation from the first child into the second
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L55-L68, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L175-L186, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L307-L396, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L111-L173, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L389-L422
- **Maturity**: GA
- **Semantics**: When child 0 produced a non-empty param set, the entire child-1 generator spec (every string field and map key, recursively, including selectors, repo URLs, file paths, plugin input) is rendered with that param set using the ApplicationSet's templating mode and options. Generator `values` maps are pre-resolved specially: with goTemplate, `missingkey=error` is forced and any value failing with a missing key is kept verbatim (to be resolved later against the child's own params); other template errors fail. Only the second child may consume the first child's params; circular use is not supported.
- **Edge cases**: Interpolation errors are reported as generation errors. With fasttemplate, unknown tags are left intact (so the child's own params can resolve them later only in `values`).
- **Configuration surface**: `matrix.generators[1].*` string fields.
- **Exposure**: CRD.
- **Tests required**:
  - Git files (child 0) feeds `{{.path.basename}}` into Clusters selector (child 1).
  - Child-1 `values` mixing child-0 params and child-1 params resolves both.
  - Consumer placed first → params unresolved (no interpolation), as upstream.

### ASET-065 Merge generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/merge.go#L18-L228, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L327-L379, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/merge_test.go#L52-L260, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Merge.md#L1-L221
- **Maturity**: GA
- **Semantics**: Requires ≥2 children (`found less than two generators, Merge requires two or more`) and ≥1 `mergeKeys` (`no merge keys were specified, Merge requires at least one`). Each child is evaluated independently (no interpolation). Param sets of every child are indexed by the JSON of `{mergeKey: paramSet[mergeKey]}` (top-level key lookup; duplicated mergeKeys deduplicated); duplicates within any child → `the parameters from a generator were not unique by the given mergeKeys, Merge requires all param sets to be unique. Duplicate key was <json>`. Starting from child 0 (base), each later child in order overrides matching base sets (fasttemplate: shallow key overwrite; goTemplate: deep merge with override). Non-matching sets from later children are discarded; unmatched base sets are kept. Requeue: min non-zero of children.
- **Edge cases**: Output order is not deterministic. In goTemplate mode nested keys (e.g. `values.selector`) cannot be used as merge keys (lookup is top-level only); in fasttemplate mode flattened keys like `values.selector` work. Param sets missing the merge key all share key `null` and collide.
- **Configuration surface**: `merge.generators[]`, `merge.mergeKeys[]`, `merge.template`.
- **Exposure**: CRD.
- **Tests required**:
  - Base clusters + override clusters + list on `server` produces upstream doc result.
  - Later child wins over earlier override.
  - Duplicate merge-key values in a child → error.
  - Unmatched base entries retained, unmatched override entries dropped.

### ASET-066 Combination generator nesting rules
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L213-L274, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/utils.go#L14-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/matrix.go#L82-L123, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Matrix.md#L355-L388
- **Maturity**: GA
- **Semantics**: Matrix/Merge children are `ApplicationSetNestedGenerator`s: any terminal generator (List, Clusters, Git, SCMProvider, ClusterDecisionResource, PullRequest, Plugin, each with optional `selector`) or one nested Matrix/Merge stored as free-form JSON and decoded into `NestedMatrixGenerator`/`NestedMergeGenerator` whose children may only be terminal generators. Therefore combination generators nest exactly one level. Child-level `template` is never applied. Each child entry must contain exactly one generator.
- **Edge cases**: A third level is silently dropped during decoding (unknown field) and results in `child generator generated no parameters`. Invalid nested JSON → decode error surfaced as generation error. Two generators in one child entry → `found more than one generator in matrix.Generators` (also used for Merge).
- **Configuration surface**: `matrix.generators[].matrix|merge`, `merge.generators[].matrix|merge`.
- **Exposure**: CRD.
- **Tests required**:
  - Matrix(Matrix(list,list), list) works; Matrix(Matrix(Matrix(...))) yields the no-parameters error.
  - Child template ignored.

### ASET-067 Post-selector on generators
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L30-L102, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/generator_spec_processor.go#L126-L163, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/selector.go#L94-L258, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Post-Selector.md#L1-L87
- **Maturity**: GA
- **Semantics**: `selector` (LabelSelector) on a top-level generator entry or on any Matrix/Merge child filters that generator's param sets after generation. Each param set is flattened (dot keys; nested `map[string]any` and `map[string]string` recursed; values `%v`) and matched with a copy of the Kubernetes selector logic **without label-value validation** (so URLs etc. can be matched); keys must still be valid qualified names. `matchLabels` = equality; `matchExpressions` operators `In`, `NotIn` (values required), `Exists`, `DoesNotExist` (no values). All requirements ANDed. `NotIn` matches when the key is absent. Nil selector → no filtering; empty selector → match all.
- **Edge cases**: Invalid operator/values/key → `error parsing label selector: ...`. Flattening errors skip that param set and record an error.
- **Configuration surface**: `spec.generators[].selector`, `matrix|merge.generators[].selector`.
- **Exposure**: CRD.
- **Tests required**:
  - List + `matchLabels env=staging` keeps one element.
  - `matchExpressions server In [https://kubernetes.default.svc]` on Clusters (URL value accepted).
  - goTemplate nested key `metadata.labels.environment` selectable.
  - Selector inside nested Matrix child applied.

### ASET-068 `applyNestedSelectors` (deprecated)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L75-L79
- **Maturity**: Deprecated
- **Semantics**: `spec.applyNestedSelectors` is accepted by the schema but ignored; nested selectors are always applied (ASET-067).
- **Edge cases**: Setting it to false does not disable nested selectors.
- **Configuration surface**: `spec.applyNestedSelectors`.
- **Exposure**: CRD.
- **Tests required**:
  - Field accepted and round-tripped; behavior identical for true/false.

### ASET-069 Generator `values` field semantics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/value_interpolation.go#L8-L41, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/value_interpolation_test.go#L10-L120, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-SCM-Provider.md#L481-L525
- **Maturity**: GA
- **Semantics**: Clusters, Git, SCMProvider, PullRequest and Plugin generators accept `values` (map[string]string). For each param set, each value is rendered with the ApplicationSet's engine/options against that param set (values cannot reference other values — rendered into a temporary map then merged). Result: fasttemplate keys `values.<k>`; goTemplate key `values` = map[string]string (replacing any existing `values` key when at least one value is defined). ClusterDecisionResource `values` are copied without templating (ASET-059). List elements may carry their own `values` object (ASET-012).
- **Edge cases**: Template error → `failed to replace templated string with rendered values: ...` wrapped by the generator. With `missingkey=error`, referencing a missing param fails generation.
- **Configuration surface**: `<generator>.values`.
- **Exposure**: CRD.
- **Tests required**:
  - Same values map rendered per param set (different results per cluster/repo).
  - `values.a: '{{.values.b}}'` does not resolve `b`.

### ASET-070 Templating engine: fasttemplate (default)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L398-L442, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils_test.go#L716-L722, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Template.md#L1-L10
- **Maturity**: GA
- **Semantics**: Used when `spec.goTemplate` is false/absent. Strings not matching `.*{{.*}}.*` are returned unchanged. Otherwise `{{` … `}}` tags are replaced by the param whose key equals the whitespace-trimmed tag content, only if that param is a string; unknown/empty/non-string tags are left verbatim (original spacing kept). No functions, no conditionals. Params are flat string maps (generators flatten nested data with dot keys).
- **Edge cases**: Unclosed tag (e.g. `{{a}} {{b}`) → `invalid template: ...` error (RenderTemplateParamsError). `goTemplateOptions` have no effect in this mode.
- **Configuration surface**: `spec.goTemplate: false`.
- **Exposure**: CRD.
- **Tests required**:
  - `{{ cluster }}` and `{{cluster}}` both substitute.
  - Unknown tag left as-is.
  - Non-string param (e.g. elementsYaml number) not substituted.
  - Unclosed brace yields error, no panic.

### ASET-071 Templating engine: Go templates (`goTemplate`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L402-L423, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L68, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L1-L210, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L263-L303
- **Maturity**: GA
- **Semantics**: With `goTemplate: true` each templated string is parsed and executed independently as a Go `text/template` (cloned from a base template preloaded with the function map, ASET-073..075) against the structured param map (`.path.basename`, `index .metadata.labels "k"`, etc.). Generators emit structured params in this mode (nested maps/arrays, e.g. Git `path` object, Cluster `metadata` object, PR `labels`). Default missing-key behavior renders `<no value>`.
- **Edge cases**: Parse error → `failed to parse template <tmpl>: ...`; execution error → `failed to execute go template <tmpl>: ...`. Control structures cannot span multiple YAML fields.
- **Configuration surface**: `spec.goTemplate`.
- **Exposure**: CRD.
- **Tests required**:
  - Same ApplicationSet rendered in both modes per GoTemplate.md migration table yields identical apps.
  - Missing key without options renders `<no value>`.
  - `{{range}}` split across fields fails as documented.

### ASET-072 `goTemplateOptions`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L409-L411, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L74, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L39-L45
- **Maturity**: GA
- **Semantics**: Each entry is passed to `text/template.Option` (e.g. `missingkey=error`, `missingkey=zero`, `missingkey=invalid`, `missingkey=default`) for every template render (application template, `templatePatch`, generator `values`, Matrix interpolation). Recommended `["missingkey=error"]` (missing key → execution error, `map has no entry for key`). Ignored when `goTemplate` is false.
- **Edge cases**: `text/template` panics on an unrecognised option; the reconciler recovers and the reconcile fails. Matrix `values` pre-resolution always forces `missingkey=error` (ASET-064).
- **Configuration surface**: `spec.goTemplateOptions[]`.
- **Exposure**: CRD.
- **Tests required**:
  - `missingkey=error` + missing key → `ErrorOccurred` (RenderTemplateParamsError).
  - `missingkey=zero` renders empty.
  - Invalid option string → reconcile fails without crashing the controller.

### ASET-073 Go template function set (Sprig allow-list)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L31-L51, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L212-L223
- **Maturity**: GA
- **Semantics**: All functions of Sprig v3 `GenericFuncMap` (v3.3.0 per go.mod) are available except `env`, `expandenv` and `getHostByName` (removed to avoid leaking environment information), plus Go built-ins (`index`, `printf`, `len`, …) and the Argo CD additions `normalize`, `slugify`, `toYaml`, `fromYaml`, `fromYamlArray` (ASET-074/075). Non-hermetic Sprig functions (e.g. `now`, `randAlpha`, `uuidv4`) remain available.
- **Edge cases**: Using `env`/`expandenv`/`getHostByName` → parse error `function "env" not defined`.
- **Configuration surface**: template strings.
- **Exposure**: CRD.
- **Tests required**:
  - `dig`, `default`, `toJson`, `b64enc`, `upper` behave as Sprig v3.3.0.
  - `env "HOME"` fails to parse.

### ASET-074 `normalize` and `slugify` template functions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/template_functions.go#L10-L25, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L541-L587, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L11-L37
- **Maturity**: GA
- **Semantics**: `normalize s`: lowercase, replace every char outside `[-a-z0-9.]` with `-`, truncate to 253 chars, trim leading/trailing `-` and `.` (same function as `nameNormalized`/`basenameNormalized`/`branchNormalized`). `slugify [maxSize=50] [smartTruncate=true] s`: normalize then gosimple/slug with given max length and smart truncation (does not cut words). Argument order: first int, second bool, last the input.
- **Edge cases**: Wrong argument types to `slugify` panic inside the template (recovered by text/template as an execution error).
- **Configuration surface**: template strings.
- **Exposure**: CRD.
- **Tests required**:
  - `normalize "My_App.Name-"` → `my-app.name`.
  - `slugify 23` and `slugify 50 false` outputs match upstream test vectors.

### ASET-075 `toYaml` / `fromYaml` / `fromYamlArray` template functions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/template_functions.go#L27-L71, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L223
- **Maturity**: GA
- **Semantics**: `toYaml v` marshals with sigs.k8s.io/yaml and strips the trailing newline; `fromYaml s` → map; `fromYamlArray s` → list. Unlike Helm, errors are returned (template execution fails) rather than embedded in the result.
- **Edge cases**: Invalid YAML in `fromYaml` → execution error.
- **Configuration surface**: template strings / `templatePatch`.
- **Exposure**: CRD.
- **Tests required**:
  - `{{ .values | toYaml }}` inside `templatePatch` yields valid YAML block.
  - `fromYaml` on invalid input → `RenderTemplateParamsError`.

### ASET-076 Templating scope: string fields, map keys and raw JSON fields
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L91-L260, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils_test.go#L198-L300, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils_test.go#L673-L714, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/GoTemplate.md#L52-L113
- **Maturity**: GA
- **Semantics**: Rendering walks the Application object reflectively: every string field (metadata name, labels, annotations, finalizers, entire spec including sources, helm parameters, info, sync options …) is rendered; map keys are rendered too (e.g. annotation keys); slices element-wise; nil pointers/maps stay nil; `time.Time` copied. Raw JSON fields (`runtime.RawExtension`/`apiextensions JSON`, e.g. `helm.valuesObject`) are converted YAML→JSON, recursively rendered (string leaves and keys), and re-marshalled. Non-string scalars (bool/int) and whole objects cannot be templated (YAML type must already be valid) — use `templatePatch`.
- **Edge cases**: A field whose value is a template that produces a non-string type stays a string. Each string renders independently.
- **Configuration surface**: `spec.template.*`.
- **Exposure**: CRD.
- **Tests required**:
  - Annotation key templating in both engines.
  - `helm.valuesObject` with nested templated strings renders correctly (JSON and YAML variants).
  - Boolean `useCredentials: "{{...}}"` rejected by schema.

### ASET-077 `templatePatch`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/template.go#L50-L89, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/patch.go#L13-L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/patch_test.go#L13-L250, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Template.md#L130-L200
- **Maturity**: GA
- **Semantics**: `spec.templatePatch` (string) is rendered per param set with the ApplicationSet's engine and options, converted YAML→JSON (unless it already starts with `{`), validated by unmarshalling into an Application, and applied as a Kubernetes strategic-merge patch (Application type metadata) onto the rendered Application. Afterwards `spec.project` is forced back to the value rendered from the template (patch cannot change project), and the namespace is later forced to the ApplicationSet namespace. Enables conditional/non-string fields (automated sync, prune booleans, value-file lists).
- **Edge cases**: Render failure → `error replacing values in templatePatch: ...`; invalid YAML → `error while converting template to json ...`; type mismatch → `invalid templatePatch "<json>": ...`; patch failure → `error while applying templatePatch template to json ...` (all with reason `RenderTemplateParamsError`). A patch containing an empty `spec:` (null) clears the spec. Not restricted to goTemplate mode in code (fasttemplate substitution is applied when `goTemplate` is false).
- **Configuration surface**: `spec.templatePatch`.
- **Exposure**: CRD.
- **Tests required**:
  - Doc example: `autoSync`/`prune` booleans and value-file list produced.
  - Patch attempting to change `spec.project` has no effect.
  - Patch with `spec:` empty clears spec (regression #17040 behavior).
  - Invalid patch → error condition.

### ASET-078 Application sync policies (`applicationsSync`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L112-L145, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L321-L366, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L770-L795, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L17-L84
- **Maturity**: GA
- **Semantics**: `spec.syncPolicy.applicationsSync` enum (CRD-validated) `create-only` | `create-update` | `create-delete` | `sync`. Update allowed for `create-update` and `sync`; delete allowed for `sync` and `create-delete`. When update is not allowed, only Applications whose names are not among the currently owned Applications are created (existing ones are left untouched). When delete is allowed, owned Applications not present in the (full, including invalid) generated list are deleted. Default `sync`. The effective policy is decided by ASET-079. The policy also controls what happens on ApplicationSet deletion (ASET-087).
- **Edge cases**: `create-only`/`create-delete` never propagate template changes to existing apps (nor RollingSync operations). Deletion caused by ownerReference GC is not governed by this field.
- **Configuration surface**: `spec.syncPolicy.applicationsSync`.
- **Exposure**: CRD.
- **Tests required**:
  - Matrix of 4 policies × {template change, element removal}: update/delete performed exactly per table.
  - Invalid enum value rejected by schema.

### ASET-079 Controller-wide policy and per-ApplicationSet override
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/policy.go#L7-L22, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L133-L137, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L302-L303, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L333-L342, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L40
- **Maturity**: GA
- **Semantics**: Controller `--policy` accepts `""` (= `sync`), `sync`, `create-only`, `create-update`, `create-delete`; other values log `Policy value can be: sync, create-only, create-update, create-delete, default value: sync` and exit 1. Effective policy = the ApplicationSet's `applicationsSync` only if it is set AND policy override is enabled; otherwise the controller policy. `--enable-policy-override` default is computed at startup as "the policy taken from the environment is empty" (true when `ARGOCD_APPLICATIONSET_CONTROLLER_POLICY` is unset/empty, false otherwise); an explicit boolean in `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE` or the flag wins; empty/non-boolean env values fall back to the computed default.
- **Edge cases**: Setting `--policy` only on the command line (no env var) leaves override enabled by default. With override disabled, `applicationsSync` in the spec is silently ignored.
- **Configuration surface**: `--policy` / `ARGOCD_APPLICATIONSET_CONTROLLER_POLICY` / `applicationsetcontroller.policy`; `--enable-policy-override` / `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_POLICY_OVERRIDE` / `applicationsetcontroller.enable.policy.override`.
- **Exposure**: controller config; CRD.
- **Tests required**:
  - Controller `create-only` + appset `sync` + override false → no updates/deletes.
  - Same with override true → appset policy applies.
  - Invalid controller policy → process exits non-zero with upstream message.

### ASET-080 `preservedFields` and global preserved annotations/labels
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L681-L718, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L84-L87, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L321-L322, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L289-L330
- **Maturity**: GA
- **Semantics**: On create/update, for each key in `spec.preservedFields.annotations` + global preserved annotations (+ defaults, ASET-081), if the live Application has that annotation its live value is copied into the desired object; same for labels. Exact key match. All other labels/annotations are replaced by the generated ones.
- **Edge cases**: If the template also sets a preserved key, the live value wins (template changes to that key never apply once present). Keys absent on the live object are taken from the template.
- **Configuration surface**: `spec.preservedFields.{annotations,labels}`; `--preserved-annotations` / `ARGOCD_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_ANNOTATIONS` / `applicationsetcontroller.global.preserved.annotations`; `--preserved-labels` / `ARGOCD_APPLICATIONSET_CONTROLLER_GLOBAL_PRESERVED_LABELS` / `applicationsetcontroller.global.preserved.labels` (comma-separated).
- **Exposure**: CRD; controller config.
- **Tests required**:
  - User-added annotation/label on app survives reconcile only when listed.
  - Global list applies to all ApplicationSets.
  - Preserved key also in template: live value kept.

### ASET-081 Default preserved annotations
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L84-L88, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L697-L700, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_annotations.go#L4-L8
- **Maturity**: GA
- **Semantics**: Always preserved regardless of configuration: `notified.notifications.argoproj.io`, `argocd.argoproj.io/refresh`, `argocd.argoproj.io/hydrate`.
- **Edge cases**: None beyond ASET-080.
- **Configuration surface**: none.
- **Exposure**: controller behavior.
- **Tests required**:
  - User sets `argocd.argoproj.io/refresh=hard` on a generated app; reconcile does not remove it.

### ASET-082 Preserved deletion-hook finalizers
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L79-L82, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L720-L732, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/application_defaults.go#L9-L16
- **Maturity**: GA
- **Semantics**: Live finalizers with prefix `pre-delete-finalizer.argocd.argoproj.io` or `post-delete-finalizer.argocd.argoproj.io` (including `/<stage>` suffixed variants) are appended to the desired finalizer list so the application controller's delete hooks are not disrupted. All other finalizers are exactly the generated list (ASET-004).
- **Edge cases**: A manually added unrelated finalizer is removed on the next update.
- **Configuration surface**: none.
- **Exposure**: controller behavior.
- **Tests required**:
  - App with `post-delete-finalizer.argocd.argoproj.io/cleanup` keeps it after reconcile; a custom `foo` finalizer is removed.

### ASET-083 `ignoreApplicationDifferences`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L147-L178, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/createOrUpdate.go#L52-L88, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/createOrUpdate.go#L184-L257, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L102-L206
- **Maturity**: GA
- **Semantics**: List of rules `{name?, jsonPointers[], jqPathExpressions[]}` converted to Argo CD ignore-difference rules for group `argoproj.io`, kind `Application` (rule applies to all apps when `name` empty, else to that app name). On update (not on create), both the normalized live Application and the desired Application are normalized with these rules (ignored fields removed/neutralized on both sides), the result is renormalized, and only remaining differences are patched — so ignored live values are kept. Desired `name`, `namespace`, TypeMeta and `operation` cannot be altered by rules. The same rules drive progressive-sync spec-change detection (ASET-092).
- **Edge cases**: Invalid jq/pointer → `failed to build ignore diff config: ...` reconcile error (no Applications updated). Because updates are JSON merge patches, a change elsewhere in a list (e.g. another `sources[]` entry) rewrites the whole list including ignored fields.
- **Configuration surface**: `spec.ignoreApplicationDifferences[]`.
- **Exposure**: CRD.
- **Tests required**:
  - Ignore `/spec/syncPolicy`: manual disable of auto-sync survives reconcile.
  - Named rule affects only that app.
  - jq on `sources[] | select(...)`.targetRevision preserved until another source changes.
  - Invalid jq → error condition.

### ASET-084 Application create/update mechanics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L636-L768, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/createOrUpdate.go#L27-L50, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/createOrUpdate.go#L90-L182
- **Maturity**: GA
- **Semantics**: Valid apps are processed sorted by name. Desired spec is normalized (`NormalizeApplicationSpec`). Missing apps are created (desired metadata + owner ref). Existing apps: spec replaced, `operation` set if the desired app carries one (RollingSync), labels/annotations/finalizers replaced (with preserved keys), owner ref set; compared with the normalized live object after ignore rules using semantic equality (quantities by value, times in UTC, destinations by name/namespace/server); identical → no write; else JSON merge patch computed from normalized live. Successful create/update emits a Normal event on the ApplicationSet with reason `created`/`updated` and message `<action> Application "<name>"`. Patches are logged at debug level.
- **Edge cases**: Per-app errors do not stop other apps; the returned error is the lexicographically first failing app's error; context cancellation aborts. The object name/namespace cannot be changed by the mutate step (`MutateFn cannot mutate object name and/or object namespace`).
- **Configuration surface**: none (see ASET-086 for concurrency).
- **Exposure**: controller behavior; Kubernetes events.
- **Tests required**:
  - No-op reconcile performs zero writes (resourceVersion unchanged).
  - Changing template field produces a single patch and an `updated` event.
  - Failing app among several: others still applied, condition shows first failing app error.

### ASET-085 Controller dry-run mode
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L164-L176, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L309, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L9-L15
- **Maturity**: GA
- **Semantics**: `--dry-run` sets the controller-runtime client to server-side dry-run for all writes: reconciles run fully but no Application/ApplicationSet create/update/patch/delete (including status and finalizer changes) is persisted.
- **Edge cases**: Status conditions are therefore not visible either.
- **Configuration surface**: `--dry-run` / `ARGOCD_APPLICATIONSET_CONTROLLER_DRY_RUN` / `applicationsetcontroller.dryrun` (default false).
- **Exposure**: controller config.
- **Tests required**:
  - With dry-run, creating an ApplicationSet produces no Applications and no status changes.

### ASET-086 Concurrency controls
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L883-L905, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L317, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L328
- **Maturity**: GA
- **Semantics**: `--concurrent-application-updates` (1..200, default 1; ≤0 treated as 1) bounds parallel create/update/delete calls within one ApplicationSet reconcile. `--concurrent-reconciliations` (≥1, default 10) bounds parallel ApplicationSet reconciles.
- **Edge cases**: Results/errors are deterministic regardless of parallelism (first error by app name).
- **Configuration surface**: `--concurrent-application-updates` / `ARGOCD_APPLICATIONSET_CONTROLLER_CONCURRENT_APPLICATION_UPDATES` (no cmd-params key); `--concurrent-reconciliations` / `ARGOCD_APPLICATIONSET_CONTROLLER_CONCURRENT_RECONCILIATIONS` / `applicationsetcontroller.concurrent.reconciliations.max`.
- **Exposure**: controller config.
- **Tests required**:
  - 200 apps with concurrency 10 converge with identical results to concurrency 1.
  - Out-of-range env value falls back to default.

### ASET-087 ApplicationSet deletion, finalizer and orphaning
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L149-L179, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L973-L988, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L86-L100, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Application-Deletion.md#L1-L29
- **Maturity**: GA
- **Semantics**: When an ApplicationSet has a deletionTimestamp, the controller (1) if the effective policy does not allow delete (`create-only`, `create-update`), removes all ownerReferences from every owned Application (one Update each) so garbage collection will not delete them; (2) if Reverse deletion is configured, runs ordered deletion (ASET-095), requeueing until done; (3) removes `resources-finalizer.argocd.argoproj.io` from the ApplicationSet and updates it. No generation happens for terminating ApplicationSets. Without that finalizer the ApplicationSet disappears immediately and Kubernetes GC deletes owned Applications (background or foreground per the delete request); `--cascade=orphan` keeps them. Each deleted Application then deletes its cluster resources if it carries the resources finalizer.
- **Edge cases**: Policy-based preservation only works when the ApplicationSet has the finalizer (otherwise GC wins). Update errors while orphaning abort and retry.
- **Configuration surface**: ApplicationSet `metadata.finalizers: [resources-finalizer.argocd.argoproj.io]`; effective policy.
- **Exposure**: CRD; API `Delete`; CLI `argocd appset delete`.
- **Tests required**:
  - Delete appset (no finalizer, policy sync) → apps and resources deleted.
  - Delete appset with finalizer + `create-only` → apps survive without ownerReferences.
  - `kubectl delete --cascade=orphan` keeps apps.
  - Finalizer removed after processing; appset gone.

### ASET-088 Finalizer removal for Applications with invalid destinations
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L807-L881, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L907-L971
- **Maturity**: GA
- **Semantics**: Before deleting an owned Application that is no longer generated, if it has finalizers and its destination cannot be resolved, or the resolved cluster matches no known cluster (same server, or same non-empty name; synthetic in-cluster included), only `resources-finalizer.argocd.argoproj.io` is removed via merge patch and a Normal event `Updated Application "<name>" finalizer before deletion, because application has an invalid destination` is emitted; then the Application is deleted (event `Deleted` / `Deleted Application "<name>"`). Avoids Applications stuck terminating after their cluster was removed.
- **Edge cases**: Other finalizers are kept. Patch failure → `error updating finalizers: ...` (DeleteApplicationError).
- **Configuration surface**: none.
- **Exposure**: controller behavior; events.
- **Tests required**:
  - Remove a cluster secret used by a Clusters generator: the app is deleted without hanging on the resources finalizer.
  - App with valid destination keeps its finalizer during deletion.

### ASET-089 Progressive syncs: enablement
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L311, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L186-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L248-L273, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L45-L57, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Progressive-Syncs.md#L1-L25
- **Maturity**: Beta
- **Semantics**: Progressive syncs (`spec.strategy`, `status.applicationStatus`) are only active when the controller runs with progressive syncs enabled (default false). When disabled, any existing `status.applicationStatus` is cleared on reconcile and `spec.strategy` has no effect on syncing. When enabled, an ApplicationSet whose strategy is not RollingSync has its `status.applicationStatus` cleared; extra watch triggers apply (ASET-010).
- **Edge cases**: With the feature disabled but `strategy.type: RollingSync`, an existing `RolloutProgressing` condition is not evaluated and may remain. Reverse deletion still runs on ApplicationSet deletion if the finalizer is present, even when disabled (finalizer is only auto-added when enabled).
- **Configuration surface**: `--enable-progressive-syncs` / `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_PROGRESSIVE_SYNCS` / `applicationsetcontroller.enable.progressive.syncs`.
- **Exposure**: controller config.
- **Tests required**:
  - Disabled: RollingSync appset behaves like AllAtOnce; prior applicationStatus cleared.
  - Enabled then strategy switched to AllAtOnce: applicationStatus cleared.

### ASET-090 RollingSync strategy: steps and `matchExpressions`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L89-L110, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L279-L356, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L524-L579, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Progressive-Syncs.md#L27-L97
- **Maturity**: Beta
- **Semantics**: Active when `strategy.type == "RollingSync"` and `strategy.rollingSync` is set with ≥1 step (`type` empty or `AllAtOnce` = default behavior). Each step's `matchExpressions` (AND) are evaluated against the generated Application labels: key present → `In` requires the value in `values`, `NotIn` requires it absent; other operators log an error and do not match; key absent → `In` fails, any other operator is skipped. Empty `matchExpressions` selects every app. An app is assigned to the first matching step (warning if it matches several, but it is listed in every matching step for gating). Step N+1 apps become eligible only when every app listed in steps 1..N exists and has status `Healthy`; steps with no apps are skipped.
- **Edge cases**: Apps matching no step get step `-1`, are never synced by the controller (manual sync only) and are not deleted by Reverse deletion. Multi-step matches can block later steps until all those steps are Healthy.
- **Configuration surface**: `spec.strategy.type`, `spec.strategy.rollingSync.steps[].matchExpressions[{key,operator,values}]`.
- **Exposure**: CRD.
- **Tests required**:
  - Two steps (dev, prod): prod apps stay Waiting until all dev apps are Healthy.
  - App without the label and `NotIn` expression is selected.
  - Unselected app keeps step `-1` and is never auto-synced.
  - Empty step in the middle does not block.

### ASET-091 RollingSync `maxUpdate`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L598-L678, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset.yaml#L299-L323, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Progressive-Syncs.md#L63
- **Maturity**: Beta
- **Semantics**: Per step `maxUpdate` (int or percent string) caps how many apps of that step may be `Pending`/`Progressing` at once; unset → unlimited. Percent is applied to the number of status entries in the step, rounded down, but any percentage other than `0%` yields at least 1. `0` / `0%` → the controller never promotes apps of that step (manual sync needed; rollout waits). Apps are promoted Waiting→Pending in status order until the cap is reached.
- **Edge cases**: An unparsable string (e.g. `abc`) logs a warning and behaves as 1. Apps without a step are counted against the first step's cap.
- **Configuration surface**: `spec.strategy.rollingSync.steps[].maxUpdate`.
- **Exposure**: CRD.
- **Tests required**:
  - 10 apps, `maxUpdate: 10%` → one at a time.
  - `maxUpdate: 2` → never more than 2 Pending/Progressing.
  - `maxUpdate: 0` → step blocked until manual sync makes apps Healthy.

### ASET-092 Progressive sync per-Application status (`status.applicationStatus`)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L358-L522, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L887-L915, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L990-L1030, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L1079-L1157
- **Maturity**: Beta
- **Semantics**: One entry per owned Application `{application, status, step (1-based or "-1"), message, lastTransitionTime, targetRevisions}`, sorted by application name, written via the status subresource with conflict retries. Lifecycle: new → `Waiting` ("No Application status found, defaulting status to Waiting"); revision change (`targetRevisions` vs the app's current revisions) and/or desired-vs-live spec change (normalized, ignoring `automated.enabled` and `ignoreApplicationDifferences`) → `Waiting` with message "Application has pending changes[ (revision and spec differ)| (spec differs)], setting status to Waiting"; `Waiting` + app `Synced` → `Healthy` (if app Healthy) or `Progressing`; eligible `Waiting` within `maxUpdate` → `Pending` ("Application moved to Pending status, watching for the Application resource to start Progressing"); `Pending` → `Progressing` when the app was reconciled and an operation started after the Pending transition (message depends on phase: completed successfully / completed / other), or the app has an `InvalidSpecError`/`UnknownError` condition; `Progressing` → `Healthy` when the app is Healthy and Synced. On startup, entries with nil `targetRevisions` are migrated to `[]`.
- **Edge cases**: Spec comparison errors leave the entry unchanged. Entries for apps no longer owned are dropped. A failed sync still moves Pending→Progressing (retries are the Application's responsibility).
- **Configuration surface**: `status.applicationStatus[]` (read-only).
- **Exposure**: CRD (status); CLI `-o yaml`; UI.
- **Tests required**:
  - Full Waiting→Pending→Progressing→Healthy trace with expected messages.
  - Git revision bump returns Healthy apps to Waiting.
  - Spec-only change (e.g. helm parameter) detected; ignored-field change not detected.
  - App with InvalidSpecError moves Pending→Progressing.

### ASET-093 RollingSync sync triggering and automated-sync suppression
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L734-L806, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L309-L314, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L676-L679, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Progressive-Syncs.md#L58-L68
- **Maturity**: Beta
- **Semantics**: For RollingSync ApplicationSets with steps, every generated app with an automated sync policy gets `spec.syncPolicy.automated.enabled: false` (the controller drives syncs). For apps that are eligible and in `Pending`, the desired app carries `operation`: `initiatedBy {username: "applicationset-controller", automated: true}`, `info [{name: "Reason", value: "ApplicationSet RollingSync triggered a sync of this Application resource"}]`, `sync {syncOptions: <app syncPolicy.syncOptions>, prune: <original automated.prune>}`, `retry: <app syncPolicy.retry or {limit: 5}>`; it is written through the normal update path (so sync windows etc. are enforced by the application controller).
- **Edge cases**: Requires a policy that allows updates; with `create-only`/`create-delete` existing apps never receive operations. `prune` is only set when the app has a sync policy.
- **Configuration surface**: `spec.template.spec.syncPolicy.{automated,syncOptions,retry}`.
- **Exposure**: CRD (Application `operation`).
- **Tests required**:
  - Generated app with `automated: {prune: true}` has `automated.enabled=false` and the triggered operation has `prune: true`.
  - Operation `initiatedBy.username` and info message exact.
  - Default retry limit 5 when none configured.

### ASET-094 `RolloutProgressing` condition
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L680-L732, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L442-L493
- **Maturity**: Beta
- **Semantics**: For RollingSync ApplicationSets (progressive syncs enabled), after updating application statuses: if any step (in order) has entries not all `Healthy` → `RolloutProgressing=True`, reason `ApplicationSetModified`, message `ApplicationSet is performing rollout of step <n>` (first such step); otherwise `False`, reason `ApplicationSetRolloutComplete`, message `ApplicationSet Rollout has completed`. For non-RollingSync ApplicationSets the condition is removed. Drives `status.health=Progressing` (ASET-098).
- **Edge cases**: Steps without entries are treated as complete.
- **Configuration surface**: `status.conditions[type=RolloutProgressing]`.
- **Exposure**: CRD; CLI; UI.
- **Tests required**:
  - Condition True with step number during rollout, False when all Healthy.
  - Removed after switching to AllAtOnce.

### ASET-095 `deletionOrder: Reverse`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L83-L103, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L131-L277, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/progressivesync/progressive_sync.go#L581-L584, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L161-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Progressive-Syncs.md#L99-L156
- **Maturity**: Beta
- **Semantics**: `strategy.deletionOrder` = `AllAtOnce` (default) or `Reverse` (case-insensitive), effective only with RollingSync and ≥1 step. With progressive syncs enabled the controller adds `resources-finalizer.argocd.argoproj.io` to such ApplicationSets. When the ApplicationSet is deleted, owned apps assigned to steps are deleted one at a time in descending step order: for each app, if gone → next; if terminating → requeue 10s; otherwise issue Delete and requeue 10s. When all are gone the ApplicationSet finalizer is removed. If an app has appeared terminating for more than 2 minutes, the controller verifies with an uncached API read: if absent it evicts the stale cache entry using a UID-preconditioned delete and continues; if still present it returns `application has not been deleted in over 2 minutes` (exponential backoff); if it was recreated (UID conflict) it errors and re-plans.
- **Edge cases**: Only ApplicationSet deletion is ordered; apps dropped from generator output are deleted all at once. Apps matching no step are not part of the ordered deletion (left to GC). Order among apps of the same step is unspecified.
- **Configuration surface**: `spec.strategy.deletionOrder`.
- **Exposure**: CRD.
- **Tests required**:
  - 2 steps × 2 apps: all step-2 apps deleted before any step-1 app.
  - Finalizer auto-added when enabled; ApplicationSet removed only after all apps are gone.
  - Stuck app (>2 min, still present) surfaces error and retries; phantom cache entry recovers.

### ASET-096 Status conditions
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L827-L885, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L1006-L1076, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L199-L213, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L398-L539
- **Maturity**: GA
- **Semantics**: Condition types `ErrorOccurred`, `ParametersGenerated`, `ResourcesUpToDate`, `RolloutProgressing`; fields `type`, `status` (`True`/`False`), `reason`, `message`, `lastTransitionTime`. `ParametersGenerated` is always written: True (`ParametersGenerated`, "Successfully generated parameters for all Applications") once generation succeeded, else False with reason `ErrorOccurred` and the error message. An error sets `ErrorOccurred=True` (reason one of `ApplicationGenerationFromParamsError`, `RenderTemplateParamsError`, `ApplicationValidationError`, `CreateApplicationError`, `UpdateApplicationError`, `DeleteApplicationError`, `RefreshApplicationError`) and forces `ResourcesUpToDate=False` (reason `ErrorOccurred`, same message). Success sets `ResourcesUpToDate=True` (reason `ApplicationSetUpToDate`, "All applications have been generated successfully") and `ErrorOccurred=False` with the same reason/message. Conditions are sorted (type, status, reason, message, time); `lastTransitionTime` is kept when type/status/reason/message are unchanged; no API write when nothing changed; status updates retry on conflict and ignore NotFound.
- **Edge cases**: Validation errors with partial success leave `ErrorOccurred=True` (no `ResourcesUpToDate=True`) for that pass. Defined-but-unused reasons: `ApplicationGeneratedSuccessfully`, `SyncApplicationError`; status `Unknown` is never written.
- **Configuration surface**: `status.conditions[]`.
- **Exposure**: CRD; API; CLI (`get`, `list`); UI (conditions modal).
- **Tests required**:
  - Success path conditions exactly as upstream (3 conditions, reasons/messages).
  - Generation failure: `ParametersGenerated=False`, `ErrorOccurred=True`, `ResourcesUpToDate=False`.
  - Re-reconcile without change does not bump `lastTransitionTime`.

### ASET-097 `status.resources` and `status.resourcesCount`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/status/resource_status.go#L7-L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L368-L376, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L1032-L1077, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L812-L825, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L326
- **Maturity**: GA
- **Semantics**: After create/update/delete, the owned Applications are re-listed and `status.resources` is rebuilt: one entry per app `{group, version, kind, name, namespace, status: <app sync status>, health: {status: <app health>}}`, stale entries removed, sorted by name. If more than `--max-resources-status-count` (default 5000; 0 = unlimited) entries exist, only the first N (by name) are kept; `resourcesCount` always holds the total. Written via the status subresource with conflict retry; failure → reconcile error.
- **Edge cases**: Entries reflect app status at reconcile time (refreshed by app watch events). Truncation affects ResourceTree, UI and the owned-applications metric.
- **Configuration surface**: `--max-resources-status-count` / `ARGOCD_APPLICATIONSET_CONTROLLER_MAX_RESOURCES_STATUS_COUNT` / `applicationsetcontroller.status.max.resources.count`.
- **Exposure**: CRD (status); API `ResourceTree`; UI tree.
- **Tests required**:
  - Resource list matches owned apps with sync/health.
  - Limit 2 with 3 apps → 2 entries, `resourcesCount: 3`.
  - Deleted app removed from the list.

### ASET-098 `status.health`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L951-L1004, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L1065-L1066, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Web-UI.md#L41-L55
- **Maturity**: GA
- **Semantics**: Recomputed whenever conditions are set: no conditions → `Unknown` ("No status conditions found for ApplicationSet"); any `ErrorOccurred=True` → `Degraded` (condition message); else `RolloutProgressing=True` → `Progressing`; else `ResourcesUpToDate=True` → `Healthy`; else `Unknown` ("Waiting for health status to be determined").
- **Edge cases**: Health is only refreshed when conditions are written.
- **Configuration surface**: `status.health.{status,message}`.
- **Exposure**: CRD; CLI (`HEALTH` column / `Health Status:`); UI (health filter, status bar).
- **Tests required**:
  - Each rule produces the expected status/message.

### ASET-099 ApplicationSets in any namespace (controller)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L99-L155, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L300, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/utils.go#L60-L62, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L591-L595, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Appset-Any-Namespace.md#L1-L133
- **Maturity**: GA
- **Semantics**: `--applicationset-namespaces` lists additional namespaces (shell globs, or regexps wrapped in `/.../`); the controller's own namespace is always appended. With no extra namespaces the manager cache is restricted to the control-plane namespace; otherwise it watches cluster-wide (cluster-scoped RBAC required) and all events are filtered to allowed namespaces. In multi-namespace mode, if SCM providers are enabled and `--allowed-scm-providers` is empty, the controller exits with `When enabling applicationset in any namespace using applicationset-namespaces, you must either set --enable-scm-providers=false or specify --allowed-scm-providers`. Generated Applications live in the ApplicationSet's namespace (ASET-002) and require apps-in-any-namespace + AppProject `sourceNamespaces` to be reconciled. `tokenRef`/`caRef` objects are read from the ApplicationSet namespace; cluster secrets, AppProjects, plugin and ClusterDecisionResource ConfigMaps, GitHub App secrets come from the control-plane namespace.
- **Edge cases**: ApplicationSets in non-allowed namespaces are ignored entirely (no status). Cluster generator in a tenant namespace still sees all clusters.
- **Configuration surface**: `--applicationset-namespaces` / `ARGOCD_APPLICATIONSET_CONTROLLER_NAMESPACES` / `applicationsetcontroller.namespaces`.
- **Exposure**: controller config.
- **Tests required**:
  - Appset in `team-*` namespace reconciled; in other namespace ignored.
  - Regex entry `/^((?!not-allowed).)*$/` honoured.
  - Startup failure without allowed SCM providers.
  - tokenRef Secret resolved in the appset namespace, not argocd.

### ASET-100 API server namespace enablement for ApplicationSets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/server.go#L1060-L1082, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L609-L641, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/security/application_namespaces.go#L9-L15, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Appset-Any-Namespace.md#L198-L260
- **Maturity**: GA
- **Semantics**: The API server accepts ApplicationSets in its own namespace or in namespaces matching the API server's application namespaces list (`application.namespaces` / `--application-namespaces`, glob or `/regex/`); others are rejected with `namespace '<ns>' is not permitted` (Get, ResourceTree, ListResourceEvents, Create, Generate) or silently filtered (List, Watch). An empty `appsetNamespace` defaults to the API server namespace.
- **Edge cases**: `Delete` does not check the enabled-namespace list (only RBAC).
- **Configuration surface**: argocd-server `application.namespaces` / `--application-namespaces`.
- **Exposure**: API; CLI; UI.
- **Tests required**:
  - Get/Create in non-enabled namespace → `namespace '<ns>' is not permitted`.
  - List omits appsets in non-enabled namespaces.

### ASET-101 Allowed SCM provider API URLs
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L109-L143, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L459-L473, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L636-L656, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L307, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Appset-Any-Namespace.md#L24-L76
- **Maturity**: GA
- **Semantics**: If the allow-list is non-empty, a SCM Provider or Pull Request generator whose provider has a non-empty custom `api` must match an entry exactly (string equality, including scheme and trailing slash). Checked fields: SCM `github|gitlab|gitea|bitbucketServer|azureDevOps.api`; PR `github|gitlab|gitea|bitbucketServer|bitbucket|azuredevops.api`. Empty `api` (provider default endpoint) and providers without an `api` field (SCM Bitbucket Cloud, AWS CodeCommit) are always allowed. Violation → `scm provider not allowed: scm provider "<url>" not allowed, must use one of the following: <a>, <b>` (logged with security field).
- **Edge cases**: `https://git.example.com` vs `https://git.example.com/` are different. The same flag on argocd-server (`--appset-allowed-scm-providers`) applies to `Generate`.
- **Configuration surface**: `--allowed-scm-providers` / `ARGOCD_APPLICATIONSET_CONTROLLER_ALLOWED_SCM_PROVIDERS` / `applicationsetcontroller.allowed.scm.providers`; argocd-server `--appset-allowed-scm-providers`.
- **Exposure**: controller config; API.
- **Tests required**:
  - Disallowed Gitea `api` → error, no outbound request made.
  - Allowed exact URL works; trailing-slash mismatch rejected.
  - Empty `api` allowed with non-empty list.

### ASET-102 Enable/disable SCM-backed generators
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L107, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L154-L156, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L131-L133, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L308
- **Maturity**: GA
- **Semantics**: With SCM providers disabled, SCM Provider and Pull Request generators fail with `scm providers are disabled` (PR wrapped as `failed to select pull request service provider: scm providers are disabled`). Default enabled.
- **Edge cases**: Other generators unaffected.
- **Configuration surface**: `--enable-scm-providers` / `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_SCM_PROVIDERS` / `applicationsetcontroller.enable.scm.providers`; argocd-server `--appset-enable-scm-providers`.
- **Exposure**: controller config; API.
- **Tests required**:
  - Disabled: SCM and PR appsets get `ErrorOccurred` with the message; List appsets unaffected.

### ASET-103 `tokenRef` strict mode
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/utils/kubernetes.go#L15-L44, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L310, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L369-L381, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Appset-Any-Namespace.md#L78-L99
- **Maturity**: GA
- **Semantics**: When enabled, every Secret referenced through a `SecretRef` by SCM/PR generators (`tokenRef`, `passwordRef`, `appPasswordRef`, `accessTokenRef`, `bearerToken.tokenRef`) must carry label `argocd.argoproj.io/secret-type=scm-creds`, else `secret <ns>/<name> is not a valid SCM creds secret: secret must have label "argocd.argoproj.io/secret-type"="scm-creds"`. The API server's `Generate` and `Create --dry-run` evaluation always runs in strict mode. A nil ref yields an empty token (anonymous). Missing key → `key "<k>" in secret <ns>/<name> not found`.
- **Edge cases**: Not applied to `appSecretName`, plugin tokens or `caRef` ConfigMaps.
- **Configuration surface**: `--token-ref-strict-mode` / `ARGOCD_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE` / `applicationsetcontroller.enable.tokenref.strict.mode` (default false).
- **Exposure**: controller config; API.
- **Tests required**:
  - Strict + unlabeled secret → error; labeled → OK.
  - Non-strict + unlabeled secret → OK in controller, but API `Generate` fails.

### ASET-104 ApplicationSet webhook server
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L33-L194, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L611-L624, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L239-L246, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L333-L344, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/manifests/base/applicationset-controller/argocd-applicationset-controller-service.yaml#L1-L19, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Git.md#L462-L542
- **Maturity**: GA
- **Semantics**: The controller serves plain HTTP on `--webhook-addr` (default `:7000`, Service port `webhook`) at path `/api/webhook` (separate from the API server webhook). Provider detected by header: `X-GitHub-Event` (accepts push, pull_request, ping), `X-Gitlab-Event` (push, tag push, merge request, system hook), `X-Vss-Activityid` (Azure DevOps git.push, git.pullrequest.created/updated/merged); anything else → HTTP 400 `Unknown webhook event`. Verification: GitHub HMAC with `webhook.github.secret`, GitLab token with `webhook.gitlab.secret`, Azure DevOps basic auth with `webhook.azuredevops.username`/`webhook.azuredevops.password` (keys of `argocd-secret`, `$secret:key` references allowed), read once at startup (restart required). Parse/verification failure → 400 `Webhook processing failed: <html-escaped error>` (405 if the method is not POST). Accepted payloads are queued (capacity 50000) and processed asynchronously by `--webhook-parallelism-limit` workers (default 50, range 1..1000); full queue → 503 `Queue is full, discarding webhook payload`; otherwise 200 with empty body. Matching ApplicationSets (ASET-105..107) get annotation `argocd.argoproj.io/application-set-refresh: "true"` via merge patch with conflict retry, which triggers reconcile (ASET-011).
- **Edge cases**: Without configured secrets, unauthenticated events are accepted (only cause refreshes). If settings cannot be loaded at startup the webhook server is not started (error logged) while the controller keeps running. The handler lists every ApplicationSet visible to the controller cache, not only allowed namespaces. Panics in processing are recovered and logged.
- **Configuration surface**: `--webhook-addr`; `--webhook-parallelism-limit` / `ARGOCD_APPLICATIONSET_CONTROLLER_WEBHOOK_PARALLELISM_LIMIT` / `applicationsetcontroller.webhook.parallelism.limit`; `argocd-secret` keys above.
- **Exposure**: HTTP endpoint on the controller.
- **Tests required**:
  - Valid GitHub push with correct signature → 200 and refresh annotation set; wrong signature → 400.
  - Unknown provider header (e.g. Bitbucket) → 400 `Unknown webhook event`.
  - GET request → 405.
  - Saturated queue → 503.

### ASET-105 Webhook: Git push events
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L196-L233, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L316-L351, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L239-L242, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/webhook/webhook.go#L548-L587, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook_test.go#L50-L360
- **Maturity**: GA
- **Semantics**: GitHub push (repository `html_url`, `ref`, `default_branch`), GitLab push (project `web_url`, `ref`, default branch) and Azure DevOps `git.push` (repository remote URL, first ref update vs default branch) produce (repo URL regex, revision, touchedHead). Revision = ref with the first two path segments removed (`refs/heads/feature/x` → `feature/x`). A Git generator (top level or inside Matrix/Merge, ASET-107) matches if its `repoURL` matches the URL regex (case-insensitive; optional `http(s)://`/`ssh://`, optional `user@`, optional `ssh.`/`altssh.` host prefix, optional port, `:` or `/` separator, optional `.git`) and: generator revision empty/`HEAD` → push touched the default branch; else the parsed generator revision or the raw revision equals the pushed revision.
- **Edge cases**: GitLab tag-push and system-hook payloads other than push are parsed but do not trigger refreshes. Azure push without ref updates is dropped (recovered panic). Generators pinned to a tag refresh on pushes of that tag name (GitHub).
- **Configuration surface**: provider webhook pointing to `/api/webhook`.
- **Exposure**: HTTP endpoint.
- **Tests required**:
  - Push to default branch refreshes `revision: HEAD` appset; push to other branch does not.
  - `git@github.com:org/repo.git` repoURL matched by `https://github.com/org/repo` push.
  - Revision `refs/heads/release/1.0` matches generator revision `release/1.0`.

### ASET-106 Webhook: pull request events
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L235-L314, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L353-L414, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Generators-Pull-Request.md#L450-L495
- **Maturity**: GA
- **Semantics**: GitHub `pull_request` with action `opened|closed|synchronize|labeled|reopened|unlabeled`: refreshes PR generators with `github` whose `owner` and `repo` equal the payload (case-insensitive) and whose `api` (default `https://api.github.com/`) matches a host-only regex from the payload repository API URL (optional scheme/user/port, optional trailing `/`, no path). GitLab merge request with action `open|close|reopen|update|merge`: refreshes `gitlab` PR generators whose `project` equals the payload target project ID (string compare) and whose `api` hostname (default `https://gitlab.com/`) equals the payload project web URL hostname. Azure DevOps `git.pullrequest.created|updated|merged`: refreshes `azuredevops` generators with equal project and repo names. Other actions/providers are ignored.
- **Edge cases**: GitLab generators configured with a project path (not numeric ID) never match. GitHub Enterprise generators whose `api` includes a path (e.g. `/api/v3/`) do not match the host-only regex.
- **Configuration surface**: provider webhooks with PR/MR events enabled.
- **Exposure**: HTTP endpoint.
- **Tests required**:
  - Each allowed action refreshes; e.g. GitHub `edited` and `assigned` do not.
  - Owner/repo case-insensitivity.
  - GitLab project ID match vs path mismatch.

### ASET-107 Webhook: Plugin and combination generator propagation
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L123-L159, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L330-L332, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/webhook/webhook.go#L416-L609
- **Maturity**: GA
- **Semantics**: Any accepted push or PR event refreshes every ApplicationSet with a top-level Plugin generator. Matrix (exactly 2 children): refresh if child 0 is a matching Git/PR generator or a nested Matrix/Merge that matches; otherwise child 0's params are generated live and child 1 is interpolated with each param set and checked (Git/PR/Plugin/nested Matrix/Merge); finally child 1 uninterpolated is checked. Merge: each child checked for matching Git/PR generators and nested Matrix/Merge.
- **Edge cases**: Matrix evaluation may call external systems (Git/SCM) during webhook processing; errors → no refresh. Plugin children of a Merge (or child 0 of a Matrix) do not trigger refresh.
- **Configuration surface**: none.
- **Exposure**: HTTP endpoint.
- **Tests required**:
  - Push to repo used in Matrix(list, git) child 1 with `{{...}}` repoURL refreshes after interpolation.
  - Any push refreshes top-level Plugin appsets.

### ASET-108 Templated `project` field and ApplicationSet trust model
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L532-L548, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/template/patch.go#L39-L40, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/git.go#L74-L97, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Security.md#L1-L38, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Appset-Any-Namespace.md#L135-L196
- **Maturity**: GA
- **Semantics**: ApplicationSets may template `spec.template.spec.project` (resolved per param set and validated against existing AppProjects, ASET-008), but: the API server rejects Create/Generate when the project contains `{{` (`the Argo CD API does not currently support creating ApplicationSets with templated `project` fields`); `templatePatch` cannot change the project; the Git generator disables signature verification and project-scoped repo credentials when the project is templated. For ApplicationSets outside the control-plane namespace, generated Applications are still subject to AppProject `sourceNamespaces` enforcement by the application controller. Kubernetes RBAC on the CRD must restrict ApplicationSet authoring to admins.
- **Edge cases**: `kubectl apply` bypasses the API restriction (controller accepts templated projects).
- **Configuration surface**: `spec.template.spec.project`.
- **Exposure**: CRD; API.
- **Tests required**:
  - API create with templated project rejected with exact message; kubectl create accepted and rendered.
  - Appset in tenant namespace generating an app in a project without that source namespace → app rejected by the application controller.

### ASET-109 RBAC: `applicationsets` resource
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L61-L64, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/security/rbac.go#L8-L13, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/rbac/rbac.go#L63, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L383-L394, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L77, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/rbac.md#L228-L251
- **Maturity**: GA
- **Semantics**: Resource `applicationsets`, actions `get`, `create`, `update`, `delete`. Object: `<project>/<name>` for ApplicationSets in the API server namespace, `<project>/<namespace>/<name>` otherwise, where project is `spec.template.spec.project` (empty → `default`). Read RPCs require `get` (List/Watch filter per item); Create, Create dry-run and Generate require `create`; upserting a differing spec additionally requires `update`, and changing the project requires `create` on the new and `update` on the old object; Delete requires `delete`.
- **Edge cases**: No project-level isolation beyond the RBAC name (the template's project is trusted for RBAC). Users with `get` but not `create` cannot use UI Preview.
- **Configuration surface**: `argocd-rbac-cm` policies.
- **Exposure**: API; CLI; UI.
- **Tests required**:
  - `p, dev, applicationsets, *, dev-project/*, allow` permits create in dev-project, denies other projects.
  - Namespaced object `proj/ns/name` matched by `proj/ns/*`.
  - Upsert changing project without update on old project → PermissionDenied.

### ASET-110 API: Get ApplicationSet
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L14-L20, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L79-L82, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L234-L237, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L626-L641
- **Maturity**: GA
- **Semantics**: `ApplicationSetService.Get` — `GET /api/v1/applicationsets/{name}?appsetNamespace=<ns>`. Reads from the API server informer; checks enabled namespace then RBAC `get`. Returns the full ApplicationSet (spec + status).
- **Edge cases**: Missing → error wrapping `error getting ApplicationSet: ...` (NotFound); non-enabled namespace → `namespace '<ns>' is not permitted`.
- **Configuration surface**: none.
- **Exposure**: API (gRPC + REST); CLI `appset get`; UI details.
- **Tests required**:
  - Get existing/missing/forbidden/non-enabled namespace.

### ASET-111 API: List ApplicationSets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L22-L29, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L239-L284, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/argo.go#L134-L150
- **Maturity**: GA
- **Semantics**: `GET /api/v1/applicationsets?projects=&selector=&appsetNamespace=`. Label selector parsed (error → `error parsing the selector: ...`); optional namespace restriction; items outside enabled namespaces dropped; RBAC `get` per item; optional project filter on `spec.template.spec.project`; sorted by name; `metadata.resourceVersion` = informer last sync RV.
- **Edge cases**: Unauthorized items silently omitted.
- **Configuration surface**: none.
- **Exposure**: API; CLI `appset list`; UI list page.
- **Tests required**:
  - Selector, project and namespace filters; RBAC filtering; sort order.

### ASET-112 API: Create ApplicationSet (upsert, dry-run)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L47-L51, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L97-L103, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L286-L422, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L532-L564, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Controlling-Resource-Modification.md#L347-L357
- **Maturity**: GA
- **Semantics**: `POST /api/v1/applicationsets?upsert=&dryRun=` with the ApplicationSet as body. Steps: nil → `error creating ApplicationSets: ApplicationSets is nil in request`; validate (templated project rejected, unrecognized generators rejected — ASET-007/108); namespace enabled; RBAC `create`; AppProject must exist in the API server namespace (InvalidArgument `ApplicationSet references project <p> which does not exist`). `dryRun`: generate Applications server-side (ASET-114 engine) and return the input ApplicationSet with `status.resources` listing the would-be Applications (nothing persisted). Otherwise create (audit event `ResourceCreated`, "<user> created ApplicationSet"), then wait up to 2s for the informer. If it already exists: identical spec/labels/annotations/finalizers → return existing; different and `upsert=false` → InvalidArgument `existing ApplicationSet spec is different, use upsert flag to force update`; `upsert=true` → RBAC `update` (+ project-change checks) then update with labels/annotations merged, up to 10 conflict retries (`Failed to update ApplicationSets. Too many conflicts`), audit `ResourceUpdated`.
- **Edge cases**: Dry-run requires `create` permission and uses strict tokenRef mode. Errors wrapped `error validating ApplicationSets: ...` / `error checking create permissions for ApplicationSets <name> : ...`.
- **Configuration surface**: none.
- **Exposure**: API; CLI `appset create`.
- **Tests required**:
  - Create, idempotent re-create, conflict without upsert, upsert, upsert with project change permissions, dry-run output.

### ASET-113 API: Delete ApplicationSet
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L54-L58, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L105-L108, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L424-L445
- **Maturity**: GA
- **Semantics**: `DELETE /api/v1/applicationsets/{name}?appsetNamespace=`. Fetches live object (`error getting ApplicationSets: ...` if absent), RBAC `delete`, deletes with default propagation (Applications then handled per ASET-087), audit event `ResourceDeleted`. Returns empty `ApplicationSetResponse`.
- **Edge cases**: No enabled-namespace check (ASET-100).
- **Configuration surface**: none.
- **Exposure**: API; CLI `appset delete`; (UI has no delete action documented).
- **Tests required**:
  - Delete permitted/forbidden/missing; owned apps cascade.

### ASET-114 API: Generate (dry-run rendering)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L66-L90, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L369-L381, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L458-L503, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-server/commands/argocd_server.go#L345-L350
- **Maturity**: GA
- **Semantics**: `POST /api/v1/applicationsets/generate` with body `{applicationSet: {...}}` returns `{applications: [Application...]}` rendered exactly like the controller (generators, templating, templatePatch, namespace override) but without validation of projects/destinations and without writes. Checks: nil → error; enabled namespace; validation (templated project/unrecognized generators); RBAC `create`; project exists. Generators run in the API server with its own settings (`--appset-scm-root-ca-path`, `--appset-enable-scm-providers`, `--appset-allowed-scm-providers`, `--appset-enable-new-git-file-globbing`, `--appset-enable-github-api-metrics`, env equivalents shared with the controller) and always strict tokenRef mode, no SCM proxy; Git generator project lookups use the API server namespace. On failure the error includes the captured generator logs: `unable to generate Applications of ApplicationSet: <err>\n<logs>`.
- **Edge cases**: Duplicate names/invalid destinations are not reported (unlike the controller).
- **Configuration surface**: argocd-server flags above.
- **Exposure**: API; CLI `appset generate`; UI Preview.
- **Tests required**:
  - Output equals controller-generated apps for List/Git/Matrix fixtures.
  - Forbidden without `create`.
  - Unlabeled tokenRef secret fails even if controller strict mode is off.

### ASET-115 API: ResourceTree
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L60-L64, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L110-L113, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L447-L456, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L505-L530, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L926-L939
- **Maturity**: GA
- **Semantics**: `GET /api/v1/applicationsets/{name}/resource-tree?appsetNamespace=` returns `{nodes: [...]}` built purely from `status.resources`: one node per Application with group/version/kind/name, namespace = ApplicationSet namespace, health from status, and a parentRef to the ApplicationSet (`argoproj.io/v1alpha1/ApplicationSet`, name, namespace, uid); nodes sorted by full name. RBAC `get`.
- **Edge cases**: Truncated `status.resources` → truncated tree.
- **Configuration surface**: none.
- **Exposure**: API; UI resource tree.
- **Tests required**:
  - Tree mirrors status.resources including parentRefs.

### ASET-116 API: ListResourceEvents
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L115-L118, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L643-L665
- **Maturity**: GA
- **Semantics**: `GET /api/v1/applicationsets/{name}/events?appsetNamespace=` returns Kubernetes Events in the ApplicationSet namespace with `involvedObject.name/uid/namespace` equal to the ApplicationSet (controller events such as `created`/`updated`/`Deleted`, audit events). RBAC `get`.
- **Edge cases**: Events of a deleted-and-recreated ApplicationSet with the same name are excluded (uid filter).
- **Configuration surface**: none.
- **Exposure**: API; UI Events tab.
- **Tests required**:
  - Events emitted by the controller are returned; RBAC enforced.

### ASET-117 API: Watch ApplicationSets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L31-L38, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.proto#L120-L122, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L79-L168, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/pkg/apis/application/v1alpha1/applicationset_types.go#L1088-L1100
- **Maturity**: GA
- **Semantics**: Server-streaming `GET /api/v1/stream/applicationsets?name=&projects=&selector=&appSetNamespace=&resourceVersion=` emitting `{type: ADDED|MODIFIED|DELETED, applicationSet}`. Subscribes to the informer broadcaster before listing; when `name` is set, first sends `ADDED` for current matches (sorted by qualified name). Each event is filtered by project list, minimum resourceVersion, name, namespace, selector, enabled namespace and RBAC `get`.
- **Edge cases**: Without `name` no initial snapshot is sent. Invalid selector → `error parsing labels with selectors: ...`. Note the query parameter is `appSetNamespace` (capital S) unlike other RPCs.
- **Configuration surface**: none.
- **Exposure**: API; CLI (`--wait` flags); UI live updates.
- **Tests required**:
  - Watch by name receives initial ADDED then MODIFIED on status change and DELETED on delete.
  - Events for forbidden appsets not delivered.

### ASET-118 CLI: `argocd appset get`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L68-L130, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L500-L551, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_appset_get.md#L27-L30
- **Maturity**: GA
- **Semantics**: `argocd appset get APPSETNAME` (or `NAMESPACE/NAME`; a qualified name wins over `-N`). Flags: `-o|--output json|yaml|wide` (default `wide`), `--show-params` (prints Helm parameters of the template source), `-N|--appset-namespace`. Wide output prints Name (qualified), Project, Server (destination server or name), Namespace, Health Status, Source/Sources details, SyncPolicy (`Automated`, `Automated (Prune)` or `<none>`) and, when present, a conditions table `CONDITION STATUS MESSAGE LAST TRANSITION`.
- **Edge cases**: No args → help and exit 1. Unknown output → `unknown output format: <o>`.
- **Configuration surface**: CLI flags above.
- **Exposure**: CLI (uses API Get).
- **Tests required**:
  - Output golden files for wide/json/yaml; qualified name precedence over `-N`.

### ASET-119 CLI: `argocd appset list`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L325-L369, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L459-L498, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_appset_list.md#L21-L25
- **Maturity**: GA
- **Semantics**: Flags `-o|--output wide|name|json|yaml` (default `wide`), `-l|--selector`, `-p|--project` (repeatable), `-N|--appset-namespace`. Table columns `NAME PROJECT SYNCPOLICY HEALTH CONDITIONS` plus `REPO PATH TARGET` in wide mode (the default); `CONDITIONS` lists only conditions with status True; `name` prints qualified names one per line.
- **Edge cases**: Unknown output → error.
- **Configuration surface**: CLI flags.
- **Exposure**: CLI (API List).
- **Tests required**:
  - Filters forwarded to API; column layout golden test.

### ASET-120 CLI: `argocd appset create`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L132-L245, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L562-L603, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/util/applicationset.go#L14-L54, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_appset_create.md#L15-L32
- **Maturity**: GA
- **Semantics**: Reads the first argument (local path, or `http(s)://` URL) as multi-document YAML of ApplicationSets. Each must have `metadata.name` (`Error creating ApplicationSet ... does not have Name field set`); zero documents → `No ApplicationSets found while parsing the input file` exit 1. Namespace: `metadata.namespace` wins, else `-N` (prints notice). Fetches existing, calls Create with `--upsert`/`--dry-run`, prints to stderr `ApplicationSet '<name>' created|updated|unchanged[ (dry-run)]`, then prints the result (`-o json|yaml|wide`). `--wait` (ignored with `--dry-run`) watches until `ResourcesUpToDate=True`, failing if the ApplicationSet is deleted or the stream closes.
- **Edge cases**: Additional file arguments are ignored. `--wait` blocks indefinitely on persistent errors.
- **Configuration surface**: `--upsert`, `--dry-run`, `--wait`, `-o`, `-N`.
- **Exposure**: CLI (API Create, Get, Watch).
- **Tests required**:
  - Create/unchanged/updated messages; dry-run lists `status.resources`; `--wait` returns after controller reconcile.

### ASET-121 CLI: `argocd appset delete`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L371-L457, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_appset_delete.md#L15-L30
- **Maturity**: GA
- **Semantics**: `argocd appset delete NAME [NAME...]` (each may be `NAMESPACE/NAME`, else `-N`). When stdout is a terminal and `-y|--yes` is not set, prompts `Are you sure you want to delete '<name>' and all its Applications? [y/n]` (`[y/n/a]` for multiple; `a` confirms all). On confirm deletes via API and prints `applicationset '<name>' deleted`; otherwise `The command to delete '<name>' was cancelled.`. `--wait` watches until a DELETED event.
- **Edge cases**: Errors abort the remaining deletions.
- **Configuration surface**: `-y`, `--wait`, `-N`.
- **Exposure**: CLI (API Delete, Get, Watch).
- **Tests required**:
  - Prompt behavior with/without TTY and `-y`; `--wait` returns after finalizers complete.

### ASET-122 CLI: `argocd appset generate`
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/applicationset.go#L247-L323, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/commands/argocd_appset_generate.md#L15-L26
- **Maturity**: GA
- **Semantics**: Reads the first argument; it must contain exactly one ApplicationSet (`Input file must contain one ApplicationSet`, exit 1) with a name. Namespace precedence as `create`. Calls the `Generate` RPC and prints the Applications as the application table (`wide`, default) or as a YAML/JSON list with `apiVersion: argoproj.io/v1alpha1`, `kind: Application` backfilled.
- **Edge cases**: Server-side errors include generator logs.
- **Configuration surface**: `-o`, `-N`.
- **Exposure**: CLI (API Generate).
- **Tests required**:
  - YAML output round-trips as valid Application manifests; multi-document file rejected.

### ASET-123 CLI: admin export/import of ApplicationSets
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L122-L153, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/backup.go#L254-L310, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd/commands/admin/admin.go#L29
- **Maturity**: GA
- **Semantics**: `argocd admin export` includes ApplicationSets from the control-plane namespace plus namespaces matching `--applicationset-namespaces` (default: `applicationsetcontroller.namespaces` from `argocd-cmd-params-cm`); `argocd admin import` only imports (and prunes) ApplicationSets in those allowed namespaces.
- **Edge cases**: ApplicationSets in non-allowed namespaces are skipped silently. (Full export/import semantics belong to the admin area.)
- **Configuration surface**: `--applicationset-namespaces`; cmd-params `applicationsetcontroller.namespaces`.
- **Exposure**: CLI.
- **Tests required**:
  - Export/import round trip including an ApplicationSet in an allowed extra namespace.

### ASET-124 UI: ApplicationSets list page
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/app.tsx#L39-L68, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/applications-container.tsx#L8-L28, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/applications-list/application-sets-list.tsx#L1-L120, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-set-ui.md#L1-L47, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/feature-maturity.md#L28
- **Maturity**: Alpha
- **Semantics**: Top-level navigation item "ApplicationSets" → `/applicationsets`. Shows ApplicationSets across all namespaces the user can `get` (List + Watch APIs): substring search on name/namespace (shortcut `/`), filters by project, namespace, labels and health (`Healthy`, `Progressing`, `Degraded`, `Unknown`) persisted in the URL, a health pie summary, tile/table views (table: name, namespace, project, health, conditions, generated app count).
- **Edge cases**: Live updates via the watch stream; RBAC-filtered.
- **Configuration surface**: UI view preferences (local).
- **Exposure**: UI.
- **Tests required**:
  - List shows only RBAC-permitted appsets; filters reflected in URL; health filter works.

### ASET-125 UI: ApplicationSet details page, resource tree and status bar
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/applications-container.tsx#L19-L23, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/shared/services/applications-service.ts#L636-L660, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-set-ui.md#L49-L87, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Web-UI.md#L1-L40
- **Maturity**: Alpha
- **Semantics**: Routes `/applicationsets/:name` and `/applicationsets/:namespace/:name`. Resource tree (from `ResourceTree`) with the ApplicationSet as root and generated Applications as children with health/sync icons; clicking a child navigates to the Application. Status bar: health (`status.health`), conditions count with a conditions modal (type, status, message, time), last updated.
- **Edge cases**: Tree limited to `status.resources` (truncation applies).
- **Configuration surface**: none.
- **Exposure**: UI.
- **Tests required**:
  - Tree node count equals status.resources; conditions modal content.

### ASET-126 UI: slide-out panel (Summary, Manifest, Events) and Preview
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/resource-details/appset-generated-apps-diff.tsx#L12-L120, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-details/application-details.tsx#L918-L926, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-set-ui.md#L89-L160, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/applicationset/Web-UI.md#L34-L39
- **Maturity**: Alpha
- **Semantics**: Slide-out panel tabs: Summary (name, namespace, creation time, health, conditions, labels, annotations, sync policy), Manifest (read-only spec YAML), Events (`ListResourceEvents`), Preview. Preview calls `Generate` for the (optionally edited, never saved) spec and for the current spec and renders `DIFF` (added/removed/modified Applications), `LIVE APPS`, `DESIRED APPS`. Requires RBAC `create`; otherwise a permission-denied message.
- **Edge cases**: If generating the current spec fails, a warning is shown and the diff is computed against live apps only. External generators are re-evaluated on every preview.
- **Configuration surface**: none.
- **Exposure**: UI.
- **Tests required**:
  - Editing template name shows modified/added/removed entries; edits are not persisted; user without `create` gets permission-denied.

### ASET-127 UI: Application-view integration (owner badge, parent node, app-of-appsets preview, progressive step)
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-resource-tree/application-resource-tree.tsx#L81, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-resource-tree/application-resource-tree.tsx#L808-L817, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/application-status-panel/application-status-panel.tsx#L68-L147, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/ui/src/app/applications/components/resource-details/resource-details.tsx#L160, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/user-guide/application-set-ui.md#L161-L224
- **Maturity**: Alpha
- **Semantics**: Applications with an `ApplicationSet` ownerReference show an owner badge linking to the ApplicationSet; a view preference "Show parent ApplicationSet" adds the ApplicationSet as a synthetic root node (badge hidden then). The Application status panel shows the progressive-sync step/status for ApplicationSet-managed apps. For an Application that manages ApplicationSet resources (app-of-appsets), the ApplicationSet node's slide-out offers a Git-target-vs-live Preview, only when that ApplicationSet resource is OutOfSync.
- **Edge cases**: Preview compares whole Application manifests (no drill-down into child resources).
- **Configuration surface**: UI view preferences.
- **Exposure**: UI.
- **Tests required**:
  - Badge present only for owned apps; toggle renders parent node; app-of-appsets preview only when OutOfSync.

### ASET-128 ApplicationSet controller metrics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/metrics/metrics.go#L16-L135, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L145-L147, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L248-L253, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L140-L163
- **Maturity**: GA
- **Semantics**: Exposed on `--metrics-addr` (default `:8080`, `/metrics`, Service port `metrics`): `argocd_appset_info{namespace,name,resource_update_status}` gauge = 1 where `resource_update_status` is the `ResourcesUpToDate` condition's reason (or `Unknown`); `argocd_appset_owned_applications{namespace,name}` = number of entries in `status.resources`; `argocd_appset_reconcile{namespace,name}` histogram (seconds, default buckets) observed for every reconcile of an existing ApplicationSet; `argocd_appset_labels{namespace,name,label_<normalized>...}` only when `--metrics-applicationset-labels` lists label keys. Only ApplicationSets in allowed namespaces are reported. Kubernetes client metrics (`argocd_kubectl_*`) and controller-runtime metrics are also exposed; pprof at `/debug/pprof/` when profiling is enabled.
- **Edge cases**: Owned-applications count is capped by `--max-resources-status-count`.
- **Configuration surface**: `--metrics-addr`, `--metrics-applicationset-labels` (no env), `applicationsetcontroller.profile.enabled`.
- **Exposure**: Prometheus endpoint.
- **Tests required**:
  - Metric names/labels/values after reconcile; labels metric absent unless configured.

### ASET-129 GitHub API metrics
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/github_metrics.go#L16-L253, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/scm_provider.go#L297-L305, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/generators/pull_request.go#L219-L227, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/metrics.md#L166-L177
- **Maturity**: GA
- **Semantics**: When enabled, GitHub SCM Provider and Pull Request clients (token and GitHub App) wrap their transport: `argocd_github_api_requests_total{method,endpoint,status,appset_namespace,appset_name}` (status `0` when no response), `argocd_github_api_request_duration_seconds{method,endpoint,appset_namespace,appset_name}`, and gauges from `X-RateLimit-*` headers `argocd_github_api_rate_limit_remaining|limit|used{endpoint,appset_namespace,appset_name,resource}` and `argocd_github_api_rate_limit_reset_seconds` (seconds until reset).
- **Edge cases**: `endpoint` is the raw URL path (high cardinality).
- **Configuration surface**: `--enable-github-api-metrics` / `ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_GITHUB_API_METRICS` / `applicationsetcontroller.enable.github.api.metrics`; argocd-server `--appset-enable-github-api-metrics`.
- **Exposure**: Prometheus endpoint.
- **Tests required**:
  - Fake GitHub with rate-limit headers → gauges set; disabled → no series.

### ASET-130 Controller process configuration
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L92-L190, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L293-L330, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/argocd-cmd-params-cm.yaml#L330-L417, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/docs/operator-manual/server-commands/argocd-applicationset-controller.md#L14-L68
- **Maturity**: GA
- **Semantics**: Flags (env / cmd-params): `--metrics-addr` (`:8080`), `--probe-addr` (`:8081`), `--webhook-addr` (`:7000`), `--enable-leader-election` (`ARGOCD_APPLICATIONSET_CONTROLLER_ENABLE_LEADER_ELECTION` / `applicationsetcontroller.enable.leader.election`, lease ID `58ac56fa.applicationsets.argoproj.io`), `--debug` (overrides loglevel), `--logformat json|text` (default json), `--loglevel` (default info), `--cache-sync-period` (default 10h, 0 disables, max 24h; `ARGOCD_APPLICATIONSET_CONTROLLER_CACHE_SYNC_PERIOD`), Kubernetes client tuning keys (`applicationsetcontroller.k8s.client.qps|burst|max.idle.connections`, `k8s.tcp.timeout|keepalive`, `k8s.tls.handshake.timeout`, `k8s.tcp.idle.timeout`), gRPC TXT lookups toggle, standard kubeconfig flags. The control-plane namespace is the kubeconfig/in-cluster namespace. User agent `argocd-applicationset-controller/<version> (<platform>)`.
- **Edge cases**: Out-of-range numeric env values fall back to defaults. Startup panics are logged fatally.
- **Configuration surface**: as listed.
- **Exposure**: controller config.
- **Tests required**:
  - Two replicas with leader election: only one reconciles.
  - Each env/cmd-params key changes the corresponding behavior.

### ASET-131 Repo-server connectivity for the Git generator
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L219-L234, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L301, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/cmd/argocd-applicationset-controller/commands/applicationset_controller.go#L313-L316, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/services/repo_service.go#L29-L51
- **Maturity**: GA
- **Semantics**: The Git generator calls repo-server gRPC `GetGitFiles`/`GetGitDirectories` at `--argocd-repo-server` (default `argocd-repo-server:8081`) with timeout `--repo-server-timeout-seconds` (default 60). TLS: default TLS; `--repo-server-plaintext` disables it; `--repo-server-strict-tls` (deprecated) or `--repo-server-ca-cert-path` enable verification (strict mode loads `<config>/reposerver/tls/tls.crt` + `ca.crt`); mTLS via `--repo-server-client-cert-path` / `--repo-server-client-cert-key-path` (skipped when files absent). Repository credentials are resolved by the controller (ASET-026) and passed in the request.
- **Edge cases**: Unreachable repo-server → generation error with `error initializing new repo server client` / gRPC error.
- **Configuration surface**: flags above with `ARGOCD_APPLICATIONSET_CONTROLLER_REPO_SERVER*` envs and `applicationsetcontroller.repo.server.*` cmd-params keys.
- **Exposure**: controller config.
- **Tests required**:
  - Plaintext and TLS/mTLS connection modes; timeout surfaces as error condition.

### ASET-132 Kubernetes events and audit trail
- **Upstream**: https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L751-L759, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L871, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/applicationset/controllers/applicationset_controller.go#L965, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/server/applicationset/applicationset.go#L599-L607, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L41-L43, https://github.com/argoproj/argo-cd/blob/a5038b0e2bbac1715754380ae0084df8e3d5b48c/util/argo/audit_logger.go#L140-L156
- **Maturity**: GA
- **Semantics**: The controller (event source `applicationset-controller`) emits Normal events on the ApplicationSet: reason `created`/`updated` with message `created|updated Application "<name>"` (no event for unchanged), `Deleted` / `Deleted Application "<name>"`, and `Updated` for invalid-destination finalizer removal. The API server records audit events on the ApplicationSet: `ResourceCreated` (`<user> created ApplicationSet`), `ResourceUpdated` (`<user> updated ApplicationSets spec`), `ResourceDeleted` (`<user> deleted ApplicationSets`), user `Unknown user` when unauthenticated, subject to the API server's Kubernetes-event enablement list.
- **Edge cases**: Events are visible through `ListResourceEvents` (ASET-116).
- **Configuration surface**: argocd-server K8s event enablement setting.
- **Exposure**: Kubernetes events; API; UI Events tab.
- **Tests required**:
  - Event reasons/messages for create/update/delete flows.

## Unconfirmed

- Exact error text produced when an Application is already controlled by another ApplicationSet (controller-runtime `SetControllerReference` conflict); the vendored controller-runtime v0.24.1 source was not inspected. Checked: `applicationset/controllers/applicationset_controller.go` L734.
- Whether the Gitea SDK paginates `ListOrgRepos`, `ListRepoBranches` (SCM) and `ListRepoPullRequests` (PR); the code calls each once with zero-value options. Checked: `applicationset/services/scm_provider/gitea.go` L71-L95, `applicationset/services/pull_request/gitea.go` L52-L59.
- Effect of GitLab `includeArchivedRepos: true` (code sends `archived=true`; whether GitLab then returns only archived projects or both was not verifiable from source). Checked: `applicationset/services/scm_provider/gitlab.go` L102-L105.
- Which GitLab system-hook payloads are mapped to push payloads by go-playground/webhooks v6 (library not inspected). Checked: `applicationset/webhook/webhook.go` L168-L170, L196-L219.
- Health/readiness behavior of `--probe-addr` (no health/ready checks are registered and the manifests define no probes). Checked: `cmd/argocd-applicationset-controller/commands/applicationset_controller.go` L164-L176, `manifests/base/applicationset-controller/argocd-applicationset-controller-deployment.yaml`.
- Whether `status.resources[].group/version/kind` are always populated (depends on TypeMeta of cached Applications). Checked: `applicationset/status/resource_status.go` L7-L27.
- Default PR state for GitHub, Bitbucket Server, Bitbucket Cloud and Azure DevOps relies on provider API defaults (open/active); not set explicitly in code. Checked: `applicationset/services/pull_request/{github,bitbucket_server,bitbucket_cloud,azure_devops}.go`.
- Interaction of controller `--dry-run` with the cache-syncing client (whether dry-run write results are written into the informer store). Checked: `applicationset/utils/client.go` L1-L60, `cmd/argocd-applicationset-controller/commands/applicationset_controller.go` L173-L175.
- The exact Sprig v3.3.0 `GenericFuncMap` function list (module source not inspected); only the removed functions and Argo CD additions are confirmed. Checked: `applicationset/utils/utils.go` L31-L51.
- UI behaviors (filters sidebar, health pie chart, tile/table toggle, preview diff categorisation) are confirmed from `docs/user-guide/application-set-ui.md` and partially from `ui/src/app/app.tsx`, `ui/src/app/applications/components/applications-list/application-sets-list.tsx`, `ui/src/app/applications/components/resource-details/appset-generated-apps-diff.tsx`; the UI components were not traced exhaustively.
- AWS CodeCommit maturity: marked Alpha only by the docs heading; `docs/operator-manual/feature-maturity.md` does not list it.

## Doc/implementation differences

- `docs/operator-manual/applicationset/Controlling-Resource-Modification.md` L13 says to add `--dryrun true`; the actual flag is `--dry-run` (`cmd/argocd-applicationset-controller/commands/applicationset_controller.go` L309).
- `docs/operator-manual/applicationset/Appset-Any-Namespace.md` L233-L243 states `argocd appset get foo/bar` addresses ApplicationSet `foo` in namespace `bar`; the CLI parses `<namespace>/<name>` (`util/argo/argo.go` L1230-L1247), i.e. namespace `foo`, name `bar`.
- `Appset-Any-Namespace.md` L252 refers to an `appNamespace` query parameter; the API uses `appsetNamespace` (and `appSetNamespace` for Watch) (`server/applicationset/applicationset.proto` L18-L19, L35).
- `Appset-Any-Namespace.md` L120 shows `--application-namespaces=*` for the ApplicationSet controller; the controller flag is `--applicationset-namespaces`.
- `Appset-Any-Namespace.md` L88-L89 says to set `ARGOCD_APPLICATIONSET_CONTROLLER_TOKENREF_STRICT_MODE` on the `argocd-application-controller` deployment; it is read by `argocd-applicationset-controller` (`cmd/argocd-applicationset-controller/commands/applicationset_controller.go` L310).
- `docs/operator-manual/applicationset/Template.md` L186-L188 says `templatePatch` only works with `goTemplate: true`; the code applies it in both modes, using fasttemplate substitution when Go templates are off (`applicationset/controllers/template/template.go` L50-L64, L82-L89).
- `docs/operator-manual/applicationset/Generators-SCM-Provider.md` L191 says Gitea does not support label filtering; the code loads Gitea repository labels and `labelMatch` works (`applicationset/services/scm_provider/gitea.go` L109-L117); the same doc's L478 also lists Gitea labels.
- `docs/operator-manual/applicationset/Generators-Pull-Request.md` L368 says only GitHub and GitLab support `labels`; Gitea and Azure DevOps also filter by labels (`applicationset/services/pull_request/gitea.go` L69, `azure_devops.go` L104-L107), and the Gitea section (L121-L157) omits the field.
- `Generators-Pull-Request.md` L3 lists only GitHub, Gitea and Bitbucket Server; GitLab, Bitbucket Cloud and Azure DevOps are also implemented (and documented later on the same page).
- `Generators-Pull-Request.md` L477 lists the GitHub action `synchronized`; the code matches `synchronize` (`applicationset/webhook/webhook.go` L290-L297).
- `docs/operator-manual/applicationset.yaml` L181-L182 shows `pullRequest.awsCodeCommit`; no such field exists (`pkg/apis/application/v1alpha1/applicationset_types.go` L614-L634).
- `docs/operator-manual/applicationset/Progressive-Syncs.md` L101 says `deletionOrder` controls deletion "when they are removed from the ApplicationSet"; the code applies Reverse ordering only when the ApplicationSet itself is deleted, while apps dropped from generator output are deleted all at once (`applicationset/controllers/applicationset_controller.go` L149-L179, L351-L366).
- `Progressive-Syncs.md` L65 says warnings are logged for Applications with automated sync; the code silently sets `automated.enabled=false` (`applicationset/progressivesync/progressive_sync.go` L745-L772).
- `Progressive-Syncs.md` L4 says "Beta Feature (Since v3.3.0)" while `docs/operator-manual/feature-maturity.md` L20 says introduced in v2.6.0 (Beta).
- `docs/operator-manual/applicationset/Generators-Matrix.md` L14 and L259 say a Matrix fails when children produce identical keys with different values; that only holds with fasttemplate — with `goTemplate: true` the first child's values win (`applicationset/generators/matrix.go` L59-L75).
- `docs/operator-manual/argocd-cmd-params-cm.yaml` L373-L374 describes `applicationsetcontroller.requeue.after` as the controller default requeue time; it only changes the default for Git and ClusterDecisionResource generators — SCM Provider, Pull Request and Plugin keep a hard-coded 30-minute default (`applicationset/generators/interface.go` L38-L41, `scm_provider.go` L93-L101, `pull_request.go` L40-L48, `plugin.go` L41-L49).
- `docs/operator-manual/applicationset/Generators-Plugin.md` L121-L122 says the token Secret must carry `app.kubernetes.io/part-of: argocd`; the plugin generator reads any Secret by name without a label check (`applicationset/generators/plugin.go` L147-L175).
- `Generators-SCM-Provider.md` L244 and `Generators-Pull-Request.md` L213 say basic auth is "the only auth supported currently" for Bitbucket Server; bearer tokens are supported (`applicationset/generators/scm_provider.go` L214-L229, `pull_request.go` L176-L189).
- `docs/operator-manual/applicationset/Generators-Git.md` L466 says webhooks are supported from GitHub and GitLab; Azure DevOps push events are handled too (`applicationset/webhook/webhook.go` L170-L171, L211-L215).
- The `argocd appset create` and `generate` usage examples accept `<filename or URL> (<filename or URL>...)` (`cmd/argocd/commands/applicationset.go` L39, L144, L256; `docs/user-guide/commands/argocd_appset_create.md` L15), but only the first argument is read (`applicationset.go` L160, L269).
- `argocd-cmd-params-cm.yaml` L362-L363 describes `applicationsetcontroller.scm.root.ca.path` as being for the GitLab SCM/PR generators; Bitbucket Server uses it too (`applicationset/services/util.go` L14-L16).
- The help text for the controller flag `--metrics-applicationset-labels` names the metric `argocd_applicationset_labels`; the metric is actually `argocd_appset_labels` (`cmd/argocd-applicationset-controller/commands/applicationset_controller.go` L324, `applicationset/metrics/metrics.go` L77-L82).
- `docs/operator-manual/metrics.md` L145 says `resource_update_status` reflects the `ResourcesUpToDate` property; the code exports that condition's reason, or `Unknown` (`applicationset/metrics/metrics.go` L121-L133).
- `Generators-SCM-Provider.md` L329 marks AWS CodeCommit as Alpha; `feature-maturity.md` does not list it.
- `docs/operator-manual/applicationset/Generators-Cluster.md` L209-L215 leaves `project` out of the parameters that `values` can interpolate; `project` is available (`applicationset/generators/cluster.go` L135-L140, L81).
- `docs/operator-manual/applicationset/Generators-Cluster-Decision-Resource.md` does not document `values`. The field exists and, unlike other generators, is not templated (`pkg/apis/application/v1alpha1/applicationset_types.go` L408-L409, `applicationset/generators/duck_type.go` L170-L172).
- `continueOnRepoNotFoundError` appears only in `docs/operator-manual/applicationset.yaml` L133-L144, not in `Generators-Pull-Request.md`.
