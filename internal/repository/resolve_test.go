package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/project"
)

const (
	read  = v1alpha1.RepositoryPurposeRead
	write = v1alpha1.RepositoryPurposeWrite
)

// TestResolvePrecedence covers REPO-004, REPO-029, REPO-031, REPO-033 and REPO-034: project
// scoping with fallback, URL equivalence, longest-prefix templates and separate write
// objects.
func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("global-creds", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "g", "password": "global-pass"}),
		ownedBy(secret("team-creds", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "t", "password": "team-pass"}), "app-team"),
		ownedBy(secret("other-creds", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "other-pass"}), "only-other"),
		secret("org-creds", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "org-pass"}),
		secret("team-tpl-creds", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "teamtpl-pass"}),
		secret("write-creds", v1alpha1.LabelValueSecretTypeRepositoryWrite, map[string]string{"password": "write-pass"}),
		secret("write-tpl-creds", v1alpha1.LabelValueSecretTypeRepoCredsWrite, map[string]string{"password": "writetpl-pass"}),
		repo("app-global", "https://github.com/org/app", withCreds("global-creds")),
		repo("app-team", "https://github.com/org/app.git", withProject("team"), withCreds("team-creds")),
		repo("only-other", "https://github.com/org/scoped", withProject("other"), withCreds("other-creds")),
		repo("app-write", "https://github.com/org/app", withPurpose(write), withCreds("write-creds")),
		repo("nocreds", "https://github.com/org/nocreds"),
		cred("org", "https://github.com/org", credWithCreds("org-creds")),
		cred("org-team", "https://github.com/org/team", credWithCreds("team-tpl-creds")),
		cred("org-write", "https://github.com/org", credWithPurpose(write), credWithCreds("write-tpl-creds")),
	})
	cases := []struct {
		name, url, project string
		purpose            v1alpha1.RepositoryPurpose
		source             Source
		repo, cred         string
		password           string
	}{
		{"scoped repository wins for its project", "https://github.com/org/app", "team", read, SourceProject, "app-team", "", "team-pass"},
		{"other project falls back to the unscoped repository", "https://github.com/org/app", "dev", read, SourceGlobal, "app-global", "", "global-pass"},
		{"no project uses the unscoped repository", "https://github.com/org/app", "", read, SourceGlobal, "app-global", "", "global-pass"},
		{"URL spelling is normalized", "https://GitHub.com/Org/App.git", "team", read, SourceProject, "app-team", "", "team-pass"},
		{"repository scoped to another project is not used", "https://github.com/org/scoped", "team", read, SourceNone, "", "org", "org-pass"},
		{"stub inherits the longest prefix", "https://github.com/org/team/svc", "", read, SourceNone, "", "org-team", "teamtpl-pass"},
		{"prefix matching is not path-segment aware", "https://github.com/org/teamwork", "", read, SourceNone, "", "org-team", "teamtpl-pass"},
		{"repository without credentials inherits", "https://github.com/org/nocreds", "", read, SourceGlobal, "nocreds", "org", "org-pass"},
		{"no match is anonymous", "https://gitlab.com/x/y", "", read, SourceNone, "", "", ""},
		{"write purpose uses the write repository", "https://github.com/org/app", "team", write, SourceGlobal, "app-write", "", "write-pass"},
		{"write stub uses the write template, never read objects", "https://github.com/org/other", "", write, SourceNone, "", "org-write", "writetpl-pass"},
		{"empty purpose means read", "https://github.com/org/app", "team", "", SourceProject, "app-team", "", "team-pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := f.resolver.Resolve(context.Background(), Query{URL: tc.url, Project: tc.project, Purpose: tc.purpose})
			require.NoError(t, err)
			assert.Equal(t, tc.source, res.Source)
			assert.Equal(t, tc.repo, res.RepositoryName)
			assert.Equal(t, tc.cred, res.CredentialName)
			assert.Equal(t, tc.cred != "", res.Inherited)
			assert.Equal(t, tc.password, res.Repository.Password)
			if tc.password == "" {
				assert.Equal(t, MethodNone, res.Method)
				assert.False(t, res.Repository.HasCredentials())
			} else {
				assert.Equal(t, MethodHTTPS, res.Method)
			}
		})
	}
	// ResolveForURL returns the same material; a stub keeps the requested URL, a match the
	// Repository's URL (upstream returns the Secret's url).
	got, err := f.resolver.ResolveForURL(context.Background(), "https://github.com/org/app", "team", read)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/app.git", got.URL)
	assert.Equal(t, "team", got.Project)
	stub, err := f.resolver.ResolveForURL(context.Background(), "https://github.com/org/stub", "", read)
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/stub", stub.URL)
	assert.Empty(t, stub.Project)
}

