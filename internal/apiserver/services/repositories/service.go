// Package repositories implements the repository, credential template, certificate, GnuPG
// key and SCM provider routes of patrol-server (api-contract.md, "Repositories,
// credentials, certificates, GPG keys, SCM providers"; docs/architecture/components/
// api-repositories.md).
//
// Repositories and credential templates are Repository and RepositoryCredential objects in
// the control-plane namespace. Credentials arrive in a write-only request field and are
// stored in a Secret the handlers manage (labelled with the object's
// cd.patrol.example/secret-type, annotated cd.patrol.example/managed-by: patrol-cd and
// owned by the object) referenced by spec.credentialsRef; responses never contain
// credential values. Connection checks run on the repo-server through
// internal/repository and are recorded in Repository.status. TLS certificates, SSH known
// hosts and GnuPG keys live in patrol-tls-certs-cm, patrol-ssh-known-hosts-cm and
// patrol-gpg-keys-cm; SCM providers are SCMProvider objects that reference Secrets by name
// only.
package repositories

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// ResourceSCMProviders is the RBAC resource of the SCM provider routes (Patrol addition,
// api-contract.md). internal/rbac does not declare it yet: its owner was asked to add the
// constant, the strict-mode action table and the built-in role:readonly/role:admin
// grants. Until then policies must grant it explicitly (p, <role>, scmproviders, ...).
const ResourceSCMProviders = "scmproviders"

// Defaults of Config.
const (
	DefaultConnectionTimeout  = 30 * time.Second
	DefaultConnectionStateTTL = time.Hour
	DefaultMaxParallelChecks  = 8
	DefaultRepoServerTimeout  = 2 * time.Minute
)

// Body caps of the routes that accept bodies.
const (
	// maxWriteBodyBytes caps repository and template bodies (a Secret holds at most
	// 1 MiB).
	maxWriteBodyBytes int64 = 1 << 20
	// maxTrustBodyBytes caps certificate and GnuPG key bodies (a ConfigMap holds at most
	// 1 MiB).
	maxTrustBodyBytes int64 = 1 << 20
)

// Config is the static configuration of the handlers.
type Config struct {
	// Namespace is the control-plane namespace holding the objects and ConfigMaps.
	Namespace string
	// HydratorEnabled enables the /write-repositories routes (upstream --hydrator-enabled);
	// they answer 501 otherwise. The /write-repocreds routes are not gated (upstream).
	HydratorEnabled bool
	// ConnectionTimeout bounds one connection check (default DefaultConnectionTimeout).
	ConnectionTimeout time.Duration
	// ConnectionStateTTL is the age after which a recorded connection state is checked
	// again when repositories are read (upstream connection status cache expiration,
	// default DefaultConnectionStateTTL).
	ConnectionStateTTL time.Duration
	// MaxParallelChecks bounds concurrent connection checks of one list request (default
	// DefaultMaxParallelChecks).
	MaxParallelChecks int
	// RepoServerTimeout bounds refs, tags, apps, app details and chart requests (default
	// DefaultRepoServerTimeout).
	RepoServerTimeout time.Duration
	// TLSCertsConfigMap, SSHKnownHostsConfigMap and GPGKeysConfigMap name the trust
	// ConfigMaps (defaults: the internal/settings default names).
	TLSCertsConfigMap      string
	SSHKnownHostsConfigMap string
	GPGKeysConfigMap       string
}

// KubeClient reads and writes Kubernetes objects. It should read from the API server
// rather than an informer cache so that a response reflects the request's own writes
// (controller-runtime client.New implements it).
type KubeClient interface {
	client.Reader
	client.Writer
}

// Resolver resolves and tests repository access; *repository.Resolver implements it.
type Resolver interface {
	Resolve(ctx context.Context, q repository.Query) (*repository.Resolution, error)
	Validate(ctx context.Context, repo *v1alpha1.Repository) (v1alpha1.ConnectionState, error)
	ValidateWithSecretData(ctx context.Context, repo *v1alpha1.Repository, data map[string][]byte) (v1alpha1.ConnectionState, error)
	ListDependencyRepositories(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]reposerver.Repository, []reposerver.RepositoryCredentialTemplate, error)
}

