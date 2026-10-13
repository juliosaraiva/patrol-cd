package v1alpha1

// Condition types and reasons. All kinds report []metav1.Condition (type, status, reason, message,
// lastTransitionTime, observedGeneration). Reasons are stable CamelCase identifiers; clients may
// switch on them.
//
// Argo CD's ApplicationCondition carries no status: a condition's presence means the error or warning
// is active. Patrol keeps the same type names and reports them with status True while active; the
// controller removes them (or sets False) when resolved. Patrol adds the aggregate types Reconciled,
// Synced and Healthy so `kubectl wait --for=condition=...` works.

// Application condition types mirrored from Argo CD (pkg/apis/application/v1alpha1/types.go).
const (
	// ApplicationConditionDeletionError: the controller failed to delete the Application or its resources.
	ApplicationConditionDeletionError = "DeletionError"
	// ApplicationConditionInvalidSpecError: the spec is invalid (e.g. project, source or destination).
	ApplicationConditionInvalidSpecError = "InvalidSpecError"
	// ApplicationConditionComparisonError: the controller failed to compare desired and live state.
	ApplicationConditionComparisonError = "ComparisonError"
	// ApplicationConditionSyncError: the controller failed to start an automated sync.
	ApplicationConditionSyncError = "SyncError"
	// ApplicationConditionUnknownError: an unclassified controller error.
	ApplicationConditionUnknownError = "UnknownError"
	// ApplicationConditionSharedResourceWarning: a resource is also managed by another Application.
	ApplicationConditionSharedResourceWarning = "SharedResourceWarning"
	// ApplicationConditionRepeatedResourceWarning: the rendered manifests contain the same resource twice.
	ApplicationConditionRepeatedResourceWarning = "RepeatedResourceWarning"
	// ApplicationConditionExcludedResourceWarning: a rendered resource is excluded by settings.
	ApplicationConditionExcludedResourceWarning = "ExcludedResourceWarning"
	// ApplicationConditionOrphanedResourceWarning: the destination namespace has orphaned resources.
	ApplicationConditionOrphanedResourceWarning = "OrphanedResourceWarning"
)

// Application aggregate condition types (Patrol additions).
const (
	// ApplicationConditionReconciled is True when the latest generation was compared successfully.
	ApplicationConditionReconciled = "Reconciled"
	// ApplicationConditionSynced is True when live state matches desired state (sync.status Synced).
	ApplicationConditionSynced = "Synced"
	// ApplicationConditionHealthy is True when the aggregated health is Healthy.
	ApplicationConditionHealthy = "Healthy"
)

// Application condition reasons.
const (
	// Reasons for the Argo CD-mirrored error/warning types (status True while active).
	ReasonDeletionFailed         = "DeletionFailed"
	ReasonInvalidSpec            = "InvalidSpec"
	ReasonProjectNotFound        = "ProjectNotFound"
	ReasonProjectNotPermitted    = "ProjectNotPermitted"
	ReasonDestinationNotFound    = "DestinationNotFound"
	ReasonRepositoryNotPermitted = "RepositoryNotPermitted"
	ReasonComparisonFailed       = "ComparisonFailed"
	ReasonManifestGenerationFail = "ManifestGenerationFailed"
	ReasonRevisionResolutionFail = "RevisionResolutionFailed"
	ReasonAutoSyncFailed         = "AutoSyncFailed"
	ReasonAutoSyncRetryExhausted = "AutoSyncRetryExhausted"
	ReasonUnknownError           = "UnknownError"
	ReasonSharedResource         = "SharedResource"
	ReasonRepeatedResource       = "RepeatedResource"
	ReasonExcludedResource       = "ExcludedResource"
	ReasonOrphanedResources      = "OrphanedResources"
	ReasonResolved               = "Resolved"

	// Reasons for ApplicationConditionReconciled.
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	ReasonReconcileFailed    = "ReconcileFailed"
	ReasonReconcileSkipped   = "ReconcileSkipped"

	// Reasons for ApplicationConditionSynced (mirror SyncStatusCode).
	ReasonSynced            = "Synced"
	ReasonOutOfSync         = "OutOfSync"
	ReasonSyncStatusUnknown = "SyncStatusUnknown"

	// Reasons for ApplicationConditionHealthy (mirror HealthStatusCode).
	ReasonHealthy       = "Healthy"
	ReasonProgressing   = "Progressing"
	ReasonDegraded      = "Degraded"
	ReasonSuspended     = "Suspended"
	ReasonMissing       = "Missing"
	ReasonHealthUnknown = "HealthUnknown"
)

