// Package apiclient is the contract of the repo-server internal API (ADR-0006): request and
// response types shared by the repo-server and its callers (controller, API server,
// ApplicationSet controller, commit-server), and the Client interface they program against.
//
// Callers resolve repository credentials from Repository/RepositoryCredential/SCMProvider
// objects and short-lived tokens (GitHub App installation tokens, cloud tokens) before
// calling; the repo-server never reads Kubernetes Secrets and never holds GitHub App
// private keys. Trust material that is not secret (SSH known hosts, TLS CA bundles, GPG
// public keys) is mounted into the repo-server from ConfigMaps.
package apiclient

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Repository is the resolved access material for one repository. Secret-bearing fields are
// redacted by LogValue.
type Repository struct {
	URL     string                  `json:"url"`
	Type    v1alpha1.RepositoryType `json:"type,omitempty"` // git (default), helm, oci
	Name    string                  `json:"name,omitempty"`
	Project string                  `json:"project,omitempty"`

	Insecure              bool   `json:"insecure,omitempty"`
	InsecureIgnoreHostKey bool   `json:"insecureIgnoreHostKey,omitempty"`
	EnableLFS             bool   `json:"enableLFS,omitempty"`
	EnableOCI             bool   `json:"enableOCI,omitempty"`
	ForceHTTPBasicAuth    bool   `json:"forceHTTPBasicAuth,omitempty"`
	InsecureOCIForceHTTP  bool   `json:"insecureOCIForceHTTP,omitempty"`
	Proxy                 string `json:"proxy,omitempty"`
	NoProxy               string `json:"noProxy,omitempty"`
	Depth                 int64  `json:"depth,omitempty"`

	// Credentials. For GitHub Apps and cloud identities the caller mints a short-lived
	// token and passes it as Username/Password (git over HTTPS) or BearerToken.
	Username          string `json:"username,omitempty"`
	Password          string `json:"password,omitempty"`
	BearerToken       string `json:"bearerToken,omitempty"`
	SSHPrivateKey     string `json:"sshPrivateKey,omitempty"`
	TLSClientCertData string `json:"tlsClientCertData,omitempty"`
	TLSClientCertKey  string `json:"tlsClientCertKey,omitempty"`
	// CredentialsExpireAt is set when the credentials are short-lived; caches keyed by
	// credentials must not outlive it.
	CredentialsExpireAt *time.Time `json:"credentialsExpireAt,omitempty"`
}

// HasCredentials reports whether any credential material is set.
func (r Repository) HasCredentials() bool {
	return r.Username != "" || r.Password != "" || r.BearerToken != "" || r.SSHPrivateKey != "" ||
		r.TLSClientCertData != "" || r.TLSClientCertKey != ""
}

// LogValue redacts credentials.
func (r Repository) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", r.URL),
		slog.String("type", string(r.Type)),
		slog.String("project", r.Project),
		slog.Bool("hasCredentials", r.HasCredentials()),
	)
}

// RepositoryCredentialTemplate is a URL-prefix credential template, used for Helm
// dependency repositories that are not registered individually.
type RepositoryCredentialTemplate struct {
	URLPrefix string     `json:"urlPrefix"`
	Access    Repository `json:"access"` // URL field unused
}

// LogValue redacts credentials.
func (t RepositoryCredentialTemplate) LogValue() slog.Value {
	return slog.GroupValue(slog.String("urlPrefix", t.URLPrefix), slog.Bool("hasCredentials", t.Access.HasCredentials()))
}

// RefTarget is a multi-source `ref` source referenced by `$<ref>/path` value files.
type RefTarget struct {
	Repo           Repository `json:"repo"`
	TargetRevision string     `json:"targetRevision"`
	Chart          string     `json:"chart,omitempty"`
}

// KustomizeOptions carries kustomize settings from patrol-cm.
type KustomizeOptions struct {
	// BuildOptions is `kustomize.buildOptions`.
	BuildOptions string `json:"buildOptions,omitempty"`
	// Versions maps `kustomize.path.<version>` names to binary paths, and
	// VersionBuildOptions maps `kustomize.buildOptions.<version>`.
	Versions            map[string]string `json:"versions,omitempty"`
	VersionBuildOptions map[string]string `json:"versionBuildOptions,omitempty"`
}