// TestPickRepositoryModel checks the scoping rule against a model for every combination of
// candidate projects (REPO-004).
func TestPickRepositoryModel(t *testing.T) {
	t.Parallel()
	r := &Resolver{logger: discardLogger()}
	projects := []string{"", "a", "b"}
	// Every subset (as a 3-bit mask) of {"", "a", "b"} candidates, two objects each.
	for mask := range 1 << len(projects) {
		var cands []*v1alpha1.Repository
		for i, p := range projects {
			if mask&(1<<i) != 0 {
				cands = append(cands, repo(p+"-1", "https://h/r", withProject(p)), repo(p+"-2", "https://h/r", withProject(p)))
			}
		}
		for _, q := range projects {
			got := r.pickRepository(cands, q, "https://h/r")
			var want string
			switch {
			case mask&(1<<indexOf(projects, q)) != 0:
				want = q + "-1"
			case mask&1 != 0:
				want = "-1"
			}
			if want == "" {
				assert.Nil(t, got, "mask %b query %q", mask, q)
				continue
			}
			require.NotNil(t, got, "mask %b query %q", mask, q)
			assert.Equal(t, want, got.Name, "mask %b query %q", mask, q)
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// TestTemplateMatching covers REPO-031 edge cases: ties, trailing slashes, case and ".git".
func TestTemplateMatching(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("a", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "pass-a"}),
		secret("b", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "pass-b"}),
		secret("slash", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"password": "pass-slash"}),
		secret("ssh", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"sshPrivateKey": "ssh-key-material"}),
		// Same normalized prefix: the first by name wins.
		cred("tie-b", "https://example.com/Tie.git", credWithCreds("b")),
		cred("tie-a", "https://example.com/tie", credWithCreds("a")),
		cred("slash", "https://example.com/org/", credWithCreds("slash")),
		cred("ssh", "git@example.com:org", credWithCreds("ssh")),
	})
	cases := []struct{ url, cred string }{
		{"https://example.com/tie/x", "tie-a"},
		{"https://EXAMPLE.com/TIE/x.git", "tie-a"},
		{"https://example.com/org/x", "slash"},
		{"https://example.com/organization/x", ""},
		{"git@example.com:org/x.git", "ssh"},
		{"ssh://git@example.com/org/x", "ssh"},
		{"not a url %%", ""},
	}
	for _, tc := range cases {
		res, err := f.resolver.Resolve(context.Background(), Query{URL: tc.url})
		require.NoError(t, err, tc.url)
		assert.Equal(t, tc.cred, res.CredentialName, tc.url)
	}
	assert.Contains(t, f.logs.String(), "found multiple credentials")
}

