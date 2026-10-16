package apiclient

import (
	"context"
	"log/slog"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// Route patterns of the repository, credential template, certificate, GnuPG key and SCM
// provider API. {repo} and {url} are the URL-encoded repository URL or template URL prefix
// (Argo-compatible: the client escapes slashes as %2F).
const (
	PathRepositories            = "/api/v1/repositories"
	PathRepository              = "/api/v1/repositories/{repo}"
	PathRepositoryRefs          = "/api/v1/repositories/{repo}/refs"
	PathRepositoryOCITags       = "/api/v1/repositories/{repo}/oci-tags"
	PathRepositoryApps          = "/api/v1/repositories/{repo}/apps"
	PathRepositoryAppDetails    = "/api/v1/repositories/{repo}/appdetails"
	PathRepositoryHelmCharts    = "/api/v1/repositories/{repo}/helmcharts"
	PathRepositoryValidate      = "/api/v1/repositories/{repo}/validate"
	PathWriteRepositories       = "/api/v1/write-repositories"
	PathWriteRepository         = "/api/v1/write-repositories/{repo}"
	PathWriteRepositoryValidate = "/api/v1/write-repositories/{repo}/validate"
	PathRepoCreds               = "/api/v1/repocreds"
	PathRepoCred                = "/api/v1/repocreds/{url}"
	PathWriteRepoCreds          = "/api/v1/write-repocreds"
	PathWriteRepoCred           = "/api/v1/write-repocreds/{url}"
	PathCertificates            = "/api/v1/certificates"
	PathGPGKeys                 = "/api/v1/gpgkeys"
	PathGPGKey                  = "/api/v1/gpgkeys/{keyID}"
	PathSCMProviders            = "/api/v1/scmproviders"
	PathSCMProvider             = "/api/v1/scmproviders/{name}"
)

// Certificate types of RepositoryCertificate.CertType.
const (
	CertificateTypeHTTPS = "https"
	CertificateTypeSSH   = "ssh"
)

// redacted replaces credential values in String, GoString and LogValue output.
const redacted = "[redacted]"

// RepositoryCredentials is write-only credential material of a repository or credential
// template. The server stores it in a Secret it manages (labelled with the
// cd.patrol.example/secret-type of the object, owned by the object and referenced by
// spec.credentialsRef) and never returns it. In an update, a nil Credentials leaves the
// stored credentials unchanged, while a non-nil one replaces every recognized credential key
// (keys left empty are removed; keys the server does not recognize are kept).
type RepositoryCredentials struct {
	// Username for HTTPS (git, Helm, OCI) basic authentication.
	Username string `json:"username,omitempty"`
	// Password or personal access token for HTTPS basic authentication.
	Password string `json:"password,omitempty"`
	// BearerToken for HTTPS bearer authentication (Bitbucket Data Center).
	BearerToken string `json:"bearerToken,omitempty"`
	// SSHPrivateKey is a PEM or OpenSSH private key without passphrase.
	SSHPrivateKey string `json:"sshPrivateKey,omitempty"`
	// TLSClientCertData and TLSClientCertKey are a PEM client certificate and its key.
	TLSClientCertData string `json:"tlsClientCertData,omitempty"`
	TLSClientCertKey  string `json:"tlsClientCertKey,omitempty"`
	// GitHubAppPrivateKey is the PEM private key of the GitHub App in spec.githubApp.
	GitHubAppPrivateKey string `json:"githubAppPrivateKey,omitempty"`
	// GCPServiceAccountKey is a Google service account key (JSON).
	GCPServiceAccountKey string `json:"gcpServiceAccountKey,omitempty"`
	// AzureServicePrincipalClientSecret is the client secret of spec.azureServicePrincipal.
	AzureServicePrincipalClientSecret string `json:"azureServicePrincipalClientSecret,omitempty"`
}

// String redacts the credentials.
func (RepositoryCredentials) String() string { return redacted }

// GoString redacts the credentials.
func (RepositoryCredentials) GoString() string {
	return "apiclient.RepositoryCredentials{" + redacted + "}"
}

// LogValue redacts the credentials.
func (RepositoryCredentials) LogValue() slog.Value { return slog.StringValue(redacted) }

// RepositoryWriteRequest is the body of repository create, update and validate requests: a
// Repository object (apiVersion and kind optional) plus write-only credentials.
// spec.credentialsRef is managed by the server: it must be empty on create and unchanged on
// update. metadata.resourceVersion, when set on an update, must match the stored object
// (409 conflict otherwise); metadata.name is derived from the URL and project on create.
// status is ignored.
type RepositoryWriteRequest struct {
	metav1.TypeMeta `json:",inline"`
	// Metadata of the object; only resourceVersion is used (optimistic concurrency).
	Metadata metav1.ObjectMeta `json:"metadata,omitzero"`
	// Spec is the repository configuration.
	Spec v1alpha1.RepositorySpec `json:"spec"`
	// Status is accepted (so a fetched object can be sent back) and ignored.
	Status *v1alpha1.RepositoryStatus `json:"status,omitempty"`
	// Credentials are write-only credential material.
	Credentials *RepositoryCredentials `json:"credentials,omitempty"`
}

// RepositoryInfo is a Repository as returned by the API: the stored object, which never
// contains credential values, plus derived fields.
type RepositoryInfo struct {
	metav1.TypeMeta `json:",inline"`
	// Metadata of the Repository object (managed fields removed).
	Metadata metav1.ObjectMeta `json:"metadata"`
	// Spec is the repository configuration; spec.credentialsRef names the Secret holding its
	// credentials.
	Spec v1alpha1.RepositorySpec `json:"spec"`
	// Status holds the last recorded connection state and conditions.
	Status v1alpha1.RepositoryStatus `json:"status,omitzero"`
	// ConnectionState is status.connectionState, or Unknown when the repository was not
	// checked yet.
	ConnectionState v1alpha1.ConnectionState `json:"connectionState"`
	// HasCredentials is true when the repository has credentials of its own (a credentials
	// Secret, an SCMProvider or Azure workload identity).
	HasCredentials bool `json:"hasCredentials"`
	// InheritedCredentials is true when the repository has no credentials of its own and a
	// credential template of the same purpose matches its URL.
	InheritedCredentials bool `json:"inheritedCredentials,omitempty"`
}

// RepositoryInfoList is the response of GET /api/v1/repositories and
// /api/v1/write-repositories.
type RepositoryInfoList struct {
	Metadata metav1.ListMeta  `json:"metadata"`
	Items    []RepositoryInfo `json:"items"`
}

// RepositoryListRequest is GET /api/v1/repositories (and /write-repositories).
type RepositoryListRequest struct {
	// ForceRefresh re-checks the connection of every listed repository.
	ForceRefresh bool `query:"forceRefresh" json:"-"`
	// AppProject lists only repositories scoped to this project.
	AppProject string `query:"appProject" json:"-"`
	// Selector is a label selector.
	Selector string `query:"selector" json:"-"`
	// Limit and Continue paginate (items are filtered by RBAC, so a page may hold fewer than
	// Limit items).
	Limit    int64  `query:"limit" json:"-"`
	Continue string `query:"continue" json:"-"`
}

// RepositoryCreateRequest is POST /api/v1/repositories (and /write-repositories).
type RepositoryCreateRequest struct {
	// Upsert updates an existing repository with the same URL and project whose spec or
	// credentials differ.
	Upsert bool `query:"upsert" json:"-"`
	// CredsOnly creates a credential template (RepositoryCredential with urlPrefix =
	// spec.url) instead of a repository.
	CredsOnly bool `query:"credsOnly" json:"-"`
	// Body is the repository to create.
	Body RepositoryWriteRequest `body:"true" json:"-"`
}

// RepositoryQuery addresses one repository by URL.
type RepositoryQuery struct {
	// Repo is the repository URL.
	Repo string `path:"repo" json:"-"`
	// AppProject selects the repository scoped to this project when several are registered
	// for the URL.
	AppProject string `query:"appProject" json:"-"`
	// ForceRefresh re-checks the connection (GET only).
	ForceRefresh bool `query:"forceRefresh" json:"-"`
}

// RepositoryUpdateRequest is PUT /api/v1/repositories/{repo} (and /write-repositories).
type RepositoryUpdateRequest struct {
	// Repo is the repository URL; it must name the same repository as spec.url.
	Repo string `path:"repo" json:"-"`
	// AppProject is the project the repository is currently scoped to, when it differs from
	// spec.project (moving a repository between projects).
	AppProject string `query:"appProject" json:"-"`
	// Body is the new configuration.
	Body RepositoryWriteRequest `body:"true" json:"-"`
}

// RepositoryValidateRequest is POST /api/v1/repositories/{repo}/validate (and
// /write-repositories): the connection is tested with the given settings and credentials,
// nothing is saved.
type RepositoryValidateRequest struct {
	// Repo is the repository URL (spec.url, when set, must name the same repository).
	Repo string `path:"repo" json:"-"`
	// Body holds the settings and credentials to test.
	Body RepositoryWriteRequest `body:"true" json:"-"`
}

// RepositoryRefs lists branches and tags (git) or tags (OCI).
type RepositoryRefs struct {
	Branches []string `json:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// RepositoryAppsRequest is GET /api/v1/repositories/{repo}/apps.
type RepositoryAppsRequest struct {
	Repo string `path:"repo" json:"-"`
	// Revision to inspect (default HEAD).
	Revision string `query:"revision" json:"-"`
	// AppName and AppProject name the Application the discovery is for; the caller needs
	// applications create or update on it.
	AppName    string `query:"appName" json:"-"`
	AppProject string `query:"appProject" json:"-"`
}

// RepositoryApp is one application directory found in a repository.
type RepositoryApp struct {
	// Type is the detected source type (Helm, Kustomize, Directory, Plugin).
	Type string `json:"type"`
	Path string `json:"path"`
}

// RepositoryAppList is the response of GET /api/v1/repositories/{repo}/apps.
type RepositoryAppList struct {
	Items []RepositoryApp `json:"items"`
}

// RepositoryAppDetailsQuery is the body of POST /api/v1/repositories/{repo}/appdetails.
type RepositoryAppDetailsQuery struct {
	// Source is the application source to inspect; source.repoURL defaults to the path's
	// repository and must name the same repository.
	Source *v1alpha1.ApplicationSource `json:"source"`
	// AppName ("name" or "namespace/name") and AppProject name the Application.
	AppName    string `json:"appName,omitempty"`
	AppProject string `json:"appProject,omitempty"`
	// SourceIndex and VersionID select a source of a multi-source history entry.
	SourceIndex int32 `json:"sourceIndex,omitempty"`
	VersionID   int32 `json:"versionId,omitempty"`
}

// RepositoryAppDetailsRequest is POST /api/v1/repositories/{repo}/appdetails.
type RepositoryAppDetailsRequest struct {
	Repo string                    `path:"repo" json:"-"`
	Body RepositoryAppDetailsQuery `body:"true" json:"-"`
}

// RepositoryAppDetails are the tool-specific details of an application source.
type RepositoryAppDetails struct {
	Type      string                      `json:"type"`
	Helm      *RepositoryHelmAppSpec      `json:"helm,omitempty"`
	Kustomize *RepositoryKustomizeAppSpec `json:"kustomize,omitempty"`
	Directory *RepositoryDirectoryAppSpec `json:"directory,omitempty"`
	Plugin    *RepositoryPluginAppSpec    `json:"plugin,omitempty"`
}

// RepositoryHelmAppSpec describes a Helm chart.
type RepositoryHelmAppSpec struct {
	Name           string                       `json:"name,omitempty"`
	ValueFiles     []string                     `json:"valueFiles,omitempty"`
	Parameters     []v1alpha1.HelmParameter     `json:"parameters,omitempty"`
	Values         string                       `json:"values,omitempty"`
	FileParameters []v1alpha1.HelmFileParameter `json:"fileParameters,omitempty"`
}

// RepositoryKustomizeAppSpec describes a kustomization.
type RepositoryKustomizeAppSpec struct {
	Images []string `json:"images,omitempty"`
}

// RepositoryDirectoryAppSpec describes a plain directory.
type RepositoryDirectoryAppSpec struct{}

// RepositoryPluginAppSpec describes a config management plugin source.
type RepositoryPluginAppSpec struct {
	ParametersAnnouncement []RepositoryPluginParameter `json:"parametersAnnouncement,omitempty"`
}

// RepositoryPluginParameter is a parameter announced by a config management plugin.
type RepositoryPluginParameter struct {
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

// RepositoryHelmChart is one chart of a Helm repository index.
type RepositoryHelmChart struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions,omitempty"`
}

// RepositoryHelmChartList is the response of GET /api/v1/repositories/{repo}/helmcharts.
type RepositoryHelmChartList struct {
	Items []RepositoryHelmChart `json:"items"`
}

// RepositoryCredentialWriteRequest is the body of credential template create and update
// requests: a RepositoryCredential object plus write-only credentials (see
// RepositoryWriteRequest for the metadata, credentialsRef and status rules).
type RepositoryCredentialWriteRequest struct {
	metav1.TypeMeta `json:",inline"`
	// Metadata of the object; only resourceVersion is used (optimistic concurrency).
	Metadata metav1.ObjectMeta `json:"metadata,omitzero"`
	// Spec is the template.
	Spec v1alpha1.RepositoryCredentialSpec `json:"spec"`
	// Status is accepted and ignored.
	Status *v1alpha1.RepositoryStatus `json:"status,omitempty"`
	// Credentials are write-only credential material.
	Credentials *RepositoryCredentials `json:"credentials,omitempty"`
}

// RepositoryCredentialInfo is a credential template as returned by the API (never its
// credential values).
type RepositoryCredentialInfo struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        metav1.ObjectMeta                 `json:"metadata"`
	Spec            v1alpha1.RepositoryCredentialSpec `json:"spec"`
	Status          v1alpha1.RepositoryStatus         `json:"status,omitzero"`
	// HasCredentials is true when the template has credentials of its own (a credentials
	// Secret, an SCMProvider or Azure workload identity).
	HasCredentials bool `json:"hasCredentials"`
	// Username is the stored user name (not a secret; upstream lists it).
	Username string `json:"username,omitempty"`
}