// HelmOptions carries Helm settings from patrol-cm.
type HelmOptions struct {
	ValuesFileSchemes []string `json:"valuesFileSchemes,omitempty"`
}

// ManifestRequest asks for the rendered manifests of one source.
type ManifestRequest struct {
	Repo Repository `json:"repo"`
	// Revision may be unresolved (branch, tag, constraint) or a resolved commit/version.
	Revision        string `json:"revision"`
	NoCache         bool   `json:"noCache,omitempty"`
	NoRevisionCache bool   `json:"noRevisionCache,omitempty"`

	AppName      string `json:"appName"`
	AppNamespace string `json:"appNamespace,omitempty"`
	// Namespace is the destination namespace.
	Namespace string                     `json:"namespace,omitempty"`
	Source    v1alpha1.ApplicationSource `json:"source"`

	// Repos are all registered repositories visible to the app's project (used for Helm
	// dependencies); HelmRepoCreds are credential templates for Helm dependencies.
	Repos         []Repository                   `json:"repos,omitempty"`
	HelmRepoCreds []RepositoryCredentialTemplate `json:"helmRepoCreds,omitempty"`

	KustomizeOptions KustomizeOptions `json:"kustomizeOptions,omitempty"`
	HelmOptions      HelmOptions      `json:"helmOptions,omitempty"`
	KubeVersion      string           `json:"kubeVersion,omitempty"`
	APIVersions      []string         `json:"apiVersions,omitempty"`

	InstanceLabelKey   string          `json:"instanceLabelKey,omitempty"`
	TrackingMethod     string          `json:"trackingMethod,omitempty"`
	InstallationID     string          `json:"installationID,omitempty"`
	EnabledSourceTypes map[string]bool `json:"enabledSourceTypes,omitempty"`

	HasMultipleSources bool                 `json:"hasMultipleSources,omitempty"`
	RefSources         map[string]RefTarget `json:"refSources,omitempty"`

	ProjectName        string   `json:"projectName,omitempty"`
	ProjectSourceRepos []string `json:"projectSourceRepos,omitempty"`

	// ManifestGeneratePaths is the app's manifest-generate-paths annotation value.
	ManifestGeneratePaths string `json:"manifestGeneratePaths,omitempty"`
	// SourceIntegrity constrains verification of Git sources (from the AppProject).
	SourceIntegrity *v1alpha1.SourceIntegrity `json:"sourceIntegrity,omitempty"`
}

// SourceIntegrityResult is the verification outcome for a Git source.
type SourceIntegrityResult struct {
	Checks []SourceIntegrityCheck `json:"checks,omitempty"`
}

// SourceIntegrityCheck is one verification check.
type SourceIntegrityCheck struct {
	Name     string   `json:"name"`
	Problems []string `json:"problems,omitempty"`
}

// IsValid reports whether no check reported a problem.
func (r *SourceIntegrityResult) IsValid() bool {
	if r == nil {
		return true
	}
	for _, c := range r.Checks {
		if len(c.Problems) > 0 {
			return false
		}
	}
	return true
}

// ManifestResponse holds rendered manifests (each a JSON-encoded object).
type ManifestResponse struct {
	Manifests  []string `json:"manifests"`
	Namespace  string   `json:"namespace,omitempty"`
	Server     string   `json:"server,omitempty"`
	Revision   string   `json:"revision"`
	SourceType string   `json:"sourceType"`
	// Commands lists the rendering commands executed (shown by hydrator/debug views;
	// never contains credentials).
	Commands              []string               `json:"commands,omitempty"`
	VerifyResult          string                 `json:"verifyResult,omitempty"`
	SourceIntegrityResult *SourceIntegrityResult `json:"sourceIntegrityResult,omitempty"`
}

// ResolveRevisionRequest resolves a possibly-symbolic revision.
type ResolveRevisionRequest struct {
	Repo         Repository                 `json:"repo"`
	App          string                     `json:"app,omitempty"`
	Revision     string                     `json:"revision"`
	Source       v1alpha1.ApplicationSource `json:"source"`
	SourceIndex  int                        `json:"sourceIndex,omitempty"`
	ForceRefresh bool                       `json:"forceRefresh,omitempty"`
}