// TestInheritanceRules covers REPO-026 (HasCredentials) and REPO-032 (copy rules).
func TestInheritanceRules(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("tpl", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"username": "tpl-user", "password": "tpl-pass"}),
		secret("user-only", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "someone"}),
		secret("cert-only", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"tlsClientCertData": "CERT"}),
		secret("own", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"password": "own-pass"}),
		secret("gcp-only", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"gcpServiceAccountKey": "{}"}),
		cred("tpl", "https://h.example/", credWithCreds("tpl"), func(c *v1alpha1.RepositoryCredential) {
			c.Spec.Proxy = "http://tpl-proxy:3128"
			c.Spec.ForceHTTPBasicAuth = true
		}),
		cred("proxy-only", "https://p.example/", func(c *v1alpha1.RepositoryCredential) {
			c.Spec.Proxy = "http://tpl-proxy:3128"
			c.Spec.NoProxy = "internal"
		}),
		repo("user-only", "https://h.example/a", withCreds("user-only")),
		repo("cert-only", "https://h.example/b", withCreds("cert-only")),
		repo("own", "https://h.example/c", withCreds("own"), func(r *v1alpha1.Repository) { r.Spec.Proxy = "http://own-proxy:8080" }),
		repo("plain", "https://h.example/d", func(r *v1alpha1.Repository) { r.Spec.Proxy = "http://own-proxy:8080" }),
		repo("gcp-only", "https://p.example/e", withCreds("gcp-only")),
	})
	ctx := context.Background()

	// A username alone counts as credentials: no inheritance, and no git credentials.
	res, err := f.resolver.Resolve(ctx, Query{URL: "https://h.example/a"})
	require.NoError(t, err)
	assert.False(t, res.Inherited)
	assert.Equal(t, MethodNone, res.Method)
	assert.False(t, res.Repository.HasCredentials())

	// A client certificate alone counts too (and is not forwarded for git without a password).
	res, err = f.resolver.Resolve(ctx, Query{URL: "https://h.example/b"})
	require.NoError(t, err)
	assert.False(t, res.Inherited)
	assert.Empty(t, res.Repository.TLSClientCertData)

	// Own credentials ignore templates.
	res, err = f.resolver.Resolve(ctx, Query{URL: "https://h.example/c"})
	require.NoError(t, err)
	assert.False(t, res.Inherited)
	assert.Equal(t, "own-pass", res.Repository.Password)
	assert.False(t, res.Repository.ForceHTTPBasicAuth)

	// Inheritance fills credentials, keeps the repository's proxy and always overwrites
	// forceHTTPBasicAuth.
	res, err = f.resolver.Resolve(ctx, Query{URL: "https://h.example/d"})
	require.NoError(t, err)
	assert.True(t, res.Inherited)
	assert.Equal(t, "tpl-user", res.Repository.Username)
	assert.Equal(t, "tpl-pass", res.Repository.Password)
	assert.Equal(t, "http://own-proxy:8080", res.Repository.Proxy)
	assert.True(t, res.Repository.ForceHTTPBasicAuth)

	// A Google key does not count as credentials: the template is inherited, but the key
	// stays and is selected because the template brings no higher-precedence credentials.
	r := f.resolver
	a, err := r.repositoryAccess(repo("gcp-only", "https://p.example/e", withCreds("gcp-only")), nil)
	require.NoError(t, err)
	assert.False(t, a.hasCredentials())
	res2 := &Resolution{}
	require.NoError(t, r.inherit(a, read, res2))
	assert.True(t, res2.Inherited)
	assert.Equal(t, "proxy-only", res2.CredentialName)
	assert.Equal(t, "{}", a.gcpServiceAccountKey)
	assert.Equal(t, "http://tpl-proxy:3128", a.proxy)
	assert.Equal(t, "internal", a.noProxy)
	m, err := gitMethod(a, "test")
	require.NoError(t, err)
	assert.Equal(t, MethodGCP, m)
}

