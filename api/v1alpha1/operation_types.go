package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Operation is one sync or rollback request for an Application and the record of its execution.
// It replaces Argo CD's Application.operation field and status.operationState.syncResult (ADR-0011).
//
// The spec is immutable after creation except spec.cancel, which may only change from false to true.
// Clients should change metadata and spec.cancel with a patch (merge or server-side apply).
// Immutability is enforced by comparing the stored spec, and Go clients omit empty values: scalar
// fields of spec.sync and spec.initiatedBy are defaulted or compared as their zero value, so a full
// update of an Operation written with explicit `false`/`""`/`0` works, but one written with an explicit
// empty list or object (e.g. `syncOptions: []`) is rejected by a full update from a Go client.
//
// A ValidatingAdmissionPolicy shipped with the CRDs (OperationAdmissionPolicyName, in deploy/crds)
// additionally requires spec.initiatedBy to name the requesting user (or be empty) and to not be
// automated unless the requester holds the OperationInitiatorVerb on operations, and requires the
// LabelKeyOperationApplication label, when present, to equal spec.application.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=operations,singular=operation,scope=Namespaced,shortName=pop;pops,categories=patrol
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.application`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Initiator",type=string,JSONPath=`.spec.initiatedBy.username`
// +kubebuilder:printcolumn:name="Automated",type=boolean,JSONPath=`.spec.initiatedBy.automated`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.syncResult.revision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Operation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the requested operation.
	Spec OperationSpec `json:"spec"`
	// Status is the execution record, written by the controller.
	// +optional
	Status OperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperationList is a list of Operations.
type OperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Operation `json:"items"`
}

func init() {
	register(&Operation{}, &OperationList{})
}

const (
	// OperationAdmissionPolicyName is the name of the ValidatingAdmissionPolicy (and its binding)
	// shipped in deploy/crds that guards spec.initiatedBy and the application label of Operations.
	OperationAdmissionPolicyName = "operations." + GroupName
	// OperationInitiatorVerb is the RBAC verb on operations.cd.patrol.example that allows creating an
	// Operation whose spec.initiatedBy names another user or is automated. Grant it only to
	// patrol-server (which sets the user from verified token claims) and the application controller
	// (automated syncs). Without it, spec.initiatedBy.username must be empty or equal the requesting
	// Kubernetes user, and spec.initiatedBy.automated must be false.
	OperationInitiatorVerb = "impersonate"
)

// OperationSpec is the requested operation.
//
// +kubebuilder:validation:XValidation:rule="has(self.sync) || has(self.rollback)",message="one of spec.sync or spec.rollback must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.rollback) || !has(self.sync) || (!has(self.sync.source) && !has(self.sync.sources))",message="spec.rollback is mutually exclusive with spec.sync.source and spec.sync.sources"
// +kubebuilder:validation:XValidation:rule="has(self.sync) == has(oldSelf.sync) && (!has(self.sync) || self.sync == oldSelf.sync)",message="spec.sync is immutable; change metadata and spec.cancel with a patch, not a full update"
// +kubebuilder:validation:XValidation:rule="has(self.rollback) == has(oldSelf.rollback) && (!has(self.rollback) || self.rollback == oldSelf.rollback)",message="spec.rollback is immutable; change metadata and spec.cancel with a patch, not a full update"
// +kubebuilder:validation:XValidation:rule="has(self.info) == has(oldSelf.info) && (!has(self.info) || self.info == oldSelf.info)",message="spec.info is immutable; change metadata and spec.cancel with a patch, not a full update"
// +kubebuilder:validation:XValidation:rule="has(self.retry) == has(oldSelf.retry) && (!has(self.retry) || self.retry == oldSelf.retry)",message="spec.retry is immutable; change metadata and spec.cancel with a patch, not a full update"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancel) || !oldSelf.cancel || (has(self.cancel) && self.cancel)",message="spec.cancel cannot be unset once true"
type OperationSpec struct {
	// Application is the name of the Application, in the Operation's namespace, to operate on.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.application is immutable"
	Application string `json:"application"`
	// Sync requests a sync. Also carries the sync options of a rollback.
	// +optional
	Sync *SyncOperation `json:"sync,omitempty"`
	// Rollback requests a sync to the sources and revisions of a status.history entry.
	// +optional
	Rollback *RollbackOperation `json:"rollback,omitempty"`
	// InitiatedBy records who requested the operation. patrol-server sets it from verified token claims
	// and the controller sets automated; other Kubernetes users may only leave it empty or name
	// themselves (enforced by the OperationAdmissionPolicyName ValidatingAdmissionPolicy). Compared
	// field by field, with absent fields as their zero value, so Go clients can update the Operation.
	// +optional
	// +kubebuilder:default={}
	// +kubebuilder:validation:XValidation:rule="(has(self.username) ? self.username : '') == (has(oldSelf.username) ? oldSelf.username : '') && (has(self.automated) && self.automated) == (has(oldSelf.automated) && oldSelf.automated)",message="spec.initiatedBy is immutable"
	InitiatedBy OperationInitiator `json:"initiatedBy,omitempty"`
	// Info holds informational name/value pairs (e.g. the reason for a manual sync).
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Info []Info `json:"info,omitempty"`
	// Retry controls retries of failed attempts. Defaults to the Application's syncPolicy.retry.
	// +optional
	Retry *RetryStrategy `json:"retry,omitempty"`
	// Cancel requests cancellation: the operation moves to Terminating, running hooks are cleaned up
	// per policy, no new tasks start, and it ends Cancelled. Can only change from false to true.
	// +optional
	Cancel bool `json:"cancel,omitempty"`
}

// OperationInitiator records who requested an operation.
type OperationInitiator struct {
	// Username of the user who requested the operation.
	// +optional
	Username string `json:"username,omitempty"`
	// Automated is true if the controller requested the operation (automated sync or self-heal).
	// +optional
	Automated bool `json:"automated,omitempty"`
}

// RollbackOperation selects the status.history entry to roll back to.
type RollbackOperation struct {
	// ID of the status.history entry of the Application.
	// +kubebuilder:validation:Minimum=0
	ID int64 `json:"id"`
}

// SyncOperation is a sync request. Its scalar fields have explicit defaults so that an omitted value
// and an explicit zero value are stored identically and the spec.sync immutability rule does not
// reject a full update from a Go client (which omits zero values).
//
// +kubebuilder:validation:XValidation:rule="!(has(self.source) && has(self.sources))",message="spec.sync.source and spec.sync.sources are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.manifests) || self.manifests.map(m, size(m)).sum() <= 1048576",message="spec.sync.manifests must not exceed 1MiB in total"
// +kubebuilder:validation:XValidation:rule="!has(self.syncOptions) || self.syncOptions.all(o, o.matches('^[A-Za-z][A-Za-z0-9]*=[[:graph:]]+$'))",message="spec.sync.syncOptions entries must have the form Key=Value"
type SyncOperation struct {
	// Revision overrides the target revision of a single-source Application.
	// +optional
	// +kubebuilder:default=""
	Revision string `json:"revision,omitempty"`
	// Revisions override the target revision of each source of a multi-source Application.
	// +optional
	Revisions []string `json:"revisions,omitempty"`
	// Prune deletes resources that are no longer desired.
	// +optional
	// +kubebuilder:default=false
	Prune bool `json:"prune,omitempty"`
	// DryRun performs a server-side dry run without changing resources.
	// +optional
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`
	// SyncStrategy selects apply or hook strategy (hook is the default).
	// +optional
	SyncStrategy *SyncStrategy `json:"syncStrategy,omitempty"`
	// Resources restricts the sync to these resources (selective sync). Empty means all.
	// +optional
	Resources []SyncOperationResource `json:"resources,omitempty"`
	// Source overrides the Application source (single-source).
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// Sources override the Application sources (multi-source).
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
	// Manifests replace rendering with these YAML/JSON documents (local sync for development).
	// Each document is at most 1MiB and the total at most 1MiB.
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	// +kubebuilder:validation:items:MaxLength=1048576
	Manifests []string `json:"manifests,omitempty"`
	// SyncOptions are per-operation sync options, merged over the Application's.
	// +optional
	SyncOptions SyncOptions `json:"syncOptions,omitempty"`
	// SelfHealAttemptsCount is the number of self-heal attempts that preceded this operation.
	// +optional
	// +kubebuilder:default=0
	SelfHealAttemptsCount int64 `json:"autoHealAttemptsCount,omitempty"`
}

