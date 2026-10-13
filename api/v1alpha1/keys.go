package v1alpha1

// Annotation, label and finalizer keys.
//
// Every key Argo CD v3.5.5 reads or writes (docs/user-guide/annotations-and-labels.md, common/common.go,
// gitops-engine sync/common, the notifications engine and the ApplicationSet controller) is mapped to a
// Patrol key with a mechanical rule, so migration tooling can rewrite manifests:
//
//	argocd.argoproj.io/<key>            → cd.patrol.example/<key>
//	<sub>.argocd.argoproj.io/<key>      → <sub>.cd.patrol.example/<key>   (link., notice., pref.)
//	notifications.argoproj.io/<key>     → notifications.cd.patrol.example/<key>
//	notified.notifications.argoproj.io  → notified.notifications.cd.patrol.example
//	<name>.argocd.argoproj.io[/<v>]     → <name>.cd.patrol.example[/<v>]  (finalizers with variants)
//
// One key does not follow these rules:
//
//	managed-by: argocd.argoproj.io      → cd.patrol.example/managed-by: patrol-cd
//
// Finalizers that have variants keep the "<name>.<group>/<variant>" shape because Kubernetes finalizer
// names may contain only one "/". Finalizers without variants use "cd.patrol.example/<name>" (ADR-0002).
//
// Argo CD keys intentionally not mapped:
//   - applications.argoproj.io/app-name: legacy (v0.10) instance label, superseded upstream.
//   - <trigger>.recipients.argocd-notifications.argoproj.io: legacy argocd-notifications subscription.
//   - gitops-agent.argoproj.io/gc-mark: gitops-agent only, outside the baseline distribution.
const (
	// Prefix is the common prefix of Patrol CD annotation and label keys: "cd.patrol.example/".
	Prefix = GroupName + "/"
)

// Application annotations.
const (
	// AnnotationKeyRefresh ("cd.patrol.example/refresh") requests a refresh. On an Application the value
	// is RefreshTypeNormal or RefreshTypeHard and the controller removes the annotation once the refresh
	// completed. On a Cluster the value is an RFC 3339 timestamp requesting invalidation of the
	// controller's live-state cache for that cluster (Argo CD's Cluster.refreshRequestedAt, which Argo
	// CD stores the same way in the cluster Secret); the controller removes it once the cache was
	// rebuilt. It is deliberately not validated at admission so webhooks and clients can set it with a
	// plain patch.
	AnnotationKeyRefresh = Prefix + "refresh"
	// AnnotationKeyHydrate ("cd.patrol.example/hydrate") requests source hydration. Values are
	// HydrateTypeNormal or HydrateTypeHard. Removed by the controller after hydration.
	AnnotationKeyHydrate = Prefix + "hydrate"
	// AnnotationKeyManifestGeneratePaths ("cd.patrol.example/manifest-generate-paths") is a
	// semicolon-separated list of repository paths (absolute, or relative to the source path) whose
	// changes affect manifest generation; webhooks skip refreshes for commits outside them.
	AnnotationKeyManifestGeneratePaths = Prefix + "manifest-generate-paths"
	// AnnotationKeyManagedByURL ("cd.patrol.example/managed-by-url") is the http(s) URL of the Patrol
	// CD instance that manages the Application, used for UI deep links across instances.
	AnnotationKeyManagedByURL = Prefix + "managed-by-url"
	// AnnotationKeySkipReconcile ("cd.patrol.example/skip-reconcile") set to a value that parses as true
	// on an Application skips its reconciliation; on a Cluster it skips every Application targeting it.
	AnnotationKeySkipReconcile = Prefix + "skip-reconcile"
	// AnnotationKeyDeletionApproved ("cd.patrol.example/deletion-approved") holds an RFC 3339 timestamp
	// approving deletion/pruning of resources that use Delete=confirm or Prune=confirm.
	AnnotationKeyDeletionApproved = Prefix + "deletion-approved"
	// AnnotationKeyClientSideApplyMigrationManager ("cd.patrol.example/client-side-apply-migration-manager")
	// overrides the field manager migrated by the ClientSideApplyMigration sync option.
	AnnotationKeyClientSideApplyMigrationManager = Prefix + "client-side-apply-migration-manager"
	// AnnotationKeyApplicationSetProgressiveStatus ("cd.patrol.example/application-set-progressive-status")
	// is written by the ApplicationSet controller on generated Applications while a RollingSync strategy
	// is active. The value is a ProgressiveSyncStatusCode mirrored from the parent ApplicationSet status,
	// so Application consumers (UI, notifications) need not read the ApplicationSet.
	AnnotationKeyApplicationSetProgressiveStatus = Prefix + "application-set-progressive-status"
)