// StatusWriter records connection checks in Repository.status; *repository.StatusWriter
// implements it.
type StatusWriter interface {
	WriteConnectionState(ctx context.Context, repo *v1alpha1.Repository, state v1alpha1.ConnectionState, cause error) (*v1alpha1.Repository, error)
}

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// Deps are the collaborators of the handlers. A Service built with zero Deps registers its
// routes (OpenAPI generation); requests then fail with 503.
type Deps struct {
	Config Config
	// Client reads and writes the objects of the control-plane namespace.
	Client KubeClient
	// Resolver resolves credentials and runs connection checks (repo-server
	// TestRepository).
	Resolver Resolver
	// StatusWriter records connection states (nil: states are reported, not recorded).
	StatusWriter StatusWriter
	// RepoServer serves refs, OCI tags, apps, app details and Helm charts.
	RepoServer reposerver.Client
	// Settings provides global projects, kustomize/Helm options and enabled source types.
	Settings SettingsSource
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service serves the repository API. It implements apiserver.Service.
type Service struct {
	d   Deps
	cfg Config
	log *slog.Logger
	now func() time.Time
}

// New returns the service using d.
func New(d Deps) *Service {
	cfg := d.Config
	if cfg.ConnectionTimeout <= 0 {
		cfg.ConnectionTimeout = DefaultConnectionTimeout
	}
	if cfg.ConnectionStateTTL <= 0 {
		cfg.ConnectionStateTTL = DefaultConnectionStateTTL
	}
	if cfg.MaxParallelChecks <= 0 {
		cfg.MaxParallelChecks = DefaultMaxParallelChecks
	}
	if cfg.RepoServerTimeout <= 0 {
		cfg.RepoServerTimeout = DefaultRepoServerTimeout
	}
	if cfg.TLSCertsConfigMap == "" {
		cfg.TLSCertsConfigMap = settings.DefaultTLSCertsConfigMapName
	}
	if cfg.SSHKnownHostsConfigMap == "" {
		cfg.SSHKnownHostsConfigMap = settings.DefaultSSHKnownHostsConfigMapName
	}
	if cfg.GPGKeysConfigMap == "" {
		cfg.GPGKeysConfigMap = settings.DefaultGPGKeysConfigMapName
	}
	lg := d.Logger
	if lg == nil {
		lg = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Service{d: d, cfg: cfg, log: lg, now: now}
}

var errNotConfigured = errors.New("not configured")

// kube returns the Kubernetes client or a 503.
func (s *Service) kube() (KubeClient, error) {
	if s.d.Client == nil {
		return nil, rest.Unavailable("the repository API is not configured").WithCause(errNotConfigured)
	}
	return s.d.Client, nil
}

func (s *Service) settings() *settings.Settings {
	if s.d.Settings == nil {
		return nil
	}
	return s.d.Settings.Current()
}

// Route tags.
const (
	tagRepositories = "repositories"
	tagRepoCreds    = "repocreds"
	tagCertificates = "certificates"
	tagGPGKeys      = "gpgkeys"
	tagSCMProviders = "scmproviders"
)

// Object notations of the RBAC documentation.
const (
	repoObjectDoc = "<project>/<url>"
	credObjectDoc = "<urlPrefix>"
)

func note(a rest.Access, n string) rest.Access {
	a.Note = n
	return a
}

func emptyObject(*http.Request) (string, error) { return "", nil }

// Register adds the routes to t.
func (s *Service) Register(t *rest.Table) {
	s.registerRepositories(t, readPurpose)
	s.registerRepositories(t, writePurpose)
	s.registerBrowse(t)
	s.registerRepoCreds(t, readPurpose)
	s.registerRepoCreds(t, writePurpose)
	s.registerCertificates(t)
	s.registerGPGKeys(t)
	s.registerSCMProviders(t)
}

func (s *Service) registerRepositories(t *rest.Table, p purpose) {
	list, one, validate := apiclient.PathRepositories, apiclient.PathRepository, apiclient.PathRepositoryValidate
	op := func(verb string) string { return verb + "Repository" }
	ops := "listRepositories"
	what := "repository"
	gate := ""
	if p.write {
		list, one, validate = apiclient.PathWriteRepositories, apiclient.PathWriteRepository, apiclient.PathWriteRepositoryValidate
		op = func(verb string) string { return verb + "WriteRepository" }
		ops = "listWriteRepositories"
		what = "write repository"
		gate = " Answers 501 unless the source hydrator is enabled."
	}
	res := p.resource
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: list, OperationID: ops, Tags: []string{tagRepositories},
		Summary: "List " + what + " objects", Access: rest.Access{Mode: rest.AccessAuthenticated, Note: "items filtered by " + res + ", get, " + repoObjectDoc},
		Description: "Repositories the caller may get, sorted by project and URL, with their connection state; a missing or outdated state (older than the connection state TTL) is checked and recorded, `forceRefresh` checks every listed repository." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryListRequest) (*apiclient.RepositoryInfoList, error) {
		return s.listRepositories(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: list, OperationID: op("create"), Tags: []string{tagRepositories},
		Summary: "Create a " + what, MaxBodyBytes: maxWriteBodyBytes,
		Access:      note(rest.RBACInHandler(res, rbac.ActionCreate, repoObjectDoc), "credsOnly: "+res+", create, "+credObjectDoc+"; spec.scmProviderRef also needs scmproviders, get, <name>"),
		Description: "Creates the Repository object and the Secret holding `credentials`. Unscoped read repositories and every write repository are tested on the repo-server first; project-scoped read repositories are created even when unreachable and their state is recorded. An existing repository with the same URL and project is returned unchanged when identical, updated with `upsert`, rejected otherwise. `credsOnly` creates a credential template instead." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryCreateRequest) (*apiclient.RepositoryInfo, error) {
		return s.createRepository(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: one, OperationID: op("get"), Tags: []string{tagRepositories},
		Summary: "Get a " + what, Access: note(rest.RBACInHandler(res, rbac.ActionGet, repoObjectDoc), "missing and forbidden repositories both answer 403"),
		Description: "The URL is URL-encoded in the path. `appProject` selects among repositories registered for the same URL in several projects; `forceRefresh` checks the connection." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryQuery) (*apiclient.RepositoryInfo, error) {
		return s.getRepository(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: one, OperationID: op("update"), Tags: []string{tagRepositories},
		Summary: "Update a " + what, MaxBodyBytes: maxWriteBodyBytes,
		Access:      note(rest.RBACInHandler(res, rbac.ActionUpdate, repoObjectDoc), "checked on the current and on the new project; missing and forbidden repositories both answer 403"),
		Description: "Replaces the spec (`metadata.resourceVersion` for optimistic concurrency, 409 when stale). `credentials` replaces the stored credentials; without it they are kept. `?appProject=` names the current project when the repository moves to another one. The connection is checked and recorded." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryUpdateRequest) (*apiclient.RepositoryInfo, error) {
		return s.updateRepository(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: one, OperationID: op("delete"), Tags: []string{tagRepositories},
		Summary: "Delete a " + what, Access: note(rest.RBACInHandler(res, rbac.ActionDelete, repoObjectDoc), "missing and forbidden repositories both answer 403"),
		Description: "Deletes the Repository object and the credentials Secret managed for it; Secrets not managed by the API are kept." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryQuery) (*apiclient.Empty, error) {
		return s.deleteRepository(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: validate, OperationID: "validate" + map[bool]string{false: "Repository", true: "WriteRepository"}[p.write] + "Access", Tags: []string{tagRepositories},
		Summary: "Test a " + what + " connection without saving", MaxBodyBytes: maxWriteBodyBytes,
		Access:      note(rest.RBACInHandler(res, rbac.ActionCreate, repoObjectDoc), "spec.scmProviderRef also needs scmproviders, get, <name>"),
		Description: "Tests the settings and credentials of the body on the repo-server; nothing is stored. Read repositories without credentials use a matching credential template. A failed test answers 400 with the connection message." + gate,
	}, func(ctx context.Context, req *apiclient.RepositoryValidateRequest) (*v1alpha1.ConnectionState, error) {
		return s.validateAccess(ctx, p, req)
	})
}

func (s *Service) registerBrowse(t *rest.Table) {
	get := rest.RBACInHandler(rbac.ResourceRepositories, rbac.ActionGet, repoObjectDoc)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathRepositoryRefs, OperationID: "listRepositoryRefs", Tags: []string{tagRepositories},
		Summary: "List branches and tags of a git repository", Access: get,
		Description: "Credentials are resolved server-side from the registered repository (scoped to `appProject` when given), its credential template or SCM provider.",
	}, s.listRefs)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathRepositoryOCITags, OperationID: "listRepositoryOCITags", Tags: []string{tagRepositories},
		Summary: "List tags of an OCI repository", Access: get,
	}, s.listOCITags)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathRepositoryApps, OperationID: "listRepositoryApps", Tags: []string{tagRepositories},
		Summary: "Discover application directories",
		Access:  note(get, "also applications, create or update, <appProject>/<appName>, and the repository must be permitted by the project's sourceRepos"),
	}, s.listApps)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathRepositoryAppDetails, OperationID: "getRepositoryAppDetails", Tags: []string{tagRepositories},
		Summary: "Tool-specific details of an application source", MaxBodyBytes: maxWriteBodyBytes,
		Access: note(get, "also applications, get, <appProject>/<appName> (and create when the Application does not exist); for an existing Application the project must match and the source must be current or in its history; the repository must be permitted by the project's sourceRepos"),
	}, s.getAppDetails)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathRepositoryHelmCharts, OperationID: "listRepositoryHelmCharts", Tags: []string{tagRepositories},
		Summary: "List the charts of a Helm repository", Access: get,
	}, s.listHelmCharts)
}

