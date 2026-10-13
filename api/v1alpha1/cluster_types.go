package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KubernetesInternalAPIServerAddr is the server URL of the in-cluster destination.
const KubernetesInternalAPIServerAddr = "https://kubernetes.default.svc"

// Cluster registers a destination cluster. It replaces Argo CD's Secrets labeled
// argocd.argoproj.io/secret-type=cluster. Labels and annotations of the cluster (used by cluster
// generators and sharding) are the Cluster object's metadata. Credentials are never stored in the
// object: spec.config.credentialsRef names a Secret whose keys are the ClusterSecretKey* constants.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clusters,singular=cluster,scope=Namespaced,shortName=pcluster,categories=patrol
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.server`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.serverVersion`
// +kubebuilder:printcolumn:name="Connection",type=string,JSONPath=`.status.connectionState.status`
// +kubebuilder:printcolumn:name="Shard",type=integer,JSONPath=`.status.assignedShard`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the cluster configuration.
	Spec ClusterSpec `json:"spec"`
	// Status is the observed state of the cluster.
	// +optional
	Status ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterList is a list of Clusters.
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

func init() {
	register(&Cluster{}, &ClusterList{})
}

// ClusterSpec is the configuration of a Cluster.
type ClusterSpec struct {
	// Server is the URL of the cluster's API server.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getScheme() in ['https', 'http'] && url(self).getHostname() != ''",message="server must be an http(s) URL"
	Server string `json:"server"`
	// Name of the cluster, referenced by Application destinations (spec.destination.name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// Namespaces restricts management to these namespaces. When set, cluster-scoped resources are
	// ignored unless ClusterResources is true.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// ClusterResources allows cluster-scoped resources when Namespaces is set.
	// +optional
	ClusterResources bool `json:"clusterResources,omitempty"`
	// Project scopes the cluster to an AppProject.
	// +optional
	Project string `json:"project,omitempty"`
	// Shard pins the cluster to a controller shard. Computed by the sharding algorithm when unset.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Shard *int64 `json:"shard,omitempty"`
	// Config holds connection settings.
	// +optional
	Config ClusterConfig `json:"config,omitempty"`
}

// ClusterConfig holds the connection settings of a Cluster.
type ClusterConfig struct {
	// TLSClientConfig holds TLS settings.
	// +optional
	TLSClientConfig TLSClientConfig `json:"tlsClientConfig,omitempty"`
	// AWSAuthConfig authenticates with AWS IAM (EKS).
	// +optional
	AWSAuthConfig *AWSAuthConfig `json:"awsAuthConfig,omitempty"`
	// ExecProviderConfig authenticates with an exec credential plugin. Do not put credentials in its
	// args or env; use CredentialsRef.
	// +optional
	ExecProviderConfig *ExecProviderConfig `json:"execProviderConfig,omitempty"`
	// DisableCompression disables response compression.
	// +optional
	DisableCompression bool `json:"disableCompression,omitempty"`
	// ProxyURL is the proxy used for all requests to the cluster.
	// +optional
	ProxyURL string `json:"proxyURL,omitempty"`
	// CredentialsRef names a Secret in the same namespace holding credentials and CA data. Recognized
	// keys: bearerToken, username, password, caData, certData, keyData.
	// +optional
	CredentialsRef *LocalSecretReference `json:"credentialsRef,omitempty"`
}

// TLSClientConfig holds non-secret TLS settings; certificate data lives in the credentials Secret.
type TLSClientConfig struct {
	// Insecure skips server certificate verification. For testing only.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// ServerName is used for SNI and certificate verification instead of the server host name.
	// +optional
	ServerName string `json:"serverName,omitempty"`
}

// AWSAuthConfig authenticates with AWS IAM.
type AWSAuthConfig struct {
	// ClusterName is the EKS cluster name.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
	// RoleARN is an IAM role to assume.
	// +optional
	RoleARN string `json:"roleARN,omitempty"`
	// Profile is the AWS profile to use.
	// +optional
	Profile string `json:"profile,omitempty"`
}

// ExecProviderConfig runs an exec credential plugin (client.authentication.k8s.io).
type ExecProviderConfig struct {
	// Command to execute.
	// +optional
	Command string `json:"command,omitempty"`
	// Args passed to the command.
	// +optional
	Args []string `json:"args,omitempty"`
	// Env are additional environment variables for the command.
	// +optional
	Env map[string]string `json:"env,omitempty"`
	// APIVersion is the preferred ExecCredential version.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// InstallHint is shown when the command is not found.
	// +optional
	InstallHint string `json:"installHint,omitempty"`
	// ProvideClusterInfo passes cluster information in KUBERNETES_EXEC_INFO.
	// +optional
	ProvideClusterInfo bool `json:"provideClusterInfo,omitempty"`
	// Config is passed to the plugin as ExecCredential.spec.cluster.config.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *runtime.RawExtension `json:"config,omitempty"`
}

// ClusterStatus is the observed state of a Cluster.
type ClusterStatus struct {
	// ConnectionState is the result of the last connection check.
	// +optional
	ConnectionState *ConnectionState `json:"connectionState,omitempty"`
	// ServerVersion is the Kubernetes version of the cluster.
	// +optional
	ServerVersion string `json:"serverVersion,omitempty"`
	// Info holds cache and usage information.
	// +optional
	Info ClusterInfo `json:"info,omitempty"`
	// AssignedShard is the controller shard managing the cluster.
	// +optional
	AssignedShard *int64 `json:"assignedShard,omitempty"`
	// Conditions are the current conditions (Ready, ConnectionFailed).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// ObservedGeneration is the metadata.generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ClusterInfo holds cache and usage information of a Cluster.
type ClusterInfo struct {
	// CacheInfo describes the controller's live-state cache of the cluster.
	// +optional
	CacheInfo ClusterCacheInfo `json:"cacheInfo,omitempty"`
	// ApplicationsCount is the number of Applications targeting the cluster.
	// +optional
	ApplicationsCount int64 `json:"applicationsCount,omitempty"`
	// APIVersions are the API versions served by the cluster.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`
}

// ClusterCacheInfo describes the controller's live-state cache of a cluster.
type ClusterCacheInfo struct {
	// ResourcesCount is the number of cached resources.
	// +optional
	ResourcesCount int64 `json:"resourcesCount,omitempty"`
	// APIsCount is the number of watched APIs.
	// +optional
	APIsCount int64 `json:"apisCount,omitempty"`
	// LastCacheSyncTime is when the cache was last fully synchronized.
	// +optional
	LastCacheSyncTime *metav1.Time `json:"lastCacheSyncTime,omitempty"`
}