// Refresh and hydrate annotation values.
const (
	// RefreshTypeNormal re-renders from the repository using cached results where inputs are unchanged.
	RefreshTypeNormal RefreshType = "normal"
	// RefreshTypeHard invalidates the render cache and the target cluster state cache before refreshing.
	RefreshTypeHard RefreshType = "hard"

	// HydrateTypeNormal re-evaluates whether the dry source requires hydration.
	HydrateTypeNormal HydrateType = "normal"
	// HydrateTypeHard forces hydration of the dry source.
	HydrateTypeHard HydrateType = "hard"
)

// RefreshType is a value of the refresh annotation.
type RefreshType string

// HydrateType is a value of the hydrate annotation.
type HydrateType string

// Application UI annotations.
const (
	// AnnotationKeyLinkPrefix ("link.cd.patrol.example/") followed by a link name adds an external link
	// to the UI for the annotated Application or resource. The value is a URL or "title|URL".
	AnnotationKeyLinkPrefix = "link." + GroupName + "/"

	// AnnotationKeyNoticeContent ("notice.cd.patrol.example/content") is plain text (truncated at 500
	// characters) shown as a per-Application notice banner.
	AnnotationKeyNoticeContent = "notice." + GroupName + "/content"
	// AnnotationKeyNoticeSeverity ("notice.cd.patrol.example/severity"): info (default), warning, critical.
	AnnotationKeyNoticeSeverity = "notice." + GroupName + "/severity"
	// AnnotationKeyNoticeURL ("notice.cd.patrol.example/url") links the notice banner and icon.
	AnnotationKeyNoticeURL = "notice." + GroupName + "/url"
	// AnnotationKeyNoticePermanent ("notice.cd.patrol.example/permanent") set to "true" prevents dismissal.
	AnnotationKeyNoticePermanent = "notice." + GroupName + "/permanent"
	// AnnotationKeyNoticeIcon ("notice.cd.patrol.example/icon") overrides the severity icon (allow-listed).
	AnnotationKeyNoticeIcon = "notice." + GroupName + "/icon"
	// AnnotationKeyNoticeScope ("notice.cd.patrol.example/scope"): banner, icon or both (default).
	AnnotationKeyNoticeScope = "notice." + GroupName + "/scope"

	// AnnotationKeyPrefDefaultView ("pref.cd.patrol.example/default-view") sets the default UI view.
	AnnotationKeyPrefDefaultView = "pref." + GroupName + "/default-view"
	// AnnotationKeyPrefDefaultPodSort ("pref.cd.patrol.example/default-pod-sort") sets the default pod grouping.
	AnnotationKeyPrefDefaultPodSort = "pref." + GroupName + "/default-pod-sort"
)

