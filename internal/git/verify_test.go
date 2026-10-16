package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gpgHome creates a short GNUPGHOME (gpg-agent socket paths are length limited)
// and stops its agent at cleanup.
func gpgHome(t *testing.T, gpg string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pgpg")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o700))
	t.Cleanup(func() {
		if gpgconf, err := exec.LookPath("gpgconf"); err == nil {
			cmd := exec.Command(gpgconf, "--kill", "gpg-agent")
			cmd.Env = []string{"GNUPGHOME=" + dir, "PATH=" + os.Getenv("PATH")}
			_ = cmd.Run()
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

func runGPG(t *testing.T, gpg, home string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gpg, append([]string{"--batch", "--no-tty", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
	cmd.Env = []string{"GNUPGHOME=" + home, "PATH=" + os.Getenv("PATH"), "HOME=" + home}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		require.NoError(t, err, "gpg %v: %s", args, ee.Stderr)
	}
	require.NoError(t, err)
	return string(out)
}

func longKeyIDOf(t *testing.T, gpg, home string) string {
	t.Helper()
	for _, line := range strings.Split(runGPG(t, gpg, home, "", "--list-keys", "--with-colons"), "\n") {
		f := strings.Split(line, ":")
		if len(f) > 4 && f[0] == "pub" {
			return strings.ToUpper(f[4])
		}
	}
	t.Fatal("no public key found")
	return ""
}

func TestVerifyCommitSignature(t *testing.T) {
	t.Parallel()
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		t.Skip("gpg not available")
	}
	signHome := gpgHome(t, gpg)
	runGPG(t, gpg, signHome, "", "--quick-gen-key", "Test Signer <signer@example.com>", "ed25519", "sign", "never")
	keyID := longKeyIDOf(t, gpg, signHome)

	// Verification keyring with only the public key (no owner trust).
	verifyHome := gpgHome(t, gpg)
	pub := runGPG(t, gpg, signHome, "", "--armor", "--export", keyID)
	runGPG(t, gpg, verifyHome, pub, "--import")
	emptyHome := gpgHome(t, gpg)

	f := newRemoteFixture(t)
	signEnv := append(f.env[:len(f.env):len(f.env)], "GNUPGHOME="+signHome)
	unsigned := f.commit("unsigned", map[string]string{"a": "1\n"})
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "b"), []byte("2\n"), 0o644))
	runGit(t, f.work, signEnv, "add", "b")
	runGit(t, f.work, signEnv, "-c", "gpg.program="+gpg, "-c", "user.signingkey="+keyID, "commit", "--quiet", "-S", "-m", "signed")
	signed := f.git("rev-parse", "HEAD")
	runGit(t, f.work, signEnv, "-c", "gpg.program="+gpg, "-c", "user.signingkey="+keyID, "tag", "-s", "v1.0.0", "-m", "signed tag")
	f.git("tag", "-a", "v0.9.0", "-m", "unsigned tag", unsigned)
	f.push("main", "--tags")

	ctx := testCtx(t)
	c := newTestClient(t, f.url(), nil, GPGProgram(gpg))
	require.NoError(t, c.CheckoutRevision(ctx, "main", false))

	t.Run("good signature", func(t *testing.T) {
		got, err := c.VerifyCommitSignature(ctx, signed, verifyHome)
		require.NoError(t, err)
		assert.Equal(t, keyID, got)
		info, err := c.VerifySignature(ctx, signed, verifyHome)
		require.NoError(t, err)
		assert.Equal(t, SignatureGood, info.Result)
		assert.Contains(t, info.Signer, "Test Signer")
		assert.Len(t, info.Fingerprint, 40)
		assert.True(t, strings.HasSuffix(info.PrimaryFingerprint, keyID))
	})
	t.Run("signed annotated tag", func(t *testing.T) {
		got, err := c.VerifyCommitSignature(ctx, "v1.0.0", verifyHome)
		require.NoError(t, err)
		assert.Equal(t, keyID, got)
	})
	t.Run("unsigned commit", func(t *testing.T) {
		_, err := c.VerifyCommitSignature(ctx, unsigned, verifyHome)
		assert.ErrorIs(t, err, ErrUnsigned)
	})
	t.Run("unsigned annotated tag", func(t *testing.T) {
		_, err := c.VerifyCommitSignature(ctx, "v0.9.0", verifyHome)
		assert.ErrorIs(t, err, ErrUnsigned)
	})
	t.Run("key not in keyring", func(t *testing.T) {
		_, err := c.VerifyCommitSignature(ctx, signed, emptyHome)
		require.ErrorIs(t, err, ErrSignatureInvalid)
		var se *SignatureError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, SignatureMissingKey, se.Info.Result)
		assert.Equal(t, keyID, se.Info.KeyID)
	})
	t.Run("invalid arguments", func(t *testing.T) {
		_, err := c.VerifyCommitSignature(ctx, "--help", verifyHome)
		assert.ErrorIs(t, err, ErrInvalidRevision)
		_, err = c.VerifyCommitSignature(ctx, signed, "relative/home")
		assert.Error(t, err)
		_, err = c.VerifyCommitSignature(ctx, "doesnotexist", verifyHome)
		assert.ErrorIs(t, err, ErrRevisionNotFound)
	})
	t.Run("revision metadata carries signature info", func(t *testing.T) {
		mc, err := New(f.url(), c.Root(), nil, AllowLocalRepositories(true), GPGProgram(gpg), GnuPGHome(verifyHome))
		require.NoError(t, err)
		m, err := mc.RevisionMetadata(ctx, signed)
		require.NoError(t, err)
		require.NotNil(t, m.Signature)
		assert.Equal(t, SignatureGood, m.Signature.Result)
		assert.Equal(t, keyID, m.Signature.KeyID)
		m, err = mc.RevisionMetadata(ctx, unsigned)
		require.NoError(t, err)
		require.NotNil(t, m.Signature)
		assert.Equal(t, SignatureUnsigned, m.Signature.Result)
	})
}