func (s *Service) registerRepoCreds(t *rest.Table, p purpose) {
	list, one := apiclient.PathRepoCreds, apiclient.PathRepoCred
	suffix := "RepositoryCredential"
	what := "credential template"
	if p.write {
		list, one = apiclient.PathWriteRepoCreds, apiclient.PathWriteRepoCred
		suffix = "WriteRepositoryCredential"
		what = "write credential template"
	}
	res := p.resource
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: list, OperationID: "list" + suffix + "s", Tags: []string{tagRepoCreds},
		Summary: "List " + what + "s", Access: rest.Access{Mode: rest.AccessAuthenticated, Note: "items filtered by " + res + ", get, " + credObjectDoc},
	}, func(ctx context.Context, _ *apiclient.Empty) (*apiclient.RepositoryCredentialInfoList, error) {
		return s.listCredentials(ctx, p)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: list, OperationID: "create" + suffix, Tags: []string{tagRepoCreds},
		Summary: "Create a " + what, MaxBodyBytes: maxWriteBodyBytes,
		Access:      note(rest.RBACInHandler(res, rbac.ActionCreate, credObjectDoc), "spec.scmProviderRef also needs scmproviders, get, <name>"),
		Description: "Creates the RepositoryCredential object and the Secret holding `credentials`. An existing template with the same URL prefix is returned unchanged when identical, updated with `upsert`, rejected otherwise.",
	}, func(ctx context.Context, req *apiclient.RepositoryCredentialCreateRequest) (*apiclient.RepositoryCredentialInfo, error) {
		return s.createCredential(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: one, OperationID: "update" + suffix, Tags: []string{tagRepoCreds},
		Summary: "Update a " + what, MaxBodyBytes: maxWriteBodyBytes, Access: rest.RBACPathValue(res, rbac.ActionUpdate, "url"),
		Description: "The template is addressed by its exact URL prefix (URL-encoded).",
	}, func(ctx context.Context, req *apiclient.RepositoryCredentialUpdateRequest) (*apiclient.RepositoryCredentialInfo, error) {
		return s.updateCredential(ctx, p, req)
	})
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: one, OperationID: "delete" + suffix, Tags: []string{tagRepoCreds},
		Summary: "Delete a " + what, Access: rest.RBACPathValue(res, rbac.ActionDelete, "url"),
		Description: "Deletes the template with exactly this URL prefix and the credentials Secret managed for it.",
	}, func(ctx context.Context, req *apiclient.RepositoryCredentialQuery) (*apiclient.Empty, error) {
		return s.deleteCredential(ctx, p, req)
	})
}

