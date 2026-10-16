package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/juliosaraiva/patrol-cd/internal/rbac"
)

// TestMissingConfigMapKeepsLastLoadedSettings: deleting or unlabelling patrol-cm must not
// turn the snapshot into defaults (which would re-enable a disabled admin and drop
// resource.exclusions). Upstream GetSettings fails and subscribers keep the last settings;
// GetAccounts fails, so local logins fail meanwhile.
func TestMissingConfigMapKeepsLastLoadedSettings(t *testing.T) {
	t.Parallel()
	cm := map[string]string{
		KeyAdminEnabled:       "false",
		"accounts.alice":      "login",
		KeyURL:                "https://patrol.example.com",
		KeyResourceExclusions: "- apiGroups: ['']\n  kinds: [Secret]\n",
	}
	sec := baseSecretData()
	sec[SecretKeyAdminPassword] = "$2a$10$hash"
	secret := func() *corev1.Secret { return newSecret(DefaultSecretName, sec, true) }
	prev := Parse(in(cm, secret()))
	require.Equal(t, SourceStateLoaded, prev.SourceState(SourceConfigMap))
	require.False(t, prev.Accounts()["admin"].Enabled)
	require.True(t, prev.Accounts()["alice"].Enabled)

	unlabelled := newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://other.example.com"})
	unlabelled.Labels = nil
	for name, tc := range map[string]struct {
		input Input
		msg   string
	}{
		"deleted":    {in(nil, secret()), "ConfigMap not found; keeping the settings last loaded from it (resourceVersion 1) until it is restored; local accounts are disabled meanwhile"},
		"unlabelled": {in(nil, unlabelled, secret()), "ConfigMap is ignored because it is not labelled app.kubernetes.io/part-of=patrol-cd; keeping the settings last loaded from it"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := Parse(tc.input, WithPrevious(prev))
			assert.Equal(t, SourceStateRetained, s.SourceState(SourceConfigMap))
			assert.False(t, s.Accounts()["admin"].Enabled, "a disabled admin is never re-enabled")
			assert.False(t, s.Accounts()["alice"].Enabled, "local accounts are disabled while patrol-cm is unavailable")
			assert.True(t, s.UserLoginsDisabled())
			assert.Equal(t, "https://patrol.example.com", s.URL())
			assert.Equal(t, prev.ResourceFilter(), s.ResourceFilter())
			assert.True(t, s.IsExcludedResource("", "Secret", "https://kubernetes.default.svc"))
			requireIssue(t, s, DefaultConfigMapName, "", SeverityError, tc.msg)
			assert.NotEqual(t, prev.Fingerprint(), s.Fingerprint(), "the state change is published")

			again := Parse(tc.input, WithPrevious(s))
			assert.Equal(t, SourceStateRetained, again.SourceState(SourceConfigMap))
			assert.Equal(t, "https://patrol.example.com", again.URL(), "retained across reloads")
			assert.False(t, again.Accounts()["admin"].Enabled)
			assert.Equal(t, s.Fingerprint(), again.Fingerprint())

			restored := Parse(in(map[string]string{KeyURL: "https://restored.example.com"}, secret()), WithPrevious(again))
			assert.Equal(t, SourceStateLoaded, restored.SourceState(SourceConfigMap))
			assert.Equal(t, "https://restored.example.com", restored.URL())
			assert.True(t, restored.Accounts()["admin"].Enabled, "the restored ConfigMap is authoritative")
			assert.False(t, restored.IsExcludedResource("", "Secret", "https://kubernetes.default.svc"))
			assert.Empty(t, issuesFor(restored, DefaultConfigMapName, ""))
		})
	}
}

