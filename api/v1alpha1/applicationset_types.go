package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ApplicationSet generates Applications from a template and the parameter sets produced by
// generators.
//
// Generators nested more than one level deep inside matrix/merge generators are stored as raw JSON
// (ApplicationSetNestedGenerator.Matrix/Merge) because CRD schemas cannot be recursive; the controller
// decodes them into NestedMatrixGenerator/NestedMergeGenerator.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=applicationsets,singular=applicationset,scope=Namespaced,shortName=pappset;pappsets,categories=patrol
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health.status`
// +kubebuilder:printcolumn:name="Resources",type=integer,JSONPath=`.status.resourcesCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ApplicationSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired state of the ApplicationSet.
	Spec ApplicationSetSpec `json:"spec"`
	// Status is the observed state, written by the ApplicationSet controller.
	// +optional
	Status ApplicationSetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ApplicationSetList is a list of ApplicationSets.
type ApplicationSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationSet `json:"items"`
}

func init() {
	register(&ApplicationSet{}, &ApplicationSetList{})
}

// ApplicationSetSpec is the desired state of an ApplicationSet.
type ApplicationSetSpec struct {
	// GoTemplate renders the template with Go text/template instead of fasttemplate.
	// +optional
	GoTemplate bool `json:"goTemplate,omitempty"`
	// Generators produce the parameter sets.
	// +kubebuilder:validation:MaxItems=128
	Generators []ApplicationSetGenerator `json:"generators"`
	// Template is the Application template.
	Template ApplicationSetTemplate `json:"template"`
	// SyncPolicy controls what the controller may do to generated Applications.
	// +optional
	SyncPolicy *ApplicationSetSyncPolicy `json:"syncPolicy,omitempty"`
	// Strategy controls the order in which generated Applications are updated.
	// +optional
	Strategy *ApplicationSetStrategy `json:"strategy,omitempty"`
	// PreservedFields lists annotations and labels of generated Applications that the controller
	// does not overwrite.
	// +optional
	PreservedFields *ApplicationPreservedFields `json:"preservedFields,omitempty"`
	// GoTemplateOptions are text/template options (e.g. "missingkey=error").
	// +optional
	GoTemplateOptions []string `json:"goTemplateOptions,omitempty"`
	// ApplyNestedSelectors enables selectors of generators nested in matrix/merge generators.
	//
	// Deprecated: ignored; nested selectors are always applied.
	// +optional
	ApplyNestedSelectors bool `json:"applyNestedSelectors,omitempty"`
	// IgnoreApplicationDifferences lists fields of generated Applications the controller does not
	// overwrite.
	// +optional
	IgnoreApplicationDifferences ApplicationSetIgnoreDifferences `json:"ignoreApplicationDifferences,omitempty"`
	// TemplatePatch is a templated strategic-merge patch applied to each generated Application
	// (goTemplate only).
	// +optional
	TemplatePatch *string `json:"templatePatch,omitempty"`
}

// ApplicationPreservedFields lists annotation and label keys preserved on generated Applications.
type ApplicationPreservedFields struct {
	// +optional
	Annotations []string `json:"annotations,omitempty"`
	// +optional
	Labels []string `json:"labels,omitempty"`
}

// ApplicationSetStrategyType selects how generated Applications are updated.
// +kubebuilder:validation:Enum=AllAtOnce;RollingSync
type ApplicationSetStrategyType string

const (
	// ApplicationSetStrategyAllAtOnce updates all Applications at once (default).
	ApplicationSetStrategyAllAtOnce ApplicationSetStrategyType = "AllAtOnce"
	// ApplicationSetStrategyRollingSync updates Applications step by step (progressive sync).
	ApplicationSetStrategyRollingSync ApplicationSetStrategyType = "RollingSync"
)

// ApplicationSetDeletionOrder selects the order in which generated Applications are deleted.
// +kubebuilder:validation:Enum=AllAtOnce;Reverse
type ApplicationSetDeletionOrder string

const (
	// ApplicationSetDeletionOrderAllAtOnce deletes all Applications at once (default).
	ApplicationSetDeletionOrderAllAtOnce ApplicationSetDeletionOrder = "AllAtOnce"
	// ApplicationSetDeletionOrderReverse deletes Applications in reverse rolling-sync step order.
	ApplicationSetDeletionOrderReverse ApplicationSetDeletionOrder = "Reverse"
)

