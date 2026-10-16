package settings

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	authoidc "github.com/juliosaraiva/patrol-cd/internal/auth/oidc"
)

func TestOIDCConfigEveryKey(t *testing.T) {
	t.Parallel()
	certPEM, _ := selfSignedPEM(t)
	rootCA := strings.ReplaceAll(strings.TrimSpace(string(certPEM)), "\n", "\n  ")
	cm := map[string]string{
		KeyURL: "https://cd.example.com",
		KeyOIDCConfig: `name: Okta
issuer: https://idp.example.com
clientID: patrol
clientSecret: $oidc.okta.clientSecret
cliClientID: patrol-cli
enableUserInfoGroups: true
userInfoBaseURL: https://idp.example.com
userInfoPath: /userinfo
userInfoCacheExpiration: 5m
requestedScopes: [openid, profile, email, groups]
requestedIDTokenClaims:
  groups:
    essential: true
    values: [a, b]
logoutURL: https://idp.example.com/logout?id_token_hint={{token}}
rootCA: |
  ` + rootCA + `
enablePKCEAuthentication: true
domainHint: example.com
refreshTokenThreshold: 2m
azure:
  useWorkloadIdentity: true
  enableUserGroupOverageClaim: true
  graphApiEndpoint: https://graph.microsoft.us/v1.0
  userGroupOverageClaimCacheExpiration: 10m
allowedAudiences: [patrol, other]
skipAudienceCheckWhenTokenHasNoAudience: true
`,
		KeyOIDCTLSInsecureSkip: "true",
	}
	sec := baseSecretData()
	sec["oidc.okta.clientSecret"] = "okta-secret"
	s := Parse(in(cm, newSecret(DefaultSecretName, sec, true)))
	requireNoIssues(t, s, DefaultConfigMapName)

	c := s.OIDCConfig()
	require.NotNil(t, c)
	assert.Equal(t, "Okta", c.Name)
	assert.Equal(t, "https://idp.example.com", c.Issuer)
	assert.Equal(t, "patrol", c.ClientID)
	assert.Equal(t, "okta-secret", c.ClientSecret.Reveal())
	assert.Equal(t, "patrol-cli", c.CLIClientID)
	assert.True(t, c.EnableUserInfoGroups)
	assert.Equal(t, "https://idp.example.com", c.UserInfoBaseURL)
	assert.Equal(t, "/userinfo", c.UserInfoPath)
	assert.Equal(t, 5*time.Minute, c.UserInfoCacheExpiration)
	assert.Equal(t, []string{"openid", "profile", "email", "groups"}, c.RequestedScopes)
	require.Contains(t, c.RequestedIDTokenClaims, "groups")
	assert.Equal(t, &authoidc.Claim{Essential: true, Values: []string{"a", "b"}}, c.RequestedIDTokenClaims["groups"])
	assert.Contains(t, c.LogoutURL, "{{token}}")
	assert.Contains(t, c.RootCA, "BEGIN CERTIFICATE")
	assert.True(t, c.EnablePKCEAuthentication)
	assert.Equal(t, "example.com", c.DomainHint)
	assert.Equal(t, 2*time.Minute, c.RefreshTokenThreshold)
	assert.Equal(t, &AzureOIDCConfig{UseWorkloadIdentity: true, EnableUserGroupOverageClaim: true, GraphAPIEndpoint: "https://graph.microsoft.us/v1.0", UserGroupOverageClaimCacheExpiration: 10 * time.Minute}, c.Azure)
	assert.Equal(t, []string{"patrol", "other"}, c.AllowedAudiences)
	require.NotNil(t, c.SkipAudienceCheckWhenTokenHasNoAudience)
	assert.True(t, *c.SkipAudienceCheckWhenTokenHasNoAudience)
	assert.True(t, s.IsSSOConfigured())

	rp, ok, err := s.OIDCProviderConfig()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "okta-secret", rp.ClientSecret)
	assert.True(t, rp.InsecureSkipVerify, "oidc.tls.insecure.skip.verify")
	assert.True(t, rp.EnablePKCE)
	assert.Equal(t, []string{"patrol", "other"}, rp.AllowedAudiences())
	require.NoError(t, rp.Validate())
}

func TestOIDCConfigWarnings(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyOIDCConfig: `issuer: idp.example.com
clientID: patrol
userInfoBaseURL: ftp://x
userInfoCacheExpiration: often
rootCA: not a pem
azure:
  graphApiEndpoint: https://evil.example.com/v1.0
unknownField: 1
`}))
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, "issuer: URL must include http or https protocol")
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, "userInfoBaseURL: URL must include http or https protocol")
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, "Failed to parse 'oidc.config.userInfoCacheExpiration' key")
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, "failed to append certificates from PEM: proceeding without custom rootCA")
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, `graphApiEndpoint "https://evil.example.com/v1.0" hostname is not a known Microsoft Graph API host`)
	requireIssue(t, s, DefaultConfigMapName, KeyOIDCConfig, SeverityWarning, "unknownField")
	c := s.OIDCConfig()
	require.NotNil(t, c, "warnings do not disable OIDC")
	assert.Empty(t, c.RootCA)
	assert.Empty(t, c.Azure.GraphAPIEndpoint)
	assert.False(t, c.Azure.EnableUserGroupOverageClaim)
}

func TestAzureGraphDefaultEndpoint(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyOIDCConfig: "issuer: https://login.microsoftonline.com/t/v2.0\nclientID: c\nazure:\n  enableUserGroupOverageClaim: true\n"}))
	assert.Equal(t, DefaultAzureGraphAPIEndpoint, s.OIDCConfig().Azure.GraphAPIEndpoint)
}

func TestDexProviderConfig(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{KeyURL: "https://cd.example.com", KeyDexConfig: "connectors:\n- type: github\n  id: gh\n  name: GitHub\n"}))
	cfg, ok, err := s.OIDCProviderConfig()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "https://cd.example.com/api/dex", cfg.Issuer)
	assert.Equal(t, authoidc.DexClientID, cfg.ClientID)

	none := Parse(in(map[string]string{}))
	_, ok, err = none.OIDCProviderConfig()
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestOIDCPrecedenceOverDex(t *testing.T) {
	t.Parallel()
	s := Parse(in(map[string]string{
		KeyURL:        "https://cd.example.com",
		KeyDexConfig:  "connectors:\n- type: github\n  id: gh\n  name: GitHub\n",
		KeyOIDCConfig: "issuer: https://idp.example.com\nclientID: ext\n",
	}))
	cfg, ok, err := s.OIDCProviderConfig()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "ext", cfg.ClientID, "oidc.config wins for issuer/client (TEN-113)")
	assert.True(t, s.IsDexConfigured(), "Dex is still configured")
}