// RepositoryCredentialInfoList is the response of GET /api/v1/repocreds and
// /api/v1/write-repocreds.
type RepositoryCredentialInfoList struct {
	Metadata metav1.ListMeta            `json:"metadata"`
	Items    []RepositoryCredentialInfo `json:"items"`
}

// RepositoryCredentialCreateRequest is POST /api/v1/repocreds (and /write-repocreds).
type RepositoryCredentialCreateRequest struct {
	// Upsert updates an existing template with the same URL prefix whose spec or
	// credentials differ.
	Upsert bool                             `query:"upsert" json:"-"`
	Body   RepositoryCredentialWriteRequest `body:"true" json:"-"`
}

// RepositoryCredentialQuery addresses one credential template by its URL prefix.
type RepositoryCredentialQuery struct {
	URL string `path:"url" json:"-"`
}

// RepositoryCredentialUpdateRequest is PUT /api/v1/repocreds/{url} (and /write-repocreds).
type RepositoryCredentialUpdateRequest struct {
	// URL is the URL prefix; it must equal spec.urlPrefix.
	URL  string                           `path:"url" json:"-"`
	Body RepositoryCredentialWriteRequest `body:"true" json:"-"`
}

// RepositoryCertificate is a TLS certificate or an SSH known host entry for a repository
// server.
type RepositoryCertificate struct {
	// ServerName is the host name (TLS), or the known_hosts host field, e.g. "github.com" or
	// "[git.example.com]:2222" (SSH).
	ServerName string `json:"serverName"`
	// CertType is "https" or "ssh".
	CertType string `json:"certType"`
	// CertSubType is the SSH key type (e.g. "ssh-ed25519") or the lower-cased public key
	// algorithm of a TLS certificate ("rsa", "ecdsa", "ed25519"; "invalid" when the stored
	// PEM data cannot be decoded).
	CertSubType string `json:"certSubType,omitempty"`
	// CertData is the PEM data (https) or the base64 public key (ssh). It is required on
	// create and returned by create and delete, never by list.
	CertData string `json:"certData,omitempty"`
	// CertInfo is the subject DN (https) or "SHA256:<fingerprint>" (ssh).
	CertInfo string `json:"certInfo,omitempty"`
}