// ApplicationSetStrategy controls how generated Applications are updated and deleted.
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'RollingSync' || has(self.rollingSync)",message="strategy.rollingSync is required when strategy.type is RollingSync"
// +kubebuilder:validation:XValidation:rule="!has(self.deletionOrder) || self.deletionOrder != 'Reverse' || (has(self.type) && self.type == 'RollingSync')",message="strategy.deletionOrder Reverse requires strategy.type RollingSync"
type ApplicationSetStrategy struct {
	// Type is AllAtOnce (default) or RollingSync.
	// +optional
	Type ApplicationSetStrategyType `json:"type,omitempty"`
	// RollingSync configures progressive sync steps.
	// +optional
	RollingSync *ApplicationSetRolloutStrategy `json:"rollingSync,omitempty"`
	// DeletionOrder is AllAtOnce (default) or Reverse (only with RollingSync). Unlike Argo CD, which
	// compares case-insensitively and ignores Reverse without RollingSync, both are rejected at
	// admission.
	// +optional
	DeletionOrder ApplicationSetDeletionOrder `json:"deletionOrder,omitempty"`
}

// ApplicationSetRolloutStrategy is a list of progressive sync steps.
type ApplicationSetRolloutStrategy struct {
	// +optional
	Steps []ApplicationSetRolloutStep `json:"steps,omitempty"`
}

// ApplicationSetRolloutStep selects the Applications updated in one step.
type ApplicationSetRolloutStep struct {
	// MatchExpressions select Applications by label.
	// +optional
	MatchExpressions []ApplicationMatchExpression `json:"matchExpressions,omitempty"`
	// MaxUpdate is the number or percentage of the step's Applications updated at once.
	// +optional
	// +kubebuilder:validation:XIntOrString
	MaxUpdate *intstr.IntOrString `json:"maxUpdate,omitempty"`
}

// ApplicationMatchExpression is a label selector requirement.
type ApplicationMatchExpression struct {
	// +optional
	Key string `json:"key,omitempty"`
	// Operator is In or NotIn (the only operators Argo CD evaluates in rolling sync steps).
	// +optional
	// +kubebuilder:validation:Enum=In;NotIn
	Operator string `json:"operator,omitempty"`
	// +optional
	Values []string `json:"values,omitempty"`
}

// ApplicationsSyncPolicy restricts what the controller may do to generated Applications.
// +kubebuilder:validation:Enum=create-only;create-update;create-delete;sync
type ApplicationsSyncPolicy string

const (
	ApplicationsSyncPolicyCreateOnly   ApplicationsSyncPolicy = "create-only"
	ApplicationsSyncPolicyCreateUpdate ApplicationsSyncPolicy = "create-update"
	ApplicationsSyncPolicyCreateDelete ApplicationsSyncPolicy = "create-delete"
	ApplicationsSyncPolicySync         ApplicationsSyncPolicy = "sync"
)

// ApplicationSetSyncPolicy controls what the controller may do to generated Applications.
type ApplicationSetSyncPolicy struct {
	// PreserveResourcesOnDeletion keeps generated Applications' resources when the ApplicationSet is
	// deleted (the Applications' resources finalizer is removed first).
	// +optional
	PreserveResourcesOnDeletion bool `json:"preserveResourcesOnDeletion,omitempty"`
	// ApplicationsSync restricts create/update/delete of generated Applications. Default sync.
	// +optional
	ApplicationsSync *ApplicationsSyncPolicy `json:"applicationsSync,omitempty"`
}

// ApplicationSetIgnoreDifferences lists fields of generated Applications that are not overwritten.
type ApplicationSetIgnoreDifferences []ApplicationSetResourceIgnoreDifferences

// ApplicationSetResourceIgnoreDifferences selects fields of generated Applications.
type ApplicationSetResourceIgnoreDifferences struct {
	// Name of the Application; all Applications if empty.
	// +optional
	Name string `json:"name,omitempty"`
	// JSONPointers of ignored fields.
	// +optional
	JSONPointers []string `json:"jsonPointers,omitempty"`
	// JQPathExpressions of ignored fields.
	// +optional
	JQPathExpressions []string `json:"jqPathExpressions,omitempty"`
}

