package repository

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha1 "github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// TestGitMethodTable checks gitMethod against a model of upstream GetGitCreds for every
// combination of the inputs that select a method (REPO-026), plus Patrol's two
// configuration errors (a GitHub App ID without a private key, Azure service principal IDs
// without a client secret), where upstream silently falls through.
func TestGitMethodTable(t *testing.T) {
	t.Parallel()
	const (
		bitPassword = 1 << iota
		bitBearer
		bitSSH
		bitAppID
		bitAppKey
		bitGCP
		bitWI
		bitSPIDs
		bitSPSecret
		bitCount = iota
	)
	for mask := range 1 << bitCount {
		has := func(b int) bool { return mask&b != 0 }
		a := &access{}
		if has(bitPassword) {
			a.password = "pw"
		}
		if has(bitBearer) {
			a.bearerToken = "bt"
		}
		if has(bitSSH) {
			a.sshPrivateKey = "ssh"
		}
		if has(bitAppID) {
			a.githubAppID = 1
		}
		if has(bitAppKey) {
			a.githubAppPrivateKey = "key"
		}
		if has(bitGCP) {
			a.gcpServiceAccountKey = "{}"
		}
		a.useAzureWorkloadIdentity = has(bitWI)
		if has(bitSPIDs) {
			a.azureSPClientID, a.azureSPTenantID = "client", "tenant"
		}
		if has(bitSPSecret) {
			a.azureSPClientSecret = "secret"
		}

		var want Method
		wantErr := false
		switch {
		case has(bitPassword) || has(bitBearer):
			want = MethodHTTPS
		case has(bitSSH):
			want = MethodSSH
		case has(bitAppID) && has(bitAppKey):
			want = MethodGitHubApp
		case has(bitAppID):
			wantErr = true
		case has(bitGCP):
			want = MethodGCP
		case has(bitWI):
			want = MethodAzureWorkloadIdentity
		case has(bitSPIDs) && has(bitSPSecret):
			want = MethodAzureServicePrincipal
		case has(bitSPIDs):
			wantErr = true
		default:
			want = MethodNone
		}
		got, err := gitMethod(a, "test")
		name := fmt.Sprintf("mask %09b", mask)
		if wantErr {
			require.Error(t, err, name)
			assert.ErrorIs(t, err, ErrSecretKeyMissing, name)
			continue
		}
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)

		// HasCredentials (template inheritance) ignores the Google key and the App ID.
		wantCreds := has(bitPassword) || has(bitBearer) || has(bitSSH) || has(bitAppKey) || has(bitWI) || has(bitSPSecret)
		assert.Equal(t, wantCreds, a.hasCredentials(), name)
	}
}

// TestURLKey covers the lookup equivalence of REPO-029 (upstream SameURL).
func TestURLKey(t *testing.T) {
	t.Parallel()
	same := [][2]string{
		{"https://GitHub.com/Org/Repo.git", "https://github.com/org/repo"},
		{"git@github.com:org/repo", "ssh://git@github.com/org/repo"},
		{"git@github.com:org/repo.git", "ssh://git@github.com/org/repo"},
		{" https://github.com/org/repo ", "https://github.com/org/repo"},
		{"%%invalid", "%%invalid"},
	}
	for _, p := range same {
		assert.Equal(t, urlKey(p[0]), urlKey(p[1]), "%q vs %q", p[0], p[1])
	}
	different := [][2]string{
		{"https://github.com/org/repo", "git@github.com:org/repo"},
		{"https://github.com/org/repo/", "https://github.com/org/repo"},
		{"https://github.com/org/repo", "https://github.com/org/repo2"},
		{"https://github.com/org/repo", "http://github.com/org/repo"},
	}
	for _, p := range different {
		assert.NotEqual(t, urlKey(p[0]), urlKey(p[1]), "%q vs %q", p[0], p[1])
	}
}

// TestProviderWebHost covers the host restriction of SCMProvider credentials.
func TestProviderWebHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ, api, web, want, scheme string
		wantErr                     bool
	}{
		{typ: "github", want: "github.com", scheme: "https"},
		{typ: "gitlab", want: "gitlab.com", scheme: "https"},
		{typ: "bitbucketCloud", want: "bitbucket.org", scheme: "https"},
		{typ: "azureDevOps", want: "dev.azure.com", scheme: "https"},
		{typ: "gitea", wantErr: true},
		{typ: "github", api: "https://ghe.example.com/api/v3", want: "ghe.example.com", scheme: "https"},
		{typ: "github", api: "https://api.ghe.example.com", want: "ghe.example.com", scheme: "https"},
		{typ: "github", api: "https://api.ghe.example.com", web: "https://Code.Example.com/", want: "code.example.com", scheme: "https"},
		{typ: "gitea", api: "http://gitea.internal/api/v1", want: "gitea.internal", scheme: "http"},
		{typ: "gitea", api: "https://gitea.internal/api/v1", web: "HTTP://gitea.internal", want: "gitea.internal", scheme: "http"},
		{typ: "github", api: "::bad", wantErr: true},
		{typ: "github", web: "relative/path", wantErr: true},
	}
	for _, tc := range cases {
		spec := scmSpec(tc.typ, tc.api, tc.web)
		got, scheme, err := providerWebHost(&spec)
		if tc.wantErr {
			assert.Error(t, err, "%+v", tc)
			continue
		}
		require.NoError(t, err, "%+v", tc)
		assert.Equal(t, tc.want, got, "%+v", tc)
		assert.Equal(t, tc.scheme, scheme, "%+v", tc)
	}
	for in, want := range map[string]string{
		"https://GitHub.com/org/repo": "github.com",
		"git@github.com:org/repo":     "github.com",
		"ssh://git@host:2222/x":       "host",
		"repo":                        "",
	} {
		assert.Equal(t, want, repoHost(in), in)
	}
	for in, want := range map[string]string{
		"https://h/x": "https", "HTTP://h/x": "http", "git@h:x": "", "oci://r/x": "oci", "r/x": "", "://x": "",
	} {
		assert.Equal(t, want, urlScheme(in), in)
	}
}

func scmSpec(typ, apiURL, webURL string) v1alpha1.SCMProviderSpec {
	return v1alpha1.SCMProviderSpec{Type: v1alpha1.SCMProviderType(typ), APIURL: apiURL, WebURL: webURL}
}
