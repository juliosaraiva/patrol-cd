package repositories

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/openapi"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestValidateRepoURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url        string
		schemeless bool
		ok         bool
	}{
		{"https://github.com/org/repo.git", false, true},
		{"http://git.local:8080/repo", false, true},
		{"ssh://git@github.com/org/repo", false, true},
		{"ssh://git@github.com:org/repo", false, true}, // malformed but accepted upstream
		{"git@github.com:org/repo.git", false, true},
		{"git://git.example.com/repo", false, true},
		{"oci://registry.example.com/charts/app", false, true},
		{"https://user@github.com/org/repo", false, true},
		{"registry.example.com/charts", true, true},
		{"registry.example.com:5000/charts", true, true},
		{"registry.example.com/charts", false, false},
		{"", false, false},
		{"file:///etc/passwd", false, false},
		{"ftp://example.com/x", false, false},
		{"https://u:p@github.com/org/repo", false, false},
		{"https:///no-host", false, false},
		{"/srv/repo", true, false},
		{"../repo", true, false},
		{"ext::sh -c id", false, false},
		{"ext::id", false, false},
		{"fd::17", false, false},
		{"-uhttps://evil", false, false},
		{"--upload-pack=x:y", false, false},
		{`C:\repos\x`, false, false},
		{"https://github.com/org/repo\n", false, false},
		{"https://github.com/org/re po", false, false},
		{"https://" + strings.Repeat("a", 2100) + ".com", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			err := validateRepoURL("spec.url", tc.url, tc.schemeless)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestValidateProxyAndEndpoints(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"", "http://proxy:3128", "https://proxy.example.com", "socks5://p:1080", "socks5h://p:1080"} {
		assert.NoError(t, validateProxy("spec.proxy", p), p)
	}
	for _, p := range []string{"ftp://p", "http://u:p@proxy:3128", "http://u@proxy", "proxy:3128", "http://"} {
		assert.Error(t, validateProxy("spec.proxy", p), p)
	}
	assert.NoError(t, validateHTTPSURL("f", "https://ghe.example.com/api/v3"))
	assert.Error(t, validateHTTPSURL("f", "http://ghe.example.com/api/v3"))
	assert.Error(t, validateHTTPSURL("f", "https://u@ghe.example.com"))
	assert.NoError(t, validateHTTPURL("f", "http://gitea.local:3000"))
	assert.Error(t, validateHTTPURL("f", "javascript:alert(1)"))
	assert.Error(t, validateHTTPURL("f", "https://api.example.com/?q=1"))
}

func pemCertAndKey(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kb, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestValidateCredentials(t *testing.T) {
	t.Parallel()
	cert, key := pemCertAndKey(t)
	_, otherKey := pemCertAndKey(t)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(edKey, "")
	require.NoError(t, err)
	sshKey := string(pem.EncodeToMemory(block))
	encrypted, err := ssh.MarshalPrivateKeyWithPassphrase(edKey, "", []byte("pw"))
	require.NoError(t, err)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ghKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))
	app := &v1alpha1.RepositoryAccess{GitHubApp: &v1alpha1.RepositoryGitHubApp{AppID: 1}}
	sp := &v1alpha1.RepositoryAccess{AzureServicePrincipal: &v1alpha1.AzureServicePrincipal{ClientID: "c", TenantID: "t"}}
	none := &v1alpha1.RepositoryAccess{}
	cases := []struct {
		name   string
		c      *apiclient.RepositoryCredentials
		a      *v1alpha1.RepositoryAccess
		detail string
	}{
		{"nil", nil, none, ""},
		{"basic", &apiclient.RepositoryCredentials{Username: "u", Password: "p"}, none, ""},
		{"tls pair", &apiclient.RepositoryCredentials{TLSClientCertData: cert, TLSClientCertKey: key}, none, ""},
		{"tls mismatch", &apiclient.RepositoryCredentials{TLSClientCertData: cert, TLSClientCertKey: otherKey}, none, "not a valid PEM certificate and matching private key"},
		{"tls half", &apiclient.RepositoryCredentials{TLSClientCertKey: key}, none, "must be set together"},
		{"ssh", &apiclient.RepositoryCredentials{SSHPrivateKey: sshKey}, none, ""},
		{"ssh passphrase", &apiclient.RepositoryCredentials{SSHPrivateKey: string(pem.EncodeToMemory(encrypted))}, none, "passphrase"},
		{"ssh garbage", &apiclient.RepositoryCredentials{SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----"}, none, "not a valid private key"},
		{"github key", &apiclient.RepositoryCredentials{GitHubAppPrivateKey: ghKey}, app, ""},
		{"github key without app", &apiclient.RepositoryCredentials{GitHubAppPrivateKey: ghKey}, none, "requires spec.githubApp"},
		{"github key not rsa", &apiclient.RepositoryCredentials{GitHubAppPrivateKey: key}, app, "RSA private key"},
		{"gcp", &apiclient.RepositoryCredentials{GCPServiceAccountKey: `{"type":"service_account","client_email":"x"}`}, none, ""},
		{"gcp external", &apiclient.RepositoryCredentials{GCPServiceAccountKey: `{"type":"external_account"}`}, none, "service account key"},
		{"gcp not json", &apiclient.RepositoryCredentials{GCPServiceAccountKey: `nope`}, none, "service account key"},
		{"azure secret", &apiclient.RepositoryCredentials{AzureServicePrincipalClientSecret: "s"}, sp, ""},
		{"azure secret without sp", &apiclient.RepositoryCredentials{AzureServicePrincipalClientSecret: "s"}, none, "requires spec.azureServicePrincipal"},
		{"newline in password", &apiclient.RepositoryCredentials{Password: "a\nb"}, none, "line breaks"},
		{"control in username", &apiclient.RepositoryCredentials{Username: "a\tb"}, none, "whitespace or control"},
		{"too large", &apiclient.RepositoryCredentials{Password: strings.Repeat("x", maxCredentialBytes+1)}, none, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCredentials(tc.c, tc.a)
			if tc.detail == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.detail)
			// Messages never contain the values.
			if tc.c != nil {
				for _, f := range credentialFields(tc.c) {
					if len(f.value) > 8 {
						assert.NotContains(t, err.Error(), f.value)
					}
				}
			}
		})
	}
}