// ApplicationSet condition types (Argo CD ApplicationSetConditionType).
const (
	ApplicationSetConditionErrorOccurred       = "ErrorOccurred"
	ApplicationSetConditionParametersGenerated = "ParametersGenerated"
	ApplicationSetConditionResourcesUpToDate   = "ResourcesUpToDate"
	ApplicationSetConditionRolloutProgressing  = "RolloutProgressing"
)

// ApplicationSet condition reasons (Argo CD ApplicationSetReasonType values).
const (
	ApplicationSetReasonErrorOccurred                    = "ErrorOccurred"
	ApplicationSetReasonApplicationSetUpToDate           = "ApplicationSetUpToDate"
	ApplicationSetReasonParametersGenerated              = "ParametersGenerated"
	ApplicationSetReasonApplicationGenerated             = "ApplicationGeneratedSuccessfully"
	ApplicationSetReasonUpdateApplicationError           = "UpdateApplicationError"
	ApplicationSetReasonApplicationParamsGenerationError = "ApplicationGenerationFromParamsError"
	ApplicationSetReasonRenderTemplateParamsError        = "RenderTemplateParamsError"
	ApplicationSetReasonCreateApplicationError           = "CreateApplicationError"
	ApplicationSetReasonDeleteApplicationError           = "DeleteApplicationError"
	ApplicationSetReasonRefreshApplicationError          = "RefreshApplicationError"
	ApplicationSetReasonApplicationValidationError       = "ApplicationValidationError"
	ApplicationSetReasonApplicationSetModified           = "ApplicationSetModified"
	ApplicationSetReasonApplicationSetRolloutComplete    = "ApplicationSetRolloutComplete"
	ApplicationSetReasonSyncApplicationError             = "SyncApplicationError"
)

// Condition types for Repository, RepositoryCredential, Cluster and SCMProvider.
const (
	// ConditionReady is True when the object's configuration is valid, referenced Secrets exist and the
	// last connection check succeeded.
	ConditionReady = "Ready"
	// ConditionConnectionFailed is True while the last connection attempt failed.
	ConditionConnectionFailed = "ConnectionFailed"
)

// Reasons for ConditionReady and ConditionConnectionFailed.
const (
	ReasonConnectionSuccessful = "ConnectionSuccessful"
	ReasonConnectionFailed     = "ConnectionFailed"
	ReasonSecretNotFound       = "SecretNotFound"
	ReasonSecretKeyMissing     = "SecretKeyMissing"
	ReasonInvalidCredentials   = "InvalidCredentials"
	ReasonSCMProviderNotFound  = "SCMProviderNotFound"
	ReasonSCMProviderNotReady  = "SCMProviderNotReady"
	ReasonRateLimited          = "RateLimited"
	ReasonInvalidConfiguration = "InvalidConfiguration"
	ReasonCheckPending         = "CheckPending"
)

// Operation condition types.
const (
	// OperationConditionAdmitted is True when the Operation was accepted for execution and False when it
	// was rejected (e.g. another non-terminal Operation exists for the same Application).
	OperationConditionAdmitted = "Admitted"
	// OperationConditionPlanned is True once revisions were resolved and the task plan was persisted.
	OperationConditionPlanned = "Planned"
	// OperationConditionCompleted is True once the Operation reached a terminal phase; the reason
	// explains the outcome.
	OperationConditionCompleted = "Completed"
	// OperationConditionRetrying is True while a failed attempt waits for its retry backoff.
	OperationConditionRetrying = "Retrying"
)

// Operation condition reasons.
const (
	OperationReasonAdmitted                 = "Admitted"
	OperationReasonOperationInProgress      = "OperationInProgress"
	OperationReasonApplicationNotFound      = "ApplicationNotFound"
	OperationReasonInvalidSpec              = "InvalidSpec"
	OperationReasonPermissionDenied         = "PermissionDenied"
	OperationReasonSyncWindowClosed         = "SyncWindowClosed"
	OperationReasonRevisionsResolved        = "RevisionsResolved"
	OperationReasonRevisionResolutionFailed = "RevisionResolutionFailed"
	OperationReasonManifestGenerationFailed = "ManifestGenerationFailed"
	OperationReasonRollbackTargetNotFound   = "RollbackTargetNotFound"
	OperationReasonSucceeded                = "Succeeded"
	OperationReasonSyncFailed               = "SyncFailed"
	OperationReasonInfrastructureError      = "InfrastructureError"
	OperationReasonCancelled                = "Cancelled"
	OperationReasonRetryScheduled           = "RetryScheduled"
	OperationReasonRetryLimitReached        = "RetryLimitReached"
)
