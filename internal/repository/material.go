package repository

import (
	"fmt"
	"net/url"
	"strings"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/pkg/scm/github"
)

// access is the merged configuration of one repository before a credential method is
// selected: settings of the Repository (or of a stub), credential material from its Secret
// and SCMProvider, and, when it has no credentials of its own, the matching template's.
// It mirrors the fields of upstream v1alpha1.Repository that matter for access.
type access struct {
	url     string
	typ     v1alpha1.RepositoryType
	name    string
	project string

	// Repository settings (not inherited from templates; upstream CopySettingsFrom).
	insecure              bool
	insecureIgnoreHostKey bool
	enableLFS             bool
	depth                 int64

	// Shared access options.
	enableOCI                bool
	insecureOCIForceHTTP     bool
	forceHTTPBasicAuth       bool
	proxy                    string
	noProxy                  string
	useAzureWorkloadIdentity bool

	// Credential material.
	username             string
	password             string
	bearerToken          string
	sshPrivateKey        string
	tlsClientCertData    string
	tlsClientCertKey     string
	gcpServiceAccountKey string

	githubAppID                int64
	githubAppInstallationID    int64
	githubAppEnterpriseBaseURL string
	githubAppPrivateKey        string
	// appAPI pins the GitHub API endpoint of the private key when the key was not supplied
	// by this object's own Secret (SCMProvider or inherited template): the object that
	// supplied the key decides where its JWTs go. Nil means this object's own settings.
	appAPI *appEndpoint
	// provider is set when credentials of an SCMProvider were merged into this access.
	provider *providerBinding

	azureSPClientID     string
	azureSPTenantID     string
	azureSPClientSecret string
	azureSPEndpoint     string

	// secrets lists every secret value merged into this access, for error redaction.
	secrets []string
}

// appEndpoint is the GitHub API endpoint that receives the JWTs of a GitHub App private key,
// with the transport settings of the object that supplied the key.
type appEndpoint struct {
	// baseURL is the API URL ("" means https://api.github.com).
	baseURL  string
	insecure bool
	caData   []byte
	proxy    string
	noProxy  string
}

// appEndpoint returns the endpoint of the access's App key: the pinned one, or this
// object's own enterpriseBaseURL, TLS and proxy settings.
func (a *access) appEndpoint() appEndpoint {
	if a.appAPI != nil {
		return *a.appAPI
	}
	return appEndpoint{baseURL: a.githubAppEnterpriseBaseURL, insecure: a.isInsecure(), proxy: a.proxy, noProxy: a.noProxy}
}

// providerBinding records that credentials of an SCMProvider were merged into an access. The
// provider decides where its credentials may go, not the object that references it: they
// are sent only to the provider's host, over https (http only when the provider itself is
// served over http), with TLS verification unless the provider disables it, and through a
// proxy only over verified https.
type providerBinding struct {
	owner    string // `SCMProvider "x"`
	host     string
	scheme   string
	insecure bool
}

// check validates the final (merged) access against the binding.
func (b *providerBinding) check(a *access) error {
	scheme := urlScheme(a.url)
	if host := repoHost(a.url); host != b.host {
		return configError(ErrInvalidConfiguration, "credentials of %s are restricted to host %q and cannot be used for repository host %q", b.owner, b.host, host)
	}
	if scheme != "https" && (scheme != "http" || b.scheme != "http") {
		return configError(ErrInvalidConfiguration, "credentials of %s are only sent over https, not to a %q repository URL", b.owner, scheme)
	}
	insecure := a.isInsecure()
	if insecure && !b.insecure {
		return configError(ErrInvalidConfiguration, "credentials of %s cannot be used with insecure, insecureIgnoreHostKey or insecureOCIForceHTTP unless the SCMProvider sets insecure", b.owner)
	}
	if a.proxy != "" && (insecure || scheme != "https") {
		return configError(ErrInvalidConfiguration, "credentials of %s are sent through a proxy only over verified https", b.owner)
	}
	return nil
}

// hasCredentials is upstream Repository.HasCredentials: it decides whether a credential
// template is inherited. A Google service account key does not count (upstream quirk), a
// username alone does.
func (a *access) hasCredentials() bool {
	return a.username != "" || a.password != "" || a.bearerToken != "" || a.sshPrivateKey != "" ||
		a.tlsClientCertData != "" || a.githubAppPrivateKey != "" || a.useAzureWorkloadIdentity ||
		a.azureSPClientSecret != ""
}