// ResolveRevisionResponse is the immutable revision.
type ResolveRevisionResponse struct {
	Revision          string `json:"revision"`
	AmbiguousRevision string `json:"ambiguousRevision"`
}

// ListRefsRequest lists branches and tags (git) or tags (OCI).
type ListRefsRequest struct {
	Repo Repository `json:"repo"`
}

// Refs is the ref listing.
type Refs struct {
	Branches []string `json:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// ListAppsRequest discovers application directories at a revision.
type ListAppsRequest struct {
	Repo               Repository      `json:"repo"`
	Revision           string          `json:"revision"`
	EnabledSourceTypes map[string]bool `json:"enabledSourceTypes,omitempty"`
}

// AppList maps paths to detected source types.
type AppList struct {
	Apps map[string]string `json:"apps"`
}

// PluginInfo describes a config management plugin served by a sidecar.
type PluginInfo struct {
	Name string `json:"name"`
}

// PluginList is the set of discovered plugins.
type PluginList struct {
	Items []PluginInfo `json:"items"`
}

// AppDetailsQuery asks for source-type-specific details used by the UI/CLI.
type AppDetailsQuery struct {
	Repo               Repository                     `json:"repo"`
	Source             v1alpha1.ApplicationSource     `json:"source"`
	Repos              []Repository                   `json:"repos,omitempty"`
	HelmRepoCreds      []RepositoryCredentialTemplate `json:"helmRepoCreds,omitempty"`
	KustomizeOptions   KustomizeOptions               `json:"kustomizeOptions,omitempty"`
	HelmOptions        HelmOptions                    `json:"helmOptions,omitempty"`
	AppName            string                         `json:"appName,omitempty"`
	NoCache            bool                           `json:"noCache,omitempty"`
	NoRevisionCache    bool                           `json:"noRevisionCache,omitempty"`
	EnabledSourceTypes map[string]bool                `json:"enabledSourceTypes,omitempty"`
	RefSources         map[string]RefTarget           `json:"refSources,omitempty"`
}

// AppDetails is the per-type detail payload.
type AppDetails struct {
	Type      string            `json:"type"`
	Helm      *HelmAppSpec      `json:"helm,omitempty"`
	Kustomize *KustomizeAppSpec `json:"kustomize,omitempty"`
	Directory *DirectoryAppSpec `json:"directory,omitempty"`
	Plugin    *PluginAppSpec    `json:"plugin,omitempty"`
}

// HelmAppSpec describes a Helm chart.
type HelmAppSpec struct {
	Name           string                       `json:"name,omitempty"`
	ValueFiles     []string                     `json:"valueFiles,omitempty"`
	Parameters     []v1alpha1.HelmParameter     `json:"parameters,omitempty"`
	Values         string                       `json:"values,omitempty"`
	FileParameters []v1alpha1.HelmFileParameter `json:"fileParameters,omitempty"`
}

// KustomizeAppSpec describes a kustomization.
type KustomizeAppSpec struct {
	Images []string `json:"images,omitempty"`
}

// DirectoryAppSpec describes a plain directory.
type DirectoryAppSpec struct{}

// ParameterAnnouncement is a CMP parameter announced by a plugin.
type ParameterAnnouncement struct {
	Name           string            `json:"name,omitempty"`
	Title          string            `json:"title,omitempty"`
	Tooltip        string            `json:"tooltip,omitempty"`
	Required       bool              `json:"required,omitempty"`
	ItemType       string            `json:"itemType,omitempty"`
	CollectionType string            `json:"collectionType,omitempty"`
	String         string            `json:"string,omitempty"`
	Array          []string          `json:"array,omitempty"`
	Map            map[string]string `json:"map,omitempty"`
}

// PluginAppSpec describes a CMP-rendered app.
type PluginAppSpec struct {
	ParametersAnnouncement []ParameterAnnouncement `json:"parametersAnnouncement,omitempty"`
}

// RevisionMetadataRequest asks for commit metadata.
type RevisionMetadataRequest struct {
	Repo            Repository                `json:"repo"`
	Revision        string                    `json:"revision"`
	SourceIntegrity *v1alpha1.SourceIntegrity `json:"sourceIntegrity,omitempty"`
	CheckSignature  bool                      `json:"checkSignature,omitempty"`
}

// RevisionReference is a reference-commit trailer (Argocd-reference-commit-* equivalent).
type RevisionReference struct {
	RepoURL string    `json:"repoURL,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date,omitzero"`
	Subject string    `json:"subject,omitempty"`
	Body    string    `json:"body,omitempty"`
	Path    string    `json:"path,omitempty"`
}

// RevisionMetadata is commit metadata.
type RevisionMetadata struct {
	Author                string                 `json:"author,omitempty"`
	Date                  time.Time              `json:"date,omitzero"`
	Tags                  []string               `json:"tags,omitempty"`
	Message               string                 `json:"message,omitempty"`
	SignatureInfo         string                 `json:"signatureInfo,omitempty"`
	References            []RevisionReference    `json:"references,omitempty"`
	SourceIntegrityResult *SourceIntegrityResult `json:"sourceIntegrityResult,omitempty"`
}

// ChartDetailsRequest asks for Helm chart or OCI metadata.
type ChartDetailsRequest struct {
	Repo     Repository `json:"repo"`
	Name     string     `json:"name"`
	Revision string     `json:"revision"`
}

// ChartDetails is Helm chart metadata.
type ChartDetails struct {
	Description string   `json:"description,omitempty"`
	Home        string   `json:"home,omitempty"`
	Maintainers []string `json:"maintainers,omitempty"`
}

// OCIMetadata is OCI manifest annotation metadata.
type OCIMetadata struct {
	CreatedAt   string `json:"createdAt,omitempty"`
	Authors     string `json:"authors,omitempty"`
	ImageURL    string `json:"imageUrl,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// HelmChartsRequest lists charts in a Helm repository index.
type HelmChartsRequest struct {
	Repo Repository `json:"repo"`
}

// HelmChart is one chart with its versions.
type HelmChart struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
}