func TestParseGPGStatus(t *testing.T) {
	t.Parallel()
	const fpr = "0123456789ABCDEF0123456789ABCDEF01234567"
	tests := []struct {
		name string
		in   string
		want SignatureInfo
		ok   bool
	}{
		{"good", "[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG 89ABCDEF01234567 Alice <a@example.com>\n" +
			"[GNUPG:] VALIDSIG " + fpr + " 2024-01-01 1704067200 0 4 0 22 10 00 " + fpr + "\n[GNUPG:] TRUST_ULTIMATE 0 pgp\n",
			SignatureInfo{Result: SignatureGood, KeyID: "89ABCDEF01234567", Signer: "Alice <a@example.com>", Fingerprint: fpr, PrimaryFingerprint: fpr, Trust: "ULTIMATE"}, true},
		{"bad", "[GNUPG:] BADSIG 89abcdef01234567 Mallory\n", SignatureInfo{Result: SignatureBad, KeyID: "89ABCDEF01234567", Signer: "Mallory"}, true},
		{"expired sig", "[GNUPG:] EXPSIG 89ABCDEF01234567 A\n", SignatureInfo{Result: SignatureExpired, KeyID: "89ABCDEF01234567", Signer: "A"}, true},
		{"expired key", "[GNUPG:] EXPKEYSIG 89ABCDEF01234567 A\n", SignatureInfo{Result: SignatureExpiredKey, KeyID: "89ABCDEF01234567", Signer: "A"}, true},
		{"revoked", "[GNUPG:] REVKEYSIG 89ABCDEF01234567 A\n", SignatureInfo{Result: SignatureRevokedKey, KeyID: "89ABCDEF01234567", Signer: "A"}, true},
		{"missing key", "[GNUPG:] ERRSIG 89ABCDEF01234567 22 10 00 1704067200 9 -\n[GNUPG:] NO_PUBKEY 89ABCDEF01234567\n",
			SignatureInfo{Result: SignatureMissingKey, KeyID: "89ABCDEF01234567"}, true},
		{"first signature wins", "[GNUPG:] GOODSIG 1111111111111111 A\n[GNUPG:] BADSIG 2222222222222222 B\n",
			SignatureInfo{Result: SignatureGood, KeyID: "1111111111111111", Signer: "A"}, true},
		{"no status", "gpg: Signature made ...\nerror: no signature found\n", SignatureInfo{}, false},
		{"empty", "", SignatureInfo{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseGPGStatus(tt.in)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
	// Bounded parsing: a status line after maxStatusLines is ignored.
	long := strings.Repeat("[GNUPG:] NOTATION_DATA x\n", maxStatusLines) + "[GNUPG:] GOODSIG 1111111111111111 A\n"
	_, ok := parseGPGStatus(long)
	assert.False(t, ok)
}