// TestCopyCredentialsFrom checks every field of upstream CopyCredentialsFrom for empty and
// non-empty receivers (REPO-032).
func TestCopyCredentialsFrom(t *testing.T) {
	t.Parallel()
	tpl := &access{
		typ: v1alpha1.RepositoryTypeHelm, username: "tu", password: "tp", bearerToken: "tb", sshPrivateKey: "ts",
		tlsClientCertData: "tc", tlsClientCertKey: "tk", gcpServiceAccountKey: "tg",
		githubAppID: 7, githubAppInstallationID: 8, githubAppEnterpriseBaseURL: "https://ghe/api/v3", githubAppPrivateKey: "tkey",
		provider:        &providerBinding{owner: "SCMProvider \"p\"", host: "ghe", scheme: "https"},
		azureSPClientID: "tci", azureSPTenantID: "tti", azureSPClientSecret: "tcs", azureSPEndpoint: "https://aad",
		proxy: "http://tproxy:1", noProxy: "tnp", enableOCI: false, insecureOCIForceHTTP: false,
		forceHTTPBasicAuth: false, useAzureWorkloadIdentity: false, secrets: []string{"tp"},
	}
	empty := &access{enableOCI: true, insecureOCIForceHTTP: true, forceHTTPBasicAuth: true, useAzureWorkloadIdentity: true}
	empty.copyCredentialsFrom(tpl)
	want := *tpl
	want.secrets = []string{"tp"}
	// The inherited key brings the template's own endpoint settings (Patrol addition).
	want.appAPI = &appEndpoint{baseURL: "https://ghe/api/v3", proxy: "http://tproxy:1", noProxy: "tnp"}
	assert.Equal(t, want, *empty, "empty receiver takes every field; the four flags are overwritten")

	full := &access{
		typ: v1alpha1.RepositoryTypeGit, username: "u", password: "p", bearerToken: "b", sshPrivateKey: "s",
		tlsClientCertData: "c", tlsClientCertKey: "k", gcpServiceAccountKey: "g",
		githubAppID: 1, githubAppInstallationID: 2, githubAppEnterpriseBaseURL: "https://x", githubAppPrivateKey: "key",
		azureSPClientID: "ci", azureSPTenantID: "ti", azureSPClientSecret: "cs", azureSPEndpoint: "https://e",
		proxy: "http://p:1", noProxy: "np",
	}
	before := *full
	tpl2 := *tpl
	tpl2.enableOCI, tpl2.insecureOCIForceHTTP, tpl2.forceHTTPBasicAuth, tpl2.useAzureWorkloadIdentity = true, true, true, true
	full.copyCredentialsFrom(&tpl2)
	before.enableOCI, before.insecureOCIForceHTTP, before.forceHTTPBasicAuth, before.useAzureWorkloadIdentity = true, true, true, true
	before.secrets = []string{"tp"}
	assert.Equal(t, before, *full, "non-empty fields are kept; the four flags always come from the template")
}