func TestCredentialsRedacted(t *testing.T) {
	t.Parallel()
	c := apiclient.RepositoryCredentials{Password: "top-secret"}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, &c)} {
		assert.NotContains(t, s, "top-secret")
	}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	assert.Contains(t, string(b), "top-secret", "JSON (request bodies) is not redacted")
}

func TestNormalizeRepositorySpec(t *testing.T) {
	t.Parallel()
	spec := v1alpha1.RepositorySpec{URL: " https://github.com/org/repo "}
	require.NoError(t, normalizeRepositorySpec(&spec, readPurpose))
	assert.Equal(t, "https://github.com/org/repo", spec.URL)
	assert.Equal(t, v1alpha1.RepositoryTypeGit, spec.Type)
	assert.Equal(t, v1alpha1.RepositoryPurposeRead, spec.Purpose)
	bad := []v1alpha1.RepositorySpec{
		{URL: "https://github.com/org/repo", Type: "svn"},
		{URL: "https://github.com/org/repo", Purpose: v1alpha1.RepositoryPurposeWrite},
		{URL: "https://github.com/org/repo", Depth: -1},
		{URL: "https://github.com/org/repo", Project: "Bad_Project"},
		{URL: "https://charts.example.com", Type: v1alpha1.RepositoryTypeHelm, EnableLFS: true},
		{URL: "https://github.com/org/repo", RepositoryAccess: v1alpha1.RepositoryAccess{GitHubApp: &v1alpha1.RepositoryGitHubApp{}}},
		{URL: "https://github.com/org/repo", RepositoryAccess: v1alpha1.RepositoryAccess{AzureServicePrincipal: &v1alpha1.AzureServicePrincipal{ClientID: "x"}}},
		{URL: "https://github.com/org/repo", RepositoryAccess: v1alpha1.RepositoryAccess{SCMProviderRef: &v1alpha1.SCMProviderReference{Name: "UPPER"}}},
	}
	for i, s := range bad {
		assert.Error(t, normalizeRepositorySpec(&s, readPurpose), "case %d", i)
	}
	w := v1alpha1.RepositorySpec{URL: "https://charts.example.com", Type: v1alpha1.RepositoryTypeHelm}
	assert.Error(t, normalizeRepositorySpec(&w, writePurpose), "write purpose is git only")
}