// RepositoryCertificateList is a list of certificates.
type RepositoryCertificateList struct {
	Items []RepositoryCertificate `json:"items"`
}

// RepositoryCertificateQuery selects certificates (GET and DELETE /api/v1/certificates).
type RepositoryCertificateQuery struct {
	// HostNamePattern is a file glob on the server name (empty matches everything;
	// brackets are literal).
	HostNamePattern string `query:"hostNamePattern" json:"-"`
	// CertType is "", "*", "ssh", "https" or "tls" (alias of https).
	CertType string `query:"certType" json:"-"`
	// CertSubType filters SSH entries by key type.
	CertSubType string `query:"certSubType" json:"-"`
}

// RepositoryCertificateCreateRequest is POST /api/v1/certificates.
type RepositoryCertificateCreateRequest struct {
	// Upsert replaces existing entries with different data.
	Upsert bool                      `query:"upsert" json:"-"`
	Body   RepositoryCertificateList `body:"true" json:"-"`
}

// GnuPGPublicKey is a GnuPG public key trusted for commit verification.
type GnuPGPublicKey struct {
	// KeyID is the 16-hex-digit key ID (upper case).
	KeyID string `json:"keyID,omitempty"`
	// Fingerprint is the full fingerprint (upper-case hex).
	Fingerprint string `json:"fingerprint,omitempty"`
	// Owner is the primary identity.
	Owner string `json:"owner,omitempty"`
	// Trust is the trust level the repo-server assigns ("ultimate": every configured key
	// is trusted).
	Trust string `json:"trust,omitempty"`
	// SubType is the key algorithm and size, e.g. "rsa4096" or "ed25519".
	SubType string `json:"subType,omitempty"`
	// KeyData is the ASCII-armored public key (required on create; returned by get, never
	// by list).
	KeyData string `json:"keyData,omitempty"`
}