// SyncOperationResource identifies a resource of a selective sync. "*" matches any value.
type SyncOperationResource struct {
	// +optional
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SyncStrategy selects how resources are applied.
type SyncStrategy struct {
	// Apply applies resources without running hooks.
	// +optional
	Apply *SyncStrategyApply `json:"apply,omitempty"`
	// Hook applies resources and runs hooks. This is the default.
	// +optional
	Hook *SyncStrategyHook `json:"hook,omitempty"`
}

// SyncStrategyApply applies resources without running hooks.
type SyncStrategyApply struct {
	// Force deletes and re-creates resources when a patch fails.
	// +optional
	// +kubebuilder:default=false
	Force bool `json:"force,omitempty"`
}

// SyncStrategyHook applies resources and runs hooks.
type SyncStrategyHook struct {
	SyncStrategyApply `json:",inline"`
}

// OperationPhase is the phase of an Operation (and of a hook).
// +kubebuilder:validation:Enum=Pending;Running;Terminating;Succeeded;Failed;Error;Cancelled;Rejected
type OperationPhase string

const (
	// OperationPending: created, not yet admitted or started.
	OperationPending OperationPhase = "Pending"
	// OperationRunning: executing (re-entered after a controller restart).
	OperationRunning OperationPhase = "Running"
	// OperationTerminating: cancellation requested; cleaning up.
	OperationTerminating OperationPhase = "Terminating"
	// OperationSucceeded: completed successfully.
	OperationSucceeded OperationPhase = "Succeeded"
	// OperationFailed: a sync task failed (user-visible failure, consumes retry budget).
	OperationFailed OperationPhase = "Failed"
	// OperationError: an infrastructure error prevented completion.
	OperationError OperationPhase = "Error"
	// OperationCancelled: cancelled via spec.cancel.
	OperationCancelled OperationPhase = "Cancelled"
	// OperationRejected: not admitted (e.g. another non-terminal Operation exists).
	OperationRejected OperationPhase = "Rejected"
)

// OperationStatus is the execution record of an Operation.
type OperationStatus struct {
	// Phase of the operation.
	// +optional
	Phase OperationPhase `json:"phase,omitempty"`
	// Message describes the current phase (typically an error).
	// +optional
	Message string `json:"message,omitempty"`
	// StartedAt is when execution started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// FinishedAt is when the operation reached a terminal phase.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// RetryCount is the number of retries performed.
	// +optional
	RetryCount int64 `json:"retryCount,omitempty"`
	// ObservedGeneration is the metadata.generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions are the current Operation conditions (Admitted, Planned, Completed, Retrying).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// SyncResult holds per-resource results and the synced revisions/sources.
	// +optional
	SyncResult *SyncOperationResult `json:"syncResult,omitempty"`
	// Progress is the execution progress, persisted after every wave.
	// +optional
	Progress *OperationProgress `json:"progress,omitempty"`
	// Plan is the persisted execution plan; a restarted controller resumes from it.
	// +optional
	Plan *OperationPlan `json:"plan,omitempty"`
}

// SyncOperationResult is the result of a sync.
type SyncOperationResult struct {
	// Resources are the per-resource results.
	// +optional
	Resources []ResourceResult `json:"resources,omitempty"`
	// Revision is the revision synced (single-source).
	// +optional
	Revision string `json:"revision,omitempty"`
	// Revisions are the revisions synced for each source (multi-source).
	// +optional
	Revisions []string `json:"revisions,omitempty"`
	// Source is the source synced (single-source).
	// +optional
	Source *ApplicationSource `json:"source,omitempty"`
	// Sources are the sources synced (multi-source).
	// +optional
	Sources ApplicationSources `json:"sources,omitempty"`
	// ManagedNamespaceMetadata is the namespace metadata applied by the sync.
	// +optional
	ManagedNamespaceMetadata *ManagedNamespaceMetadata `json:"managedNamespaceMetadata,omitempty"`
}

// ResultCode is the outcome of syncing one resource.
// +kubebuilder:validation:Enum=Synced;SyncFailed;Pruned;PruneSkipped
type ResultCode string

const (
	ResultCodeSynced       ResultCode = "Synced"
	ResultCodeSyncFailed   ResultCode = "SyncFailed"
	ResultCodePruned       ResultCode = "Pruned"
	ResultCodePruneSkipped ResultCode = "PruneSkipped"
)

// HookType is a sync hook type (cd.patrol.example/hook annotation values).
type HookType string

const (
	HookTypePreSync    HookType = "PreSync"
	HookTypeSync       HookType = "Sync"
	HookTypePostSync   HookType = "PostSync"
	HookTypeSyncFail   HookType = "SyncFail"
	HookTypeSkip       HookType = "Skip"
	HookTypePreDelete  HookType = "PreDelete"
	HookTypePostDelete HookType = "PostDelete"
)

// HookDeletePolicy is a hook deletion policy (cd.patrol.example/hook-delete-policy annotation values).
type HookDeletePolicy string

const (
	HookDeletePolicyHookSucceeded      HookDeletePolicy = "HookSucceeded"
	HookDeletePolicyHookFailed         HookDeletePolicy = "HookFailed"
	HookDeletePolicyBeforeHookCreation HookDeletePolicy = "BeforeHookCreation"
)

// SyncPhase is a phase of sync execution.
type SyncPhase string

const (
	SyncPhasePreSync    SyncPhase = "PreSync"
	SyncPhaseSync       SyncPhase = "Sync"
	SyncPhasePostSync   SyncPhase = "PostSync"
	SyncPhaseSyncFail   SyncPhase = "SyncFail"
	SyncPhasePreDelete  SyncPhase = "PreDelete"
	SyncPhasePostDelete SyncPhase = "PostDelete"
)

// ResourceResult is the outcome of syncing one resource or running one hook.
type ResourceResult struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Status is the final result; empty while pending and for hooks.
	// +optional
	Status ResultCode `json:"status,omitempty"`
	// Message is an informational or error message.
	// +optional
	Message string `json:"message,omitempty"`
	// HookType is set for hooks.
	// +optional
	HookType HookType `json:"hookType,omitempty"`
	// HookPhase is the state of the hook (or of a resource with a health-based phase).
	// +optional
	HookPhase OperationPhase `json:"hookPhase,omitempty"`
	// SyncPhase is the sync phase the result was recorded in.
	// +optional
	SyncPhase SyncPhase `json:"syncPhase,omitempty"`
	// Images used by the resource.
	// +optional
	Images []string `json:"images,omitempty"`
}