// ApplicationSetTemplate is the template of generated Applications.
type ApplicationSetTemplate struct {
	ApplicationSetTemplateMeta `json:"metadata"`
	Spec                       ApplicationSpec `json:"spec"`
}

// ApplicationSetTemplateMeta is the subset of ObjectMeta that may be templated.
type ApplicationSetTemplateMeta struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	Finalizers []string `json:"finalizers,omitempty"`
}

// ApplicationSetGenerator is a top-level generator. Exactly one generator field should be set.
type ApplicationSetGenerator struct {
	// +optional
	List *ListGenerator `json:"list,omitempty"`
	// +optional
	Clusters *ClusterGenerator `json:"clusters,omitempty"`
	// +optional
	Git *GitGenerator `json:"git,omitempty"`
	// +optional
	SCMProvider *SCMProviderGenerator `json:"scmProvider,omitempty"`
	// +optional
	ClusterDecisionResource *DuckTypeGenerator `json:"clusterDecisionResource,omitempty"`
	// +optional
	PullRequest *PullRequestGenerator `json:"pullRequest,omitempty"`
	// +optional
	Matrix *MatrixGenerator `json:"matrix,omitempty"`
	// +optional
	Merge *MergeGenerator `json:"merge,omitempty"`
	// Selector post-filters the generated parameter sets.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// +optional
	Plugin *PluginGenerator `json:"plugin,omitempty"`
}

// ApplicationSetNestedGenerator is a generator inside a matrix or merge generator. Deeper matrix and
// merge generators are raw JSON decoded as NestedMatrixGenerator / NestedMergeGenerator.
type ApplicationSetNestedGenerator struct {
	// +optional
	List *ListGenerator `json:"list,omitempty"`
	// +optional
	Clusters *ClusterGenerator `json:"clusters,omitempty"`
	// +optional
	Git *GitGenerator `json:"git,omitempty"`
	// +optional
	SCMProvider *SCMProviderGenerator `json:"scmProvider,omitempty"`
	// +optional
	ClusterDecisionResource *DuckTypeGenerator `json:"clusterDecisionResource,omitempty"`
	// +optional
	PullRequest *PullRequestGenerator `json:"pullRequest,omitempty"`
	// Matrix has the form of NestedMatrixGenerator.
	// +optional
	Matrix *apiextensionsv1.JSON `json:"matrix,omitempty"`
	// Merge has the form of NestedMergeGenerator.
	// +optional
	Merge *apiextensionsv1.JSON `json:"merge,omitempty"`
	// Selector post-filters the generated parameter sets.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// +optional
	Plugin *PluginGenerator `json:"plugin,omitempty"`
}

// ApplicationSetNestedGenerators is a list of nested generators.
type ApplicationSetNestedGenerators []ApplicationSetNestedGenerator