// GnuPGPublicKeyList is a list of GnuPG public keys.
type GnuPGPublicKeyList struct {
	Items []GnuPGPublicKey `json:"items"`
}

// GnuPGPublicKeyCreateRequest is POST /api/v1/gpgkeys.
type GnuPGPublicKeyCreateRequest struct {
	// Upsert is accepted for compatibility and ignored: existing keys are always skipped.
	Upsert bool `query:"upsert" json:"-"`
	// Body.KeyData holds one or more ASCII-armored public keys.
	Body GnuPGPublicKey `body:"true" json:"-"`
}

// GnuPGPublicKeyCreateResponse reports the keys added and the IDs of keys already
// configured.
type GnuPGPublicKeyCreateResponse struct {
	Created GnuPGPublicKeyList `json:"created"`
	Skipped []string           `json:"skipped,omitempty"`
}

// GnuPGPublicKeyQuery addresses one key by its 16-hex-digit key ID or 40-hex-digit
// fingerprint.
type GnuPGPublicKeyQuery struct {
	KeyID string `path:"keyID" json:"-"`
}

// SCMProviderWriteRequest is the body of SCM provider create and update requests: an
// SCMProvider object. Secrets are referenced by name only (spec.caSecretRef, spec.auth.*,
// spec.webhook.secretRef); the API never accepts secret values for providers. status is
// ignored.
type SCMProviderWriteRequest struct {
	metav1.TypeMeta `json:",inline"`
	// Metadata: name (required on create, must match the path on update), labels,
	// annotations and resourceVersion (optimistic concurrency on update).
	Metadata metav1.ObjectMeta `json:"metadata"`
	// Spec is the provider configuration.
	Spec v1alpha1.SCMProviderSpec `json:"spec"`
	// Status is accepted and ignored.
	Status *v1alpha1.SCMProviderStatus `json:"status,omitempty"`
}

