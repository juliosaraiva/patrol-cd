package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultAppProjectName is the project used when Application.spec.project is empty.
const DefaultAppProjectName = "default"

// AppProject groups Applications and restricts what they may deploy (source repositories, resource
// kinds), where (destinations, sync windows), and who may act on them (roles, tokens).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=appprojects,singular=appproject,scope=Namespaced,shortName=pproj;pprojs,categories=patrol
// +kubebuilder:printcolumn:name="Description",type=string,JSONPath=`.spec.description`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AppProject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the project policy.
	Spec AppProjectSpec `json:"spec"`
	// Status is the observed state of the project.
	// +optional
	Status AppProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppProjectList is a list of AppProjects.
type AppProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AppProject `json:"items"`
}

func init() {
	register(&AppProject{}, &AppProjectList{})
}

// AppProjectSpec is the policy of an AppProject.
type AppProjectSpec struct {
	// SourceRepos are the repository URLs (globs, "!" negation) Applications may use.
	// +optional
	SourceRepos []string `json:"sourceRepos,omitempty"`
	// Destinations are the clusters and namespaces Applications may deploy to.
	// +optional
	Destinations []ApplicationDestination `json:"destinations,omitempty"`
	// Description of the project.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	Description string `json:"description,omitempty"`
	// Roles are project-scoped RBAC roles.
	// +optional
	Roles []ProjectRole `json:"roles,omitempty"`
	// ClusterResourceWhitelist lists the cluster-scoped kinds Applications may manage.
	// +optional
	ClusterResourceWhitelist []ClusterResourceRestrictionItem `json:"clusterResourceWhitelist,omitempty"`
	// NamespaceResourceBlacklist lists namespaced kinds Applications may not manage.
	// +optional
	NamespaceResourceBlacklist []metav1.GroupKind `json:"namespaceResourceBlacklist,omitempty"`
	// OrphanedResources configures orphaned-resource monitoring.
	// +optional
	OrphanedResources *OrphanedResourcesMonitorSettings `json:"orphanedResources,omitempty"`
	// SyncWindows restrict when syncs may run.
	// +optional
	SyncWindows SyncWindows `json:"syncWindows,omitempty"`
	// NamespaceResourceWhitelist lists the namespaced kinds Applications may manage (all if empty).
	// +optional
	NamespaceResourceWhitelist []metav1.GroupKind `json:"namespaceResourceWhitelist,omitempty"`
	// SignatureKeys are GPG key IDs commits must be signed with.
	//
	// Deprecated: use SourceIntegrity. Kept for migration; treated as a git GPG "head" policy for "*".
	// +optional
	SignatureKeys []SignatureKey `json:"signatureKeys,omitempty"`
	// ClusterResourceBlacklist lists cluster-scoped kinds Applications may not manage.
	// +optional
	ClusterResourceBlacklist []ClusterResourceRestrictionItem `json:"clusterResourceBlacklist,omitempty"`
	// SourceNamespaces are the namespaces (globs) Applications of this project may be created in.
	// +optional
	SourceNamespaces []string `json:"sourceNamespaces,omitempty"`
	// PermitOnlyProjectScopedClusters restricts destinations to Clusters with spec.project set to this
	// project.
	// +optional
	PermitOnlyProjectScopedClusters bool `json:"permitOnlyProjectScopedClusters,omitempty"`
	// DestinationServiceAccounts select the service account impersonated when syncing to a destination.
	// +optional
	DestinationServiceAccounts []ApplicationDestinationServiceAccount `json:"destinationServiceAccounts,omitempty"`
	// SourceIntegrity are verification requirements sources must meet before use.
	// +optional
	SourceIntegrity *SourceIntegrity `json:"sourceIntegrity,omitempty"`
}

// ProjectRole is a project-scoped RBAC role.
type ProjectRole struct {
	// Name of the role.
	Name string `json:"name"`
	// Description of the role.
	// +optional
	Description string `json:"description,omitempty"`
	// Policies are Casbin-style policy lines ("p, proj:<project>:<role>, <resource>, <action>, <object>, <effect>").
	// +optional
	Policies []string `json:"policies,omitempty"`
	// JWTTokens are tokens issued for the role.
	//
	// Deprecated: tokens are recorded in status.jwtTokensByRole; this field is read for migration.
	// +optional
	JWTTokens []JWTToken `json:"jwtTokens,omitempty"`
	// Groups are OIDC groups bound to the role.
	// +optional
	Groups []string `json:"groups,omitempty"`
}

// JWTToken records a token issued for a project role.
type JWTToken struct {
	// IssuedAt is the token's iat claim (Unix seconds).
	IssuedAt int64 `json:"iat"`
	// ExpiresAt is the token's exp claim (Unix seconds); 0 means no expiry.
	// +optional
	ExpiresAt int64 `json:"exp,omitempty"`
	// ID is the token's jti claim.
	// +optional
	ID string `json:"id,omitempty"`
}

// JWTTokens is a list of tokens.
type JWTTokens struct {
	// +optional
	Items []JWTToken `json:"items,omitempty"`
}

// ClusterResourceRestrictionItem selects cluster-scoped resources by group, kind and name glob.
type ClusterResourceRestrictionItem struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
	// Name is a glob of resource names; all names if empty.
	// +optional
	Name string `json:"name,omitempty"`
}