// TestTypesAndSettings covers REPO-002, REPO-010..014, REPO-021, REPO-022 and REPO-024.
func TestTypesAndSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("helm", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "hu", "password": "hp", "tlsClientCertData": "C", "tlsClientCertKey": "K", "sshPrivateKey": "unused-ssh"}),
		repo("git", "https://git.example/r", func(r *v1alpha1.Repository) {
			r.Spec.InsecureIgnoreHostKey = true //nolint:staticcheck // the deprecated field is still honored
			r.Spec.EnableLFS = true
			r.Spec.Depth = 3
			r.Spec.Proxy = "http://proxy:3128"
			r.Spec.NoProxy = "a,b"
		}),
		repo("git-ocihttp", "https://git.example/o", func(r *v1alpha1.Repository) { r.Spec.InsecureOCIForceHTTP = true }),
		repo("helm", "https://charts.example", withType(v1alpha1.RepositoryTypeHelm), withCreds("helm"), func(r *v1alpha1.Repository) {
			r.Spec.Name = "stable"
			r.Spec.InsecureIgnoreHostKey = true //nolint:staticcheck // the deprecated field is still honored
			r.Spec.EnableLFS = true
			r.Spec.Depth = 5
		}),
		repo("helm-oci", "registry.example/charts", withType(v1alpha1.RepositoryTypeHelm)),
		repo("oci", "oci://registry.example/app", withType(v1alpha1.RepositoryTypeOCI), func(r *v1alpha1.Repository) {
			r.Spec.Insecure = true
			r.Spec.InsecureOCIForceHTTP = true
		}),
	})
	ctx := context.Background()
	get := func(q Query) *Resolution {
		t.Helper()
		res, err := f.resolver.Resolve(ctx, q)
		require.NoError(t, err)
		return res
	}

	g := get(Query{URL: "https://git.example/r"}).Repository
	assert.Equal(t, v1alpha1.RepositoryTypeGit, g.Type)
	assert.True(t, g.Insecure, "insecureIgnoreHostKey makes git insecure (IsInsecure)")
	assert.True(t, g.InsecureIgnoreHostKey)
	assert.True(t, g.EnableLFS)
	assert.Equal(t, int64(3), g.Depth)
	assert.Equal(t, "http://proxy:3128", g.Proxy)
	assert.Equal(t, "a,b", g.NoProxy)

	assert.True(t, get(Query{URL: "https://git.example/o"}).Repository.Insecure, "insecureOCIForceHTTP makes git insecure")

	h := get(Query{URL: "https://charts.example"})
	assert.Equal(t, v1alpha1.RepositoryTypeHelm, h.Repository.Type)
	assert.Equal(t, "stable", h.Repository.Name)
	assert.False(t, h.Repository.Insecure, "Helm honors only spec.insecure")
	assert.False(t, h.Repository.InsecureIgnoreHostKey)
	assert.False(t, h.Repository.EnableLFS)
	assert.Zero(t, h.Repository.Depth)
	assert.Equal(t, MethodBasic, h.Method)
	assert.Equal(t, "hu", h.Repository.Username)
	assert.Equal(t, "hp", h.Repository.Password)
	assert.Equal(t, "C", h.Repository.TLSClientCertData)
	assert.Empty(t, h.Repository.SSHPrivateKey, "only material of the selected method is forwarded")
	assert.False(t, h.Repository.EnableOCI)

	assert.True(t, get(Query{URL: "registry.example/charts"}).Repository.EnableOCI, "schemeless Helm URLs are OCI registries")

	o := get(Query{URL: "oci://registry.example/app"}).Repository
	assert.Equal(t, v1alpha1.RepositoryTypeOCI, o.Type)
	assert.True(t, o.Insecure)
	assert.True(t, o.InsecureOCIForceHTTP)

	// Type inference for stubs and hints (upstream TestRepoWithKnownType).
	assert.Equal(t, v1alpha1.RepositoryTypeOCI, get(Query{URL: "oci://other.example/x"}).Repository.Type)
	assert.Equal(t, v1alpha1.RepositoryTypeGit, get(Query{URL: "https://other.example/x"}).Repository.Type)
	hinted := get(Query{URL: "other.example/charts", Type: v1alpha1.RepositoryTypeHelm}).Repository
	assert.Equal(t, v1alpha1.RepositoryTypeHelm, hinted.Type)
	assert.True(t, hinted.EnableOCI)
	assert.Equal(t, v1alpha1.RepositoryTypeOCI, get(Query{URL: "oci://registry.example/app", Type: v1alpha1.RepositoryTypeGit}).Repository.Type,
		"a git hint keeps a configured oci type")
	assert.Equal(t, v1alpha1.RepositoryTypeHelm, get(Query{URL: "https://git.example/r", Type: v1alpha1.RepositoryTypeHelm}).Repository.Type)
}

// TestGitMethods covers REPO-005..009 and REPO-026: the credential precedence and that only
// the selected method's material is forwarded.
func TestGitMethods(t *testing.T) {
	t.Parallel()
	all := map[string]string{
		"username": "user", "password": "pw", "bearerToken": "bt", "sshPrivateKey": "sshkey",
		"tlsClientCertData": "CERT", "tlsClientCertKey": "KEY",
	}
	f := newFixture(t, []runtime.Object{
		secret("all", v1alpha1.LabelValueSecretTypeRepository, all),
		secret("bearer", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"bearerToken": "bt", "sshPrivateKey": "sshkey"}),
		secret("ssh", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"sshPrivateKey": "sshkey", "username": "u", "tlsClientCertData": "CERT", "tlsClientCertKey": "KEY"}),
		repo("all", "https://h.example/all", withCreds("all"), func(r *v1alpha1.Repository) { r.Spec.ForceHTTPBasicAuth = true }),
		repo("bearer", "https://h.example/bearer", withCreds("bearer")),
		repo("ssh", "git@h.example:org/ssh", withCreds("ssh")),
	})
	ctx := context.Background()
	res, err := f.resolver.Resolve(ctx, Query{URL: "https://h.example/all"})
	require.NoError(t, err)
	assert.Equal(t, MethodHTTPS, res.Method)
	r := res.Repository
	assert.Equal(t, []string{"user", "pw", "bt", "", "CERT", "KEY"}, []string{r.Username, r.Password, r.BearerToken, r.SSHPrivateKey, r.TLSClientCertData, r.TLSClientCertKey})
	assert.True(t, r.ForceHTTPBasicAuth)
	assert.Nil(t, r.CredentialsExpireAt)

	res, err = f.resolver.Resolve(ctx, Query{URL: "https://h.example/bearer"})
	require.NoError(t, err)
	assert.Equal(t, MethodHTTPS, res.Method)
	assert.Equal(t, "bt", res.Repository.BearerToken)
	assert.Empty(t, res.Repository.SSHPrivateKey)

	res, err = f.resolver.Resolve(ctx, Query{URL: "git@h.example:org/ssh.git"})
	require.NoError(t, err)
	assert.Equal(t, MethodSSH, res.Method)
	assert.Equal(t, "sshkey", res.Repository.SSHPrivateKey)
	assert.Empty(t, res.Repository.Username)
	assert.Empty(t, res.Repository.TLSClientCertData)
}

