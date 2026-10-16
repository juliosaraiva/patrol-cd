package reposerver

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/git"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

func TestTrustMaterialAndCredentialMapping(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certs := filepath.Join(dir, "tls")
	require.NoError(t, os.Mkdir(certs, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(certs, "git.example.com"), []byte("PEM-git"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("not trust material"), 0o600))
	kh := filepath.Join(dir, "known_hosts")
	require.NoError(t, os.WriteFile(kh, []byte("git.example.com ssh-ed25519 AAAA\n"), 0o600))
	s := &Service{trust: &trust{knownHostsPath: kh, tlsCertsDir: certs, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	assert.Equal(t, []byte("PEM-git"), s.trust.CAData("git.example.com"))
	for _, bad := range []string{"", "../secret", "..", "a/b", "other.example.com"} {
		assert.Nil(t, s.trust.CAData(bad), bad)
	}
	assert.Equal(t, "git.example.com ssh-ed25519 AAAA\n", string(s.trust.KnownHosts()))
	require.NoError(t, os.WriteFile(kh, []byte("changed ssh-ed25519 BBBB BBBB\n"), 0o600))
	assert.Contains(t, string(s.trust.KnownHosts()), "changed", "re-read after a change")

	for _, tc := range []struct{ url, host string }{
		{"https://git.example.com/org/repo.git", "git.example.com"},
		{"https://git.example.com:8443/org/repo.git", "git.example.com"},
		{"git@git.example.com:org/repo.git", "git.example.com"},
		{"ssh://git@git.example.com:2222/org/repo.git", "git.example.com"},
		{"oci://registry.example.com/charts", "registry.example.com"},
		{"registry.example.com:5000/charts", "registry.example.com"},
	} {
		assert.Equal(t, tc.host, hostOf(tc.url), tc.url)
	}

	// HTTPS with a configured CA, credentials or neither.
	c := s.gitCreds(apiclient.Repository{URL: "https://git.example.com/org/repo.git", Username: "u", Password: "p"})
	https, ok := c.(git.HTTPSCreds)
	require.True(t, ok)
	assert.Equal(t, []byte("PEM-git"), https.CAData)
	assert.Equal(t, "p", https.Password)
	assert.IsType(t, git.NopCreds{}, s.gitCreds(apiclient.Repository{URL: "https://other.example.com/r.git"}))
	ca, ok := s.gitCreds(apiclient.Repository{URL: "https://git.example.com/r.git"}).(git.HTTPSCreds)
	require.True(t, ok, "anonymous access still trusts the configured CA")
	assert.NotEmpty(t, ca.CAData)

	// SSH: key plus known hosts; insecure flags disable host key checking.
	sshc, ok := s.gitCreds(apiclient.Repository{URL: "git@git.example.com:org/repo.git", SSHPrivateKey: "KEY"}).(git.SSHCreds)
	require.True(t, ok)
	assert.Equal(t, "KEY", sshc.PrivateKey)
	assert.Contains(t, string(sshc.KnownHosts), "changed")
	sshc, ok = s.gitCreds(apiclient.Repository{URL: "git@git.example.com:org/repo.git", SSHPrivateKey: "KEY", InsecureIgnoreHostKey: true}).(git.SSHCreds)
	require.True(t, ok)
	assert.True(t, sshc.InsecureIgnoreHostKey)

	// Helm: bearer tokens are passed as registry passwords; client certs only in pairs.
	hc := s.helmCreds(apiclient.Repository{URL: "https://charts.example.com", BearerToken: "tok", TLSClientCertData: "cert"}, true)
	assert.Equal(t, "tok", hc.Password)
	assert.True(t, hc.PassCredentials)
	assert.Nil(t, hc.CertData)

	assert.Equal(t, kindOCI, kindOf(apiclient.Repository{URL: "oci://r/x"}, v1alpha1.ApplicationSource{}))
	assert.Equal(t, kindHelm, kindOf(apiclient.Repository{URL: "https://c"}, v1alpha1.ApplicationSource{Chart: "x"}))
	assert.Equal(t, kindGit, kindOf(apiclient.Repository{URL: "https://g"}, v1alpha1.ApplicationSource{Path: "."}))
}

func TestErrorCodes(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		errorf(apiclient.CodeNotFound, "x"):               apiclient.CodeNotFound,
		&git.RevisionNotFoundError{Revision: "x"}:         apiclient.CodeInternal,
		git.ErrInvalidRevision:                            apiclient.CodeInvalidArgument,
		git.ErrAuthentication:                             apiclient.CodePermissionDenied,
		&apiclient.Error{Code: apiclient.CodeUnavailable}: apiclient.CodeUnavailable,
	} {
		assert.Equal(t, want, ErrorCode(err), "%v", err)
	}
	assert.Empty(t, ErrorCode(nil))
	// Sanitized errors keep their code and sentinels.
	ps := newPathSanitizer("/root")
	err := ps.sanitize(withCode(apiclient.CodeInvalidArgument, git.ErrInvalidRevision))
	assert.Equal(t, apiclient.CodeInvalidArgument, ErrorCode(err))
	err = ps.sanitize(errorf(apiclient.CodeNotFound, "open /root/r-1/x: %w", os.ErrNotExist))
	assert.Equal(t, "open "+placeholderCachedSource+"/x: file does not exist", err.Error())
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, apiclient.CodeNotFound, ErrorCode(err))
}