// copyCredentialsFrom applies a credential template (upstream Repository.CopyCredentialsFrom):
// credential fields, proxy, noProxy and type are filled only when empty; enableOCI,
// insecureOCIForceHTTP, forceHTTPBasicAuth and useAzureWorkloadIdentity are always taken
// from the template. Unlike upstream, an inherited GitHub App private key brings the
// template's API endpoint and transport settings with it (the receiver's enterpriseBaseURL,
// insecure flags and proxy never apply to the template's key), and an SCMProvider binding
// of the template applies to the receiver.
func (a *access) copyCredentialsFrom(t *access) {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	// SCMProvider material (username, password, App key) keeps its restrictions.
	if t.provider != nil && ((a.username == "" && t.username != "") || (a.password == "" && t.password != "") ||
		(a.githubAppPrivateKey == "" && t.githubAppPrivateKey != "")) {
		a.provider = t.provider
	}
	fill(&a.username, t.username)
	fill(&a.password, t.password)
	fill(&a.bearerToken, t.bearerToken)
	fill(&a.sshPrivateKey, t.sshPrivateKey)
	fill(&a.tlsClientCertData, t.tlsClientCertData)
	fill(&a.tlsClientCertKey, t.tlsClientCertKey)
	if a.githubAppPrivateKey == "" && t.githubAppPrivateKey != "" {
		a.githubAppPrivateKey = t.githubAppPrivateKey
		ep := t.appEndpoint()
		a.appAPI = &ep
		a.githubAppEnterpriseBaseURL = ep.baseURL
	}
	if a.githubAppID == 0 {
		a.githubAppID = t.githubAppID
	}
	if a.githubAppInstallationID == 0 {
		a.githubAppInstallationID = t.githubAppInstallationID
	}
	fill(&a.githubAppEnterpriseBaseURL, t.githubAppEnterpriseBaseURL)
	fill(&a.gcpServiceAccountKey, t.gcpServiceAccountKey)
	fill(&a.azureSPClientID, t.azureSPClientID)
	fill(&a.azureSPClientSecret, t.azureSPClientSecret)
	fill(&a.azureSPTenantID, t.azureSPTenantID)
	fill(&a.azureSPEndpoint, t.azureSPEndpoint)
	fill(&a.proxy, t.proxy)
	fill(&a.noProxy, t.noProxy)
	if a.typ == "" {
		a.typ = t.typ
	}
	a.enableOCI = t.enableOCI
	a.insecureOCIForceHTTP = t.insecureOCIForceHTTP
	a.forceHTTPBasicAuth = t.forceHTTPBasicAuth
	a.useAzureWorkloadIdentity = t.useAzureWorkloadIdentity
	a.secrets = append(a.secrets, t.secrets...)
}

// isInsecure is upstream Repository.IsInsecure (git): any of the three flags.
func (a *access) isInsecure() bool {
	return a.insecure || a.insecureIgnoreHostKey || a.insecureOCIForceHTTP
}

// applyAccessOptions copies the shared spec options and the non-secret parts of the
// credential configuration.
func (a *access) applyAccessOptions(o *v1alpha1.RepositoryAccess) {
	a.enableOCI = o.EnableOCI
	a.insecureOCIForceHTTP = o.InsecureOCIForceHTTP
	a.forceHTTPBasicAuth = o.ForceHTTPBasicAuth
	a.proxy = o.Proxy
	a.noProxy = o.NoProxy
	a.useAzureWorkloadIdentity = o.UseAzureWorkloadIdentity
	if sp := o.AzureServicePrincipal; sp != nil {
		a.azureSPClientID = sp.ClientID
		a.azureSPTenantID = sp.TenantID
		a.azureSPEndpoint = sp.ActiveDirectoryEndpoint
	}
	if app := o.GitHubApp; app != nil {
		a.githubAppID = app.AppID
		if app.InstallationID != nil {
			a.githubAppInstallationID = *app.InstallationID
		}
		a.githubAppEnterpriseBaseURL = app.EnterpriseBaseURL
	}
}