// TestResolveErrors covers missing Secrets and keys and other configuration errors.
func TestResolveErrors(t *testing.T) {
	t.Parallel()
	unlabeled := secret("unlabeled", "", map[string]string{"password": "p"})
	unlabeled.Labels = nil
	f := newFixture(t, []runtime.Object{
		unlabeled,
		secret("empty", v1alpha1.LabelValueSecretTypeRepository, nil),
		secret("provider-key", v1alpha1.LabelValueSecretTypeSCMCreds, map[string]string{"other": "x"}),
		repo("missing-secret", "https://h.example/missing", withCreds("nope")),
		repo("unlabeled", "https://h.example/unlabeled", withCreds("unlabeled")),
		repo("app-no-key", "https://h.example/app", withCreds("empty"), func(r *v1alpha1.Repository) {
			r.Spec.GitHubApp = githubApp(1, 2, "")
		}),
		repo("sp-no-secret", "https://h.example/sp", withCreds("empty"), func(r *v1alpha1.Repository) {
			r.Spec.AzureServicePrincipal = &v1alpha1.AzureServicePrincipal{ClientID: "c", TenantID: "t"}
		}),
		repo("no-provider", "https://h.example/np", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "nope"}
		}),
		repo("provider-missing-key", "https://h.example/pk", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "gh"}
		}),
		repo("provider-other-host", "https://elsewhere.example/x", func(r *v1alpha1.Repository) {
			r.Spec.SCMProviderRef = &v1alpha1.SCMProviderReference{Name: "gh"}
		}),
		&v1alpha1.SCMProvider{
			ObjectMeta: metaName("gh"),
			Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub, APIURL: "https://h.example/api/v3", Auth: &v1alpha1.SCMProviderAuth{
				GitHubApp: &v1alpha1.SCMProviderGitHubApp{AppID: 1, PrivateKeySecretRef: v1alpha1.SecretKeyReference{Name: "provider-key", Key: "key"}},
			}},
		},
		cred("broken-tpl", "https://tpl.example/", credWithCreds("gone")),
	})
	cases := []struct {
		url      string
		sentinel error
		reason   string
		contains []string
	}{
		{"https://h.example/missing", ErrSecretNotFound, v1alpha1.ReasonSecretNotFound, []string{`Repository "missing-secret"`, `secret "nope" not found`, v1alpha1.LabelKeySecretType}},
		{"https://h.example/unlabeled", ErrSecretNotFound, v1alpha1.ReasonSecretNotFound, []string{`secret "unlabeled" not found`}},
		{"https://h.example/app", ErrSecretKeyMissing, v1alpha1.ReasonSecretKeyMissing, []string{"githubApp is configured", "githubAppPrivateKey"}},
		{"https://h.example/sp", ErrSecretKeyMissing, v1alpha1.ReasonSecretKeyMissing, []string{"azureServicePrincipalClientSecret"}},
		{"https://h.example/np", ErrSCMProviderNotFound, v1alpha1.ReasonSCMProviderNotFound, []string{`SCMProvider "nope" not found`}},
		{"https://h.example/pk", ErrSecretKeyMissing, v1alpha1.ReasonSecretKeyMissing, []string{`secret "provider-key" has no key "key"`}},
		{"https://elsewhere.example/x", ErrInvalidConfiguration, v1alpha1.ReasonInvalidConfiguration, []string{`restricted to host "h.example"`}},
		{"https://tpl.example/repo", ErrSecretNotFound, v1alpha1.ReasonSecretNotFound, []string{`RepositoryCredential "broken-tpl"`}},
		{"", ErrInvalidConfiguration, v1alpha1.ReasonInvalidConfiguration, []string{"empty"}},
		{"https://h.example/" + strings.Repeat("a", maxURLLength), ErrInvalidConfiguration, v1alpha1.ReasonInvalidConfiguration, []string{"exceeds"}},
	}
	for _, tc := range cases {
		_, err := f.resolver.ResolveForURL(context.Background(), tc.url, "", read)
		require.Error(t, err, tc.url)
		assert.ErrorIs(t, err, tc.sentinel, tc.url)
		assert.True(t, IsConfigurationError(err), tc.url)
		assert.Equal(t, tc.reason, ConditionReason(err), tc.url)
		for _, s := range tc.contains {
			assert.Contains(t, err.Error(), s, tc.url)
		}
	}
	_, err := f.resolver.Resolve(context.Background(), Query{URL: "https://h.example/x", Purpose: "admin"})
	assert.ErrorIs(t, err, ErrInvalidConfiguration)
	assert.Positive(t, f.counterValue(t, "patrol_repository_resolution_errors_total", map[string]string{"reason": v1alpha1.ReasonSecretNotFound}))
}

