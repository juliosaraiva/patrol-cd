package generators

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/awscodecommit"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/azuredevops"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketcloud"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/bitbucketserver"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitea"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/gitlab"
)

// SCMConfig configures the SCM Provider and Pull Request generators.
type SCMConfig struct {
	// Enabled enables both generators (applicationsetcontroller.enable.scm.providers).
	Enabled bool
	// AllowedProviders, when non-empty, lists the only custom API URLs generators may use
	// (exact string match; an empty api is always allowed).
	AllowedProviders []string
	// RootCAPath is a PEM file appended to the trusted CAs of GitLab and Bitbucket Server.
	RootCAPath string
	// TokenRefStrictMode requires Secrets referenced by generators (and SCMProviders) to carry
	// cd.patrol.example/secret-type=scm-creds.
	TokenRefStrictMode bool
	// ProxyURL and NoProxy route GitHub, GitLab, Gitea and Bitbucket Server API traffic through a
	// proxy (--scm-proxy-url, --scm-no-proxy).
	ProxyURL string
	NoProxy  string
	// Observer, when set, returns the request observer of an ApplicationSet's GitHub clients
	// (GitHub API metrics).
	Observer func(appSetNamespace, appSetName string) scm.RequestObserver
	// NewProvider builds providers; nil uses DefaultProviderFactory(Registry).
	NewProvider ProviderFactory
	// Registry serves SCMProvider types without generator-specific options (e.g. fake); nil
	// uses scm.DefaultRegistry.
	Registry *scm.Registry
	// Logger receives provider debug logs (redacted); nil discards them.
	Logger *slog.Logger
}

// ProviderRequest is everything needed to construct one SCM provider instance.
type ProviderRequest struct {
	// Type is a pkg/scm provider type (github, gitlab, gitea, bitbucketserver, bitbucketcloud,
	// azuredevops, awscodecommit) or another type registered in the registry (fake).
	Type string
	// Config is the provider configuration (API URL, TLS, proxy, credentials).
	Config scm.Config
	// Provider-specific options.
	GitLab          gitlab.Options
	BitbucketServer bitbucketserver.Options
	BitbucketCloud  bitbucketcloud.Options
	AzureDevOps     azuredevops.Options
	CodeCommit      awscodecommit.Options
}

// ProviderFactory constructs providers. Factories must not perform network I/O.
type ProviderFactory func(ctx context.Context, req ProviderRequest) (scm.Provider, error)

// DefaultProviderFactory builds the pkg/scm providers with their generator options and falls
// back to registry (scm.DefaultRegistry when nil) for other types.
func DefaultProviderFactory(registry *scm.Registry) ProviderFactory {
	return func(ctx context.Context, req ProviderRequest) (scm.Provider, error) {
		req.Config.Type = req.Type
		switch req.Type {
		case github.Type:
			return github.NewProvider(ctx, req.Config)
		case gitlab.Type:
			return gitlab.NewProvider(ctx, req.Config, req.GitLab)
		case gitea.Type:
			return gitea.NewProvider(ctx, req.Config)
		case bitbucketserver.Type:
			return bitbucketserver.NewProvider(ctx, req.Config, req.BitbucketServer)
		case bitbucketcloud.Type:
			return bitbucketcloud.NewProvider(ctx, req.Config, req.BitbucketCloud)
		case azuredevops.Type:
			return azuredevops.NewProvider(ctx, req.Config, req.AzureDevOps)
		case awscodecommit.Type:
			return awscodecommit.NewProvider(ctx, req.Config, req.CodeCommit)
		}
		if registry == nil {
			registry = scm.DefaultRegistry
		}
		return registry.New(ctx, req.Config)
	}
}

func (c SCMConfig) factory() ProviderFactory {
	if c.NewProvider != nil {
		return c.NewProvider
	}
	return DefaultProviderFactory(c.Registry)
}

// ErrDisallowedSCMProvider reports a custom API URL that is not in the allow-list.
type ErrDisallowedSCMProvider struct {
	Provider string
	Allowed  []string
}

func (e ErrDisallowedSCMProvider) Error() string {
	return fmt.Sprintf("scm provider %q not allowed, must use one of the following: %s", e.Provider, strings.Join(e.Allowed, ", "))
}

// checkAllowed applies the allowed SCM providers list to a custom API URL.
func (c SCMConfig) checkAllowed(log *slog.Logger, appSet *v1alpha1.ApplicationSet, apiURL string) error {
	if apiURL == "" || len(c.AllowedProviders) == 0 || slices.Contains(c.AllowedProviders, apiURL) {
		return nil
	}
	log.Warn("attempted to use a disallowed SCM provider", appSetLogAttrs(appSet),
		slog.String("security", "medium"), slog.String("api", apiURL))
	return fmt.Errorf("scm provider not allowed: %w", ErrDisallowedSCMProvider{Provider: apiURL, Allowed: c.AllowedProviders})
}

