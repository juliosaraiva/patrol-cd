package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// Defaults of Options.
const (
	DefaultTokenRefreshBefore = 5 * time.Minute
	DefaultTokenCacheSize     = 1024
	DefaultMintTimeout        = 30 * time.Second
	// EnvAzureARMTokenResource overrides DefaultAzureARMTokenResource (upstream name).
	EnvAzureARMTokenResource = "AZURE_ARM_TOKEN_RESOURCE"
)

// Options configures a Resolver.
type Options struct {
	// Logger receives structured logs (never secret values). Nil discards them.
	Logger *slog.Logger
	// Registerer registers the Prometheus collectors. Nil keeps them unregistered.
	Registerer prometheus.Registerer
	// Now is the clock of token caching. Nil means time.Now.
	Now func() time.Time
	// Transport is the base transport of token endpoint requests (GitHub, Google, Microsoft
	// Entra ID, Azure Container Registry). Nil means a clone of http.DefaultTransport.
	Transport http.RoundTripper
	// RepoServer runs connection tests for Validate. Optional.
	RepoServer apiclient.Client
	// AzureWorkloadIdentity issues tokens of the process workload identity. Nil uses
	// azidentity's WorkloadIdentityCredential (created on first use).
	AzureWorkloadIdentity AzureTokenProvider
	// AzureARMTokenResource is the ARM resource exchanged for Azure Container Registry
	// tokens. Empty reads AZURE_ARM_TOKEN_RESOURCE, then DefaultAzureARMTokenResource.
	AzureARMTokenResource string
	// AzureServicePrincipalCacheTTL caps the reuse of service principal tokens; zero or more
	// than MaxAzureServicePrincipalCacheTTL means MaxAzureServicePrincipalCacheTTL.
	AzureServicePrincipalCacheTTL time.Duration
	// TokenRefreshBefore is how long before expiry a cached token is replaced (zero means
	// DefaultTokenRefreshBefore; at most half of the token lifetime is used).
	TokenRefreshBefore time.Duration
	// TokenCacheSize bounds the token cache (zero means DefaultTokenCacheSize).
	TokenCacheSize int
	// MintTimeout bounds one token mint, shared by concurrent callers (zero means
	// DefaultMintTimeout).
	MintTimeout time.Duration
	// UserAgent of GitHub API requests. Empty uses the pkg/scm default.
	UserAgent string
	// LookupEnv reads environment variables. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)

	// Endpoint policy of token requests (see docs/architecture/components/repository.md).

	// GoogleTokenHosts lists host names Google credentials may use as token_uri or service
	// account impersonation endpoint in addition to Google's own (oauth2.googleapis.com,
	// accounts.google.com, www.googleapis.com, iamcredentials.googleapis.com), for example a
	// Private Service Connect endpoint.
	GoogleTokenHosts []string
	// AzureAuthorityHosts lists host names accepted as
	// azureServicePrincipal.activeDirectoryEndpoint in addition to the Microsoft Entra
	// authorities of the public, US Government and China clouds.
	AzureAuthorityHosts []string
	// AzureContainerRegistryHosts lists the registry host patterns ("host" or "*.domain")
	// whose token exchange may receive the process workload identity's ARM token (Helm
	// repositories with useAzureWorkloadIdentity). Nil means *.azurecr.io, *.azurecr.cn and
	// *.azurecr.us; an empty non-nil list disables the exchange.
	AzureContainerRegistryHosts []string
	// AzureDevOpsHosts lists the host patterns of https git repositories that may receive
	// the process workload identity's Azure DevOps token. Nil means dev.azure.com and
	// *.visualstudio.com; an empty non-nil list disables workload identity for git.
	AzureDevOpsHosts []string
	// DeniedTokenNetworks are address ranges token requests (and their proxies) never
	// connect to, in addition to link-local (instance metadata), unspecified and multicast
	// addresses and, unless AllowLoopbackTokenEndpoints, loopback. Set it to the cluster's
	// pod and service CIDRs.
	DeniedTokenNetworks []netip.Prefix
	// AllowLoopbackTokenEndpoints permits token requests to, and proxies on, loopback
	// addresses (tests, local development, a loopback egress proxy).
	AllowLoopbackTokenEndpoints bool
}