// Annotations on managed (rendered) resources.
const (
	// AnnotationKeyTrackingID ("cd.patrol.example/tracking-id") records which Application manages a
	// resource when TrackingMethodAnnotation or TrackingMethodAnnotationAndLabel is used.
	AnnotationKeyTrackingID = Prefix + "tracking-id"
	// AnnotationKeyInstallationID ("cd.patrol.example/installation-id") distinguishes Patrol CD
	// installations that share destination clusters.
	AnnotationKeyInstallationID = Prefix + "installation-id"
	// AnnotationKeySyncWave ("cd.patrol.example/sync-wave") is the integer wave of a resource.
	AnnotationKeySyncWave = Prefix + "sync-wave"
	// AnnotationKeyHook ("cd.patrol.example/hook") is a comma-separated list of HookType values.
	AnnotationKeyHook = Prefix + "hook"
	// AnnotationKeyHookDeletePolicy ("cd.patrol.example/hook-delete-policy") is a comma-separated list of
	// HookDeletePolicy values.
	AnnotationKeyHookDeletePolicy = Prefix + "hook-delete-policy"
	// AnnotationKeySyncOptions ("cd.patrol.example/sync-options") is a comma-separated list of per-resource
	// sync options (see SyncOption* constants).
	AnnotationKeySyncOptions = Prefix + "sync-options"
	// AnnotationKeyCompareOptions ("cd.patrol.example/compare-options") is a comma-separated list of
	// per-resource compare options (see CompareOption* constants).
	AnnotationKeyCompareOptions = Prefix + "compare-options"
	// AnnotationKeyIgnoreHealthCheck ("cd.patrol.example/ignore-healthcheck") set to "true" on an
	// Application's direct child excludes it from the Application health aggregation.
	AnnotationKeyIgnoreHealthCheck = Prefix + "ignore-healthcheck"
	// AnnotationKeyIgnoreDefaultLinks ("cd.patrol.example/ignore-default-links") set to "true" stops
	// automatic external URLs being derived from the resource (e.g. Ingress hosts).
	AnnotationKeyIgnoreDefaultLinks = Prefix + "ignore-default-links"
	// AnnotationKeyIgnoreResourceUpdates ("cd.patrol.example/ignore-resource-updates") set to "true"
	// makes the controller ignore updates of the resource that only touch ignored fields.
	AnnotationKeyIgnoreResourceUpdates = Prefix + "ignore-resource-updates"
	// AnnotationKeyManagedBy ("cd.patrol.example/managed-by") marks objects (typically credential
	// Secrets) created by Patrol CD itself; the value is AnnotationValueManagedByPatrol.
	AnnotationKeyManagedBy = Prefix + "managed-by"
	// AnnotationValueManagedByPatrol is the value of AnnotationKeyManagedBy.
	AnnotationValueManagedByPatrol = "patrol-cd"
)

// TrackingMethod selects how managed resources are associated with their Application
// (application.resourceTrackingMethod setting).
type TrackingMethod string

const (
	// TrackingMethodAnnotation tracks resources with AnnotationKeyTrackingID (default).
	TrackingMethodAnnotation TrackingMethod = "annotation"
	// TrackingMethodLabel tracks resources with the instance label (LabelKeyAppInstanceDefault unless
	// application.instanceLabelKey is set).
	TrackingMethodLabel TrackingMethod = "label"
	// TrackingMethodAnnotationAndLabel tracks with AnnotationKeyTrackingID and also writes the instance
	// label (truncated to 63 characters) for selection by other tools.
	TrackingMethodAnnotationAndLabel TrackingMethod = "annotation+label"
)

// ApplicationSet annotations.
const (
	// AnnotationKeyApplicationSetRefresh ("cd.patrol.example/application-set-refresh") set to "true"
	// requests regeneration of an ApplicationSet (set by webhooks; removed after reconciliation).
	AnnotationKeyApplicationSetRefresh = Prefix + "application-set-refresh"
)

// Notification annotations.
const (
	// AnnotationKeyNotificationSubscribePrefix ("notifications.cd.patrol.example/subscribe.") starts a
	// subscription key; the value lists recipients separated by semicolons.
	// "subscribe.<trigger>.<service>" subscribes to one trigger; "subscribe.<service>" (no trigger
	// segment) subscribes to the service's default triggers.
	AnnotationKeyNotificationSubscribePrefix = "notifications." + GroupName + "/subscribe."
	// AnnotationKeyNotificationSubscriptions ("notifications.cd.patrol.example/subscriptions") holds a
	// YAML list of subscriptions, each with "trigger" (list of trigger names) and "destinations" (list
	// of {service, recipients}), for subscribing several triggers and services in one annotation.
	AnnotationKeyNotificationSubscriptions = "notifications." + GroupName + "/subscriptions"
	// AnnotationKeyNotified ("notified.notifications.cd.patrol.example") records notification delivery
	// state (trigger, condition hash, recipient) so deliveries are deduplicated across restarts.
	AnnotationKeyNotified = "notified.notifications." + GroupName
)

// Labels.
const (
	// LabelKeyAppInstanceDefault ("app.kubernetes.io/instance") is the default label used for
	// label-based resource tracking; configurable through application.instanceLabelKey.
	LabelKeyAppInstanceDefault = "app.kubernetes.io/instance"
	// LabelKeyInstance ("cd.patrol.example/instance") is the recommended instance label to avoid
	// conflicts with other tools that write app.kubernetes.io/instance.
	LabelKeyInstance = Prefix + "instance"
	// LabelKeyAppName ("app.kubernetes.io/name") identifies Patrol CD's own workloads.
	LabelKeyAppName = "app.kubernetes.io/name"
	// LabelKeyComponent ("app.kubernetes.io/component") identifies the Patrol CD component.
	LabelKeyComponent = "app.kubernetes.io/component"
	// LabelKeyPartOf ("app.kubernetes.io/part-of") is set to LabelValuePartOf on Patrol CD workloads.
	LabelKeyPartOf = "app.kubernetes.io/part-of"
	// LabelValuePartOf is the value of LabelKeyPartOf for Patrol CD workloads.
	LabelValuePartOf = "patrol-cd"

	// LabelKeySecretType ("cd.patrol.example/secret-type") classifies Secrets referenced by Patrol kinds,
	// so components can watch only the Secrets they need. Values are the LabelValueSecretType* constants.
	LabelKeySecretType = Prefix + "secret-type"
	// LabelValueSecretTypeCluster labels Secrets referenced by Cluster.spec.config.credentialsRef.
	LabelValueSecretTypeCluster = "cluster"
	// LabelValueSecretTypeRepository labels Secrets referenced by read-purpose Repository objects.
	LabelValueSecretTypeRepository = "repository"
	// LabelValueSecretTypeRepoCreds labels Secrets referenced by read-purpose RepositoryCredential objects.
	LabelValueSecretTypeRepoCreds = "repo-creds"
	// LabelValueSecretTypeRepositoryWrite labels Secrets referenced by write-purpose Repository objects.
	LabelValueSecretTypeRepositoryWrite = "repository-write"
	// LabelValueSecretTypeRepoCredsWrite labels Secrets referenced by write-purpose RepositoryCredential objects.
	LabelValueSecretTypeRepoCredsWrite = "repo-write-creds"
	// LabelValueSecretTypeSCMCreds labels Secrets referenced by SCMProvider objects and generators.
	LabelValueSecretTypeSCMCreds = "scm-creds"

	// LabelKeyAutoLabelClusterInfo ("cd.patrol.example/auto-label-cluster-info") set to "true" on a
	// Cluster makes the controller maintain LabelKeyClusterKubernetesVersion on it.
	LabelKeyAutoLabelClusterInfo = Prefix + "auto-label-cluster-info"
	// LabelKeyClusterKubernetesVersion ("cd.patrol.example/kubernetes-version") holds the cluster's
	// Kubernetes version (maintained when LabelKeyAutoLabelClusterInfo is "true").
	LabelKeyClusterKubernetesVersion = Prefix + "kubernetes-version"

	// LabelKeyOperationApplication ("cd.patrol.example/application") is set on Operations to the name
	// of their Application, so operations of one Application can be listed by label selector. Labels are
	// client-writable and admission cannot tie this one to spec.application: patrol-server sets it, the
	// controller corrects it, and decisions that must be correct (one non-terminal Operation per
	// Application, retention) are keyed on spec.application, never on this label.
	LabelKeyOperationApplication = Prefix + "application"
	// LabelKeyOperationPhase ("cd.patrol.example/operation-phase") mirrors Operation.status.phase, so
	// non-terminal operations can be found by label selector.
	LabelKeyOperationPhase = Prefix + "operation-phase"
)

