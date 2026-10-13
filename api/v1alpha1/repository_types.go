package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Repository registers one git, Helm or OCI repository and how to access it. It replaces Argo CD's
// Secrets labeled argocd.argoproj.io/secret-type=repository (and repository-write). Credentials are
// never stored in the object: spec.credentialsRef names a Secret in the same namespace whose keys are
// the RepositorySecretKey* constants.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=repositories,singular=repository,scope=Namespaced,shortName=prepo,categories=patrol
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Purpose",type=string,JSONPath=`.spec.purpose`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`,priority=1
// +kubebuilder:printcolumn:name="Connection",type=string,JSONPath=`.status.connectionState.status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Repository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the repository configuration.
	Spec RepositorySpec `json:"spec"`
	// Status is the observed state of the repository.
	// +optional
	Status RepositoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RepositoryList is a list of Repositories.
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Repository `json:"items"`
}

func init() {
	register(&Repository{}, &RepositoryList{})
}

// RepositoryType is the kind of repository.
// +kubebuilder:validation:Enum=git;helm;oci
type RepositoryType string

const (
	RepositoryTypeGit  RepositoryType = "git"
	RepositoryTypeHelm RepositoryType = "helm"
	RepositoryTypeOCI  RepositoryType = "oci"
)

// RepositoryPurpose is what the credentials are used for.
// +kubebuilder:validation:Enum=read;write
type RepositoryPurpose string

const (
	// RepositoryPurposeRead credentials are used to fetch sources.
	RepositoryPurposeRead RepositoryPurpose = "read"
	// RepositoryPurposeWrite credentials are used by the commit server (source hydrator) to push.
	RepositoryPurposeWrite RepositoryPurpose = "write"
)

// RepositorySpec is the configuration of a Repository.
// +kubebuilder:validation:XValidation:rule="!has(self.purpose) || self.purpose != 'write' || !has(self.type) || self.type == 'git'",message="purpose write is only supported for git repositories"
// +kubebuilder:validation:XValidation:rule="!has(self.enableLFS) || !self.enableLFS || !has(self.type) || self.type == 'git'",message="enableLFS is only supported for git repositories"
type RepositorySpec struct {
	// URL of the repository (https://, ssh://, git@host:path, oci://, or a Helm repository URL).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`
	// Type of the repository.
	// +optional
	// +kubebuilder:default=git
	Type RepositoryType `json:"type,omitempty"`
	// Name is the Helm repository name used in dependency resolution (Helm repositories only).
	// +optional
	Name string `json:"name,omitempty"`
	// Project scopes the repository to an AppProject; it is then implicitly allowed in that project's
	// sourceRepos and usable only by its Applications.
	// +optional
	Project string `json:"project,omitempty"`
	// Purpose of the credentials.
	// +optional
	// +kubebuilder:default=read
	Purpose RepositoryPurpose `json:"purpose,omitempty"`
	// Insecure skips TLS certificate and SSH host key verification.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// InsecureIgnoreHostKey skips SSH host key verification (git only).
	//
	// Deprecated: use Insecure.
	// +optional
	InsecureIgnoreHostKey bool `json:"insecureIgnoreHostKey,omitempty"`
	// EnableLFS fetches git-lfs objects (git only).
	// +optional
	EnableLFS bool `json:"enableLFS,omitempty"`
	// Depth is the clone depth for shallow clones; 0 means a full clone.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Depth int64 `json:"depth,omitempty"`
	// WebhookManifestCacheWarmDisabled skips pre-rendering (manifest cache warming) of affected
	// Applications when a webhook for this repository arrives; they are refreshed instead. Useful for
	// large monorepos.
	// +optional
	WebhookManifestCacheWarmDisabled bool `json:"webhookManifestCacheWarmDisabled,omitempty"`

	RepositoryAccess `json:",inline"`
}

// RepositoryAccess holds the connection and credential options shared by Repository and
// RepositoryCredential.
type RepositoryAccess struct {
	// EnableOCI enables Helm OCI support for a Helm repository.
	// +optional
	EnableOCI bool `json:"enableOCI,omitempty"`
	// InsecureOCIForceHTTP connects to an OCI registry over plain HTTP (OCI only).
	// +optional
	InsecureOCIForceHTTP bool `json:"insecureOCIForceHTTP,omitempty"`
	// ForceHTTPBasicAuth sends basic auth on the first request instead of waiting for a challenge.
	// +optional
	ForceHTTPBasicAuth bool `json:"forceHTTPBasicAuth,omitempty"`
	// Proxy is the HTTP(S) proxy URL used to reach the repository.
	// +optional
	Proxy string `json:"proxy,omitempty"`
	// NoProxy is a comma-separated list of hosts that bypass Proxy.
	// +optional
	NoProxy string `json:"noProxy,omitempty"`
	// UseAzureWorkloadIdentity authenticates with Azure Workload Identity.
	// +optional
	UseAzureWorkloadIdentity bool `json:"useAzureWorkloadIdentity,omitempty"`
	// AzureServicePrincipal authenticates with an Azure service principal; the client secret is read
	// from the credentials Secret (key azureServicePrincipalClientSecret).
	// +optional
	AzureServicePrincipal *AzureServicePrincipal `json:"azureServicePrincipal,omitempty"`
	// GitHubApp authenticates as a GitHub App installation; the private key is read from the
	// credentials Secret (key githubAppPrivateKey) unless SCMProviderRef supplies the App.
	// +optional
	GitHubApp *RepositoryGitHubApp `json:"githubApp,omitempty"`
	// CredentialsRef names a Secret in the same namespace holding credentials. Recognized keys:
	// username, password, bearerToken, sshPrivateKey, tlsClientCertData, tlsClientCertKey,
	// githubAppPrivateKey, gcpServiceAccountKey, azureServicePrincipalClientSecret.
	// +optional
	CredentialsRef *LocalSecretReference `json:"credentialsRef,omitempty"`
	// SCMProviderRef obtains credentials (e.g. GitHub App installation tokens) from an SCMProvider in
	// the same namespace.
	// +optional
	SCMProviderRef *SCMProviderReference `json:"scmProviderRef,omitempty"`
}

// RepositoryGitHubApp identifies a GitHub App installation.
type RepositoryGitHubApp struct {
	// AppID of the GitHub App.
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appID"`
	// InstallationID of the App installation. When unset, the installation is discovered from the
	// organization (owner) in the repository URL, which lets one RepositoryCredential serve every
	// organization the App is installed in.
	// +optional
	// +kubebuilder:validation:Minimum=1
	InstallationID *int64 `json:"installationID,omitempty"`
	// EnterpriseBaseURL is the GitHub Enterprise API URL (default https://api.github.com).
	// +optional
	EnterpriseBaseURL string `json:"enterpriseBaseURL,omitempty"`
}

// AzureServicePrincipal identifies an Azure service principal.
type AzureServicePrincipal struct {
	// ClientID of the service principal.
	ClientID string `json:"clientID"`
	// TenantID of the service principal.
	TenantID string `json:"tenantID"`
	// ActiveDirectoryEndpoint (default https://login.microsoftonline.com).
	// +optional
	ActiveDirectoryEndpoint string `json:"activeDirectoryEndpoint,omitempty"`
}

// RepositoryStatus is the observed state of a Repository or RepositoryCredential.
type RepositoryStatus struct {
	// ConnectionState is the result of the last connection check.
	// +optional
	ConnectionState *ConnectionState `json:"connectionState,omitempty"`
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