// TestMissingConfigMapWithoutHistoryFailsClosed: without a previously loaded patrol-cm the
// account settings are unknown, so local accounts are disabled (upstream GetAccounts fails).
func TestMissingConfigMapWithoutHistoryFailsClosed(t *testing.T) {
	t.Parallel()
	sec := baseSecretData()
	sec[SecretKeyAdminPassword] = "$2a$10$hash"
	s := Parse(in(nil, newSecret(DefaultSecretName, sec, true)))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceConfigMap))
	assert.False(t, s.Accounts()["admin"].Enabled)
	assert.True(t, s.UserLoginsDisabled())
	assert.Equal(t, DefaultUserSessionDuration, s.UserSessionDuration(), "other keys use their defaults")
	requireIssue(t, s, DefaultConfigMapName, "", SeverityError, "ConfigMap not found; local accounts are disabled and every other key uses its default")

	// A snapshot without history does not make a later one retain anything.
	next := Parse(in(nil, newSecret(DefaultSecretName, sec, true)), WithPrevious(s))
	assert.Equal(t, SourceStateAbsent, next.SourceState(SourceConfigMap))
	assert.Equal(t, s.Fingerprint(), next.Fingerprint())
}

// TestMissingSecretKeepsLastLoadedSecrets: deleting patrol-secret must not clear the webhook
// secrets (an empty secret disables signature checks), the TLS pair, the server key or the
// "$<key>" reference targets.
func TestMissingSecretKeepsLastLoadedSecrets(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM := selfSignedPEM(t)
	d := baseSecretData()
	d[SecretKeyWebhookGitHub] = "gh"
	d[SecretKeyWebhookGitLab] = "$gitlab.token"
	d["gitlab.token"] = "gl"
	d[SecretKeyWebhookGogs] = "gogs"
	d[SecretKeyTLSCert], d[SecretKeyTLSKey] = string(certPEM), string(keyPEM)
	d[SecretKeyAdminPassword] = "$2a$10$hash"
	d["oidc.secret"] = "s3cr3t"
	cm := map[string]string{KeyOIDCConfig: "issuer: https://idp.example.com\nclientID: c\nclientSecret: $oidc.secret\n"}
	prev := Parse(in(cm, newSecret(DefaultSecretName, d, true)))
	require.Equal(t, SourceStateLoaded, prev.SourceState(SourceSecret))
	require.True(t, prev.Accounts()["admin"].Enabled)
	require.True(t, prev.Webhook().SecretsLoaded)

	withoutSecret := Input{Namespace: testNamespace, ConfigMaps: []*corev1.ConfigMap{newCM(DefaultConfigMapName, cm)}}
	s := Parse(withoutSecret, WithPrevious(prev))
	assert.Equal(t, SourceStateRetained, s.SourceState(SourceSecret))
	wh := s.Webhook()
	assert.True(t, wh.SecretsLoaded)
	assert.Equal(t, "gh", wh.GitHubSecret.Reveal(), "an absent patrol-secret must not turn signature checks off")
	assert.Equal(t, "gl", wh.GitLabSecret.Reveal())
	assert.Equal(t, "gogs", wh.GogsSecret.Reveal())
	assert.Equal(t, testServerKey, s.ServerSecretKey().Reveal())
	assert.NotNil(t, s.ServerCertificate())
	v, err := s.ResolveSecretRef("$oidc.secret")
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", v)
	assert.Equal(t, "s3cr3t", s.OIDCConfig().ClientSecret.Reveal())
	assert.False(t, s.Accounts()["admin"].Enabled, "local accounts are disabled while patrol-secret is unavailable")
	requireIssue(t, s, DefaultSecretName, "", SeverityError, "Secret not found; keeping the settings last loaded from it (resourceVersion 1) until it is restored; local accounts are disabled meanwhile")
	for _, is := range s.Validate() {
		for _, secretValue := range []string{"gh", "gl", "s3cr3t", testServerKey} {
			assert.NotContains(t, is.Message, secretValue, is.String())
		}
	}

	again := Parse(withoutSecret, WithPrevious(s))
	assert.Equal(t, "gh", again.Webhook().GitHubSecret.Reveal(), "retained across reloads")

	none := Parse(withoutSecret)
	assert.Equal(t, SourceStateAbsent, none.SourceState(SourceSecret))
	assert.False(t, none.Webhook().SecretsLoaded, "never loaded: handlers must reject payloads")
	assert.True(t, none.UserLoginsDisabled())
	requireIssue(t, none, DefaultSecretName, SecretKeyServerSecretKey, SeverityError, `Secret "patrol-secret" not found`)
	requireIssue(t, none, DefaultSecretName, "", SeverityError, "Secret not found; local accounts are disabled")

	restored := Parse(in(cm, newSecret(DefaultSecretName, baseSecretData(), true)), WithPrevious(again))
	assert.Equal(t, SourceStateLoaded, restored.SourceState(SourceSecret))
	assert.True(t, restored.Webhook().GitHubSecret.IsZero(), "the restored Secret is authoritative")
	assert.True(t, restored.Accounts()["admin"].Enabled)
	assert.Empty(t, issuesFor(restored, DefaultSecretName, ""))
}