// rootCA reads the SCM root CA file. A missing, unreadable or invalid file is logged and
// ignored (the system pool is used), as upstream.
func (c SCMConfig) rootCA(log *slog.Logger) []byte {
	if c.RootCAPath == "" {
		return nil
	}
	data, err := os.ReadFile(c.RootCAPath)
	if err != nil {
		log.Error("cannot read the SCM root CA file, proceeding without it", slog.String("path", c.RootCAPath), slog.Any("error", err))
		return nil
	}
	return data
}

// caBundle joins PEM bundles; an invalid bundle is logged and dropped so the system pool is
// used (upstream behavior).
func caBundle(log *slog.Logger, parts ...[]byte) []byte {
	var all []byte
	for _, p := range parts {
		if len(p) > 0 {
			all = append(all, p...)
			all = append(all, '\n')
		}
	}
	if len(all) == 0 {
		return nil
	}
	if !x509.NewCertPool().AppendCertsFromPEM(all) {
		log.Error("failed to append certificates from PEM: proceeding without custom root CA")
		return nil
	}
	return all
}

// credentials resolves Secrets and ConfigMaps referenced by generators.
type credentials struct {
	client    client.Reader
	namespace string // control-plane namespace (GitHub App Secrets)
	strict    bool
}

// secretRef returns the value of a key of a Secret in namespace. A nil ref yields "".
func (c credentials) secretRef(ctx context.Context, ref *v1alpha1.SecretRef, namespace string) (string, error) {
	if ref == nil {
		return "", nil
	}
	return c.secretKey(ctx, namespace, ref.SecretName, ref.Key, c.strict)
}

func (c credentials) secretKey(ctx context.Context, namespace, name, key string, strict bool) (string, error) {
	secret := &corev1.Secret{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return "", fmt.Errorf("error fetching secret %s/%s: %w", namespace, name, err)
	}
	if strict && secret.Labels[v1alpha1.LabelKeySecretType] != v1alpha1.LabelValueSecretTypeSCMCreds {
		return "", fmt.Errorf("secret %s/%s is not a valid SCM creds secret: secret must have label %q=%q",
			namespace, name, v1alpha1.LabelKeySecretType, v1alpha1.LabelValueSecretTypeSCMCreds)
	}
	value, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q in secret %s/%s not found", key, namespace, name)
	}
	return string(value), nil
}

// configMapData returns a key of a ConfigMap in namespace.
func (c credentials) configMapData(ctx context.Context, ref *v1alpha1.ConfigMapKeyRef, namespace string) ([]byte, error) {
	if ref == nil {
		return nil, nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.ConfigMapName}, cm); err != nil {
		return nil, err
	}
	data, ok := cm.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("key %s not found in ConfigMap %s", ref.Key, cm.Name)
	}
	return []byte(data), nil
}

// GitHub App Secret keys (Argo CD repository credential format).
const (
	secretKeyGitHubAppID                = "githubAppID"
	secretKeyGitHubAppInstallationID    = "githubAppInstallationID"
	secretKeyGitHubAppEnterpriseBaseURL = "githubAppEnterpriseBaseUrl"
)

// ErrAppSecretNotPermitted is returned when an ApplicationSet outside the control-plane
// namespace references a GitHub App Secret with appSecretName.
var ErrAppSecretNotPermitted = errors.New("appSecretName can only be used by ApplicationSets in the control-plane namespace")

// githubApp loads the GitHub App credentials of appSecretName from the control-plane namespace.
// The Secret holds the control plane's own credentials, so only ApplicationSets in the
// control-plane namespace may name it (others use providerRef with an SCMProvider and Secret in
// their own namespace). tokenRef strict mode does not apply.
func (c credentials) githubApp(ctx context.Context, appSetNamespace, name, owner string) (*scm.GitHubAppConfig, error) {
	if appSetNamespace != c.namespace {
		return nil, fmt.Errorf("%w (%s); use providerRef with an SCMProvider in namespace %s", ErrAppSecretNotPermitted, c.namespace, appSetNamespace)
	}
	secret := &corev1.Secret{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: c.namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("error getting creds for %s: %w", name, err)
	}
	key := secret.Data[v1alpha1.RepositorySecretKeyGitHubAppPrivateKey]
	if len(key) == 0 {
		return nil, fmt.Errorf("no github app found for %s", name)
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(secret.Data[secretKeyGitHubAppID])), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("error getting creds for %s: invalid %s: %w", name, secretKeyGitHubAppID, err)
	}
	var installationID int64
	if raw := strings.TrimSpace(string(secret.Data[secretKeyGitHubAppInstallationID])); raw != "" {
		if installationID, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return nil, fmt.Errorf("error getting creds for %s: invalid %s: %w", name, secretKeyGitHubAppInstallationID, err)
		}
	}
	return &scm.GitHubAppConfig{
		AppID:             appID,
		InstallationID:    installationID,
		PrivateKey:        key,
		EnterpriseBaseURL: string(secret.Data[secretKeyGitHubAppEnterpriseBaseURL]),
		Owner:             owner,
	}, nil
}