// Resolver resolves repository access for repo-server and commit-server requests. It is
// safe for concurrent use.
type Resolver struct {
	lister           *Lister
	logger           *slog.Logger
	now              func() time.Time
	transport        http.RoundTripper
	repoServer       apiclient.Client
	azureWI          AzureTokenProvider
	armTokenResource string
	azureSPCacheTTL  time.Duration
	userAgent        string
	tokens           *tokenCache
	metrics          *metrics

	guard          *egressGuard
	googleHosts    hostPatterns
	authorityHosts hostPatterns
	acrHosts       hostPatterns
	devOpsHosts    hostPatterns
}

// NewResolver returns a Resolver over lister (which must be started before lookups).
func NewResolver(lister *Lister, opts Options) (*Resolver, error) {
	if lister == nil {
		return nil, errors.New("repository resolver: nil lister")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	transport := opts.Transport
	if transport == nil {
		dt, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("repository resolver: http.DefaultTransport is not *http.Transport")
		}
		transport = dt.Clone()
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	arm := opts.AzureARMTokenResource
	if arm == "" {
		if v, ok := lookup(EnvAzureARMTokenResource); ok && v != "" {
			arm = v
		} else {
			arm = DefaultAzureARMTokenResource
		}
	}
	spTTL := opts.AzureServicePrincipalCacheTTL
	if spTTL <= 0 || spTTL > MaxAzureServicePrincipalCacheTTL {
		if spTTL > MaxAzureServicePrincipalCacheTTL {
			logger.Warn("Azure service principal token cache duration capped", "requested", spTTL, "max", MaxAzureServicePrincipalCacheTTL)
		}
		spTTL = MaxAzureServicePrincipalCacheTTL
	}
	refresh := opts.TokenRefreshBefore
	if refresh <= 0 {
		refresh = DefaultTokenRefreshBefore
	}
	size := opts.TokenCacheSize
	if size <= 0 {
		size = DefaultTokenCacheSize
	}
	mintTimeout := opts.MintTimeout
	if mintTimeout <= 0 {
		mintTimeout = DefaultMintTimeout
	}
	googleHosts, err := compileHostPatterns("GoogleTokenHosts", googleTokenHosts, opts.GoogleTokenHosts)
	if err != nil {
		return nil, err
	}
	authorityHosts, err := compileHostPatterns("AzureAuthorityHosts", azureAuthorityHosts, opts.AzureAuthorityHosts)
	if err != nil {
		return nil, err
	}
	acrList := opts.AzureContainerRegistryHosts
	if acrList == nil {
		acrList = defaultACRHosts
	}
	acrHosts, err := compileHostPatterns("AzureContainerRegistryHosts", acrList)
	if err != nil {
		return nil, err
	}
	devOpsList := opts.AzureDevOpsHosts
	if devOpsList == nil {
		devOpsList = defaultAzureDevOpsHosts
	}
	devOpsHosts, err := compileHostPatterns("AzureDevOpsHosts", devOpsList)
	if err != nil {
		return nil, err
	}
	denied := make([]netip.Prefix, 0, len(opts.DeniedTokenNetworks))
	for _, p := range opts.DeniedTokenNetworks {
		if !p.IsValid() {
			return nil, fmt.Errorf("repository resolver: invalid DeniedTokenNetworks entry %v", p)
		}
		denied = append(denied, p.Masked())
	}
	m, err := newMetrics(opts.Registerer)
	if err != nil {
		return nil, err
	}
	tokens, err := newTokenCache(size, now, refresh, mintTimeout, m)
	if err != nil {
		return nil, err
	}
	r := &Resolver{
		lister:           lister,
		logger:           logger.With("component", "repository-resolver"),
		now:              now,
		transport:        transport,
		repoServer:       opts.RepoServer,
		armTokenResource: strings.TrimRight(arm, "/"),
		azureSPCacheTTL:  spTTL,
		userAgent:        opts.UserAgent,
		tokens:           tokens,
		metrics:          m,
		guard:            &egressGuard{allowLoopback: opts.AllowLoopbackTokenEndpoints, denied: denied},
		googleHosts:      googleHosts,
		authorityHosts:   authorityHosts,
		acrHosts:         acrHosts,
		devOpsHosts:      devOpsHosts,
	}
	r.azureWI = opts.AzureWorkloadIdentity
	if r.azureWI == nil {
		r.azureWI = &workloadIdentityProvider{transport: &http.Client{Transport: transport, Timeout: azureRequestTimeout}}
	}
	return r, nil
}

// Query selects the repository access to resolve.
type Query struct {
	// URL of the repository as written in the Application source (or the hydrator target).
	URL string
	// Project is the AppProject of the Application; project-scoped Repositories of other
	// projects are never used.
	Project string
	// Purpose is read (default) or write (commit-server pushes).
	Purpose v1alpha1.RepositoryPurpose
	// Type is how the caller uses the repository: helm for Helm chart sources, oci for
	// oci:// sources, git otherwise (upstream TestRepoWithKnownType). Empty uses the
	// configured type, then the URL (oci:// means oci), then git.
	Type v1alpha1.RepositoryType
}

// Source says which Repository object matched a URL.
type Source string

// Sources.
const (
	// SourceProject: a Repository scoped to the requested project.
	SourceProject Source = "project"
	// SourceGlobal: a Repository without spec.project.
	SourceGlobal Source = "global"
	// SourceNone: no Repository; the URL was resolved as a stub (templates may apply).
	SourceNone Source = "none"
)

// Method is the credential method selected for a repository.
type Method string

// Methods.
const (
	MethodNone                  Method = "none"
	MethodHTTPS                 Method = "https"
	MethodSSH                   Method = "ssh"
	MethodGitHubApp             Method = "githubApp"
	MethodGCP                   Method = "gcp"
	MethodAzureWorkloadIdentity Method = "azureWorkloadIdentity"
	MethodAzureServicePrincipal Method = "azureServicePrincipal"
	// MethodBasic is username/password access to a Helm or OCI repository.
	MethodBasic Method = "basic"
)

// Resolution is a resolved repository access and how it was obtained.
type Resolution struct {
	// Repository is the access material to forward to the repo-server or commit-server. It
	// holds secrets: never log it except through its LogValue.
	Repository *apiclient.Repository
	// Source says which Repository matched (none for a stub).
	Source Source
	// RepositoryName is the matched Repository ("" for a stub).
	RepositoryName string
	// CredentialName is the RepositoryCredential whose credentials were inherited.
	CredentialName string
	// Inherited is true when the credentials come from a credential template (upstream
	// inheritedCreds).
	Inherited bool
	// Method is the selected credential method.
	Method Method
}

// LogValue implements slog.LogValuer without secrets.
func (r *Resolution) LogValue() slog.Value {
	if r == nil {
		return slog.Value{}
	}
	attrs := []slog.Attr{
		slog.String("source", string(r.Source)),
		slog.String("repository", r.RepositoryName),
		slog.String("credential", r.CredentialName),
		slog.Bool("inherited", r.Inherited),
		slog.String("method", string(r.Method)),
	}
	if r.Repository != nil {
		attrs = append(attrs, slog.String("repo", git.SanitizeURL(r.Repository.URL)), slog.String("type", string(r.Repository.Type)))
	}
	return slog.GroupValue(attrs...)
}

// ResolveForURL resolves the access to repoURL for an Application of project (purpose read
// or write). See Resolve.
func (r *Resolver) ResolveForURL(ctx context.Context, repoURL, project string, purpose v1alpha1.RepositoryPurpose) (*apiclient.Repository, error) {
	res, err := r.Resolve(ctx, Query{URL: repoURL, Project: project, Purpose: purpose})
	if err != nil {
		return nil, err
	}
	return res.Repository, nil
}

// Resolve resolves the access to q.URL with upstream precedence: a Repository of q.Purpose
// at the same location scoped to q.Project, else an unscoped one, else a stub; without
// credentials of its own, the longest-prefix RepositoryCredential of the same purpose is
// inherited; then the credential method is selected and short-lived tokens are minted.
func (r *Resolver) Resolve(ctx context.Context, q Query) (*Resolution, error) {
	purpose := v1alpha1.RepositoryPurpose(purposeKey(q.Purpose))
	res, err := r.resolve(ctx, q, purpose)
	if err != nil {
		r.metrics.resolutionError(string(purpose), err)
		if !errors.Is(err, ErrNotSynced) && ctx.Err() == nil {
			r.logger.Warn("failed to resolve repository access", logRepo(q.URL), "project", q.Project,
				"purpose", string(purpose), "reason", ConditionReason(err), "error", err)
		}
		return nil, err
	}
	r.metrics.resolution(string(purpose), sourceLabel(res), string(res.Method))
	r.logger.Debug("resolved repository access", "project", q.Project, "purpose", string(purpose), "resolution", res)
	return res, nil
}

func sourceLabel(res *Resolution) string {
	switch {
	case res.Inherited:
		return "template"
	case res.Source == SourceNone:
		return "anonymous"
	default:
		return string(res.Source)
	}
}

func (r *Resolver) resolve(ctx context.Context, q Query, purpose v1alpha1.RepositoryPurpose) (*Resolution, error) {
	if strings.TrimSpace(q.URL) == "" {
		return nil, configError(ErrInvalidConfiguration, "repository URL is empty")
	}
	if len(q.URL) > maxURLLength {
		return nil, configError(ErrInvalidConfiguration, "repository URL exceeds %d bytes", maxURLLength)
	}
	switch purpose {
	case v1alpha1.RepositoryPurposeRead, v1alpha1.RepositoryPurposeWrite:
	default:
		return nil, configError(ErrInvalidConfiguration, "unknown repository purpose %q", purpose)
	}
	candidates, err := r.lister.repositoriesForURL(purpose, q.URL)
	if err != nil {
		return nil, err
	}
	repo := r.pickRepository(candidates, q.Project, q.URL)
	res := &Resolution{Source: SourceNone}
	var a *access
	if repo != nil {
		res.RepositoryName = repo.Name
		res.Source = SourceGlobal
		if repo.Spec.Project != "" {
			res.Source = SourceProject
		}
		if a, err = r.repositoryAccess(repo, nil); err != nil {
			return nil, wrapResolve(q.URL, err, nil)
		}
	} else {
		a = &access{url: q.URL}
	}
	if err := r.inherit(a, purpose, res); err != nil {
		return nil, wrapResolve(q.URL, err, a.secrets)
	}
	out, method, err := r.materialize(ctx, a, q.Type)
	if err != nil {
		return nil, wrapResolve(q.URL, err, a.secrets)
	}
	res.Repository, res.Method = out, method
	return res, nil
}

func wrapResolve(repoURL string, err error, secrets []string) error {
	return redact(fmt.Errorf("unable to resolve repository %q: %w", git.SanitizeURL(repoURL), err), secrets)
}

// pickRepository applies upstream getRepositorySecret: the first Repository (by name) scoped
// to project wins, otherwise the first unscoped one.
func (r *Resolver) pickRepository(candidates []*v1alpha1.Repository, projectName, repoURL string) *v1alpha1.Repository {
	var scoped, global []*v1alpha1.Repository
	for _, c := range candidates {
		switch c.Spec.Project {
		case projectName:
			scoped = append(scoped, c)
		case "":
			global = append(global, c)
		}
	}
	pick := scoped
	if len(pick) == 0 {
		pick = global
	}
	if len(pick) == 0 {
		return nil
	}
	if len(pick) > 1 {
		names := make([]string, len(pick))
		for i, p := range pick {
			names[i] = p.Name
		}
		r.logger.Warn("found multiple credentials for repoURL; using the first by name", logRepo(repoURL), "project", projectName, "repositories", names)
	}
	return pick[0]
}

// repositoryAccess loads the access of a Repository object (settings, Secret, SCMProvider).
func (r *Resolver) repositoryAccess(repo *v1alpha1.Repository, inline map[string][]byte) (*access, error) {
	use := secretUse{
		owner:      fmt.Sprintf("Repository %q", repo.Name),
		secretType: repositorySecretType(repo.Spec.Purpose),
		project:    repo.Spec.Project,
		ownerName:  repo.Name,
		ownerUID:   repo.UID,
	}
	a, err := r.loadAccess(&repo.Spec.RepositoryAccess, use, repo.Spec.URL, inline)
	if err != nil {
		return nil, err
	}
	a.url = repo.Spec.URL
	a.typ = repo.Spec.Type
	a.name = repo.Spec.Name
	a.project = repo.Spec.Project
	a.insecure = repo.Spec.Insecure
	a.insecureIgnoreHostKey = repo.Spec.InsecureIgnoreHostKey //nolint:staticcheck // deprecated upstream but still honored (IsInsecure parity)
	a.enableLFS = repo.Spec.EnableLFS
	a.depth = repo.Spec.Depth
	return a, nil
}

// inherit applies the longest-prefix credential template of purpose when a has no
// credentials of its own (upstream enrichCredsToRepo).
func (r *Resolver) inherit(a *access, purpose v1alpha1.RepositoryPurpose, res *Resolution) error {
	if a.hasCredentials() {
		return nil
	}
	creds, err := r.lister.credentials(purpose)
	if err != nil {
		return err
	}
	best := r.pickTemplate(creds, a.url)
	if best == nil {
		return nil
	}
	t, err := r.loadAccess(&best.Spec.RepositoryAccess, credentialUse(best), a.url, nil)
	if err != nil {
		return err
	}
	t.typ = best.Spec.Type
	a.copyCredentialsFrom(t)
	res.Inherited = true
	res.CredentialName = best.Name
	return nil
}

// credentialUse describes the credentialsRef of a RepositoryCredential.
func credentialUse(c *v1alpha1.RepositoryCredential) secretUse {
	return secretUse{owner: fmt.Sprintf("RepositoryCredential %q", c.Name), secretType: credentialSecretType(c.Spec.Purpose)}
}

// pickTemplate returns the template whose normalized prefix is the longest string prefix of
// the normalized URL (ties: first by name, with a warning).
func (r *Resolver) pickTemplate(creds []*v1alpha1.RepositoryCredential, repoURL string) *v1alpha1.RepositoryCredential {
	rk := prefixKey(repoURL)
	var best *v1alpha1.RepositoryCredential
	bestLen, ties := 0, 0
	for _, c := range creds {
		n, ok := prefixMatches(rk, c.Spec.URLPrefix)
		if !ok {
			continue
		}
		switch {
		case n > bestLen:
			best, bestLen, ties = c, n, 0
		case n == bestLen:
			ties++
		}
	}
	if ties > 0 {
		r.logger.Warn("found multiple credentials for repoURL; using the first by name", logRepo(repoURL), "credential", best.Name)
	}
	return best
}

// effectiveType decides the repository type (see Query.Type).
func effectiveType(configured, hint v1alpha1.RepositoryType, repoURL string) v1alpha1.RepositoryType {
	switch hint {
	case v1alpha1.RepositoryTypeHelm, v1alpha1.RepositoryTypeOCI:
		return hint
	case v1alpha1.RepositoryTypeGit:
		if configured == v1alpha1.RepositoryTypeOCI {
			return configured
		}
		return v1alpha1.RepositoryTypeGit
	}
	if len(repoURL) >= 6 && strings.EqualFold(repoURL[:6], "oci://") {
		return v1alpha1.RepositoryTypeOCI
	}
	if configured != "" {
		return configured
	}
	return v1alpha1.RepositoryTypeGit
}

// gitMethod selects the git credential method in upstream GetGitCreds order. A GitHub App
// or service principal that is configured but lacks its secret is an error (upstream silently
// falls back to anonymous access).
func gitMethod(a *access, owner string) (Method, error) {
	switch {
	case a.password != "" || a.bearerToken != "":
		return MethodHTTPS, nil
	case a.sshPrivateKey != "":
		return MethodSSH, nil
	case a.githubAppID != 0 && a.githubAppPrivateKey != "":
		return MethodGitHubApp, nil
	case a.githubAppID != 0:
		return "", configError(ErrSecretKeyMissing, "%s: githubApp is configured but no private key is available (credentials Secret key %q or an SCMProvider with a GitHub App)",
			owner, v1alpha1.RepositorySecretKeyGitHubAppPrivateKey)
	case a.gcpServiceAccountKey != "":
		return MethodGCP, nil
	case a.useAzureWorkloadIdentity:
		return MethodAzureWorkloadIdentity, nil
	case a.azureSPClientID != "" && a.azureSPTenantID != "" && a.azureSPClientSecret != "":
		return MethodAzureServicePrincipal, nil
	case a.azureSPClientID != "" || a.azureSPTenantID != "":
		return "", configError(ErrSecretKeyMissing, "%s: azureServicePrincipal is configured but the credentials Secret has no key %q",
			owner, v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret)
	default:
		return MethodNone, nil
	}
}

// materialize turns an access into the repo-server's Repository: settings per type and only
// the credential material of the selected method (tokens minted as needed).
func (r *Resolver) materialize(ctx context.Context, a *access, hint v1alpha1.RepositoryType) (*apiclient.Repository, Method, error) {
	typ := effectiveType(a.typ, hint, a.url)
	out := &apiclient.Repository{
		URL:                  a.url,
		Type:                 typ,
		Name:                 a.name,
		Project:              a.project,
		EnableOCI:            a.enableOCI,
		InsecureOCIForceHTTP: a.insecureOCIForceHTTP,
		Proxy:                a.proxy,
		NoProxy:              a.noProxy,
	}
	owner := "repository " + fmt.Sprintf("%q", git.SanitizeURL(a.url))
	if a.provider != nil {
		if err := a.provider.check(a); err != nil {
			return nil, "", fmt.Errorf("%s: %w", owner, err)
		}
	}
	setClientCert := func() {
		if a.tlsClientCertData != "" && a.tlsClientCertKey != "" {
			out.TLSClientCertData, out.TLSClientCertKey = a.tlsClientCertData, a.tlsClientCertKey
		}
	}
	setToken := func(t mintedToken) {
		if !t.expiresAt.IsZero() {
			exp := t.expiresAt
			out.CredentialsExpireAt = &exp
		}
	}
	switch typ {
	case v1alpha1.RepositoryTypeGit:
		out.Insecure = a.isInsecure()
		out.InsecureIgnoreHostKey = a.insecureIgnoreHostKey
		out.EnableLFS = a.enableLFS
		out.Depth = a.depth
		method, err := gitMethod(a, owner)
		if err != nil {
			return nil, "", err
		}
		switch method {
		case MethodHTTPS:
			out.Username, out.Password, out.BearerToken = a.username, a.password, a.bearerToken
			out.ForceHTTPBasicAuth = a.forceHTTPBasicAuth
			setClientCert()
		case MethodSSH:
			out.SSHPrivateKey = a.sshPrivateKey
		case MethodGitHubApp:
			t, err := r.githubAppToken(ctx, a)
			if err != nil {
				return nil, "", err
			}
			out.Username, out.Password = t.username, t.value
			setClientCert()
			setToken(t)
		case MethodGCP:
			t, err := r.gcpToken(ctx, a.gcpServiceAccountKey)
			if err != nil {
				return nil, "", err
			}
			out.Username, out.Password = t.username, t.value
			setToken(t)
		case MethodAzureWorkloadIdentity:
			if err := r.checkAzureDevOpsURL(a.url); err != nil {
				return nil, "", fmt.Errorf("%s: %w", owner, err)
			}
			t, err := r.azureWorkloadIdentityToken(ctx, AzureDevOpsScope)
			if err != nil {
				return nil, "", err
			}
			out.Username, out.Password, out.BearerToken = AzureEmptyGUID, t.value, t.value
			setToken(t)
		case MethodAzureServicePrincipal:
			t, err := r.azureServicePrincipalToken(ctx, a)
			if err != nil {
				return nil, "", err
			}
			out.Username, out.Password, out.BearerToken = AzureEmptyGUID, t.value, t.value
			setClientCert()
			setToken(t)
		}
		return out, method, nil
	case v1alpha1.RepositoryTypeHelm:
		out.Insecure = a.insecure
		out.EnableOCI = a.enableOCI || isHelmOCIRepo(a.url)
		setClientCert()
		if a.useAzureWorkloadIdentity {
			t, err := r.acrRefreshToken(ctx, a.url)
			if err != nil {
				return nil, "", err
			}
			out.Username, out.Password = t.username, t.value
			setToken(t)
			return out, MethodAzureWorkloadIdentity, nil
		}
		return out, basicCreds(out, a), nil
	case v1alpha1.RepositoryTypeOCI:
		out.Insecure = a.insecure
		setClientCert()
		return out, basicCreds(out, a), nil
	default:
		return nil, "", configError(ErrInvalidConfiguration, "%s: unsupported repository type %q", owner, typ)
	}
}

func basicCreds(out *apiclient.Repository, a *access) Method {
	out.Username, out.Password = a.username, a.password
	if a.username != "" || a.password != "" {
		return MethodBasic
	}
	return MethodNone
}

// ResolveRepository resolves the access of one Repository object (rather than a URL
// lookup): its own credentials, or the matching template of its purpose when it has none.
// inline, when non-nil, replaces the data of the Secret named by spec.credentialsRef
// (validation of unsaved input). typeHint is as Query.Type.
func (r *Resolver) ResolveRepository(ctx context.Context, repo *v1alpha1.Repository, inline map[string][]byte, typeHint v1alpha1.RepositoryType) (*Resolution, error) {
	return r.resolveObject(ctx, repo, inline, true, typeHint)
}

func (r *Resolver) resolveObject(ctx context.Context, repo *v1alpha1.Repository, inline map[string][]byte, templates bool, typeHint v1alpha1.RepositoryType) (*Resolution, error) {
	if repo == nil {
		return nil, configError(ErrInvalidConfiguration, "nil repository")
	}
	if err := r.lister.check(); err != nil {
		return nil, err
	}
	if len(repo.Spec.URL) > maxURLLength || strings.TrimSpace(repo.Spec.URL) == "" {
		return nil, configError(ErrInvalidConfiguration, "Repository %q: invalid URL", repo.Name)
	}
	res := &Resolution{RepositoryName: repo.Name, Source: SourceGlobal}
	if repo.Spec.Project != "" {
		res.Source = SourceProject
	}
	a, err := r.repositoryAccess(repo, inline)
	if err != nil {
		return nil, wrapResolve(repo.Spec.URL, err, nil)
	}
	if templates {
		if err := r.inherit(a, v1alpha1.RepositoryPurpose(purposeKey(repo.Spec.Purpose)), res); err != nil {
			return nil, wrapResolve(repo.Spec.URL, err, a.secrets)
		}
	}
	out, method, err := r.materialize(ctx, a, typeHint)
	if err != nil {
		return nil, wrapResolve(repo.Spec.URL, err, a.secrets)
	}
	res.Repository, res.Method = out, method
	return res, nil
}

// ReposForProject returns the registered repositories the repo-server may use for Helm
// dependencies of an Application in proj (ManifestRequest.Repos): read-purpose Helm
// repositories with a name and, with includeOCI (OCI sources), OCI repositories, whose URL is
// permitted by proj's sourceRepos (pass the effective project). Unlike upstream, Repositories
// scoped to another project are excluded; when a URL is registered both scoped to proj and
// unscoped, the scoped one is used. Repositories whose credentials cannot be resolved are
// skipped with a warning. The result is ordered by Repository name.
func (r *Resolver) ReposForProject(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.Repository, error) {
	if proj == nil {
		return nil, errors.New("repos for project: nil project")
	}
	all, err := r.lister.allRepositories(v1alpha1.RepositoryPurposeRead)
	if err != nil {
		return nil, err
	}
	chosen := map[string]*v1alpha1.Repository{}
	for _, repo := range all {
		eligible := (repo.Spec.Type == v1alpha1.RepositoryTypeHelm && repo.Spec.Name != "") ||
			(includeOCI && repo.Spec.Type == v1alpha1.RepositoryTypeOCI)
		if !eligible || (repo.Spec.Project != "" && repo.Spec.Project != proj.Name) || !project.IsSourcePermitted(proj, repo.Spec.URL) {
			continue
		}
		k := string(repo.Spec.Type) + "\x00" + urlKey(repo.Spec.URL)
		if prev, ok := chosen[k]; ok && (prev.Spec.Project != "" || repo.Spec.Project == "") {
			continue
		}
		chosen[k] = repo
	}
	picked := make([]*v1alpha1.Repository, 0, len(chosen))
	for _, repo := range chosen {
		picked = append(picked, repo)
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Name < picked[j].Name })
	out := make([]apiclient.Repository, 0, len(picked))
	for _, repo := range picked {
		res, err := r.resolveObject(ctx, repo, nil, true, repo.Spec.Type)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			r.logger.Warn("skipping Helm dependency repository", "repository", repo.Name, "reason", ConditionReason(err), "error", err)
			continue
		}
		out = append(out, *res.Repository)
	}
	return out, nil
}