// HelmChartsResponse lists charts.
type HelmChartsResponse struct {
	Items []HelmChart `json:"items"`
}

// GitFilesRequest reads files matching a glob (ApplicationSet git file generator).
type GitFilesRequest struct {
	Repo                      Repository `json:"repo"`
	SubmoduleEnabled          bool       `json:"submoduleEnabled,omitempty"`
	Revision                  string     `json:"revision"`
	Path                      string     `json:"path"`
	NewGitFileGlobbingEnabled bool       `json:"newGitFileGlobbingEnabled,omitempty"`
	NoRevisionCache           bool       `json:"noRevisionCache,omitempty"`
	VerifyCommit              bool       `json:"verifyCommit,omitempty"`
	// SourceIntegrity constrains verification of the repository (from the AppProject of
	// the ApplicationSet); a failed check fails the request.
	SourceIntegrity *v1alpha1.SourceIntegrity `json:"sourceIntegrity,omitempty"`
}

// GitFilesResponse maps file path to content.
type GitFilesResponse struct {
	Map map[string][]byte `json:"map"`
}

// GitDirectoriesRequest lists directories (ApplicationSet git directory generator).
type GitDirectoriesRequest struct {
	Repo             Repository `json:"repo"`
	SubmoduleEnabled bool       `json:"submoduleEnabled,omitempty"`
	Revision         string     `json:"revision"`
	NoRevisionCache  bool       `json:"noRevisionCache,omitempty"`
	VerifyCommit     bool       `json:"verifyCommit,omitempty"`
	// SourceIntegrity constrains verification of the repository (from the AppProject of
	// the ApplicationSet); a failed check fails the request.
	SourceIntegrity *v1alpha1.SourceIntegrity `json:"sourceIntegrity,omitempty"`
}

// GitDirectoriesResponse lists directory paths.
type GitDirectoriesResponse struct {
	Paths []string `json:"paths"`
}