// TestNotSynced checks lookups before Start.
func TestNotSynced(t *testing.T) {
	t.Parallel()
	l, err := NewLister(dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds()), kubefake.NewClientset(), ListerOptions{Namespace: testNS})
	require.NoError(t, err)
	r, err := NewResolver(l, Options{})
	require.NoError(t, err)
	_, err = r.ResolveForURL(context.Background(), "https://h.example/x", "", read)
	assert.ErrorIs(t, err, ErrNotSynced)
	_, err = l.Repositories(read)
	assert.ErrorIs(t, err, ErrNotSynced)
	_, err = NewLister(nil, nil, ListerOptions{Namespace: testNS})
	assert.Error(t, err)
	_, err = NewResolver(nil, Options{})
	assert.Error(t, err)
}

// TestListerLookups covers the public lookups and project.ScopedRepositoryLister.
func TestListerLookups(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		repo("b", "https://h.example/b", withProject("team")),
		repo("a", "https://h.example/a", withProject("team")),
		repo("w", "https://h.example/w", withProject("team"), withPurpose(write)),
		repo("g", "https://h.example/g"),
		cred("c1", "https://h.example/"),
		cred("c2", "https://h.example/", credWithPurpose(write)),
		&v1alpha1.SCMProvider{ObjectMeta: metaName("p"), Spec: v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderTypeGitHub}},
	})
	var _ project.ScopedRepositoryLister = f.lister
	urls, err := f.lister.GetProjectRepositoryURLs(context.Background(), "team")
	require.NoError(t, err)
	assert.Equal(t, []string{"https://h.example/a", "https://h.example/b"}, urls)
	urls, err = f.lister.GetProjectRepositoryURLs(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, urls)

	reads, err := f.lister.Repositories(read)
	require.NoError(t, err)
	require.Len(t, reads, 3)
	assert.Equal(t, "a", reads[0].Name)
	reads[0].Spec.URL = "mutated"
	again, err := f.lister.Repository("a")
	require.NoError(t, err)
	assert.Equal(t, "https://h.example/a", again.Spec.URL, "lookups return copies")
	_, err = f.lister.Repository("missing")
	assert.ErrorIs(t, err, ErrRepositoryNotFound)

	writes, err := f.lister.Repositories(write)
	require.NoError(t, err)
	require.Len(t, writes, 1)
	creds, err := f.lister.RepositoryCredentials(write)
	require.NoError(t, err)
	require.Len(t, creds, 1)
	assert.Equal(t, "c2", creds[0].Name)
	p, err := f.lister.SCMProvider("p")
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.SCMProviderTypeGitHub, p.Spec.Type)
	_, err = f.lister.SCMProvider("q")
	assert.ErrorIs(t, err, ErrSCMProviderNotFound)
	assert.Equal(t, testNS, f.lister.Namespace())
}

// TestReposForProject covers REPO-056 lists: named Helm repositories (and OCI ones for OCI
// sources) permitted by the project, with Helm and OCI credential templates.
func TestReposForProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, []runtime.Object{
		secret("helm", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "u", "password": "global-helm"}),
		ownedBy(secret("helm-team", v1alpha1.LabelValueSecretTypeRepository, map[string]string{"username": "u", "password": "team-helm"}), "stable-team"),
		secret("tpl", v1alpha1.LabelValueSecretTypeRepoCreds, map[string]string{"username": "tu", "password": "tpl-pass"}),
		repo("stable", "https://charts.example/stable", withType(v1alpha1.RepositoryTypeHelm), withCreds("helm"), func(r *v1alpha1.Repository) { r.Spec.Name = "stable" }),
		repo("stable-team", "https://charts.example/stable", withType(v1alpha1.RepositoryTypeHelm), withProject("team"), withCreds("helm-team"), func(r *v1alpha1.Repository) { r.Spec.Name = "stable" }),
		repo("other-project", "https://charts.example/other", withType(v1alpha1.RepositoryTypeHelm), withProject("other"), func(r *v1alpha1.Repository) { r.Spec.Name = "other" }),
		repo("unnamed", "https://charts.example/unnamed", withType(v1alpha1.RepositoryTypeHelm)),
		repo("denied", "https://denied.example/charts", withType(v1alpha1.RepositoryTypeHelm), func(r *v1alpha1.Repository) { r.Spec.Name = "denied" }),
		repo("oci", "oci://registry.example/x", withType(v1alpha1.RepositoryTypeOCI)),
		repo("git", "https://charts.example/git"),
		repo("broken", "https://charts.example/broken", withType(v1alpha1.RepositoryTypeHelm), withCreds("missing"), func(r *v1alpha1.Repository) { r.Spec.Name = "broken" }),
		cred("helm-tpl", "https://charts.example/", credWithType(v1alpha1.RepositoryTypeHelm), credWithCreds("tpl")),
		cred("oci-tpl", "oci://registry.example/", credWithType(v1alpha1.RepositoryTypeOCI), credWithCreds("tpl")),
		cred("git-tpl", "https://charts.example/", credWithCreds("tpl")),
		cred("denied-tpl", "https://denied.example/", credWithType(v1alpha1.RepositoryTypeHelm), credWithCreds("tpl")),
		cred("broken-tpl", "https://charts.example/b", credWithType(v1alpha1.RepositoryTypeHelm), credWithCreds("missing")),
	})
	proj := &v1alpha1.AppProject{}
	proj.Name = "team"
	proj.Spec.SourceRepos = []string{"*", "!https://denied.example/*"}
	ctx := context.Background()

	repos, err := f.resolver.ReposForProject(ctx, proj, false)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "team-helm", repos[0].Password, "the project-scoped registration wins")
	assert.Equal(t, "stable", repos[0].Name)

	repos, err = f.resolver.ReposForProject(ctx, proj, true)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	assert.Equal(t, "oci://registry.example/x", repos[0].URL)
	assert.Equal(t, "tpl-pass", repos[0].Password, "OCI repositories inherit templates")

	tpls, err := f.resolver.HelmCredentialTemplates(ctx, proj, false)
	require.NoError(t, err)
	require.Len(t, tpls, 1)
	assert.Equal(t, "https://charts.example/", tpls[0].URLPrefix)
	assert.Equal(t, "tpl-pass", tpls[0].Access.Password)
	assert.Empty(t, tpls[0].Access.URL)
	tpls, err = f.resolver.HelmCredentialTemplates(ctx, proj, true)
	require.NoError(t, err)
	require.Len(t, tpls, 2)
	assert.Contains(t, f.logs.String(), "skipping Helm credential template")
	assert.Contains(t, f.logs.String(), "skipping Helm dependency repository")

	_, err = f.resolver.ReposForProject(ctx, nil, false)
	assert.Error(t, err)
	_, err = f.resolver.HelmCredentialTemplates(ctx, nil, false)
	assert.Error(t, err)
}