// OperationProgress is the execution progress of an Operation.
type OperationProgress struct {
	// SyncPhase is the phase being executed.
	// +optional
	SyncPhase SyncPhase `json:"syncPhase,omitempty"`
	// Wave is the wave being executed.
	// +optional
	Wave int64 `json:"wave,omitempty"`
	// StartedAt is when the current wave started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedTasks is the number of completed tasks.
	// +optional
	CompletedTasks int64 `json:"completedTasks,omitempty"`
	// TotalTasks is the number of planned tasks.
	// +optional
	TotalTasks int64 `json:"totalTasks,omitempty"`
}

// OperationPlan is the persisted execution plan of an Operation.
type OperationPlan struct {
	// ResolvedRevisions are the resolved revision of each source, persisted before any mutation.
	// +optional
	ResolvedRevisions []string `json:"resolvedRevisions,omitempty"`
	// RenderedInputDigest is a digest of every rendering input, used to detect drift on resume.
	// +optional
	RenderedInputDigest string `json:"renderedInputDigest,omitempty"`
	// Destination is the resolved destination (server URL resolved from a cluster name).
	// +optional
	Destination ApplicationDestination `json:"destination,omitempty"`
	// Warnings are planning warnings (e.g. destructive changes).
	// +optional
	Warnings []string `json:"warnings,omitempty"`
}