// SCMProviderCreateRequest is POST /api/v1/scmproviders.
type SCMProviderCreateRequest struct {
	Body SCMProviderWriteRequest `body:"true" json:"-"`
}

// SCMProviderQuery addresses one SCM provider by name.
type SCMProviderQuery struct {
	Name string `path:"name" json:"-"`
}

// SCMProviderUpdateRequest is PUT /api/v1/scmproviders/{name}.
type SCMProviderUpdateRequest struct {
	Name string                  `path:"name" json:"-"`
	Body SCMProviderWriteRequest `body:"true" json:"-"`
}

func repoPaths(write bool) (list, one, validate string) {
	if write {
		return PathWriteRepositories, PathWriteRepository, PathWriteRepositoryValidate
	}
	return PathRepositories, PathRepository, PathRepositoryValidate
}

func credPaths(write bool) (list, one string) {
	if write {
		return PathWriteRepoCreds, PathWriteRepoCred
	}
	return PathRepoCreds, PathRepoCred
}

// ListRepositories lists the repositories the caller may get.
func (c *Client) ListRepositories(ctx context.Context, req *RepositoryListRequest) (*RepositoryInfoList, error) {
	return c.listRepositories(ctx, false, req)
}

// ListWriteRepositories lists the write repositories the caller may get.
func (c *Client) ListWriteRepositories(ctx context.Context, req *RepositoryListRequest) (*RepositoryInfoList, error) {
	return c.listRepositories(ctx, true, req)
}