func (s *Service) registerCertificates(t *rest.Table) {
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathCertificates, OperationID: "listCertificates", Tags: []string{tagCertificates},
		Summary: "List TLS certificates and SSH known hosts", Access: rest.RBAC(rbac.ResourceCertificates, rbac.ActionGet, `""`, emptyObject),
	}, s.listCertificates)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathCertificates, OperationID: "createCertificates", Tags: []string{tagCertificates},
		Summary: "Add TLS certificates and SSH known hosts", MaxBodyBytes: maxTrustBodyBytes,
		Access:      rest.RBAC(rbac.ResourceCertificates, rbac.ActionCreate, `""`, emptyObject),
		Description: "Validates every entry first (nothing is saved when one fails). Existing entries with different data need `upsert`; identical entries are skipped and not reported.",
	}, s.createCertificates)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathCertificates, OperationID: "deleteCertificates", Tags: []string{tagCertificates},
		Summary: "Remove TLS certificates and SSH known hosts", Access: rest.RBAC(rbac.ResourceCertificates, rbac.ActionDelete, `""`, emptyObject),
		Description: "Removes every entry matching the query and returns them; an empty query removes every entry (upstream API semantics).",
	}, s.deleteCertificates)
}

func (s *Service) registerGPGKeys(t *rest.Table) {
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathGPGKeys, OperationID: "listGPGKeys", Tags: []string{tagGPGKeys},
		Summary: "List GnuPG public keys", Access: rest.RBAC(rbac.ResourceGPGKeys, rbac.ActionGet, `""`, emptyObject),
	}, s.listGPGKeys)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathGPGKeys, OperationID: "createGPGKeys", Tags: []string{tagGPGKeys},
		Summary: "Add GnuPG public keys", MaxBodyBytes: maxTrustBodyBytes, Access: rest.RBAC(rbac.ResourceGPGKeys, rbac.ActionCreate, `""`, emptyObject),
		Description: "`keyData` holds one or more ASCII-armored public keys; keys already configured are reported in `skipped`. Private keys are rejected.",
	}, s.createGPGKeys)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathGPGKey, OperationID: "getGPGKey", Tags: []string{tagGPGKeys},
		Summary: "Get a GnuPG public key", Access: rest.RBAC(rbac.ResourceGPGKeys, rbac.ActionGet, `""`, emptyObject),
		Description: "`keyID` is the 16-hex-digit key ID or the 40-hex-digit fingerprint.",
	}, s.getGPGKey)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathGPGKey, OperationID: "deleteGPGKey", Tags: []string{tagGPGKeys},
		Summary: "Remove a GnuPG public key", Access: rest.RBAC(rbac.ResourceGPGKeys, rbac.ActionDelete, `""`, emptyObject),
	}, s.deleteGPGKey)
}

