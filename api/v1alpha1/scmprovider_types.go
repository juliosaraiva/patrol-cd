package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SCMProvider is an SCM API endpoint and its credentials, webhook secret and commit status
// reporting settings (ADR-0010). Repositories, ApplicationSet generators and notifications reference
// it by name instead of carrying their own tokens.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=scmproviders,singular=scmprovider,scope=Namespaced,shortName=pscm,categories=patrol
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="API URL",type=string,JSONPath=`.spec.apiURL`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SCMProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the provider configuration.
	Spec SCMProviderSpec `json:"spec"`
	// Status is the observed state of the provider.
	// +optional
	Status SCMProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SCMProviderList is a list of SCMProviders.
type SCMProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SCMProvider `json:"items"`
}

func init() {
	register(&SCMProvider{}, &SCMProviderList{})
}

// SCMProviderType is the kind of SCM.
// +kubebuilder:validation:Enum=github;gitlab;gitea;bitbucketCloud;bitbucketServer;azureDevOps;awsCodeCommit;fake
type SCMProviderType string

const (
	SCMProviderTypeGitHub          SCMProviderType = "github"
	SCMProviderTypeGitLab          SCMProviderType = "gitlab"
	SCMProviderTypeGitea           SCMProviderType = "gitea"
	SCMProviderTypeBitbucketCloud  SCMProviderType = "bitbucketCloud"
	SCMProviderTypeBitbucketServer SCMProviderType = "bitbucketServer"
	SCMProviderTypeAzureDevOps     SCMProviderType = "azureDevOps"
	SCMProviderTypeAWSCodeCommit   SCMProviderType = "awsCodeCommit"
	// SCMProviderTypeFake is the in-process reference implementation used by tests and demos.
	SCMProviderTypeFake SCMProviderType = "fake"
)

// StatusReportingMode selects how sync results are reported on commits.
// +kubebuilder:validation:Enum=checks;statuses
type StatusReportingMode string

const (
	// StatusReportingModeChecks reports GitHub check runs (requires GitHub App authentication).
	StatusReportingModeChecks StatusReportingMode = "checks"
	// StatusReportingModeStatuses reports commit statuses.
	StatusReportingModeStatuses StatusReportingMode = "statuses"
)

// SCMProviderSpec is the configuration of an SCMProvider.
// +kubebuilder:validation:XValidation:rule="!has(self.statusReporting) || !has(self.statusReporting.mode) || self.statusReporting.mode != 'checks' || (self.type == 'github' && has(self.auth) && has(self.auth.githubApp))",message="statusReporting.mode checks requires type github with auth.githubApp"
type SCMProviderSpec struct {
	// Type of the SCM.
	Type SCMProviderType `json:"type"`
	// APIURL is the API base URL (provider default when empty, e.g. https://api.github.com).
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	APIURL string `json:"apiURL,omitempty"`
	// WebURL is the web UI base URL used for links (provider default when empty).
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	WebURL string `json:"webURL,omitempty"`
	// Insecure skips TLS certificate verification of the API.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// CASecretRef references a PEM CA bundle trusted for the API.
	// +optional
	CASecretRef *SecretKeyReference `json:"caSecretRef,omitempty"`
	// Auth configures API authentication. Anonymous access when empty.
	// +optional
	Auth *SCMProviderAuth `json:"auth,omitempty"`
	// Webhook configures inbound webhook verification.
	// +optional
	Webhook *SCMProviderWebhook `json:"webhook,omitempty"`
	// StatusReporting configures commit status / check run reporting of sync results.
	// +optional
	StatusReporting *SCMProviderStatusReporting `json:"statusReporting,omitempty"`
	// RateLimit bounds the request rate to the API.
	// +optional
	RateLimit *SCMProviderRateLimit `json:"rateLimit,omitempty"`
}

// SCMProviderAuth configures API authentication; at most one method may be set.
// +kubebuilder:validation:XValidation:rule="(has(self.githubApp) ? 1 : 0) + (has(self.tokenSecretRef) ? 1 : 0) + (has(self.basicAuthSecretRef) ? 1 : 0) <= 1",message="at most one of githubApp, tokenSecretRef or basicAuthSecretRef may be set"
type SCMProviderAuth struct {
	// GitHubApp authenticates as a GitHub App.
	// +optional
	GitHubApp *SCMProviderGitHubApp `json:"githubApp,omitempty"`
	// TokenSecretRef references an access token.
	// +optional
	TokenSecretRef *SecretKeyReference `json:"tokenSecretRef,omitempty"`
	// BasicAuthSecretRef references a Secret with username and password keys.
	// +optional
	BasicAuthSecretRef *LocalSecretReference `json:"basicAuthSecretRef,omitempty"`
}

// SCMProviderGitHubApp identifies a GitHub App.
type SCMProviderGitHubApp struct {
	// AppID of the GitHub App.
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appID"`
	// InstallationID of the App. When empty it is discovered per organization/repository.
	// +optional
	// +kubebuilder:validation:Minimum=1
	InstallationID *int64 `json:"installationID,omitempty"`
	// PrivateKeySecretRef references the App's PEM private key.
	PrivateKeySecretRef SecretKeyReference `json:"privateKeySecretRef"`
}

// SCMProviderWebhook configures inbound webhook verification.
type SCMProviderWebhook struct {
	// SecretRef references the shared webhook secret used to verify signatures.
	SecretRef SecretKeyReference `json:"secretRef"`
}

// SCMProviderStatusReporting configures reporting of sync results to commits.
type SCMProviderStatusReporting struct {
	// Enabled turns reporting on.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Mode is checks (GitHub App only) or statuses.
	// +optional
	// +kubebuilder:default=statuses
	Mode StatusReportingMode `json:"mode,omitempty"`
	// ContextPrefix prefixes the status context / check name.
	// +optional
	// +kubebuilder:default=patrol-cd
	// +kubebuilder:validation:MaxLength=100
	ContextPrefix string `json:"contextPrefix,omitempty"`
}

// SCMProviderRateLimit bounds the request rate to the API.
type SCMProviderRateLimit struct {
	// RequestsPerSecond is the sustained request rate.
	// +kubebuilder:validation:Minimum=1
	RequestsPerSecond int32 `json:"requestsPerSecond"`
	// Burst is the maximum burst size.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Burst int32 `json:"burst,omitempty"`
}

// SCMProviderStatus is the observed state of an SCMProvider.
type SCMProviderStatus struct {
	// Capabilities are the contract capabilities the provider implements (e.g. RepositoryLister,
	// PullRequestLister, CheckRunReporter).
	// +optional
	// +listType=set
	Capabilities []string `json:"capabilities,omitempty"`
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
	// RateLimit is the API rate limit observed in the last response.
	// +optional
	RateLimit *SCMProviderRateLimitStatus `json:"rateLimit,omitempty"`
	// LastCheckedAt is when the provider was last checked.
	// +optional
	LastCheckedAt *metav1.Time `json:"lastCheckedAt,omitempty"`
}

// SCMProviderRateLimitStatus is an observed API rate limit.
type SCMProviderRateLimitStatus struct {
	// Limit is the request quota of the window.
	// +optional
	Limit int64 `json:"limit,omitempty"`
	// Remaining is the remaining quota.
	// +optional
	Remaining int64 `json:"remaining,omitempty"`
	// ResetAt is when the quota resets.
	// +optional
	ResetAt *metav1.Time `json:"resetAt,omitempty"`
}