// ApplicationSetTerminalGenerator is a generator nested two levels deep; it cannot be a matrix or
// merge generator. Not part of the CRD schema: decoded from ApplicationSetNestedGenerator.Matrix/Merge.
type ApplicationSetTerminalGenerator struct {
	// +optional
	List *ListGenerator `json:"list,omitempty"`
	// +optional
	Clusters *ClusterGenerator `json:"clusters,omitempty"`
	// +optional
	Git *GitGenerator `json:"git,omitempty"`
	// +optional
	SCMProvider *SCMProviderGenerator `json:"scmProvider,omitempty"`
	// +optional
	ClusterDecisionResource *DuckTypeGenerator `json:"clusterDecisionResource,omitempty"`
	// +optional
	PullRequest *PullRequestGenerator `json:"pullRequest,omitempty"`
	// +optional
	Plugin *PluginGenerator `json:"plugin,omitempty"`
	// Selector post-filters the generated parameter sets.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// ApplicationSetTerminalGenerators is a list of terminal generators.
type ApplicationSetTerminalGenerators []ApplicationSetTerminalGenerator

// NestedMatrixGenerator is the decoded form of ApplicationSetNestedGenerator.Matrix. Like
// MatrixGenerator it has exactly two generators; ToNestedMatrixGenerator enforces this because the
// CRD schema cannot see inside the raw JSON.
type NestedMatrixGenerator struct {
	Generators ApplicationSetTerminalGenerators `json:"generators"`
}

// NestedMergeGenerator is the decoded form of ApplicationSetNestedGenerator.Merge. Like
// MergeGenerator it has at least two generators; ToNestedMergeGenerator enforces this.
type NestedMergeGenerator struct {
	Generators ApplicationSetTerminalGenerators `json:"generators"`
	MergeKeys  []string                         `json:"mergeKeys"`
}

// ListGenerator generates one parameter set per element.
type ListGenerator struct {
	// Elements are the parameter sets.
	// +optional
	Elements []apiextensionsv1.JSON `json:"elements,omitempty"`
	// Template overrides the ApplicationSet template for this generator.
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// ElementsYaml is a YAML list of parameter sets (typically templated).
	// +optional
	ElementsYaml string `json:"elementsYaml,omitempty"`
}

// MatrixGenerator generates the cartesian product of two generators.
type MatrixGenerator struct {
	// Generators are exactly two child generators.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	Generators []ApplicationSetNestedGenerator `json:"generators"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
}

// MergeGenerator merges the parameter sets of generators on MergeKeys.
type MergeGenerator struct {
	// Generators are two or more child generators; the first is the base the others merge into.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=64
	Generators []ApplicationSetNestedGenerator `json:"generators"`
	MergeKeys  []string                        `json:"mergeKeys"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
}

// ClusterGenerator generates one parameter set per Cluster matching Selector.
type ClusterGenerator struct {
	// Selector matches Cluster labels.
	// +optional
	Selector metav1.LabelSelector `json:"selector,omitempty,omitzero"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// Values are passed to the template as parameters.
	// +optional
	Values map[string]string `json:"values,omitempty"`
	// FlatList returns all clusters as a single "clusters" parameter.
	// +optional
	FlatList bool `json:"flatList,omitempty"`
}

// DuckTypeGenerator generates parameter sets from the status of a duck-typed resource (e.g. an OCM
// PlacementDecision).
type DuckTypeGenerator struct {
	// ConfigMapRef names the ConfigMap with the duck-type definition (apiVersion, kind, matchKey).
	ConfigMapRef string `json:"configMapRef"`
	// Name of the duck-typed resource.
	// +optional
	Name string `json:"name,omitempty"`
	// RequeueAfterSeconds is the polling interval.
	// +optional
	RequeueAfterSeconds *int64 `json:"requeueAfterSeconds,omitempty"`
	// LabelSelector selects duck-typed resources.
	// +optional
	LabelSelector metav1.LabelSelector `json:"labelSelector,omitempty,omitzero"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// Values are passed to the template as parameters.
	// +optional
	Values map[string]string `json:"values,omitempty"`
}

// GitGenerator generates parameter sets from directories or files of a git repository.
type GitGenerator struct {
	RepoURL string `json:"repoURL"`
	// +optional
	Directories []GitDirectoryGeneratorItem `json:"directories,omitempty"`
	// +optional
	Files    []GitFileGeneratorItem `json:"files,omitempty"`
	Revision string                 `json:"revision"`
	// +optional
	RequeueAfterSeconds *int64 `json:"requeueAfterSeconds,omitempty"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// PathParamPrefix prefixes the path parameters (useful with matrix of two git generators).
	// +optional
	PathParamPrefix string `json:"pathParamPrefix,omitempty"`
	// Values are passed to the template as parameters.
	// +optional
	Values map[string]string `json:"values,omitempty"`
}

// GitDirectoryGeneratorItem is a directory glob.
type GitDirectoryGeneratorItem struct {
	Path string `json:"path"`
	// +optional
	Exclude bool `json:"exclude,omitempty"`
}

// GitFileGeneratorItem is a file glob.
type GitFileGeneratorItem struct {
	Path string `json:"path"`
	// +optional
	Exclude bool `json:"exclude,omitempty"`
}

// SecretRef references a key of a Secret in the ApplicationSet's namespace.
type SecretRef struct {
	SecretName string `json:"secretName"`
	Key        string `json:"key"`
}

// ConfigMapKeyRef references a key of a ConfigMap in the ApplicationSet's namespace.
type ConfigMapKeyRef struct {
	ConfigMapName string `json:"configMapName"`
	Key           string `json:"key"`
}

// SCMProviderGenerator generates one parameter set per repository (or branch) found through an SCM
// API. Each provider block may reference a Patrol SCMProvider (providerRef) instead of inline
// credentials; the referenced SCMProvider must be of the matching type.
type SCMProviderGenerator struct {
	// +optional
	Github *SCMProviderGeneratorGithub `json:"github,omitempty"`
	// +optional
	Gitlab *SCMProviderGeneratorGitlab `json:"gitlab,omitempty"`
	// +optional
	Bitbucket *SCMProviderGeneratorBitbucket `json:"bitbucket,omitempty"`
	// +optional
	BitbucketServer *SCMProviderGeneratorBitbucketServer `json:"bitbucketServer,omitempty"`
	// +optional
	Gitea *SCMProviderGeneratorGitea `json:"gitea,omitempty"`
	// +optional
	AzureDevOps *SCMProviderGeneratorAzureDevOps `json:"azureDevOps,omitempty"`
	// Filters select repositories; all filters of one entry must pass (AND), entries are OR'd.
	// +optional
	Filters []SCMProviderGeneratorFilter `json:"filters,omitempty"`
	// CloneProtocol is the protocol of the generated clone URL (provider default, usually ssh).
	// +optional
	CloneProtocol string `json:"cloneProtocol,omitempty"`
	// +optional
	RequeueAfterSeconds *int64 `json:"requeueAfterSeconds,omitempty"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// Values are passed to the template as parameters.
	// +optional
	Values map[string]string `json:"values,omitempty"`
	// +optional
	AWSCodeCommit *SCMProviderGeneratorAWSCodeCommit `json:"awsCodeCommit,omitempty"`
}

// SCMProviderGeneratorGitea scans a Gitea organization or user.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || !has(self.tokenRef)",message="providerRef and tokenRef are mutually exclusive"
type SCMProviderGeneratorGitea struct {
	// Owner is the Gitea organization or user.
	Owner string `json:"owner"`
	// API is the Gitea URL (e.g. https://gitea.example.com/).
	API string `json:"api"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// +optional
	ExcludeArchivedRepos bool `json:"excludeArchivedRepos,omitempty"`
	// ProviderRef references an SCMProvider of type gitea for API URL and credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorGithub scans a GitHub organization.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || (!has(self.tokenRef) && !has(self.appSecretName))",message="providerRef is mutually exclusive with tokenRef and appSecretName"
type SCMProviderGeneratorGithub struct {
	// Organization to scan.
	Organization string `json:"organization"`
	// API is the GitHub API URL (default https://api.github.com/).
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// AppSecretName names a Secret with GitHub App credentials.
	// +optional
	AppSecretName string `json:"appSecretName,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// +optional
	ExcludeArchivedRepos bool `json:"excludeArchivedRepos,omitempty"`
	// ProviderRef references an SCMProvider of type github for API URL and credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorGitlab scans a GitLab group.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || !has(self.tokenRef)",message="providerRef and tokenRef are mutually exclusive"
type SCMProviderGeneratorGitlab struct {
	// Group is the group ID (recommended) or full path.
	Group string `json:"group"`
	// +optional
	IncludeSubgroups bool `json:"includeSubgroups,omitempty"`
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// IncludeSharedProjects includes projects shared into the group (default true).
	// +optional
	IncludeSharedProjects *bool `json:"includeSharedProjects,omitempty"`
	// Topic filters repositories by topic.
	// +optional
	Topic string `json:"topic,omitempty"`
	// +optional
	CARef *ConfigMapKeyRef `json:"caRef,omitempty"`
	// +optional
	IncludeArchivedRepos bool `json:"includeArchivedRepos,omitempty"`
	// ProviderRef references an SCMProvider of type gitlab for API URL and credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorBitbucket scans a Bitbucket Cloud workspace.
// +kubebuilder:validation:XValidation:rule="has(self.providerRef) != (has(self.user) || has(self.appPasswordRef))",message="set either providerRef or user and appPasswordRef"
// +kubebuilder:validation:XValidation:rule="has(self.user) == has(self.appPasswordRef)",message="user and appPasswordRef must be set together"
type SCMProviderGeneratorBitbucket struct {
	// Owner is the workspace.
	Owner string `json:"owner"`
	// User authenticates with AppPasswordRef. Required unless providerRef is set.
	// +optional
	User string `json:"user,omitempty"`
	// AppPasswordRef references the app password. Required unless providerRef is set.
	// +optional
	AppPasswordRef *SecretRef `json:"appPasswordRef,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// ProviderRef references an SCMProvider of type bitbucketCloud for credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorBitbucketServer scans a Bitbucket Server/Data Center project.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || (!has(self.basicAuth) && !has(self.bearerToken))",message="providerRef is mutually exclusive with basicAuth and bearerToken"
type SCMProviderGeneratorBitbucketServer struct {
	Project string `json:"project"`
	// API is the REST API URL.
	API string `json:"api"`
	// +optional
	BasicAuth *BasicAuthBitbucketServer `json:"basicAuth,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// +optional
	BearerToken *BearerTokenBitbucket `json:"bearerToken,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// +optional
	CARef *ConfigMapKeyRef `json:"caRef,omitempty"`
	// ProviderRef references an SCMProvider of type bitbucketServer for credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorAzureDevOps scans an Azure DevOps team project.
// +kubebuilder:validation:XValidation:rule="has(self.providerRef) != has(self.accessTokenRef)",message="set exactly one of providerRef or accessTokenRef"
type SCMProviderGeneratorAzureDevOps struct {
	Organization string `json:"organization"`
	// API is the Azure DevOps URL (default https://dev.azure.com).
	// +optional
	API         string `json:"api,omitempty"`
	TeamProject string `json:"teamProject"`
	// AccessTokenRef references a personal access token. Required unless providerRef is set.
	// +optional
	AccessTokenRef *SecretRef `json:"accessTokenRef,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// ProviderRef references an SCMProvider of type azureDevOps for credentials.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// TagFilter filters AWS CodeCommit repositories by tag.
type TagFilter struct {
	Key string `json:"key"`
	// +optional
	Value string `json:"value,omitempty"`
}

// SCMProviderGeneratorAWSCodeCommit discovers AWS CodeCommit repositories.
type SCMProviderGeneratorAWSCodeCommit struct {
	// +optional
	TagFilters []TagFilter `json:"tagFilters,omitempty"`
	// Role is an IAM role to assume for cross-account discovery.
	// +optional
	Role string `json:"role,omitempty"`
	// Region to discover repositories in (default: the controller's region).
	// +optional
	Region string `json:"region,omitempty"`
	// +optional
	AllBranches bool `json:"allBranches,omitempty"`
	// ProviderRef references an SCMProvider of type awsCodeCommit.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// SCMProviderGeneratorFilter is one repository filter; all set fields must match.
type SCMProviderGeneratorFilter struct {
	// RepositoryMatch is a regular expression matched against repository names.
	// +optional
	RepositoryMatch *string `json:"repositoryMatch,omitempty"`
	// PathsExist are paths that must all exist.
	// +optional
	PathsExist []string `json:"pathsExist,omitempty"`
	// PathsDoNotExist are paths that must all be absent.
	// +optional
	PathsDoNotExist []string `json:"pathsDoNotExist,omitempty"`
	// LabelMatch is a regular expression that must match at least one label/topic.
	// +optional
	LabelMatch *string `json:"labelMatch,omitempty"`
	// BranchMatch is a regular expression matched against branch names.
	// +optional
	BranchMatch *string `json:"branchMatch,omitempty"`
}

// PullRequestGenerator generates one parameter set per open pull request. Each provider block may
// reference a Patrol SCMProvider (providerRef) instead of inline credentials.
type PullRequestGenerator struct {
	// +optional
	Github *PullRequestGeneratorGithub `json:"github,omitempty"`
	// +optional
	GitLab *PullRequestGeneratorGitLab `json:"gitlab,omitempty"`
	// +optional
	Gitea *PullRequestGeneratorGitea `json:"gitea,omitempty"`
	// +optional
	BitbucketServer *PullRequestGeneratorBitbucketServer `json:"bitbucketServer,omitempty"`
	// Filters select pull requests; all fields of one entry must match, entries are OR'd.
	// +optional
	Filters []PullRequestGeneratorFilter `json:"filters,omitempty"`
	// +optional
	RequeueAfterSeconds *int64 `json:"requeueAfterSeconds,omitempty"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// +optional
	Bitbucket *PullRequestGeneratorBitbucket `json:"bitbucket,omitempty"`
	// +optional
	AzureDevOps *PullRequestGeneratorAzureDevOps `json:"azuredevops,omitempty"`
	// Values are passed to the template as parameters.
	// +optional
	Values map[string]string `json:"values,omitempty"`
	// ContinueOnRepoNotFoundError generates no parameters instead of failing when the repository is
	// not found.
	// +optional
	ContinueOnRepoNotFoundError bool `json:"continueOnRepoNotFoundError,omitempty"`
}

// PullRequestGeneratorGitea lists Gitea pull requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || !has(self.tokenRef)",message="providerRef and tokenRef are mutually exclusive"
type PullRequestGeneratorGitea struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	API   string `json:"api"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// Labels filter pull requests.
	// +optional
	Labels []string `json:"labels,omitempty"`
	// ProviderRef references an SCMProvider of type gitea.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// PullRequestGeneratorAzureDevOps lists Azure DevOps pull requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || !has(self.tokenRef)",message="providerRef and tokenRef are mutually exclusive"
type PullRequestGeneratorAzureDevOps struct {
	Organization string `json:"organization"`
	Project      string `json:"project"`
	Repo         string `json:"repo"`
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	Labels []string `json:"labels,omitempty"`
	// ProviderRef references an SCMProvider of type azureDevOps.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// PullRequestGeneratorGithub lists GitHub pull requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || (!has(self.tokenRef) && !has(self.appSecretName))",message="providerRef is mutually exclusive with tokenRef and appSecretName"
type PullRequestGeneratorGithub struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	AppSecretName string `json:"appSecretName,omitempty"`
	// +optional
	Labels []string `json:"labels,omitempty"`
	// ProviderRef references an SCMProvider of type github.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// PullRequestGeneratorGitLab lists GitLab merge requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || !has(self.tokenRef)",message="providerRef and tokenRef are mutually exclusive"
type PullRequestGeneratorGitLab struct {
	// Project is the project ID or full path.
	Project string `json:"project"`
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	TokenRef *SecretRef `json:"tokenRef,omitempty"`
	// +optional
	Labels []string `json:"labels,omitempty"`
	// PullRequestState filters by state (opened, closed, merged, locked); all if empty.
	// +optional
	PullRequestState string `json:"pullRequestState,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// +optional
	CARef *ConfigMapKeyRef `json:"caRef,omitempty"`
	// ProviderRef references an SCMProvider of type gitlab.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// PullRequestGeneratorBitbucketServer lists Bitbucket Server pull requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || (!has(self.basicAuth) && !has(self.bearerToken))",message="providerRef is mutually exclusive with basicAuth and bearerToken"
type PullRequestGeneratorBitbucketServer struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	API     string `json:"api"`
	// +optional
	BasicAuth *BasicAuthBitbucketServer `json:"basicAuth,omitempty"`
	// +optional
	BearerToken *BearerTokenBitbucket `json:"bearerToken,omitempty"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// +optional
	CARef *ConfigMapKeyRef `json:"caRef,omitempty"`
	// ProviderRef references an SCMProvider of type bitbucketServer.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// PullRequestGeneratorBitbucket lists Bitbucket Cloud pull requests.
// +kubebuilder:validation:XValidation:rule="!has(self.providerRef) || (!has(self.basicAuth) && !has(self.bearerToken))",message="providerRef is mutually exclusive with basicAuth and bearerToken"
type PullRequestGeneratorBitbucket struct {
	// Owner is the workspace.
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	// API is the REST API URL (default https://api.bitbucket.org/2.0).
	// +optional
	API string `json:"api,omitempty"`
	// +optional
	BasicAuth *BasicAuthBitbucketServer `json:"basicAuth,omitempty"`
	// +optional
	BearerToken *BearerTokenBitbucketCloud `json:"bearerToken,omitempty"`
	// ProviderRef references an SCMProvider of type bitbucketCloud.
	// +optional
	ProviderRef *SCMProviderReference `json:"providerRef,omitempty"`
}

// BearerTokenBitbucket is a Bitbucket Server access token.
type BearerTokenBitbucket struct {
	TokenRef *SecretRef `json:"tokenRef"`
}

// BearerTokenBitbucketCloud is a Bitbucket Cloud app token.
type BearerTokenBitbucketCloud struct {
	TokenRef *SecretRef `json:"tokenRef"`
}

// BasicAuthBitbucketServer is a username and password (or personal access token).
type BasicAuthBitbucketServer struct {
	Username    string     `json:"username"`
	PasswordRef *SecretRef `json:"passwordRef"`
}

// PullRequestGeneratorFilter is one pull request filter; all set fields must match.
type PullRequestGeneratorFilter struct {
	// +optional
	BranchMatch *string `json:"branchMatch,omitempty"`
	// +optional
	TargetBranchMatch *string `json:"targetBranchMatch,omitempty"`
	// +optional
	TitleMatch *string `json:"titleMatch,omitempty"`
}

// PluginConfigMapRef names the ConfigMap that configures a generator plugin.
type PluginConfigMapRef struct {
	Name string `json:"name"`
}

// PluginParameters are arbitrary parameters passed to a generator plugin.
type PluginParameters map[string]apiextensionsv1.JSON

// PluginInput is the input of a generator plugin.
type PluginInput struct {
	// +optional
	Parameters PluginParameters `json:"parameters,omitempty"`
}

// PluginGenerator obtains parameter sets from an HTTP generator plugin.
type PluginGenerator struct {
	ConfigMapRef PluginConfigMapRef `json:"configMapRef"`
	// +optional
	Input PluginInput `json:"input,omitempty,omitzero"`
	// +optional
	RequeueAfterSeconds *int64 `json:"requeueAfterSeconds,omitempty"`
	// +optional
	Template ApplicationSetTemplate `json:"template,omitempty,omitzero"`
	// Values are passed to the template (not to the plugin).
	// +optional
	Values map[string]string `json:"values,omitempty"`
}

// ProgressiveSyncStatusCode is the progressive sync status of a generated Application.
type ProgressiveSyncStatusCode string

const (
	// ProgressiveSyncWaiting: the Application waits for its step.
	ProgressiveSyncWaiting ProgressiveSyncStatusCode = "Waiting"
	// ProgressiveSyncPending: a sync was requested; no status reported yet.
	ProgressiveSyncPending ProgressiveSyncStatusCode = "Pending"
	// ProgressiveSyncProgressing: synced but not yet Healthy.
	ProgressiveSyncProgressing ProgressiveSyncStatusCode = "Progressing"
	// ProgressiveSyncHealthy: synced and Healthy.
	ProgressiveSyncHealthy ProgressiveSyncStatusCode = "Healthy"
)

// ApplicationSetStatus is the observed state of an ApplicationSet.
type ApplicationSetStatus struct {
	// Conditions are the current ApplicationSet conditions.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// ApplicationStatus is the progressive sync status of each generated Application.
	// +optional
	ApplicationStatus []ApplicationSetApplicationStatus `json:"applicationStatus,omitempty"`
	// Resources lists generated Applications (bounded by the controller).
	// +optional
	Resources []ResourceStatus `json:"resources,omitempty"`
	// ResourcesCount is the number of generated Applications; may exceed len(resources).
	// +optional
	ResourcesCount int64 `json:"resourcesCount,omitempty"`
	// Health is derived from the conditions (ErrorOccurred → Degraded, RolloutProgressing →
	// Progressing, ResourcesUpToDate → Healthy).
	// +optional
	Health HealthStatus `json:"health,omitempty"`
	// ObservedGeneration is the metadata.generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ApplicationSetApplicationStatus is the progressive sync status of one generated Application.
type ApplicationSetApplicationStatus struct {
	// Application is the name of the Application.
	Application string `json:"application"`
	// LastTransitionTime is when Status last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// Message describes the status.
	// +optional
	Message string `json:"message,omitempty"`
	// Status is the progressive sync status.
	Status ProgressiveSyncStatusCode `json:"status"`
	// Step is the rolling sync step (1-based) of the Application.
	// +optional
	Step string `json:"step,omitempty"`
	// TargetRevisions are the revisions the Application should be synced to.
	// +optional
	TargetRevisions []string `json:"targetRevisions,omitempty"`
}