func (s *Service) registerSCMProviders(t *rest.Table) {
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathSCMProviders, OperationID: "listSCMProviders", Tags: []string{tagSCMProviders},
		Summary: "List SCM providers", Access: rest.Access{Mode: rest.AccessAuthenticated, Note: "items filtered by scmproviders, get, <name>"},
	}, s.listSCMProviders)
	rest.Register(t, rest.Route{
		Method: http.MethodPost, Path: apiclient.PathSCMProviders, OperationID: "createSCMProvider", Tags: []string{tagSCMProviders},
		Summary: "Create an SCM provider", MaxBodyBytes: maxWriteBodyBytes,
		Access:      rest.RBACInHandler(ResourceSCMProviders, rbac.ActionCreate, "<metadata.name>"),
		Description: "Secrets are referenced by name only; the API never accepts secret values for providers. Granting create or update lets the caller send the referenced Secrets' credentials to the provider URLs it chooses.",
	}, s.createSCMProvider)
	rest.Register(t, rest.Route{
		Method: http.MethodGet, Path: apiclient.PathSCMProvider, OperationID: "getSCMProvider", Tags: []string{tagSCMProviders},
		Summary: "Get an SCM provider", Access: rest.RBACPathValue(ResourceSCMProviders, rbac.ActionGet, "name"),
	}, s.getSCMProvider)
	rest.Register(t, rest.Route{
		Method: http.MethodPut, Path: apiclient.PathSCMProvider, OperationID: "updateSCMProvider", Tags: []string{tagSCMProviders},
		Summary: "Update an SCM provider", MaxBodyBytes: maxWriteBodyBytes, Access: rest.RBACPathValue(ResourceSCMProviders, rbac.ActionUpdate, "name"),
		Description: "Replaces spec, labels and annotations (`metadata.resourceVersion` for optimistic concurrency, 409 when stale).",
	}, s.updateSCMProvider)
	rest.Register(t, rest.Route{
		Method: http.MethodDelete, Path: apiclient.PathSCMProvider, OperationID: "deleteSCMProvider", Tags: []string{tagSCMProviders},
		Summary: "Delete an SCM provider", Access: rest.RBACPathValue(ResourceSCMProviders, rbac.ActionDelete, "name"),
	}, s.deleteSCMProvider)
}