// applySecretData copies the recognized credential keys (RepositorySecretKey*).
func (a *access) applySecretData(data map[string][]byte) {
	get := func(key string, secret bool) string {
		v := string(data[key])
		if secret && v != "" {
			a.secrets = append(a.secrets, v)
		}
		return v
	}
	a.username = get(v1alpha1.RepositorySecretKeyUsername, false)
	a.password = get(v1alpha1.RepositorySecretKeyPassword, true)
	a.bearerToken = get(v1alpha1.RepositorySecretKeyBearerToken, true)
	a.sshPrivateKey = get(v1alpha1.RepositorySecretKeySSHPrivateKey, true)
	a.tlsClientCertData = get(v1alpha1.RepositorySecretKeyTLSClientCertData, false)
	a.tlsClientCertKey = get(v1alpha1.RepositorySecretKeyTLSClientCertKey, true)
	a.githubAppPrivateKey = get(v1alpha1.RepositorySecretKeyGitHubAppPrivateKey, true)
	a.gcpServiceAccountKey = get(v1alpha1.RepositorySecretKeyGCPServiceAccountKey, true)
	a.azureSPClientSecret = get(v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret, true)
}

// loadAccess builds the access of a Repository or RepositoryCredential (spec options,
// credentials Secret, SCMProvider). use describes the object and how its credentialsRef
// must be labeled and bound. inline, when non-nil, replaces the Secret named by
// credentialsRef (validation of unsaved input). target is the repository URL the
// credentials will be sent to (SCMProvider credentials are restricted to its host).
func (r *Resolver) loadAccess(o *v1alpha1.RepositoryAccess, use secretUse, target string, inline map[string][]byte) (*access, error) {
	a := &access{}
	a.applyAccessOptions(o)
	switch {
	case inline != nil:
		a.applySecretData(inline)
	case o.CredentialsRef != nil:
		data, err := r.lister.secretFor(o.CredentialsRef.Name, use)
		if err != nil {
			return nil, err
		}
		a.applySecretData(data)
	}
	if o.SCMProviderRef != nil {
		if err := r.applySCMProvider(a, o.SCMProviderRef.Name, use.owner, target); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// applySCMProvider merges the credentials of an SCMProvider into a (only into empty
// fields: the object's own Secret wins). A GitHub App supplies the App ID, installation ID
// and private key, and its JWTs always go to the provider's API URL with the provider's TLS
// settings; a token becomes the HTTPS password; basic auth supplies username and password.
// The access records a providerBinding, which materialize enforces on the final access, so
// the referencing object cannot redirect the credentials to another host, a plain http URL,
// unverified TLS or a proxy that could read them.
func (r *Resolver) applySCMProvider(a *access, name, owner, target string) error {
	p, err := r.lister.scmProvider(name)
	if err != nil {
		return fmt.Errorf("%s: %w", owner, err)
	}
	auth := p.Spec.Auth
	if auth == nil || (auth.GitHubApp == nil && auth.TokenSecretRef == nil && auth.BasicAuthSecretRef == nil) {
		return nil
	}
	providerOwner := fmt.Sprintf("SCMProvider %q", name)
	use := secretUse{owner: providerOwner, secretType: v1alpha1.LabelValueSecretTypeSCMCreds}
	host, scheme, err := providerWebHost(&p.Spec)
	if err != nil {
		return configError(ErrInvalidConfiguration, "%s: %s", providerOwner, err)
	}
	if th := repoHost(target); th == "" || th != host {
		return configError(ErrInvalidConfiguration, "%s: credentials of %s are restricted to host %q and cannot be used for repository host %q",
			owner, providerOwner, host, th)
	}
	binding := &providerBinding{owner: providerOwner, host: host, scheme: scheme, insecure: p.Spec.Insecure}
	switch {
	case auth.GitHubApp != nil:
		app := auth.GitHubApp
		if p.Spec.Type != v1alpha1.SCMProviderTypeGitHub {
			return configError(ErrInvalidConfiguration, "%s: githubApp authentication requires type github, got %q", providerOwner, p.Spec.Type)
		}
		if a.githubAppPrivateKey != "" {
			return nil
		}
		if a.githubAppID != 0 && a.githubAppID != app.AppID {
			return configError(ErrInvalidConfiguration, "%s: githubApp.appID %d does not match the App %d of %s", owner, a.githubAppID, app.AppID, providerOwner)
		}
		providerAPI, _, err := github.APIURLs(p.Spec.APIURL)
		if err != nil {
			return configError(ErrInvalidConfiguration, "%s: %s", providerOwner, err)
		}
		if a.githubAppEnterpriseBaseURL != "" {
			if own, _, err := github.APIURLs(a.githubAppEnterpriseBaseURL); err != nil || own != providerAPI {
				return configError(ErrInvalidConfiguration, "%s: githubApp.enterpriseBaseURL must be empty or the API URL of %s when its App is used", owner, providerOwner)
			}
		}
		if a.proxy != "" && p.Spec.Insecure {
			return configError(ErrInvalidConfiguration, "%s: the proxy of %s cannot carry GitHub App requests of %s, whose API TLS verification is disabled", owner, owner, providerOwner)
		}
		key, err := r.lister.secretKeyValue(app.PrivateKeySecretRef, use)
		if err != nil {
			return err
		}
		ep := appEndpoint{baseURL: p.Spec.APIURL, insecure: p.Spec.Insecure, proxy: a.proxy, noProxy: a.noProxy}
		if p.Spec.CASecretRef != nil {
			ca, err := r.lister.secretKeyValue(*p.Spec.CASecretRef, use)
			if err != nil {
				return err
			}
			ep.caData = []byte(ca)
		}
		a.githubAppID = app.AppID
		a.githubAppPrivateKey = key
		a.secrets = append(a.secrets, key)
		if a.githubAppInstallationID == 0 && app.InstallationID != nil {
			a.githubAppInstallationID = *app.InstallationID
		}
		a.githubAppEnterpriseBaseURL = p.Spec.APIURL
		a.appAPI = &ep
		a.provider = binding
	case auth.TokenSecretRef != nil:
		tok, err := r.lister.secretKeyValue(*auth.TokenSecretRef, use)
		if err != nil {
			return err
		}
		if a.password == "" && a.bearerToken == "" {
			a.password = tok
			a.secrets = append(a.secrets, tok)
			a.provider = binding
		}
	case auth.BasicAuthSecretRef != nil:
		data, err := r.lister.secretFor(auth.BasicAuthSecretRef.Name, use)
		if err != nil {
			return err
		}
		user, pass := string(data[v1alpha1.SCMProviderSecretKeyUsername]), string(data[v1alpha1.SCMProviderSecretKeyPassword])
		if pass == "" {
			return configError(ErrSecretKeyMissing, "%s: secret %q has no key %q", providerOwner, auth.BasicAuthSecretRef.Name, v1alpha1.SCMProviderSecretKeyPassword)
		}
		if a.username == "" && a.password == "" {
			a.username, a.password = user, pass
			a.secrets = append(a.secrets, pass)
			a.provider = binding
		}
	}
	return nil
}

// providerWebHost returns the host Git repositories of an SCMProvider are served from and
// the scheme they use: spec.webURL, otherwise github.com (https) for the public API and the
// API host without its "api." label (GitHub Enterprise Cloud) or as is (GitHub Enterprise
// Server), with the API URL's scheme.
func providerWebHost(spec *v1alpha1.SCMProviderSpec) (host, scheme string, err error) {
	if spec.WebURL != "" {
		u, err := url.Parse(spec.WebURL)
		if err != nil || u.Hostname() == "" {
			return "", "", fmt.Errorf("invalid webURL %q", spec.WebURL)
		}
		return strings.ToLower(u.Hostname()), strings.ToLower(u.Scheme), nil
	}
	if spec.APIURL == "" {
		switch spec.Type {
		case v1alpha1.SCMProviderTypeGitHub:
			return "github.com", "https", nil
		case v1alpha1.SCMProviderTypeGitLab:
			return "gitlab.com", "https", nil
		case v1alpha1.SCMProviderTypeBitbucketCloud:
			return "bitbucket.org", "https", nil
		case v1alpha1.SCMProviderTypeAzureDevOps:
			return "dev.azure.com", "https", nil
		default:
			return "", "", fmt.Errorf("type %q requires apiURL or webURL", spec.Type)
		}
	}
	u, err := url.Parse(spec.APIURL)
	if err != nil || u.Hostname() == "" {
		return "", "", fmt.Errorf("invalid apiURL %q", spec.APIURL)
	}
	host = strings.ToLower(u.Hostname())
	if after, ok := strings.CutPrefix(host, "api."); ok && after != "" {
		host = after
	}
	return host, strings.ToLower(u.Scheme), nil
}