// UpdateRevisionForPathsRequest lets the controller reuse cached manifests when the
// manifest-generate-paths of an app did not change between two revisions.
type UpdateRevisionForPathsRequest struct {
	Repo               Repository                     `json:"repo"`
	AppLabelKey        string                         `json:"appLabelKey,omitempty"`
	AppName            string                         `json:"appName"`
	Namespace          string                         `json:"namespace,omitempty"`
	Source             v1alpha1.ApplicationSource     `json:"source"`
	TrackingMethod     string                         `json:"trackingMethod,omitempty"`
	RefSources         map[string]RefTarget           `json:"refSources,omitempty"`
	KubeVersion        string                         `json:"kubeVersion,omitempty"`
	APIVersions        []string                       `json:"apiVersions,omitempty"`
	HasMultipleSources bool                           `json:"hasMultipleSources,omitempty"`
	SyncedRevision     string                         `json:"syncedRevision"`
	Revision           string                         `json:"revision"`
	Paths              []string                       `json:"paths"`
	InstallationID     string                         `json:"installationID,omitempty"`
	HelmRepoCreds      []RepositoryCredentialTemplate `json:"helmRepoCreds,omitempty"`
	// SyncedRefSources are the ref sources of the synced revision (multi-source
	// applications), compared with RefSources for `$ref` value files.
	SyncedRefSources map[string]RefTarget `json:"syncedRefSources,omitempty"`
	// NoRevisionCache bypasses the git reference cache when resolving revisions.
	NoRevisionCache bool `json:"noRevisionCache,omitempty"`
}

// UpdateRevisionForPathsResponse reports whether the paths changed.
type UpdateRevisionForPathsResponse struct {
	Changes  bool   `json:"changes"`
	Revision string `json:"revision"`
}

// TestRepositoryRequest validates access.
type TestRepositoryRequest struct {
	Repo Repository `json:"repo"`
}

// TestRepositoryResponse reports validity.
type TestRepositoryResponse struct {
	VerifiedRepository bool `json:"verifiedRepository"`
}

// Client is the repo-server API as seen by callers. Implementations: the HTTP client in
// this package and in-process fakes for tests.
type Client interface {
	GenerateManifest(ctx context.Context, req *ManifestRequest) (*ManifestResponse, error)
	// GenerateManifestWithFiles renders a source from a caller-supplied gzipped tar of
	// the source directory (local `--local` sources); files is closed by the client.
	GenerateManifestWithFiles(ctx context.Context, req *ManifestRequest, files io.Reader) (*ManifestResponse, error)
	ResolveRevision(ctx context.Context, req *ResolveRevisionRequest) (*ResolveRevisionResponse, error)
	TestRepository(ctx context.Context, req *TestRepositoryRequest) (*TestRepositoryResponse, error)
	ListRefs(ctx context.Context, req *ListRefsRequest) (*Refs, error)
	ListOCITags(ctx context.Context, req *ListRefsRequest) (*Refs, error)
	ListApps(ctx context.Context, req *ListAppsRequest) (*AppList, error)
	ListPlugins(ctx context.Context) (*PluginList, error)
	GetAppDetails(ctx context.Context, req *AppDetailsQuery) (*AppDetails, error)
	GetRevisionMetadata(ctx context.Context, req *RevisionMetadataRequest) (*RevisionMetadata, error)
	GetRevisionChartDetails(ctx context.Context, req *ChartDetailsRequest) (*ChartDetails, error)
	GetOCIMetadata(ctx context.Context, req *ChartDetailsRequest) (*OCIMetadata, error)
	GetHelmCharts(ctx context.Context, req *HelmChartsRequest) (*HelmChartsResponse, error)
	GetGitFiles(ctx context.Context, req *GitFilesRequest) (*GitFilesResponse, error)
	GetGitDirectories(ctx context.Context, req *GitDirectoriesRequest) (*GitDirectoriesResponse, error)
	UpdateRevisionForPaths(ctx context.Context, req *UpdateRevisionForPathsRequest) (*UpdateRevisionForPathsResponse, error)
}

// Error codes returned in problem+json bodies by the repo-server; the HTTP client maps
// them to *Error.
const (
	CodeInvalidArgument   = "invalid_argument"
	CodeNotFound          = "not_found"
	CodePermissionDenied  = "permission_denied"
	CodeUnauthenticated   = "unauthenticated"
	CodeUnavailable       = "unavailable"
	CodeDeadlineExceeded  = "deadline_exceeded"
	CodeResourceExhausted = "resource_exhausted"
	CodeInternal          = "internal"
)

// Error is a repo-server error with a stable code.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }
