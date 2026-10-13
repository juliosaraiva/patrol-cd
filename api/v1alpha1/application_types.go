package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Application deploys the manifests rendered from one or more sources to a destination cluster and
// namespace, under the policy of an AppProject.
//
// Unlike Argo CD there is no top-level `operation` field: sync and rollback requests are Operation
// objects owned by the Application, and status.operationState summarizes the most recent one.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=applications,singular=application,scope=Namespaced,shortName=papp;papps,categories=patrol
// +kubebuilder:printcolumn:name="Sync",type=string,JSONPath=`.status.sync.status`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health.status`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.sync.shortRevision`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired state of the Application.
	// +kubebuilder:validation:XValidation:rule="(has(self.source) ? 1 : 0) + (has(self.sources) && size(self.sources) > 0 ? 1 : 0) + (has(self.sourceHydrator) ? 1 : 0) == 1",message="exactly one of spec.source, spec.sources or spec.sourceHydrator must be set"
	// +kubebuilder:validation:XValidation:rule="(has(self.destination.server) && self.destination.server != '') != (has(self.destination.name) && self.destination.name != '')",message="exactly one of spec.destination.server or spec.destination.name must be set"
	Spec ApplicationSpec `json:"spec"`
	// Status is the observed state of the Application, written by the controller.
	// +optional
	Status ApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationList is a list of Applications.
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	register(&Application{}, &ApplicationList{})
}

// ApplicationSpec is the desired state of an Application. The same type is used in ApplicationSet
// templates, which is why the source/destination exclusivity rules are attached to Application.spec
// and not to this type.
type ApplicationSpec struct {
	// Source is the single source of the Application. Mutually exclusive with Sources and
	// SourceHydrator.
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// Destination is the target cluster and namespace.
	Destination ApplicationDestination `json:"destination"`
	// Project is the AppProject the Application belongs to. The empty string means "default".
	Project string `json:"project"`
	// SyncPolicy controls when and how syncs are performed.
	// +optional
	SyncPolicy *SyncPolicy `json:"syncPolicy,omitempty"`
	// IgnoreDifferences lists resource fields ignored during comparison.
	// +optional
	IgnoreDifferences IgnoreDifferences `json:"ignoreDifferences,omitempty"`
	// Info holds informational name/value pairs (URLs, contacts, notes) shown in the UI.
	// +optional
	Info []Info `json:"info,omitempty"`
	// RevisionHistoryLimit is the number of entries kept in status.history and the number of terminal
	// Operations retained for the Application. Defaults to 10 when unset.
	// +optional
	// +kubebuilder:validation:Minimum=0
	RevisionHistoryLimit *int64 `json:"revisionHistoryLimit,omitempty"`
	// Sources are the sources of a multi-source Application. Mutually exclusive with Source and
	// SourceHydrator.
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
	// SourceHydrator renders a dry source and commits the hydrated manifests to git before syncing
	// them. Mutually exclusive with Source and Sources.
	// +optional
	SourceHydrator *SourceHydrator `json:"sourceHydrator,omitempty"`
}

// ApplicationDestination is a target cluster and namespace. Exactly one of Server or Name identifies
// the cluster.
type ApplicationDestination struct {
	// Server is the URL of the target cluster's API server.
	// +optional
	Server string `json:"server,omitempty"`
	// Namespace is the target namespace for namespaced resources without a namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Name is the name of the target Cluster.
	// +optional
	Name string `json:"name,omitempty"`
}

// IgnoreDifferences is a list of ignore rules.
type IgnoreDifferences []ResourceIgnoreDifferences

// ResourceIgnoreDifferences selects resources and the fields of them ignored during comparison.
type ResourceIgnoreDifferences struct {
	// Group of the resources ("" for the core group).
	// +optional
	Group string `json:"group,omitempty"`
	// Kind of the resources.
	Kind string `json:"kind"`
	// Name of the resource; all resources of the kind if empty.
	// +optional
	Name string `json:"name,omitempty"`
	// Namespace of the resource; all namespaces if empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// JSONPointers (RFC 6901) of ignored fields.
	// +optional
	JSONPointers []string `json:"jsonPointers,omitempty"`
	// JQPathExpressions of ignored fields.
	// +optional
	JQPathExpressions []string `json:"jqPathExpressions,omitempty"`
	// ManagedFieldsManagers are field managers whose changes are ignored.
	// +optional
	ManagedFieldsManagers []string `json:"managedFieldsManagers,omitempty"`
}

