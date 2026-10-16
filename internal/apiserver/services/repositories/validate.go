package repositories

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// Input limits.
const (
	// maxURLBytes is the CRD limit of spec.url and spec.urlPrefix.
	maxURLBytes = 2048
	// maxTextBytes bounds short free-text fields (Helm repository name, noProxy).
	maxTextBytes = 2048
	// maxCredentialBytes bounds one credential value; maxCredentialsBytes all of them (a
	// Secret holds at most 1 MiB).
	maxCredentialBytes  = 256 << 10
	maxCredentialsBytes = 768 << 10
	maxUsernameBytes    = 1024
)

var (
	// scpLikeRE matches scp-style git URLs "[user@]host:path".
	scpLikeRE = regexp.MustCompile(`^(?:[^@/\s]+@)?([^:/\s\[\]]+):(.*)$`)
	// schemelessRE matches a scheme-less registry reference "host[:port][/path]" (Helm OCI).
	schemelessRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[^\s]*)?$`)
	// transportHelperRE matches git remote helper syntax ("ext::cmd", "fd::0"), which can
	// run commands.
	transportHelperRE = regexp.MustCompile(`^[A-Za-z0-9+.-]+::`)
)

// allowedRepoSchemes are the URL schemes a repository URL may use (threat model T8).
var allowedRepoSchemes = map[string]bool{"https": true, "http": true, "ssh": true, "git": true, "oci": true}

func invalid(format string, args ...any) error { return rest.InvalidArgument(format, args...) }

// checkText rejects control characters and, unless spaces are allowed, whitespace.
func checkText(field, v string, maxLen int, allowSpace bool) error {
	if len(v) > maxLen {
		return invalid("%s exceeds %d bytes", field, maxLen)
	}
	for _, r := range v {
		if unicode.IsControl(r) || (!allowSpace && unicode.IsSpace(r)) {
			return invalid("%s must not contain whitespace or control characters", field)
		}
	}
	return nil
}

// validateRepoURL checks a repository URL or URL prefix: an http(s), ssh, git or oci URL
// with a host and without a password, an scp-style "[user@]host:path", or (when
// schemeless is true, Helm and OCI) "host[:port]/path". Local paths, file:// URLs, git
// remote helpers ("ext::") and option-like values are rejected.
func validateRepoURL(field, raw string, schemeless bool) error {
	if raw == "" {
		return invalid("%s is required", field)
	}
	if err := checkText(field, raw, maxURLBytes, false); err != nil {
		return err
	}
	if strings.HasPrefix(raw, "-") || strings.Contains(raw, `\`) || transportHelperRE.MatchString(raw) {
		return invalid("%s is not a supported repository URL", field)
	}
	if i := strings.Index(raw, "://"); i > 0 {
		scheme := strings.ToLower(raw[:i])
		if !allowedRepoSchemes[scheme] {
			return invalid("%s: unsupported URL scheme %q (allowed: https, http, ssh, git, oci)", field, scheme)
		}
		u, err := url.Parse(raw)
		if err != nil {
			// ssh://git@host:org/repo is malformed but accepted upstream.
			remainder := raw[i+3:]
			if scheme == "ssh" && scpLikeRE.MatchString(remainder) {
				return nil
			}
			return invalid("%s is not a valid URL", field)
		}
		if u.Host == "" {
			return invalid("%s must name a host", field)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return invalid("%s must not contain a password; send it in credentials.password", field)
			}
		}
		return nil
	}
	if m := scpLikeRE.FindStringSubmatch(raw); m != nil && !strings.HasPrefix(m[1], "-") {
		return nil
	}
	if schemeless && schemelessRE.MatchString(raw) {
		return nil
	}
	return invalid("%s is not a supported repository URL (https, http, ssh, git or oci URL, or [user@]host:path)", field)
}

// validateHTTPSURL checks an https endpoint (GitHub Enterprise API, Entra authority).
func validateHTTPSURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	if err := checkText(field, raw, maxURLBytes, false); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return invalid("%s must be an https URL", field)
	}
	if u.User != nil {
		return invalid("%s must not contain user information", field)
	}
	return nil
}

// validateHTTPURL checks an http(s) endpoint without user information (SCM provider).
func validateHTTPURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	if err := checkText(field, raw, maxURLBytes, false); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return invalid("%s must be an http or https URL", field)
	}
	if u.User != nil {
		return invalid("%s must not contain user information", field)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return invalid("%s must not contain a query or fragment", field)
	}
	return nil
}

// validateProxy checks a proxy URL: http, https or socks5(h), with a host and without
// user information (spec fields are not secret; authenticating proxies are not supported
// through the API).
func validateProxy(field, raw string) error {
	if raw == "" {
		return nil
	}
	if err := checkText(field, raw, maxURLBytes, false); err != nil {
		return err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return invalid("%s must be a proxy URL such as http://proxy:3128", field)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return invalid("%s: unsupported proxy scheme %q (allowed: http, https, socks5, socks5h)", field, u.Scheme)
	}
	if u.User != nil {
		return invalid("%s must not contain user information", field)
	}
	return nil
}

func validateDNSName(field, v string) error {
	if errs := validation.IsDNS1123Subdomain(v); len(errs) > 0 {
		return invalid("%s %q is invalid: %s", field, v, strings.Join(errs, "; "))
	}
	return nil
}

// validateAccess checks the access options shared by Repository and RepositoryCredential.
// credentialsRef is checked by the callers (it is managed by the server).
func validateAccess(a *v1alpha1.RepositoryAccess) error {
	if err := validateProxy("spec.proxy", a.Proxy); err != nil {
		return err
	}
	if err := checkText("spec.noProxy", a.NoProxy, maxTextBytes, true); err != nil {
		return err
	}
	if app := a.GitHubApp; app != nil {
		if app.AppID < 1 {
			return invalid("spec.githubApp.appID must be at least 1")
		}
		if app.InstallationID != nil && *app.InstallationID < 1 {
			return invalid("spec.githubApp.installationID must be at least 1 when set")
		}
		if err := validateHTTPSURL("spec.githubApp.enterpriseBaseURL", app.EnterpriseBaseURL); err != nil {
			return err
		}
	}
	if sp := a.AzureServicePrincipal; sp != nil {
		if strings.TrimSpace(sp.ClientID) == "" || strings.TrimSpace(sp.TenantID) == "" {
			return invalid("spec.azureServicePrincipal.clientID and tenantID are required")
		}
		if err := checkText("spec.azureServicePrincipal.clientID", sp.ClientID, 256, false); err != nil {
			return err
		}
		if err := checkText("spec.azureServicePrincipal.tenantID", sp.TenantID, 256, false); err != nil {
			return err
		}
		if err := validateHTTPSURL("spec.azureServicePrincipal.activeDirectoryEndpoint", sp.ActiveDirectoryEndpoint); err != nil {
			return err
		}
	}
	if ref := a.SCMProviderRef; ref != nil {
		if err := validateDNSName("spec.scmProviderRef.name", ref.Name); err != nil {
			return err
		}
	}
	return nil
}

func validateType(t v1alpha1.RepositoryType) error {
	switch t {
	case v1alpha1.RepositoryTypeGit, v1alpha1.RepositoryTypeHelm, v1alpha1.RepositoryTypeOCI:
		return nil
	}
	return invalid("spec.type %q is invalid (git, helm or oci)", t)
}

// normalizeRepositorySpec defaults and validates a repository spec for purpose p.
// credentialsRef is left to the caller.
func normalizeRepositorySpec(spec *v1alpha1.RepositorySpec, p purpose) error {
	spec.URL = strings.TrimSpace(spec.URL)
	if spec.Type == "" {
		spec.Type = v1alpha1.RepositoryTypeGit
	}
	if err := validateType(spec.Type); err != nil {
		return err
	}
	if spec.Purpose == "" {
		spec.Purpose = p.value
	}
	if spec.Purpose != p.value {
		return invalid("spec.purpose must be %q on this route", p.value)
	}
	if err := validateRepoURL("spec.url", spec.URL, spec.Type != v1alpha1.RepositoryTypeGit); err != nil {
		return err
	}
	if p.write && spec.Type != v1alpha1.RepositoryTypeGit {
		return invalid("purpose write is only supported for git repositories")
	}
	if spec.EnableLFS && spec.Type != v1alpha1.RepositoryTypeGit {
		return invalid("enableLFS is only supported for git repositories")
	}
	if spec.Depth < 0 {
		return invalid("spec.depth must not be negative")
	}
	if spec.Project != "" {
		if err := validateDNSName("spec.project", spec.Project); err != nil {
			return err
		}
	}
	if err := checkText("spec.name", spec.Name, 253, false); err != nil {
		return err
	}
	return validateAccess(&spec.RepositoryAccess)
}

// normalizeCredentialSpec defaults and validates a credential template spec.
func normalizeCredentialSpec(spec *v1alpha1.RepositoryCredentialSpec, p purpose) error {
	spec.URLPrefix = strings.TrimSpace(spec.URLPrefix)
	if spec.URLPrefix == "" {
		return invalid("must specify URL")
	}
	if spec.Type == "" {
		spec.Type = v1alpha1.RepositoryTypeGit
	}
	if err := validateType(spec.Type); err != nil {
		return err
	}
	if spec.Purpose == "" {
		spec.Purpose = p.value
	}
	if spec.Purpose != p.value {
		return invalid("spec.purpose must be %q on this route", p.value)
	}
	if p.write && spec.Type != v1alpha1.RepositoryTypeGit {
		return invalid("purpose write is only supported for git repositories")
	}
	if err := validateRepoURL("spec.urlPrefix", spec.URLPrefix, spec.Type != v1alpha1.RepositoryTypeGit); err != nil {
		return err
	}
	return validateAccess(&spec.RepositoryAccess)
}

// validateTypeMeta accepts an empty apiVersion/kind or the object's own.
func validateTypeMeta(apiVersion, kind, want string) error {
	if apiVersion != "" && apiVersion != v1alpha1.GroupVersion.String() {
		return invalid("apiVersion must be %s", v1alpha1.GroupVersion.String())
	}
	if kind != "" && kind != want {
		return invalid("kind must be %s", want)
	}
	return nil
}

// credentialFields lists the credential values with their Secret keys.
func credentialFields(c *apiclient.RepositoryCredentials) []struct{ key, value string } {
	return []struct{ key, value string }{
		{v1alpha1.RepositorySecretKeyUsername, c.Username},
		{v1alpha1.RepositorySecretKeyPassword, c.Password},
		{v1alpha1.RepositorySecretKeyBearerToken, c.BearerToken},
		{v1alpha1.RepositorySecretKeySSHPrivateKey, c.SSHPrivateKey},
		{v1alpha1.RepositorySecretKeyTLSClientCertData, c.TLSClientCertData},
		{v1alpha1.RepositorySecretKeyTLSClientCertKey, c.TLSClientCertKey},
		{v1alpha1.RepositorySecretKeyGitHubAppPrivateKey, c.GitHubAppPrivateKey},
		{v1alpha1.RepositorySecretKeyGCPServiceAccountKey, c.GCPServiceAccountKey},
		{v1alpha1.RepositorySecretKeyAzureServicePrincipalClientSecret, c.AzureServicePrincipalClientSecret},
	}
}

// credentialKeys are the Secret keys the API manages.
var credentialKeys = func() []string {
	var out []string
	for _, f := range credentialFields(&apiclient.RepositoryCredentials{}) {
		out = append(out, f.key)
	}
	return out
}()

// credentialData returns the non-empty credential values by Secret key (nil for nil c).
func credentialData(c *apiclient.RepositoryCredentials) map[string][]byte {
	if c == nil {
		return nil
	}
	out := map[string][]byte{}
	for _, f := range credentialFields(c) {
		if f.value != "" {
			out[f.key] = []byte(f.value)
		}
	}
	return out
}

// validateCredentials checks write-only credential material against the access options.
// Messages never contain the values.
func validateCredentials(c *apiclient.RepositoryCredentials, a *v1alpha1.RepositoryAccess) error {
	if c == nil {
		return nil
	}
	total := 0
	for _, f := range credentialFields(c) {
		if len(f.value) > maxCredentialBytes {
			return invalid("credentials.%s exceeds %d bytes", f.key, maxCredentialBytes)
		}
		total += len(f.value)
	}
	if total > maxCredentialsBytes {
		return invalid("credentials exceed %d bytes", maxCredentialsBytes)
	}
	if err := checkText("credentials.username", c.Username, maxUsernameBytes, false); err != nil {
		return err
	}
	for _, f := range []struct{ key, value string }{{"password", c.Password}, {"bearerToken", c.BearerToken}, {"azureServicePrincipalClientSecret", c.AzureServicePrincipalClientSecret}} {
		if strings.ContainsAny(f.value, "\r\n\x00") {
			return invalid("credentials.%s must not contain line breaks or NUL characters", f.key)
		}
	}
	if (c.TLSClientCertData == "") != (c.TLSClientCertKey == "") {
		return invalid("credentials.tlsClientCertData and credentials.tlsClientCertKey must be set together")
	}
	if c.TLSClientCertData != "" {
		if _, err := tls.X509KeyPair([]byte(c.TLSClientCertData), []byte(c.TLSClientCertKey)); err != nil {
			return invalid("credentials.tlsClientCertData and tlsClientCertKey are not a valid PEM certificate and matching private key")
		}
	}
	if c.SSHPrivateKey != "" {
		if _, err := ssh.ParseRawPrivateKey([]byte(c.SSHPrivateKey)); err != nil {
			var pm *ssh.PassphraseMissingError
			if errors.As(err, &pm) {
				return invalid("credentials.sshPrivateKey is protected by a passphrase, which is not supported")
			}
			return invalid("credentials.sshPrivateKey is not a valid private key")
		}
	}
	if c.GitHubAppPrivateKey != "" {
		if a.GitHubApp == nil {
			return invalid("credentials.githubAppPrivateKey requires spec.githubApp")
		}
		if !validRSAKey(c.GitHubAppPrivateKey) {
			return invalid("credentials.githubAppPrivateKey is not a valid PEM RSA private key")
		}
	}
	if c.GCPServiceAccountKey != "" {
		var key struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(c.GCPServiceAccountKey), &key); err != nil || key.Type != "service_account" {
			return invalid("credentials.gcpServiceAccountKey must be a Google service account key (JSON with type service_account)")
		}
	}
	if c.AzureServicePrincipalClientSecret != "" && a.AzureServicePrincipal == nil {
		return invalid("credentials.azureServicePrincipalClientSecret requires spec.azureServicePrincipal")
	}
	return nil
}

func validRSAKey(s string) bool {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return false
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return true
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return false
	}
	_, ok := k.(*rsa.PrivateKey)
	return ok
}

// validHost reports whether h is a host name accepted for certificates (upstream
// IsValidHostname or an IP address).
func validHost(h string, isValidHostname func(string) bool) bool {
	if h == "" {
		return false
	}
	if net.ParseIP(h) != nil {
		return true
	}
	return isValidHostname(h)
}

// specDiff lists the top-level JSON keys whose values differ between a and b (upstream
// GenerateSpecIsDifferentErrorMessage reports the differing paths).
func specDiff(a, b any) []string {
	ma, mb := toMap(a), toMap(b)
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		ja, _ := json.Marshal(ma[k])
		jb, _ := json.Marshal(mb[k])
		if string(ja) != string(jb) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func toMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// specIsDifferent is upstream's message for a create that conflicts with an existing
// object.
func specIsDifferent(entity string, diff []string) error {
	msg := fmt.Sprintf("existing %s spec is different; use upsert flag to force update", entity)
	if len(diff) > 0 {
		msg += fmt.Sprintf("; difference in keys %q", strings.Join(diff, ","))
	}
	return invalid("%s", msg)
}