// resolvedProviderRef is the configuration of a referenced SCMProvider.
type resolvedProviderRef struct {
	scmType   string
	apiURL    string
	insecure  bool
	caData    []byte
	token     string
	username  string
	githubApp *scm.GitHubAppConfig
}

// scmProviderTypes maps SCMProvider types to pkg/scm provider types.
var scmProviderTypes = map[v1alpha1.SCMProviderType]string{
	v1alpha1.SCMProviderTypeGitHub:          github.Type,
	v1alpha1.SCMProviderTypeGitLab:          gitlab.Type,
	v1alpha1.SCMProviderTypeGitea:           gitea.Type,
	v1alpha1.SCMProviderTypeBitbucketCloud:  bitbucketcloud.Type,
	v1alpha1.SCMProviderTypeBitbucketServer: bitbucketserver.Type,
	v1alpha1.SCMProviderTypeAzureDevOps:     azuredevops.Type,
	v1alpha1.SCMProviderTypeAWSCodeCommit:   awscodecommit.Type,
	v1alpha1.SCMProviderTypeFake:            "fake",
}

// providerRef resolves an SCMProvider in namespace. Its type must equal want (the generator
// block); the fake reference provider is accepted for any block.
func (c credentials) providerRef(ctx context.Context, ref *v1alpha1.SCMProviderReference, namespace string, want v1alpha1.SCMProviderType, owner string) (*resolvedProviderRef, error) {
	if ref == nil {
		return nil, nil
	}
	p := &v1alpha1.SCMProvider{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, p); err != nil {
		return nil, fmt.Errorf("error getting SCMProvider %s/%s: %w", namespace, ref.Name, err)
	}
	if p.Spec.Type != want && p.Spec.Type != v1alpha1.SCMProviderTypeFake {
		return nil, fmt.Errorf("SCMProvider %s/%s has type %q, but the generator requires %q", namespace, ref.Name, p.Spec.Type, want)
	}
	out := &resolvedProviderRef{scmType: scmProviderTypes[p.Spec.Type], apiURL: p.Spec.APIURL, insecure: p.Spec.Insecure}
	if p.Spec.CASecretRef != nil {
		ca, err := c.secretKey(ctx, namespace, p.Spec.CASecretRef.Name, p.Spec.CASecretRef.Key, false)
		if err != nil {
			return nil, err
		}
		out.caData = []byte(ca)
	}
	if auth := p.Spec.Auth; auth != nil {
		switch {
		case auth.GitHubApp != nil:
			key, err := c.secretKey(ctx, namespace, auth.GitHubApp.PrivateKeySecretRef.Name, auth.GitHubApp.PrivateKeySecretRef.Key, c.strict)
			if err != nil {
				return nil, err
			}
			app := &scm.GitHubAppConfig{AppID: auth.GitHubApp.AppID, PrivateKey: []byte(key), Owner: owner}
			if auth.GitHubApp.InstallationID != nil {
				app.InstallationID = *auth.GitHubApp.InstallationID
			}
			out.githubApp = app
		case auth.TokenSecretRef != nil:
			token, err := c.secretKey(ctx, namespace, auth.TokenSecretRef.Name, auth.TokenSecretRef.Key, c.strict)
			if err != nil {
				return nil, err
			}
			out.token = token
		case auth.BasicAuthSecretRef != nil:
			user, err := c.secretKey(ctx, namespace, auth.BasicAuthSecretRef.Name, v1alpha1.SCMProviderSecretKeyUsername, c.strict)
			if err != nil {
				return nil, err
			}
			pass, err := c.secretKey(ctx, namespace, auth.BasicAuthSecretRef.Name, v1alpha1.SCMProviderSecretKeyPassword, c.strict)
			if err != nil {
				return nil, err
			}
			out.username, out.token = user, pass
		}
	}
	return out, nil
}

// staticToken returns a TokenFunc for a resolved token; an empty token means anonymous.
func staticToken(token string) scm.TokenFunc {
	if token == "" {
		return nil
	}
	return func(context.Context) (string, error) { return token, nil }
}

// errNoProvider is returned when no SCM provider block is configured.
var errNoProvider = errors.New("no SCM provider implementation configured")
