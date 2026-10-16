package settings

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSecretRef(t *testing.T) {
	t.Parallel()
	unlabelled := newSecret("plain", map[string]string{"k": "v"}, false)
	s := Parse(in(map[string]string{},
		newSecret(DefaultSecretName, map[string]string{SecretKeyServerSecretKey: testServerKey, "oidc.secret": "  from-patrol-secret\n"}, false),
		newSecret("idp", map[string]string{"clientSecret": "from-labelled"}, true),
		unlabelled,
	))
	tests := []struct {
		in      string
		want    string
		wantErr error
		msg     string
	}{
		{in: "literal", want: "literal"},
		{in: "", want: ""},
		{in: "$", want: "$", msg: "a lone $ is not a reference"},
		{in: "$oidc.secret", want: "from-patrol-secret"},
		{in: "$idp:clientSecret", want: "from-labelled"},
		{in: "$missing", want: "$missing", wantErr: ErrSecretKeyNotFound},
		{in: "$idp:nope", want: "$idp:nope", wantErr: ErrSecretKeyNotFound},
		{in: "$ghost:key", want: "$ghost:key", wantErr: ErrSecretNotFound},
		{in: "$plain:k", want: "$plain:k", wantErr: ErrSecretNotFound, msg: "unlabelled Secrets are not resolvable (TEN-131)"},
		{in: "$patrol-secret:oidc.secret", want: "$patrol-secret:oidc.secret", wantErr: ErrSecretNotFound, msg: "patrol-secret is unlabelled in this fixture"},
	}
	for _, tc := range tests {
		got, err := s.ResolveSecretRef(tc.in)
		assert.Equal(t, tc.want, got, tc.in)
		if tc.wantErr == nil {
			assert.NoError(t, err, tc.in)
			continue
		}
		require.ErrorIs(t, err, tc.wantErr, tc.in)
		assert.NotContains(t, err.Error(), tc.in, "errors never quote the reference")
	}
}

func TestResolveSecretRefUnlabelledMessage(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		KeyOIDCConfig: "issuer: https://idp.example.com\nclientID: patrol\nclientSecret: $plain:k\n",
	}, newSecret("plain", map[string]string{"k": "v"}, false)))
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, `Secret "plain" is not labelled app.kubernetes.io/part-of=patrol-cd`)
	assert.Equal(t, "$plain:k", s.OIDCConfig().ClientSecret.Reveal(), "unresolved references are kept as-is (upstream)")
}

func TestServerSecretKey(t *testing.T) {
	t.Parallel()
	t.Run("present", func(t *testing.T) {
		t.Parallel()
		s := Parse(in(map[string]string{}))
		assert.Equal(t, testServerKey, s.ServerSecretKey().Reveal())
		ks, err := s.SigningKeys()
		require.NoError(t, err)
		assert.NotEmpty(t, ks.Current.ID())
	})
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		s := Parse(in(map[string]string{}, newSecret(DefaultSecretName, map[string]string{}, true)))
		requireIssue(t, s, DefaultSecretName, SecretKeyServerSecretKey, SeverityError, "server.secretkey is missing")
		assert.True(t, s.ServerSecretKey().IsZero())
		_, err := s.SigningKeys()
		assert.Error(t, err)
	})
	t.Run("missing secret", func(t *testing.T) {
		t.Parallel()
		s := Parse(Input{Namespace: testNamespace})
		requireIssue(t, s, DefaultSecretName, SecretKeyServerSecretKey, SeverityError, `Secret "patrol-secret" not found`)
	})
	t.Run("weak key keeps the previous one", func(t *testing.T) {
		t.Parallel()
		prev := Parse(in(map[string]string{}))
		s := Parse(in(map[string]string{}, newSecret(DefaultSecretName, map[string]string{SecretKeyServerSecretKey: "short"}, true)), WithPrevious(prev))
		requireIssue(t, s, DefaultSecretName, SecretKeyServerSecretKey, SeverityError, "shorter than 32 bytes")
		requireIssue(t, s, DefaultSecretName, SecretKeyServerSecretKey, SeverityWarning, "keeping the last valid server.secretkey")
		assert.Equal(t, testServerKey, s.ServerSecretKey().Reveal())
	})
	t.Run("previous key", func(t *testing.T) {
		t.Parallel()
		d := baseSecretData()
		d[SecretKeyServerSecretKeyPrevious] = strings.Repeat("p", 40)
		s := Parse(in(map[string]string{}, newSecret(DefaultSecretName, d, true)))
		ks, err := s.SigningKeys()
		require.NoError(t, err)
		assert.Len(t, ks.Previous, 1)
	})
}

func selfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	kd, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

func TestServerCertificate(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := selfSignedPEM(t)
	d := baseSecretData()
	d[SecretKeyTLSCert], d[SecretKeyTLSKey] = string(certPEM), string(keyPEM)
	good := Parse(in(map[string]string{}, newSecret(DefaultSecretName, d, true)))
	requireNoIssues(t, good, DefaultSecretName)
	require.NotNil(t, good.ServerCertificate())

	bad := baseSecretData()
	bad[SecretKeyTLSCert], bad[SecretKeyTLSKey] = string(certPEM), "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"
	s := Parse(in(map[string]string{}, newSecret(DefaultSecretName, bad, true)), WithPrevious(good))
	requireIssue(t, s, DefaultSecretName, SecretKeyTLSCert, SeverityError, "keeping the last valid certificate")
	assert.NotNil(t, s.ServerCertificate())
	for _, is := range s.Validate() {
		assert.NotContains(t, is.Message, "AAAA", "no key material in issues")
	}

	half := baseSecretData()
	half[SecretKeyTLSCert] = string(certPEM)
	s = Parse(in(map[string]string{}, newSecret(DefaultSecretName, half, true)))
	requireIssue(t, s, DefaultSecretName, SecretKeyTLSCert, SeverityWarning, "must be set together")
	assert.Nil(t, s.ServerCertificate())
}

func TestWebhookSecretReferences(t *testing.T) {
	t.Parallel()
	d := baseSecretData()
	d[SecretKeyWebhookGitHub] = "$hooks:github"
	d[SecretKeyWebhookGitLab] = "$gitlab.token"
	d["gitlab.token"] = "gl"
	d[SecretKeyWebhookBitbucketUUID] = "{uuid}"
	d[SecretKeyWebhookBitbucketServer] = "bbs"
	d[SecretKeyWebhookGogs] = "gogs"
	d[SecretKeyWebhookAzureDevOpsUser] = "user"
	d[SecretKeyWebhookAzureDevOpsPass] = "$hooks:missing"
	s := Parse(in(map[string]string{}, newSecret(DefaultSecretName, d, true), newSecret("hooks", map[string]string{"github": "gh\n"}, true)))
	wh := s.Webhook()
	assert.Equal(t, "gh", wh.GitHubSecret.Reveal())
	assert.Equal(t, "gl", wh.GitLabSecret.Reveal())
	assert.Equal(t, "{uuid}", wh.BitbucketUUID.Reveal())
	assert.Equal(t, "bbs", wh.BitbucketServerSecret.Reveal())
	assert.Equal(t, "gogs", wh.GogsSecret.Reveal())
	assert.Equal(t, "user", wh.AzureDevOpsUsername.Reveal())
	requireIssue(t, s, DefaultSecretName, SecretKeyWebhookAzureDevOpsPass, SeverityWarning, "secret key does not exist in secret")
}

func TestSecretValuesNeverLeak(t *testing.T) {
	t.Parallel()
	const canary = "PATROL_SETTINGS_CANARY_VALUE"
	d := baseSecretData()
	d[SecretKeyWebhookGitHub] = "$" + canary // looks like a reference, cannot be resolved
	d[SecretKeyWebhookGitLab] = canary
	d["accounts.alice.tokens"] = canary
	d["accounts.alice.passwordMtime"] = canary
	s := Parse(in(map[string]string{"accounts.alice": "apiKey", KeyOIDCConfig: "issuer: https://i\nclientID: c\nclientSecret: $" + canary + "\n"}, newSecret(DefaultSecretName, d, true)))
	require.NotEmpty(t, s.Validate())
	for _, is := range s.Validate() {
		assert.NotContains(t, is.Message, canary, is.String())
	}
	wh := s.Webhook()
	for _, out := range []string{fmt.Sprint(wh), fmt.Sprintf("%+v", wh), fmt.Sprintf("%#v", wh), fmt.Sprintf("%s", wh.GitLabSecret), fmt.Sprintf("%q", wh.GitLabSecret)} {
		assert.NotContains(t, out, canary)
	}
	b, err := json.Marshal(wh)
	require.NoError(t, err)
	assert.NotContains(t, string(b), canary)
	var logBuf strings.Builder
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("x", "webhook", wh.GitLabSecret, "oidc", s.OIDCConfig().ClientSecret)
	assert.NotContains(t, logBuf.String(), canary)
	assert.Equal(t, canary, wh.GitLabSecret.Reveal())
}

func TestSensitive(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", NewSensitive("").String())
	assert.Equal(t, "[redacted]", NewSensitive("x").String())
	assert.True(t, NewSensitive("a").Equal(NewSensitive("a")))
	assert.False(t, NewSensitive("a").Equal(NewSensitive("b")))
	assert.Equal(t, 3, NewSensitive("abc").Len())
}

func TestAccountsFromSettings(t *testing.T) {
	t.Parallel()
	d := baseSecretData()
	d["admin.password"] = "$2a$10$hash"
	d["admin.passwordMtime"] = "2026-01-02T03:04:05Z"
	d["accounts.ci.tokens"] = `[{"id":"t1","iat":1700000000}]`
	d["accounts.broken.passwordMtime"] = "yesterday"
	s := Parse(in(map[string]string{
		"accounts.ci":            "apiKey",
		"accounts.broken":        "login",
		"accounts.off":           "login",
		"accounts.off.enabled":   "false",
		"accounts.weird.enabled": "perhaps",
	}, newSecret(DefaultSecretName, d, true)))
	accts := s.Accounts()
	assert.Equal(t, "$2a$10$hash", accts["admin"].PasswordHash)
	assert.Len(t, accts["ci"].Tokens, 1)
	assert.False(t, accts["off"].Enabled)
	assert.False(t, accts["broken"].Enabled, "invalid passwordMtime disables the account (fail closed)")
	requireIssue(t, s, DefaultSecretName, "accounts.broken.passwordMtime", SeverityError, "fail-closed")
	requireIssue(t, s, DefaultConfigMapName, "accounts.weird.enabled", SeverityError, "accounts.weird.enabled")
}