func TestObjectNames(t *testing.T) {
	t.Parallel()
	a := objectName("repo", "https://GitHub.com/Org/Repo.git", "")
	assert.Equal(t, a, objectName("repo", "https://github.com/org/repo", ""), "same location, same name")
	assert.NotEqual(t, a, objectName("repo", "https://github.com/org/repo", "team"), "per project")
	assert.NotEqual(t, a, objectName("repo-write", "https://github.com/org/repo", ""), "per purpose")
	assert.Regexp(t, `^repo-[0-9a-f]{16}$`, a)
	assert.Equal(t, "repo-x-creds", managedSecretName("repo-x"))
	long := managedSecretName(strings.Repeat("a", 250))
	assert.LessOrEqual(t, len(long), 253)
	assert.True(t, strings.HasPrefix(long, "creds-"))
}

func TestSpecDiff(t *testing.T) {
	t.Parallel()
	a := v1alpha1.RepositorySpec{URL: "https://x", Type: "git", Depth: 1}
	b := v1alpha1.RepositorySpec{URL: "https://x", Type: "git", Depth: 2, RepositoryAccess: v1alpha1.RepositoryAccess{Proxy: "http://p:1"}}
	assert.Equal(t, []string{"depth", "proxy"}, specDiff(a, b))
	assert.Empty(t, specDiff(a, a))
	err := specIsDifferent("repository", []string{"depth"})
	assert.Equal(t, `existing repository spec is different; use upsert flag to force update; difference in keys "depth"`, err.(*rest.Error).Detail)
}

func TestKnownHostLines(t *testing.T) {
	t.Parallel()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(k.Marshal())
	data := "# comment\n\ngithub.com ssh-ed25519 " + b64 + " comment\nnot a valid line\n@cert-authority *.example.com ssh-ed25519 " + b64 + "\n"
	lines := parseKnownHostLines(data)
	require.Len(t, lines, 5)
	assert.False(t, lines[0].ok)
	assert.True(t, lines[2].ok)
	assert.Equal(t, "github.com", lines[2].host)
	assert.Equal(t, "ssh-ed25519", lines[2].keyType)
	assert.Equal(t, b64, lines[2].keyData)
	assert.Equal(t, ssh.FingerprintSHA256(k), lines[2].fingerprint)
	assert.False(t, lines[3].ok)
	assert.True(t, lines[4].ok)
	assert.Equal(t, "*.example.com", lines[4].host)
	assert.Equal(t, data, renderKnownHostLines(lines), "round trip keeps every line")

	sel, err := newCertSelector(&apiclient.RepositoryCertificateQuery{CertType: "ssh", HostNamePattern: "github.com"})
	require.NoError(t, err)
	ch := removeCertificates(data, nil, sel)
	assert.Len(t, ch.items, 1)
	assert.Equal(t, "# comment\n\nnot a valid line\n@cert-authority *.example.com ssh-ed25519 "+b64+"\n", ch.ssh)
	assert.False(t, ch.tlsChanged)
}

