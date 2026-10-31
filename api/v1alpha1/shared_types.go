package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocalSecretReference references a Secret in the namespace of the referencing object.
type LocalSecretReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SecretKeyReference references one key of a Secret in the namespace of the referencing object.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Key within the Secret's data.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// SCMProviderReference references an SCMProvider in the namespace of the referencing object.
type SCMProviderReference struct {
	// Name of the SCMProvider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ConnectionStatus is the result of the last connection attempt to a remote endpoint.
// +kubebuilder:validation:Enum=Successful;Failed;Unknown
type ConnectionStatus string

const (
	// ConnectionStatusSuccessful indicates that a connection has been successfully established.
	ConnectionStatusSuccessful ConnectionStatus = "Successful"
	// ConnectionStatusFailed indicates that a connection attempt has failed.
	ConnectionStatusFailed ConnectionStatus = "Failed"
	// ConnectionStatusUnknown indicates that the connection status could not be reliably determined.
	ConnectionStatusUnknown ConnectionStatus = "Unknown"
)

// ConnectionState describes the last connection attempt to a repository, cluster or SCM API.
type ConnectionState struct {
	// Status of the last connection attempt.
	Status ConnectionStatus `json:"status"`
	// Message is a human-readable description of the connection status.
	// +optional
	Message string `json:"message,omitempty"`
	// AttemptedAt is when the connection status was determined. On a Cluster object it moves only
	// when the status or message changes, so it says since when that state holds.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
}

// SyncStatusCode is the result of comparing desired and live state.
type SyncStatusCode string

const (
	// SyncStatusCodeUnknown indicates that the sync status could not be reliably determined.
	SyncStatusCodeUnknown SyncStatusCode = "Unknown"
	// SyncStatusCodeSynced indicates that desired and live states match.
	SyncStatusCodeSynced SyncStatusCode = "Synced"
	// SyncStatusCodeOutOfSync indicates that there is a drift between desired and live states.
	SyncStatusCodeOutOfSync SyncStatusCode = "OutOfSync"
)

// HealthStatusCode is the health of a resource or of an Application.
type HealthStatusCode string

const (
	// HealthStatusUnknown indicates that health assessment failed and the actual health is unknown.
	HealthStatusUnknown HealthStatusCode = "Unknown"
	// HealthStatusProgressing indicates that the resource is not healthy yet but may become healthy.
	HealthStatusProgressing HealthStatusCode = "Progressing"
	// HealthStatusHealthy indicates that the resource is healthy.
	HealthStatusHealthy HealthStatusCode = "Healthy"
	// HealthStatusSuspended indicates that the resource is suspended (e.g. a paused rollout or CronJob).
	HealthStatusSuspended HealthStatusCode = "Suspended"
	// HealthStatusDegraded indicates that the resource failed or cannot reach a healthy state.
	HealthStatusDegraded HealthStatusCode = "Degraded"
	// HealthStatusMissing indicates that the resource is desired but absent from the cluster.
	HealthStatusMissing HealthStatusCode = "Missing"
)

// HealthStatus is the health of a single resource.
type HealthStatus struct {
	// Status is the health status code.
	// +optional
	Status HealthStatusCode `json:"status,omitempty"`
	// Message is a human-readable explanation of the health status.
	// +optional
	Message string `json:"message,omitempty"`
}

// ResourceStatus is the sync and health status of one resource managed by an Application (or of one
// Application managed by an ApplicationSet).
type ResourceStatus struct {
	// Group is the API group of the resource ("" for the core group).
	// +optional
	Group string `json:"group,omitempty"`
	// Version is the API version of the resource.
	// +optional
	Version string `json:"version,omitempty"`
	// Kind is the kind of the resource.
	// +optional
	Kind string `json:"kind,omitempty"`
	// Namespace of the resource ("" for cluster-scoped resources).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Name of the resource.
	// +optional
	Name string `json:"name,omitempty"`
	// Status is the comparison result for this resource.
	// +optional
	Status SyncStatusCode `json:"status,omitempty"`
	// Health of the resource.
	// +optional
	Health *HealthStatus `json:"health,omitempty"`
	// Hook is true if the resource is a sync hook.
	// +optional
	Hook bool `json:"hook,omitempty"`
	// RequiresPruning is true if the resource is live but no longer desired.
	// +optional
	RequiresPruning bool `json:"requiresPruning,omitempty"`
	// SyncWave is the sync wave of the resource.
	// +optional
	SyncWave int64 `json:"syncWave,omitempty"`
	// RequiresDeletionConfirmation is true if deleting the resource requires explicit approval
	// (Delete=confirm or Prune=confirm sync options).
	// +optional
	RequiresDeletionConfirmation bool `json:"requiresDeletionConfirmation,omitempty"`
}

// Info is a name/value pair of informational metadata.
type Info struct {
	// Name of the item.
	Name string `json:"name"`
	// Value of the item.
	Value string `json:"value"`
}