func (c *Client) listRepositories(ctx context.Context, write bool, req *RepositoryListRequest) (*RepositoryInfoList, error) {
	list, _, _ := repoPaths(write)
	var out RepositoryInfoList
	if err := c.Do(ctx, http.MethodGet, list, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateRepository registers a repository (or, with CredsOnly, a credential template).
func (c *Client) CreateRepository(ctx context.Context, req *RepositoryCreateRequest) (*RepositoryInfo, error) {
	return c.createRepository(ctx, false, req)
}

// CreateWriteRepository registers a write repository (source hydrator).
func (c *Client) CreateWriteRepository(ctx context.Context, req *RepositoryCreateRequest) (*RepositoryInfo, error) {
	return c.createRepository(ctx, true, req)
}

func (c *Client) createRepository(ctx context.Context, write bool, req *RepositoryCreateRequest) (*RepositoryInfo, error) {
	list, _, _ := repoPaths(write)
	var out RepositoryInfo
	if err := c.Do(ctx, http.MethodPost, list, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRepository returns one repository.
func (c *Client) GetRepository(ctx context.Context, req *RepositoryQuery) (*RepositoryInfo, error) {
	return c.getRepository(ctx, false, req)
}

// GetWriteRepository returns one write repository.
func (c *Client) GetWriteRepository(ctx context.Context, req *RepositoryQuery) (*RepositoryInfo, error) {
	return c.getRepository(ctx, true, req)
}

func (c *Client) getRepository(ctx context.Context, write bool, req *RepositoryQuery) (*RepositoryInfo, error) {
	_, one, _ := repoPaths(write)
	var out RepositoryInfo
	if err := c.Do(ctx, http.MethodGet, one, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateRepository updates a repository.
func (c *Client) UpdateRepository(ctx context.Context, req *RepositoryUpdateRequest) (*RepositoryInfo, error) {
	return c.updateRepository(ctx, false, req)
}

// UpdateWriteRepository updates a write repository.
func (c *Client) UpdateWriteRepository(ctx context.Context, req *RepositoryUpdateRequest) (*RepositoryInfo, error) {
	return c.updateRepository(ctx, true, req)
}

func (c *Client) updateRepository(ctx context.Context, write bool, req *RepositoryUpdateRequest) (*RepositoryInfo, error) {
	_, one, _ := repoPaths(write)
	var out RepositoryInfo
	if err := c.Do(ctx, http.MethodPut, one, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRepository deletes a repository and the credentials Secret the server manages for
// it.
func (c *Client) DeleteRepository(ctx context.Context, req *RepositoryQuery) error {
	_, one, _ := repoPaths(false)
	return c.Do(ctx, http.MethodDelete, one, req, nil)
}

// DeleteWriteRepository deletes a write repository.
func (c *Client) DeleteWriteRepository(ctx context.Context, req *RepositoryQuery) error {
	_, one, _ := repoPaths(true)
	return c.Do(ctx, http.MethodDelete, one, req, nil)
}

// ValidateRepositoryAccess tests a connection without saving anything. A failed test is
// an *APIError (400) whose detail is the connection message.
func (c *Client) ValidateRepositoryAccess(ctx context.Context, req *RepositoryValidateRequest) (*v1alpha1.ConnectionState, error) {
	return c.validateRepository(ctx, false, req)
}

// ValidateWriteRepositoryAccess tests write access without saving anything.
func (c *Client) ValidateWriteRepositoryAccess(ctx context.Context, req *RepositoryValidateRequest) (*v1alpha1.ConnectionState, error) {
	return c.validateRepository(ctx, true, req)
}

func (c *Client) validateRepository(ctx context.Context, write bool, req *RepositoryValidateRequest) (*v1alpha1.ConnectionState, error) {
	_, _, validate := repoPaths(write)
	var out v1alpha1.ConnectionState
	if err := c.Do(ctx, http.MethodPost, validate, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepositoryRefs lists the branches and tags of a git repository.
func (c *Client) ListRepositoryRefs(ctx context.Context, req *RepositoryQuery) (*RepositoryRefs, error) {
	var out RepositoryRefs
	if err := c.Do(ctx, http.MethodGet, PathRepositoryRefs, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepositoryOCITags lists the tags of an OCI repository.
func (c *Client) ListRepositoryOCITags(ctx context.Context, req *RepositoryQuery) (*RepositoryRefs, error) {
	var out RepositoryRefs
	if err := c.Do(ctx, http.MethodGet, PathRepositoryOCITags, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepositoryApps discovers application directories at a revision.
func (c *Client) ListRepositoryApps(ctx context.Context, req *RepositoryAppsRequest) (*RepositoryAppList, error) {
	var out RepositoryAppList
	if err := c.Do(ctx, http.MethodGet, PathRepositoryApps, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRepositoryAppDetails returns the tool-specific details of an application source.
func (c *Client) GetRepositoryAppDetails(ctx context.Context, req *RepositoryAppDetailsRequest) (*RepositoryAppDetails, error) {
	var out RepositoryAppDetails
	if err := c.Do(ctx, http.MethodPost, PathRepositoryAppDetails, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepositoryHelmCharts lists the charts of a Helm repository.
func (c *Client) ListRepositoryHelmCharts(ctx context.Context, req *RepositoryQuery) (*RepositoryHelmChartList, error) {
	var out RepositoryHelmChartList
	if err := c.Do(ctx, http.MethodGet, PathRepositoryHelmCharts, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRepositoryCredentials lists the credential templates the caller may get.
func (c *Client) ListRepositoryCredentials(ctx context.Context) (*RepositoryCredentialInfoList, error) {
	return c.listRepositoryCredentials(ctx, false)
}

// ListWriteRepositoryCredentials lists the write credential templates the caller may get.
func (c *Client) ListWriteRepositoryCredentials(ctx context.Context) (*RepositoryCredentialInfoList, error) {
	return c.listRepositoryCredentials(ctx, true)
}

func (c *Client) listRepositoryCredentials(ctx context.Context, write bool) (*RepositoryCredentialInfoList, error) {
	list, _ := credPaths(write)
	var out RepositoryCredentialInfoList
	if err := c.Do(ctx, http.MethodGet, list, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateRepositoryCredential creates a credential template.
func (c *Client) CreateRepositoryCredential(ctx context.Context, req *RepositoryCredentialCreateRequest) (*RepositoryCredentialInfo, error) {
	return c.createRepositoryCredential(ctx, false, req)
}

// CreateWriteRepositoryCredential creates a write credential template.
func (c *Client) CreateWriteRepositoryCredential(ctx context.Context, req *RepositoryCredentialCreateRequest) (*RepositoryCredentialInfo, error) {
	return c.createRepositoryCredential(ctx, true, req)
}

func (c *Client) createRepositoryCredential(ctx context.Context, write bool, req *RepositoryCredentialCreateRequest) (*RepositoryCredentialInfo, error) {
	list, _ := credPaths(write)
	var out RepositoryCredentialInfo
	if err := c.Do(ctx, http.MethodPost, list, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateRepositoryCredential updates a credential template.
func (c *Client) UpdateRepositoryCredential(ctx context.Context, req *RepositoryCredentialUpdateRequest) (*RepositoryCredentialInfo, error) {
	return c.updateRepositoryCredential(ctx, false, req)
}

// UpdateWriteRepositoryCredential updates a write credential template.
func (c *Client) UpdateWriteRepositoryCredential(ctx context.Context, req *RepositoryCredentialUpdateRequest) (*RepositoryCredentialInfo, error) {
	return c.updateRepositoryCredential(ctx, true, req)
}

func (c *Client) updateRepositoryCredential(ctx context.Context, write bool, req *RepositoryCredentialUpdateRequest) (*RepositoryCredentialInfo, error) {
	_, one := credPaths(write)
	var out RepositoryCredentialInfo
	if err := c.Do(ctx, http.MethodPut, one, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteRepositoryCredential deletes a credential template (exact URL prefix).
func (c *Client) DeleteRepositoryCredential(ctx context.Context, urlPrefix string) error {
	_, one := credPaths(false)
	return c.Do(ctx, http.MethodDelete, one, &RepositoryCredentialQuery{URL: urlPrefix}, nil)
}

// DeleteWriteRepositoryCredential deletes a write credential template.
func (c *Client) DeleteWriteRepositoryCredential(ctx context.Context, urlPrefix string) error {
	_, one := credPaths(true)
	return c.Do(ctx, http.MethodDelete, one, &RepositoryCredentialQuery{URL: urlPrefix}, nil)
}

// ListCertificates lists TLS certificates and SSH known hosts matching q.
func (c *Client) ListCertificates(ctx context.Context, q *RepositoryCertificateQuery) (*RepositoryCertificateList, error) {
	var out RepositoryCertificateList
	if err := c.Do(ctx, http.MethodGet, PathCertificates, q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateCertificates adds TLS certificates and SSH known hosts; the response lists the
// entries created or updated.
func (c *Client) CreateCertificates(ctx context.Context, req *RepositoryCertificateCreateRequest) (*RepositoryCertificateList, error) {
	var out RepositoryCertificateList
	if err := c.Do(ctx, http.MethodPost, PathCertificates, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteCertificates removes every entry matching q (an empty query removes everything)
// and returns the removed entries.
func (c *Client) DeleteCertificates(ctx context.Context, q *RepositoryCertificateQuery) (*RepositoryCertificateList, error) {
	var out RepositoryCertificateList
	if err := c.Do(ctx, http.MethodDelete, PathCertificates, q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListGPGKeys lists the configured GnuPG public keys (without key data).
func (c *Client) ListGPGKeys(ctx context.Context) (*GnuPGPublicKeyList, error) {
	var out GnuPGPublicKeyList
	if err := c.Do(ctx, http.MethodGet, PathGPGKeys, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetGPGKey returns one GnuPG public key, by key ID or fingerprint.
func (c *Client) GetGPGKey(ctx context.Context, keyID string) (*GnuPGPublicKey, error) {
	var out GnuPGPublicKey
	if err := c.Do(ctx, http.MethodGet, PathGPGKey, &GnuPGPublicKeyQuery{KeyID: keyID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateGPGKeys adds the ASCII-armored public keys of keyData.
func (c *Client) CreateGPGKeys(ctx context.Context, keyData string) (*GnuPGPublicKeyCreateResponse, error) {
	var out GnuPGPublicKeyCreateResponse
	req := &GnuPGPublicKeyCreateRequest{Body: GnuPGPublicKey{KeyData: keyData}}
	if err := c.Do(ctx, http.MethodPost, PathGPGKeys, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteGPGKey removes a GnuPG public key, by key ID or fingerprint.
func (c *Client) DeleteGPGKey(ctx context.Context, keyID string) error {
	return c.Do(ctx, http.MethodDelete, PathGPGKey, &GnuPGPublicKeyQuery{KeyID: keyID}, nil)
}

// ListSCMProviders lists the SCM providers the caller may get.
func (c *Client) ListSCMProviders(ctx context.Context) (*v1alpha1.SCMProviderList, error) {
	var out v1alpha1.SCMProviderList
	if err := c.Do(ctx, http.MethodGet, PathSCMProviders, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSCMProvider returns one SCM provider.
func (c *Client) GetSCMProvider(ctx context.Context, name string) (*v1alpha1.SCMProvider, error) {
	var out v1alpha1.SCMProvider
	if err := c.Do(ctx, http.MethodGet, PathSCMProvider, &SCMProviderQuery{Name: name}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSCMProvider creates an SCM provider.
func (c *Client) CreateSCMProvider(ctx context.Context, body *SCMProviderWriteRequest) (*v1alpha1.SCMProvider, error) {
	var out v1alpha1.SCMProvider
	req := &SCMProviderCreateRequest{}
	if body != nil {
		req.Body = *body
	}
	if err := c.Do(ctx, http.MethodPost, PathSCMProviders, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSCMProvider replaces the spec, labels and annotations of an SCM provider.
func (c *Client) UpdateSCMProvider(ctx context.Context, name string, body *SCMProviderWriteRequest) (*v1alpha1.SCMProvider, error) {
	var out v1alpha1.SCMProvider
	req := &SCMProviderUpdateRequest{Name: name}
	if body != nil {
		req.Body = *body
	}
	if err := c.Do(ctx, http.MethodPut, PathSCMProvider, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSCMProvider deletes an SCM provider.
func (c *Client) DeleteSCMProvider(ctx context.Context, name string) error {
	return c.Do(ctx, http.MethodDelete, PathSCMProvider, &SCMProviderQuery{Name: name}, nil)
}