func TestCertificateSelectors(t *testing.T) {
	t.Parallel()
	assert.True(t, matchHostName("[git.example.com]:2222", "[git.example.com]:2222"))
	assert.True(t, matchHostName("git.example.com", "*.example.com"))
	assert.False(t, matchHostName("git.example.org", "*.example.com"))
	assert.True(t, matchHostName("anything", ""))
	for _, ct := range []string{"", "*", "ssh", "https", "tls"} {
		_, err := newCertSelector(&apiclient.RepositoryCertificateQuery{CertType: ct})
		assert.NoError(t, err, ct)
	}
	_, err := newCertSelector(&apiclient.RepositoryCertificateQuery{CertType: "x509"})
	assert.Error(t, err)
	_, err = newCertSelector(&apiclient.RepositoryCertificateQuery{HostNamePattern: `a\`})
	assert.Error(t, err)
}

func TestApplyCertificates(t *testing.T) {
	t.Parallel()
	cert, _ := pemCertAndKey(t)
	ch, err := applyCertificates("", nil, []apiclient.RepositoryCertificate{{ServerName: "git.example.com", CertType: "https", CertData: "junk before\n" + cert + "junk after\n"}}, false)
	require.NoError(t, err)
	assert.Equal(t, cert, ch.tls["git.example.com"], "only certificate blocks are stored")
	assert.True(t, ch.tlsChanged)
	again, err := applyCertificates("", ch.tls, []apiclient.RepositoryCertificate{{ServerName: "git.example.com", CertType: "https", CertData: cert}}, false)
	require.NoError(t, err)
	assert.False(t, again.tlsChanged)
	assert.Empty(t, again.items)
	_, err = applyCertificates("", nil, []apiclient.RepositoryCertificate{{ServerName: "10.0.0.1", CertType: "https", CertData: cert}}, false)
	assert.NoError(t, err, "IPv4 addresses are valid server names")
	_, err = applyCertificates("", nil, []apiclient.RepositoryCertificate{{ServerName: "2001:db8::1", CertType: "https", CertData: cert}}, false)
	assert.Error(t, err, "IPv6 literals cannot be ConfigMap keys")
	_, err = applyCertificates("", nil, []apiclient.RepositoryCertificate{{ServerName: "git.example.com:443", CertType: "https", CertData: cert}}, false)
	assert.Error(t, err, "no port")
}

func TestSourceInHistory(t *testing.T) {
	t.Parallel()
	cur := v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "a", TargetRevision: "HEAD"}
	app := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{Source: &cur}}
	app.Status.History = v1alpha1.RevisionHistories{
		{ID: 1, Revision: "sha1", Source: &v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "old", TargetRevision: "HEAD"}},
		{ID: 2, Revisions: []string{"s1", "s2"}, Sources: v1alpha1.ApplicationSources{{RepoURL: "https://x/r", Path: "m0"}, {RepoURL: "https://x/r", Path: "m1"}}},
	}
	withHelm := cur
	withHelm.Helm = &v1alpha1.ApplicationSourceHelm{}
	cases := []struct {
		name  string
		src   v1alpha1.ApplicationSource
		index int
		ver   int64
		want  bool
	}{
		{"current", cur, 0, 0, true},
		{"current with empty helm block", withHelm, 0, 0, true},
		{"history synced revision", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "old", TargetRevision: "sha1"}, 0, 0, true},
		{"history symbolic revision", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "old", TargetRevision: "HEAD"}, 0, 0, false},
		{"multi-source version", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "m1", TargetRevision: "s2"}, 1, 2, true},
		{"multi-source wrong version", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "m1", TargetRevision: "s2"}, 1, 1, false},
		{"multi-source index out of range", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "m1", TargetRevision: "s2"}, 5, 2, false},
		{"arbitrary", v1alpha1.ApplicationSource{RepoURL: "https://x/r", Path: "../../etc"}, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, sourceInHistory(app, &tc.src, tc.index, tc.ver))
		})
	}
	hyd := &v1alpha1.Application{Spec: v1alpha1.ApplicationSpec{SourceHydrator: &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: "https://x/dry", TargetRevision: "main", Path: "p"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env", Path: "out"},
	}}}
	dry := hyd.Spec.SourceHydrator.GetDrySource()
	assert.True(t, sourceInHistory(hyd, &dry, 0, 0))
	assert.False(t, sourceInHistory(hyd, &cur, 0, 0))
}

func TestSplitArmoredKeysRejectsPrivate(t *testing.T) {
	t.Parallel()
	_, err := splitArmoredKeys("-----BEGIN PGP PRIVATE KEY BLOCK-----\nx\n-----END PGP PRIVATE KEY BLOCK-----")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "private key material")
	_, err = splitArmoredKeys("-----BEGIN PGP PUBLIC KEY BLOCK-----\nno end")
	assert.Error(t, err)
	_, err = splitArmoredKeys("plain text")
	assert.Error(t, err)
	id, err := normalizeKeyID("0123456789abcdef0123456789abcdef01234567")
	require.NoError(t, err)
	assert.Equal(t, "89ABCDEF01234567", id)
}

// TestRoutes registers the service with zero Deps (OpenAPI generation), checks every
// route of the area in api-contract.md, and answers 503 when not configured.
func TestRoutes(t *testing.T) {
	t.Parallel()
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	require.NoError(t, tbl.Err())
	got := map[string]rest.Route{}
	for _, r := range tbl.Routes() {
		got[r.Pattern()] = r
	}
	want := []string{
		"GET /api/v1/repositories", "POST /api/v1/repositories",
		"GET /api/v1/repositories/{repo}", "PUT /api/v1/repositories/{repo}", "DELETE /api/v1/repositories/{repo}",
		"GET /api/v1/repositories/{repo}/refs", "GET /api/v1/repositories/{repo}/oci-tags", "GET /api/v1/repositories/{repo}/apps",
		"POST /api/v1/repositories/{repo}/appdetails", "GET /api/v1/repositories/{repo}/helmcharts", "POST /api/v1/repositories/{repo}/validate",
		"GET /api/v1/write-repositories", "POST /api/v1/write-repositories",
		"GET /api/v1/write-repositories/{repo}", "PUT /api/v1/write-repositories/{repo}", "DELETE /api/v1/write-repositories/{repo}",
		"POST /api/v1/write-repositories/{repo}/validate",
		"GET /api/v1/repocreds", "POST /api/v1/repocreds", "PUT /api/v1/repocreds/{url}", "DELETE /api/v1/repocreds/{url}",
		"GET /api/v1/write-repocreds", "POST /api/v1/write-repocreds", "PUT /api/v1/write-repocreds/{url}", "DELETE /api/v1/write-repocreds/{url}",
		"GET /api/v1/certificates", "POST /api/v1/certificates", "DELETE /api/v1/certificates",
		"GET /api/v1/gpgkeys", "POST /api/v1/gpgkeys", "GET /api/v1/gpgkeys/{keyID}", "DELETE /api/v1/gpgkeys/{keyID}",
		"GET /api/v1/scmproviders", "POST /api/v1/scmproviders", "GET /api/v1/scmproviders/{name}", "PUT /api/v1/scmproviders/{name}", "DELETE /api/v1/scmproviders/{name}",
	}
	assert.Len(t, got, len(want))
	for _, p := range want {
		r, ok := got[p]
		if !assert.True(t, ok, "route %s", p) {
			continue
		}
		if r.Access.Mode == rest.AccessRBAC {
			assert.NotEmpty(t, r.Access.Resource, p)
		}
		assert.NotEqual(t, rest.AccessPublic, r.Access.Mode, "%s must not be public", p)
	}
	assert.Equal(t, "certificates", got["DELETE /api/v1/certificates"].Access.Resource)
	assert.Equal(t, ResourceSCMProviders, got["PUT /api/v1/scmproviders/{name}"].Access.Resource)
	assert.Equal(t, "write-repositories", got["PUT /api/v1/write-repocreds/{url}"].Access.Resource)

	// Zero Deps: 503 (with auth disabled so the handler runs).
	h, err := tbl.Handler(&rest.Env{DisableAuth: true, Authenticator: &rest.Authenticator{DisableAuth: true}})
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL, PlainText: true})
	require.NoError(t, err)
	_, err = c.ListRepositories(context.Background(), &apiclient.RepositoryListRequest{})
	var ae *apiclient.APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusServiceUnavailable, ae.StatusCode)
}

// TestOpenAPI generates the OpenAPI document of the service's routes with the CRD schemas
// (the types must be documentable) and checks that credential values appear in request
// schemas only.
func TestOpenAPI(t *testing.T) {
	t.Parallel()
	crds, err := openapi.LoadCRDSchemas(filepath.Join("..", "..", "..", "..", "deploy", "crds"))
	require.NoError(t, err)
	tbl := rest.NewTable()
	New(Deps{}).Register(tbl)
	doc, err := openapi.Generate(tbl.Routes(), openapi.Options{Title: "t", Version: "v1", CRDs: crds})
	require.NoError(t, err)
	var m struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(doc, &m))
	for _, name := range []string{"RepositoryWriteRequest", "RepositoryInfo", "RepositoryCredentials", "RepositoryCertificateList", "GnuPGPublicKeyCreateResponse", "SCMProviderWriteRequest"} {
		assert.Contains(t, m.Components.Schemas, name)
	}
	assert.NotContains(t, string(m.Components.Schemas["RepositoryInfo"]), "password")
	assert.NotContains(t, string(m.Components.Schemas["RepositoryCredentialInfo"]), "password")
	assert.NotContains(t, string(m.Components.Schemas["SCMProviderWriteRequest"]), "credentials")
}

func TestRepositoryInfo(t *testing.T) {
	t.Parallel()
	r := &v1alpha1.Repository{ObjectMeta: metav1.ObjectMeta{Name: "r", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
		Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}}, Spec: v1alpha1.RepositorySpec{URL: "https://x/r"}}
	info := repositoryInfo(r, true)
	assert.Nil(t, info.Metadata.ManagedFields)
	assert.Nil(t, info.Metadata.Annotations)
	assert.False(t, info.HasCredentials)
	assert.True(t, info.InheritedCredentials)
	assert.Equal(t, v1alpha1.ConnectionStatusUnknown, info.ConnectionState.Status)
	r.Spec.CredentialsRef = &v1alpha1.LocalSecretReference{Name: "s"}
	info = repositoryInfo(r, true)
	assert.True(t, info.HasCredentials)
	assert.False(t, info.InheritedCredentials, "own credentials win")
	assert.True(t, templateMatches("https://github.com/org/", "https://GitHub.com/org/repo.git"))
	assert.False(t, templateMatches("https://github.com/org/", "https://github.com/organization/repo"))
}

func TestRepoServerErrorRedacted(t *testing.T) {
	t.Parallel()
	repo := &reposerver.Repository{Password: "very-secret", SSHPrivateKey: "-----BEGIN KEY-----abc"}
	for _, code := range []string{reposerver.CodeInvalidArgument, reposerver.CodeNotFound, reposerver.CodeUnavailable, reposerver.CodeInternal, reposerver.CodeResourceExhausted} {
		err := repoServerError(&reposerver.Error{Code: code, Message: "failed with very-secret"}, repo)
		var re *rest.Error
		require.ErrorAs(t, err, &re, code)
		assert.NotContains(t, re.Detail, "very-secret", code)
		require.Error(t, re.Cause)
		assert.NotContains(t, re.Cause.Error(), "very-secret", code)
	}
	err := repoServerError(fmt.Errorf("dial: very-secret"), repo)
	var re *rest.Error
	require.ErrorAs(t, err, &re)
	assert.Equal(t, http.StatusServiceUnavailable, re.Status)
	assert.NotContains(t, re.Cause.Error(), "very-secret")
}

func TestRedactURLPassword(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"https://bot:hunter2@git.example.com/org/x", "https://bot:******@git.example.com/org/x"},
		{"https://bot:hunter2@git.example.com", "https://bot:******@git.example.com"},
		{"https://bot:hunter2@git.example.com?x=1#f", "https://bot:******@git.example.com?x=1#f"},
		{"http://u:p@ss@proxy.example.com:3128", "http://u:******@proxy.example.com:3128"},
		{"ssh://git:pw@host:2222/org/x", "ssh://git:******@host:2222/org/x"},
		{"https://u:pw@host:badport/x", "https://u:******@host:badport/x"},
		{"team-a/https://u:pw@host/x", "team-a/https://u:******@host/x"},
		{"https://bot@git.example.com/org/x", "https://bot@git.example.com/org/x"},
		{"https://git.example.com/org/x@v1:y", "https://git.example.com/org/x@v1:y"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"registry.example.com/charts", "registry.example.com/charts"},
		{"HTTPS://Bot:Secret@Host/X", "HTTPS://Bot:******@Host/X"},
		{"", ""},
	} {
		assert.Equal(t, tc.want, redactURLPassword(tc.in), tc.in)
	}
}

func TestInheritedTemplateSelection(t *testing.T) {
	t.Parallel()
	tpl := func(name, prefix string) v1alpha1.RepositoryCredential {
		return v1alpha1.RepositoryCredential{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.RepositoryCredentialSpec{URLPrefix: prefix}}
	}
	templates := []v1alpha1.RepositoryCredential{
		tpl("a", "https://git.example.com/"), tpl("b", "https://git.example.com/org/"), tpl("c", "https://git.example.com/org/"),
	}
	assert.Equal(t, "b", inheritedTemplate(templates, "https://GIT.example.com/org/x.git").Name, "longest prefix, first by name")
	assert.Equal(t, "a", inheritedTemplate(templates, "https://git.example.com/other/x").Name)
	assert.Nil(t, inheritedTemplate(templates, "https://git.other.example/org/x"))

	spec := &v1alpha1.RepositorySpec{URL: "https://git.example.com/org/x", Insecure: true}
	spec.Proxy = "http://p.example:1"
	assert.Equal(t, []string{"spec.proxy", "spec.insecure"}, transportOverrides(spec, &templates[1]))
	templates[1].Spec.Proxy = "http://p.example:1"
	assert.Equal(t, []string{"spec.insecure"}, transportOverrides(spec, &templates[1]))
	templates[1].Spec.InsecureOCIForceHTTP = true
	assert.Empty(t, transportOverrides(&v1alpha1.RepositorySpec{RepositoryAccess: v1alpha1.RepositoryAccess{InsecureOCIForceHTTP: true}}, &templates[1]))
}