// HelmCredentialTemplates returns the read-purpose RepositoryCredentials of type helm (and,
// with includeOCI, oci) whose prefix is permitted by proj's sourceRepos, resolved for the
// repo-server (ManifestRequest.HelmRepoCreds), ordered by name. Templates whose credentials
// cannot be resolved are skipped with a warning.
func (r *Resolver) HelmCredentialTemplates(ctx context.Context, proj *v1alpha1.AppProject, includeOCI bool) ([]apiclient.RepositoryCredentialTemplate, error) {
	if proj == nil {
		return nil, errors.New("helm credential templates: nil project")
	}
	creds, err := r.lister.credentials(v1alpha1.RepositoryPurposeRead)
	if err != nil {
		return nil, err
	}
	var out []apiclient.RepositoryCredentialTemplate
	for _, c := range creds {
		typ := c.Spec.Type
		if typ != v1alpha1.RepositoryTypeHelm && (!includeOCI || typ != v1alpha1.RepositoryTypeOCI) {
			continue
		}
		if !project.IsSourcePermitted(proj, c.Spec.URLPrefix) {
			continue
		}
		a, err := r.loadAccess(&c.Spec.RepositoryAccess, credentialUse(c), c.Spec.URLPrefix, nil)
		if err == nil {
			a.url, a.typ = c.Spec.URLPrefix, typ
			var acc *apiclient.Repository
			if acc, _, err = r.materialize(ctx, a, typ); err == nil {
				acc.URL = ""
				out = append(out, apiclient.RepositoryCredentialTemplate{URLPrefix: c.Spec.URLPrefix, Access: *acc})
				continue
			}
			err = redact(err, a.secrets)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		r.logger.Warn("skipping Helm credential template", "credential", c.Name, "reason", ConditionReason(err), "error", err)
	}
	return out, nil
}