// SyncPolicy controls when and how syncs are performed.
type SyncPolicy struct {
	// Automated enables automated sync.
	// +optional
	Automated *SyncPolicyAutomated `json:"automated,omitempty"`
	// SyncOptions are Application-wide sync options ("Key=Value").
	// +optional
	SyncOptions SyncOptions `json:"syncOptions,omitempty"`
	// Retry controls retries of failed syncs.
	// +optional
	Retry *RetryStrategy `json:"retry,omitempty"`
	// ManagedNamespaceMetadata is applied to the destination namespace when CreateNamespace=true.
	// +optional
	ManagedNamespaceMetadata *ManagedNamespaceMetadata `json:"managedNamespaceMetadata,omitempty"`
}

// SyncPolicyAutomated configures automated sync.
type SyncPolicyAutomated struct {
	// Prune deletes resources that are no longer desired during automated sync (default false).
	// +optional
	Prune *bool `json:"prune,omitempty"`
	// SelfHeal re-syncs when live state drifts from desired state (default false).
	// +optional
	SelfHeal *bool `json:"selfHeal,omitempty"`
	// AllowEmpty allows automated sync to prune all resources (default false).
	// +optional
	AllowEmpty *bool `json:"allowEmpty,omitempty"`
	// Enabled turns automated sync on or off while keeping its settings (default true).
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// RetryStrategy controls retries of failed syncs.
type RetryStrategy struct {
	// Limit is the maximum number of retries; 0 disables retries and a negative value retries forever.
	// +optional
	Limit int64 `json:"limit,omitempty"`
	// Backoff controls the delay between retries.
	// +optional
	Backoff *Backoff `json:"backoff,omitempty"`
	// Refresh retries with the latest revision instead of the originally resolved one.
	// +optional
	Refresh bool `json:"refresh,omitempty"`
}

// Backoff is an exponential backoff: duration * factor^attempt, capped at maxDuration.
type Backoff struct {
	// Duration is the base delay, in seconds or as a Go duration ("5s", "2m"). Default "5s".
	// +optional
	Duration string `json:"duration,omitempty"`
	// Factor multiplies the delay after each failed attempt. Default 2.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Factor *int64 `json:"factor,omitempty"`
	// MaxDuration caps the delay, in seconds or as a Go duration. Default "3m".
	// +optional
	MaxDuration string `json:"maxDuration,omitempty"`
}

// ManagedNamespaceMetadata is metadata applied to a namespace created by the CreateNamespace option.
type ManagedNamespaceMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SourceHydrator configures source hydration: a dry source is rendered and committed to a branch,
// and the Application syncs from that branch.
type SourceHydrator struct {
	// DrySource is where the un-rendered manifests live.
	DrySource DrySource `json:"drySource"`
	// SyncSource is where hydrated manifests are committed (unless HydrateTo is set) and synced from.
	SyncSource SyncSource `json:"syncSource"`
	// HydrateTo is an optional staging branch; an external process moves manifests to SyncSource.
	// +optional
	HydrateTo *HydrateTo `json:"hydrateTo,omitempty"`
}

// DrySource is the location and rendering options of the dry source.
type DrySource struct {
	// RepoURL is the URL of the git repository holding the dry manifests.
	RepoURL string `json:"repoURL"`
	// TargetRevision is the revision of the dry source to hydrate.
	TargetRevision string `json:"targetRevision"`
	// Path is the directory of the dry manifests within the repository.
	Path string `json:"path"`
	// +optional
	Helm *ApplicationSourceHelm `json:"helm,omitempty"`
	// +optional
	Kustomize *ApplicationSourceKustomize `json:"kustomize,omitempty"`
	// +optional
	Directory *ApplicationSourceDirectory `json:"directory,omitempty"`
	// +optional
	Plugin *ApplicationSourcePlugin `json:"plugin,omitempty"`
}

// SyncSource is where hydrated manifests are synced from.
type SyncSource struct {
	// TargetBranch is the branch hydrated manifests are synced from (and committed to, unless
	// HydrateTo is set).
	TargetBranch string `json:"targetBranch"`
	// Path is the directory of hydrated manifests. It must not be the repository root.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^.{2,}|[^./]$`
	Path string `json:"path"`
	// RepoURL is the repository of hydrated manifests. Defaults to DrySource.RepoURL.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`
}

// HydrateTo is a staging branch for hydrated manifests.
type HydrateTo struct {
	// TargetBranch is the branch hydrated manifests are committed to.
	TargetBranch string `json:"targetBranch"`
}

// ApplicationStatus is the observed state of an Application.
type ApplicationStatus struct {
	// Resources lists the resources managed by the Application (bounded by the controller).
	// +optional
	Resources []ResourceStatus `json:"resources,omitempty"`
	// Sync is the result of the latest comparison.
	// +optional
	Sync SyncStatus `json:"sync,omitempty"`
	// Health is the aggregated health of the managed resources.
	// +optional
	Health AppHealthStatus `json:"health,omitempty"`
	// History holds the most recent successful syncs, oldest first (bounded by revisionHistoryLimit).
	// +optional
	History RevisionHistories `json:"history,omitempty"`
	// Conditions are the current Application conditions (see conditions.go for types and reasons).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// ReconciledAt is when the Application was last compared against the latest revision.
	// +optional
	ReconciledAt *metav1.Time `json:"reconciledAt,omitempty"`
	// ObservedGeneration is the metadata.generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// OperationState summarizes the most recent Operation of the Application. Per-resource results
	// live in the Operation's status.
	// +optional
	OperationState *ApplicationOperationState `json:"operationState,omitempty"`
	// SourceType is the detected rendering tool of a single-source Application.
	// +optional
	SourceType ApplicationSourceType `json:"sourceType,omitempty"`
	// SourceTypes are the detected rendering tools of each source of a multi-source Application.
	// +optional
	SourceTypes []ApplicationSourceType `json:"sourceTypes,omitempty"`
	// Summary lists external URLs and images used by the managed resources.
	// +optional
	Summary ApplicationSummary `json:"summary,omitempty"`
	// ResourceHealthSource is where per-resource health is stored: inline ("") or appTree.
	// +optional
	ResourceHealthSource ResourceHealthLocation `json:"resourceHealthSource,omitempty"`
	// ControllerNamespace is the namespace of the controller that reconciles the Application.
	// +optional
	ControllerNamespace string `json:"controllerNamespace,omitempty"`
	// SourceHydrator is the state of source hydration.
	// +optional
	SourceHydrator SourceHydratorStatus `json:"sourceHydrator,omitempty"`
}

// ResourceHealthLocation is where per-resource health is stored.
type ResourceHealthLocation string

const (
	// ResourceHealthLocationInline stores health in status.resources.
	ResourceHealthLocationInline ResourceHealthLocation = ""
	// ResourceHealthLocationAppTree stores health only in the resource tree served by the controller.
	ResourceHealthLocationAppTree ResourceHealthLocation = "appTree"
)

// SyncStatus is the result of the latest comparison.
type SyncStatus struct {
	// Status is the comparison result.
	// +optional
	Status SyncStatusCode `json:"status,omitempty"`
	// ComparedTo is what the comparison was performed against.
	// +optional
	ComparedTo ComparedTo `json:"comparedTo,omitempty"`
	// Revision is the resolved revision of a single-source Application.
	// +optional
	Revision string `json:"revision,omitempty"`
	// Revisions are the resolved revisions of each source of a multi-source Application.
	// +optional
	Revisions []string `json:"revisions,omitempty"`
	// ShortRevision is a display form of Revision/Revisions for printer columns (abbreviated commit
	// SHA or chart version; "+N" appended for additional sources). Not used for comparison.
	// +optional
	ShortRevision string `json:"shortRevision,omitempty"`
}

// ComparedTo records the inputs of a comparison.
type ComparedTo struct {
	// Source is the compared source of a single-source Application.
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// Destination is the compared destination.
	// +optional
	Destination ApplicationDestination `json:"destination,omitempty"`
	// Sources are the compared sources of a multi-source Application.
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
	// IgnoreDifferences are the ignore rules used for the comparison.
	// +optional
	IgnoreDifferences IgnoreDifferences `json:"ignoreDifferences,omitempty"`
}

// AppHealthStatus is the aggregated health of an Application.
type AppHealthStatus struct {
	// Status is the aggregated health status code.
	// +optional
	Status HealthStatusCode `json:"status,omitempty"`
	// LastTransitionTime is when Status last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// RevisionHistories is a list of history entries, oldest first.
type RevisionHistories []RevisionHistory

// RevisionHistory is one successful sync.
type RevisionHistory struct {
	// ID is an auto-incrementing identifier used by rollback.
	ID int64 `json:"id"`
	// Revision is the revision synced for a single-source Application.
	// +optional
	Revision string `json:"revision,omitempty"`
	// Revisions are the revisions synced for each source of a multi-source Application.
	// +optional
	Revisions []string `json:"revisions,omitempty"`
	// DeployedAt is when the sync completed.
	DeployedAt metav1.Time `json:"deployedAt"`
	// DeployStartedAt is when the sync started.
	// +optional
	DeployStartedAt *metav1.Time `json:"deployStartedAt,omitempty"`
	// Source is the source used by the sync of a single-source Application.
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// Sources are the sources used by the sync of a multi-source Application.
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
	// InitiatedBy records who started the sync.
	// +optional
	InitiatedBy OperationInitiator `json:"initiatedBy,omitempty"`
	// OperationName is the name of the Operation that performed the sync (it may have been deleted
	// by retention).
	// +optional
	OperationName string `json:"operationName,omitempty"`
}

// ApplicationOperationState summarizes the most recent Operation of an Application.
type ApplicationOperationState struct {
	// OperationName is the name of the Operation in the Application's namespace.
	OperationName string `json:"operationName"`
	// Phase is the Operation's phase.
	// +optional
	Phase OperationPhase `json:"phase,omitempty"`
	// Message is the Operation's status message.
	// +optional
	Message string `json:"message,omitempty"`
	// StartedAt is when the Operation started running.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the Operation reached a terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// RetryCount is the number of retries performed.
	// +optional
	RetryCount int64 `json:"retryCount,omitempty"`
	// SyncResult summarizes what was synced; used to decide whether automated sync already
	// attempted the current revision.
	// +optional
	SyncResult *SyncResultSummary `json:"syncResult,omitempty"`
	// InitiatedBy records who started the Operation.
	// +optional
	InitiatedBy OperationInitiator `json:"initiatedBy,omitempty"`
}

// SyncResultSummary is the revision and source information of a sync, without per-resource results.
type SyncResultSummary struct {
	// +optional
	Revision string `json:"revision,omitempty"`
	// +optional
	Revisions []string `json:"revisions,omitempty"`
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
}

// ApplicationSummary lists external URLs and images used by the managed resources.
type ApplicationSummary struct {
	// ExternalURLs of the managed resources (Ingress hosts, link annotations).
	// +optional
	ExternalURLs []string `json:"externalURLs,omitempty"`
	// Images used by the managed resources.
	// +optional
	Images []string `json:"images,omitempty"`
	// IsAppOfApps is true if the Application manages other Applications.
	// +optional
	IsAppOfApps bool `json:"isAppOfApps,omitempty"`
}

// SourceHydratorStatus is the state of source hydration.
type SourceHydratorStatus struct {
	// LastSuccessfulOperation is the most recent successful hydration.
	// +optional
	LastSuccessfulOperation *SuccessfulHydrateOperation `json:"lastSuccessfulOperation,omitempty"`
	// CurrentOperation is the in-progress or most recent hydration.
	// +optional
	CurrentOperation *HydrateOperation `json:"currentOperation,omitempty"`
	// LastComparedDryRevision is the dry revision of the latest comparison, updated even when
	// hydration was skipped because nothing changed.
	// +optional
	LastComparedDryRevision string `json:"lastComparedDryRevision,omitempty"`
}

// HydrateOperationPhase is the phase of a hydration.
// +kubebuilder:validation:Enum=Hydrating;Failed;Hydrated
type HydrateOperationPhase string

const (
	HydrateOperationPhaseHydrating HydrateOperationPhase = "Hydrating"
	HydrateOperationPhaseFailed    HydrateOperationPhase = "Failed"
	HydrateOperationPhaseHydrated  HydrateOperationPhase = "Hydrated"
)

// HydrateOperation is a hydration attempt.
type HydrateOperation struct {
	// StartedAt is when hydration started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when hydration finished.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Phase of the hydration.
	Phase HydrateOperationPhase `json:"phase"`
	// Message describes the hydration state.
	// +optional
	Message string `json:"message,omitempty"`
	// DrySHA is the resolved dry revision.
	// +optional
	DrySHA string `json:"drySHA,omitempty"`
	// HydratedSHA is the commit containing the hydrated manifests.
	// +optional
	HydratedSHA string `json:"hydratedSHA,omitempty"`
	// SourceHydrator is the configuration used for the hydration.
	// +optional
	SourceHydrator *SourceHydrator `json:"sourceHydrator,omitempty"`
}

// SuccessfulHydrateOperation is the most recent successful hydration.
type SuccessfulHydrateOperation struct {
	// +optional
	DrySHA string `json:"drySHA,omitempty"`
	// +optional
	HydratedSHA string `json:"hydratedSHA,omitempty"`
	// +optional
	SourceHydrator *SourceHydrator `json:"sourceHydrator,omitempty"`
}