// OrphanedResourcesMonitorSettings configures orphaned-resource monitoring.
type OrphanedResourcesMonitorSettings struct {
	// Warn adds the OrphanedResourceWarning condition to Applications with orphaned resources.
	// +optional
	Warn *bool `json:"warn,omitempty"`
	// Ignore lists resources excluded from monitoring.
	// +optional
	Ignore []OrphanedResourceKey `json:"ignore,omitempty"`
}

// OrphanedResourceKey selects resources excluded from orphaned-resource monitoring.
type OrphanedResourceKey struct {
	// +optional
	Group string `json:"group,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
}

// SyncWindows is a list of sync windows.
type SyncWindows []SyncWindow

// SyncWindow allows or denies syncs during a recurring time window.
type SyncWindow struct {
	// Kind is "allow" or "deny".
	// +optional
	// +kubebuilder:validation:Enum=allow;deny
	Kind string `json:"kind,omitempty"`
	// Schedule is a 5-field cron expression or a descriptor (@hourly, @daily, ...).
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^\s*(@(yearly|annually|monthly|weekly|daily|midnight|hourly)|@every\s+\S+|\S+(\s+\S+){4})\s*$`
	Schedule string `json:"schedule,omitempty"`
	// Duration is how long the window stays open (Go duration, e.g. "1h").
	// +optional
	Duration string `json:"duration,omitempty"`
	// Applications are Application name globs the window applies to.
	// +optional
	Applications []string `json:"applications,omitempty"`
	// Namespaces are namespace globs the window applies to.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
	// Clusters are cluster name/server globs the window applies to.
	// +optional
	Clusters []string `json:"clusters,omitempty"`
	// ManualSync allows manual syncs while the window would block them.
	// +optional
	ManualSync bool `json:"manualSync,omitempty"`
	// TimeZone of the schedule (IANA name). Defaults to UTC.
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
	// UseAndOperator matches applications AND namespaces AND clusters instead of OR.
	// +optional
	UseAndOperator bool `json:"andOperator,omitempty"`
	// Description of the window (e.g. a change ticket).
	// +optional
	Description string `json:"description,omitempty"`
	// SyncOverrun lets syncs that started before a deny window (or inside an allow window) continue
	// after the window boundary.
	// +optional
	SyncOverrun bool `json:"syncOverrun,omitempty"`
}

// SignatureKey is a GPG key ID.
type SignatureKey struct {
	// KeyID in hexadecimal notation.
	KeyID string `json:"keyID"`
}

// ApplicationDestinationServiceAccount selects the service account impersonated for a destination.
type ApplicationDestinationServiceAccount struct {
	// Server is the destination server URL (glob).
	Server string `json:"server"`
	// Namespace is the destination namespace (glob).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// DefaultServiceAccount is the service account to impersonate ("<namespace>:<name>" or "<name>").
	DefaultServiceAccount string `json:"defaultServiceAccount"`
}

// SourceIntegrity are verification requirements for sources.
type SourceIntegrity struct {
	// Git holds the git verification policies.
	Git *SourceIntegrityGit `json:"git"`
}

// SourceIntegrityGit holds git verification policies.
type SourceIntegrityGit struct {
	Policies []SourceIntegrityGitPolicy `json:"policies"`
}

// SourceIntegrityGitPolicy is a verification policy for repositories matching Repos.
type SourceIntegrityGitPolicy struct {
	// Repos restrict the repositories the policy applies to.
	Repos []SourceIntegrityGitPolicyRepo `json:"repos"`
	// GPG verifies commit/tag signatures.
	GPG *SourceIntegrityGitPolicyGPG `json:"gpg"`
}

// SourceIntegrityGitPolicyRepo selects repositories by URL glob.
type SourceIntegrityGitPolicyRepo struct {
	// URL glob.
	URL string `json:"url"`
}

// SourceIntegrityGitPolicyGPGMode is the GPG verification mode.
// +kubebuilder:validation:Enum=none;head;strict
type SourceIntegrityGitPolicyGPGMode string

const (
	// SourceIntegrityGitPolicyGPGModeNone performs no verification.
	SourceIntegrityGitPolicyGPGModeNone SourceIntegrityGitPolicyGPGMode = "none"
	// SourceIntegrityGitPolicyGPGModeHead verifies the target revision (commit or annotated tag).
	SourceIntegrityGitPolicyGPGModeHead SourceIntegrityGitPolicyGPGMode = "head"
	// SourceIntegrityGitPolicyGPGModeStrict verifies the full ancestry of the target revision.
	SourceIntegrityGitPolicyGPGModeStrict SourceIntegrityGitPolicyGPGMode = "strict"
)

// SourceIntegrityGitPolicyGPG verifies GPG signatures against trusted keys.
type SourceIntegrityGitPolicyGPG struct {
	Mode SourceIntegrityGitPolicyGPGMode `json:"mode"`
	// Keys are the trusted key IDs; they must be present in the GPG keyring.
	Keys []string `json:"keys"`
}

// AppProjectStatus is the observed state of an AppProject.
type AppProjectStatus struct {
	// JWTTokensByRole lists the tokens issued per role.
	// +optional
	JWTTokensByRole map[string]JWTTokens `json:"jwtTokensByRole,omitempty"`
	// Conditions are the current project conditions.
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