// Finalizers.
const (
	// FinalizerResources ("resources-finalizer.cd.patrol.example") on an Application cascades deletion
	// to its managed resources with foreground propagation.
	FinalizerResources = "resources-finalizer." + GroupName
	// FinalizerResourcesForeground ("resources-finalizer.cd.patrol.example/foreground") cascades with
	// foreground propagation.
	FinalizerResourcesForeground = FinalizerResources + "/foreground"
	// FinalizerResourcesBackground ("resources-finalizer.cd.patrol.example/background") cascades with
	// background propagation.
	FinalizerResourcesBackground = FinalizerResources + "/background"
	// FinalizerPreDelete ("pre-delete-finalizer.cd.patrol.example") holds Application deletion until
	// PreDelete hooks completed.
	FinalizerPreDelete = "pre-delete-finalizer." + GroupName
	// FinalizerPreDeleteCleanup ("pre-delete-finalizer.cd.patrol.example/cleanup") holds deletion until
	// PreDelete hook objects have been cleaned up.
	FinalizerPreDeleteCleanup = FinalizerPreDelete + "/cleanup"
	// FinalizerPostDelete ("post-delete-finalizer.cd.patrol.example") holds Application deletion until
	// PostDelete hooks completed.
	FinalizerPostDelete = "post-delete-finalizer." + GroupName
	// FinalizerPostDeleteCleanup ("post-delete-finalizer.cd.patrol.example/cleanup") holds deletion
	// until PostDelete hook objects have been cleaned up.
	FinalizerPostDeleteCleanup = FinalizerPostDelete + "/cleanup"
	// FinalizerHook ("cd.patrol.example/hook-finalizer") is placed on hook objects created during a
	// sync so their completion is observed before they can disappear.
	FinalizerHook = Prefix + "hook-finalizer"
	// FinalizerApplicationSet ("cd.patrol.example/applicationset-finalizer") on an ApplicationSet holds
	// its deletion until generated Applications were deleted in the configured deletion order (or
	// released when preserveResourcesOnDeletion is true).
	FinalizerApplicationSet = Prefix + "applicationset-finalizer"
)

// Secret data keys read from Secrets referenced by Repository.spec.credentialsRef and
// RepositoryCredential.spec.credentialsRef. Only the keys relevant to the configured auth method are read.
const (
	RepositorySecretKeyUsername                          = "username"
	RepositorySecretKeyPassword                          = "password"
	RepositorySecretKeyBearerToken                       = "bearerToken"
	RepositorySecretKeySSHPrivateKey                     = "sshPrivateKey"
	RepositorySecretKeyTLSClientCertData                 = "tlsClientCertData"
	RepositorySecretKeyTLSClientCertKey                  = "tlsClientCertKey"
	RepositorySecretKeyGitHubAppPrivateKey               = "githubAppPrivateKey"
	RepositorySecretKeyGCPServiceAccountKey              = "gcpServiceAccountKey"
	RepositorySecretKeyAzureServicePrincipalClientSecret = "azureServicePrincipalClientSecret"
)

// Secret data keys read from Secrets referenced by Cluster.spec.config.credentialsRef.
const (
	ClusterSecretKeyBearerToken = "bearerToken"
	ClusterSecretKeyUsername    = "username"
	ClusterSecretKeyPassword    = "password"
	ClusterSecretKeyCAData      = "caData"
	ClusterSecretKeyCertData    = "certData"
	ClusterSecretKeyKeyData     = "keyData"
)

// Secret data keys read from Secrets referenced by SCMProvider.spec.auth.basicAuthSecretRef.
const (
	SCMProviderSecretKeyUsername = "username"
	SCMProviderSecretKeyPassword = "password"
)
