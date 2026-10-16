package reposerver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
)

// signer is a GnuPG key in a throwaway home used to sign fixture commits.
type signer struct {
	gpg    string
	home   string
	keyID  string // 16 hex digits, upper case
	public string // armored public key
}

func runGPG(t *testing.T, gpg, home, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gpg, append([]string{"--batch", "--no-tty", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
	cmd.Env = []string{"GNUPGHOME=" + home, "PATH=" + os.Getenv("PATH"), "HOME=" + home}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		require.NoError(t, err, "gpg %v: %s", args, ee.Stderr)
	}
	require.NoError(t, err)
	return string(out)
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	gpg := gpgBin(t)
	home := shortTempDir(t, "pgs")
	require.NoError(t, os.Chmod(home, 0o700))
	t.Cleanup(func() {
		if gpgconf, err := exec.LookPath("gpgconf"); err == nil {
			cmd := exec.Command(gpgconf, "--kill", "gpg-agent")
			cmd.Env = []string{"GNUPGHOME=" + home, "PATH=" + os.Getenv("PATH")}
			_ = cmd.Run()
		}
	})
	runGPG(t, gpg, home, "", "--quick-gen-key", "Test Signer <signer@example.com>", "ed25519", "sign", "never")
	s := &signer{gpg: gpg, home: home}
	for _, line := range strings.Split(runGPG(t, gpg, home, "", "--list-keys", "--with-colons"), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 4 && f[0] == "pub" {
			s.keyID = strings.ToUpper(f[4])
		}
	}
	require.Len(t, s.keyID, 16)
	s.public = runGPG(t, gpg, home, "", "--armor", "--export", s.keyID)
	return s
}

// signedCommit commits files with a signature.
func (s *signer) signedCommit(t *testing.T, r *remote, msg string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(r.work, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	env := append(append([]string{}, r.env...), "GNUPGHOME="+s.home)
	runGit(t, r.work, env, "add", "--all")
	runGit(t, r.work, env, "-c", "gpg.program="+s.gpg, "-c", "user.signingkey="+s.keyID, "commit", "--quiet", "-S", "-m", msg)
	r.git("push", "--quiet", "origin", "main")
	return r.git("rev-parse", "HEAD")
}

func policy(mode v1alpha1.SourceIntegrityGitPolicyGPGMode, keys ...string) *v1alpha1.SourceIntegrity {
	return &v1alpha1.SourceIntegrity{Git: &v1alpha1.SourceIntegrityGit{Policies: []v1alpha1.SourceIntegrityGitPolicy{{
		Repos: []v1alpha1.SourceIntegrityGitPolicyRepo{{URL: "*"}},
		GPG:   &v1alpha1.SourceIntegrityGitPolicyGPG{Mode: mode, Keys: keys},
	}}}}
}

func TestSourceIntegrity(t *testing.T) {
	t.Parallel()
	sg := newSigner(t)
	r := newRemote(t)
	signed := sg.signedCommit(t, r, "signed", map[string]string{"app/cm.yaml": configMap("cm", nil)})

	keysDir := t.TempDir()
	s := newTestService(t, func(c *Config) {
		c.GPGKeysDir = keysDir
		c.GPGPath = sg.gpg
	})
	c := startServer(t, s)
	gen := func(si *v1alpha1.SourceIntegrity, rev string) *apiclient.ManifestResponse {
		t.Helper()
		req := dirRequest(r, "app", "app", rev)
		req.SourceIntegrity = si
		req.NoRevisionCache = true
		resp, err := c.GenerateManifest(bg(), req)
		require.NoError(t, err)
		return resp
	}

	// The key is not in the keyring yet: verification fails with "key not in keyring".
	resp := gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), signed)
	require.NotNil(t, resp.SourceIntegrityResult)
	assert.False(t, resp.SourceIntegrityResult.IsValid())
	assert.Contains(t, resp.SourceIntegrityResult.Checks[0].Problems[0], "signed with key not in keyring")

	// Mounting the key (file named by key ID) rebuilds the keyring.
	require.NoError(t, os.WriteFile(filepath.Join(keysDir, sg.keyID), []byte(sg.public), 0o644))
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), signed)
	require.NotNil(t, resp.SourceIntegrityResult)
	assert.True(t, resp.SourceIntegrityResult.IsValid(), "%+v", resp.SourceIntegrityResult)
	assert.Equal(t, integrityCheckGPG, resp.SourceIntegrityResult.Checks[0].Name)
	assert.Contains(t, resp.VerifyResult, "Good signature from Fixture Author <author@example.com> key "+sg.keyID)

	// Signed with a key that is not allowed.
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, "0123456789ABCDEF"), signed)
	assert.Equal(t, []string{"Failed verifying revision " + signed + " by 'Fixture Author <author@example.com>': signed with unallowed key (key_id=" + sg.keyID + ")"},
		resp.SourceIntegrityResult.Checks[0].Problems)

	// Unsigned commit.
	unsigned := r.commit("unsigned", map[string]string{"app/cm2.yaml": configMap("cm2", nil)})
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), unsigned)
	assert.Equal(t, []string{"Failed verifying revision " + unsigned + " by 'Fixture Author <author@example.com>': unsigned (key_id=)"},
		resp.SourceIntegrityResult.Checks[0].Problems)
	assert.Equal(t, "Revision is not signed.", resp.VerifyResult)

	// A signed annotated tag on an unsigned commit passes (the tag is verified); a
	// lightweight tag verifies the commit.
	env := append(append([]string{}, r.env...), "GNUPGHOME="+sg.home)
	runGit(t, r.work, env, "-c", "gpg.program="+sg.gpg, "-c", "user.signingkey="+sg.keyID, "tag", "-s", "v-signed", "-m", "release", unsigned)
	r.git("tag", "v-light", unsigned)
	r.git("push", "--quiet", "--tags", "origin")
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), "v-signed")
	assert.Equal(t, unsigned, resp.Revision)
	assert.True(t, resp.SourceIntegrityResult.IsValid(), "%+v", resp.SourceIntegrityResult)
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), "v-light")
	assert.False(t, resp.SourceIntegrityResult.IsValid())

	// Strict mode fails closed; mode none and non-matching policies verify nothing.
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeStrict, sg.keyID), signed)
	assert.False(t, resp.SourceIntegrityResult.IsValid())
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeNone), unsigned)
	assert.Nil(t, resp.SourceIntegrityResult)
	other := policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)
	other.Git.Policies[0].Repos[0].URL = "https://elsewhere.example.com/*"
	assert.Nil(t, gen(other, unsigned).SourceIntegrityResult)

	// Ambiguous policies fail the request (upstream).
	two := policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)
	two.Git.Policies = append(two.Git.Policies, two.Git.Policies[0])
	req := dirRequest(r, "app", "app", signed)
	req.SourceIntegrity = two
	_, err := c.GenerateManifest(bg(), req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple (2) git source integrity policies found")

	// Revision metadata carries the result and the legacy signature line.
	md, err := c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: signed,
		SourceIntegrity: policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)})
	require.NoError(t, err)
	require.NotNil(t, md.SourceIntegrityResult)
	assert.True(t, md.SourceIntegrityResult.IsValid())
	assert.Contains(t, md.SignatureInfo, "Good signature")
	md, err = c.GetRevisionMetadata(bg(), &apiclient.RevisionMetadataRequest{Repo: r.repo(), Revision: unsigned, CheckSignature: true})
	require.NoError(t, err)
	assert.Equal(t, "Revision is not signed.", md.SignatureInfo)

	// ApplicationSet file reads fail when verification fails.
	_, err = c.GetGitFiles(bg(), &apiclient.GitFilesRequest{Repo: r.repo(), Revision: unsigned, Path: "app/*.yaml",
		SourceIntegrity: policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GIT/GPG: Failed verifying revision")
	assert.True(t, apiclient.IsCode(err, apiclient.CodePermissionDenied))
	files, err := c.GetGitFiles(bg(), &apiclient.GitFilesRequest{Repo: r.repo(), Revision: signed, Path: "app/*.yaml",
		SourceIntegrity: policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)})
	require.NoError(t, err)
	assert.Contains(t, files.Map, "app/cm.yaml")

	// Removing the key from the mounted directory revokes it.
	require.NoError(t, os.Remove(filepath.Join(keysDir, sg.keyID)))
	resp = gen(policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID), signed)
	assert.False(t, resp.SourceIntegrityResult.IsValid())

	// GnuPG disabled: nothing is verified.
	off := newTestService(t, func(c *Config) { c.DisableGPG = true; c.GPGKeysDir = keysDir })
	offResp, err := off.GenerateManifest(bg(), func() *apiclient.ManifestRequest {
		req := dirRequest(r, "app", "app", unsigned)
		req.SourceIntegrity = policy(v1alpha1.SourceIntegrityGitPolicyGPGModeHead, sg.keyID)
		return req
	}())
	require.NoError(t, err)
	assert.Nil(t, offResp.SourceIntegrityResult)
}