// TestMissingRBACConfigMapKeepsLastPolicy: upstream's RBAC informer has no delete handler, so
// the last policy stays in force; reverting to the built-in policy would drop deny rules.
func TestMissingRBACConfigMapKeepsLastPolicy(t *testing.T) {
	t.Parallel()
	rbacCM := newCM(DefaultRBACConfigMapName, map[string]string{
		RBACKeyPolicyCSV:     "p, role:admin, applications, delete, */*, deny\n",
		RBACKeyPolicyDefault: "role:readonly",
	})
	prev := Parse(in(map[string]string{}, rbacCM))
	s := Parse(in(map[string]string{}), WithPrevious(prev))
	assert.Equal(t, SourceStateRetained, s.SourceState(SourceRBACConfigMap))
	assert.Equal(t, prev.RBAC(), s.RBAC())
	requireIssue(t, s, DefaultRBACConfigMapName, "", SeverityError, "ConfigMap not found; keeping the settings last loaded from it (resourceVersion 1) until it is restored")
	assert.True(t, s.Accounts()["admin"].Enabled, "RBAC retention does not affect accounts")

	r := s.RBAC()
	enf := rbac.NewEnforcer()
	require.NoError(t, enf.SetPolicySources(rbac.BuiltinPolicy(), r.Sources, r.DefaultRole, r.MatchMode))
	assert.False(t, enf.Enforce(rbac.Subjects{User: "admin"}, "applications", "delete", "p/a").Allowed, "the deny rule stays in force")

	none := Parse(in(map[string]string{}))
	assert.Equal(t, SourceStateAbsent, none.SourceState(SourceRBACConfigMap))
	assert.False(t, none.RBAC().Present, "without history the built-in policy applies")
	assert.Empty(t, issuesFor(none, DefaultRBACConfigMapName, ""), "patrol-rbac-cm is optional")
}

// TestRetentionRequiresSameObject: data is only retained for the same object name and
// namespace.
func TestRetentionRequiresSameObject(t *testing.T) {
	t.Parallel()
	prev := Parse(in(map[string]string{KeyURL: "https://a"}))
	other := in(nil)
	other.Names = Names{ConfigMap: "renamed-cm"}
	s := Parse(other, WithPrevious(prev))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceConfigMap))
	assert.Equal(t, "", s.URL())

	elsewhere := in(nil)
	elsewhere.Namespace = "elsewhere"
	elsewhere.Secrets = nil
	s = Parse(elsewhere, WithPrevious(prev))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceConfigMap))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceSecret))
}

func TestSourceStateOfOptionalSources(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{}, newCM(DefaultGPGKeysConfigMapName, map[string]string{})))
	assert.Equal(t, SourceStateLoaded, s.SourceState(SourceGPGKeysConfigMap))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceTLSCertsConfigMap))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceNotificationsSecret))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceReferencedSecret))
	prev := s
	s = Parse(in(map[string]string{}), WithPrevious(prev))
	assert.Equal(t, SourceStateAbsent, s.SourceState(SourceGPGKeysConfigMap), "only patrol-cm, patrol-rbac-cm and patrol-secret are retained")
}
