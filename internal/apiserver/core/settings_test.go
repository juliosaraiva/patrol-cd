package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const customizations = `apps/Deployment:
  health.lua: |
    hs = {}
    hs.status = "Healthy"
    return hs
  ignoreDifferences: |
    jsonPointers:
    - /spec/replicas
`

func settingsCM(extra map[string]string) map[string]string {
	cm := defaultCM()
	for k, v := range map[string]string{
		"ui.bannercontent":        "Maintenance tonight",
		"ui.bannerurl":            "https://status.example.com",
		"ui.bannerpermanent":      "true",
		"ui.cssurl":               "./custom/style.css",
		"passwordPattern":         "^.{10,64}$",
		"resource.customizations": customizations,
		"statusbadge.enabled":     "true",
		"help.chatUrl":            "https://chat.example.com",
		"ga.trackingid":           "UA-1",
		"kustomize.buildOptions":  "--enable-helm",
		"exec.enabled":            "true",
		"installationID":          "inst-1",
	} {
		cm[k] = v
	}
	for k, v := range extra {
		cm[k] = v
	}
	return cm
}

func TestSettingsPublicVersusAuthenticated(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := newEnv(t, envOptions{cm: settingsCM(map[string]string{"oidc.config": idp.oidcConfig("requestedIDTokenClaims: {groups: {essential: true}}\nenablePKCEAuthentication: true\n")})})
	ctx := context.Background()

	raw := func(c *apiclient.Client) string {
		resp, err := c.DoRaw(ctx, http.MethodGet, apiclient.PathSettings, nil, nil, "")
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return string(b)
	}

	anon := e.client("")
	pub, err := anon.GetSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, e.url, pub.URL)
	assert.True(t, pub.StatusBadgeEnabled)
	assert.True(t, pub.ExecEnabled)
	assert.True(t, pub.AppsInAnyNamespaceEnabled)
	assert.Equal(t, "./custom/style.css", pub.UICSSURL)
	assert.Equal(t, "inst-1", pub.InstallationID)
	assert.Equal(t, "annotation", pub.TrackingMethod)
	require.NotNil(t, pub.Help)
	assert.Equal(t, "Chat now!", pub.Help.ChatText)
	require.NotNil(t, pub.KustomizeOptions)
	assert.Equal(t, "--enable-helm", pub.KustomizeOptions.BuildOptions)
	require.NotNil(t, pub.OIDCConfig)
	assert.Equal(t, idp.srv.URL, pub.OIDCConfig.Issuer)
	assert.Equal(t, idp.cliID, pub.OIDCConfig.CLIClientID)
	assert.True(t, pub.OIDCConfig.EnablePKCEAuthentication)
	require.NotNil(t, pub.OIDCConfig.IDTokenClaims["groups"])
	assert.True(t, pub.OIDCConfig.IDTokenClaims["groups"].Essential)
	assert.False(t, pub.UserLoginsDisabled)
	// Logged-in-only fields.
	assert.Empty(t, pub.UIBannerContent)
	assert.Empty(t, pub.ControllerNamespace)
	assert.Nil(t, pub.ResourceOverrides)
	assert.Empty(t, pub.PasswordPattern)
	body := raw(anon)
	assert.NotContains(t, body, idp.secret, "the OIDC client secret is never returned")
	assert.NotContains(t, body, "server.secretkey")

	full, err := e.client("dave").GetSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Maintenance tonight", full.UIBannerContent)
	assert.Equal(t, "https://status.example.com", full.UIBannerURL)
	assert.True(t, full.UIBannerPermanent)
	assert.Equal(t, testNamespace, full.ControllerNamespace)
	assert.Equal(t, "^.{10,64}$", full.PasswordPattern)
	require.Contains(t, full.ResourceOverrides, "apps/Deployment")
	ro := full.ResourceOverrides["apps/Deployment"]
	assert.Contains(t, ro.HealthLua, `hs.status = "Healthy"`)
	require.NotNil(t, ro.IgnoreDifferences)
	assert.Equal(t, []string{"/spec/replicas"}, ro.IgnoreDifferences.JSONPointers)
	assert.NotContains(t, raw(e.client("dave")), idp.secret)
}

func TestSettingsLoginsDisabledAndAuthDisabled(t *testing.T) {
	t.Parallel()
	cm := map[string]string{"admin.enabled": "false", "accounts.ci": "apiKey", "ui.bannercontent": "hello", "users.anonymous.enabled": "true"}
	e := newEnv(t, envOptions{cm: cm, disableAuth: true})
	s, err := e.client("").GetSettings(context.Background())
	require.NoError(t, err)
	assert.True(t, s.UserLoginsDisabled)
	assert.True(t, s.AnonymousEnabled)
	assert.Equal(t, "hello", s.UIBannerContent, "authentication disabled: the logged-in subset is returned")
	assert.Empty(t, s.PasswordPattern, "the password pattern is for logged-in callers only")
}

func TestSettingsDexConnectors(t *testing.T) {
	t.Parallel()
	cm := defaultCM()
	cm["dex.config"] = "connectors:\n- type: github\n  id: github\n  name: GitHub\n  config:\n    clientID: x\n    clientSecret: very-secret\n"
	e := newEnv(t, envOptions{cm: cm})
	s, err := e.client("").GetSettings(context.Background())
	require.NoError(t, err)
	require.NotNil(t, s.DexConfig)
	assert.Equal(t, []apiclient.DexConnector{{Name: "GitHub", Type: "github"}}, s.DexConfig.Connectors)
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "very-secret")
}

func TestPluginSettings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, envOptions{repo: &fakeRepo{plugins: []string{"kasane", "argocd-vault-plugin"}}})
	_, err := e.client("").GetPluginSettings(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
	ps, err := e.client("dave").GetPluginSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, []apiclient.Plugin{{Name: "argocd-vault-plugin"}, {Name: "kasane"}}, ps.Plugins)

	e = newEnv(t, envOptions{repo: &fakeRepo{err: errors.New("dial tcp: connection refused")}})
	_, err = e.client("dave").GetPluginSettings(ctx)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "failed to list sidecar plugins")
	assert.NotContains(t, ae.Problem.Detail, "dial tcp")

	e = newEnv(t, envOptions{})
	_, err = e.client("dave").GetPluginSettings(ctx)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr(t, err).StatusCode)
}

func TestVersion(t *testing.T) {
	t.Parallel()
	calls := 0
	vi := &core.VersionInfo{
		Version: "v1.2.3", BuildDate: "2026-10-16T00:00:00Z", GitCommit: "abc123", GitTreeState: "clean",
		ExtraBuildInfo: map[string]string{"distro": "test"},
		Tools: func(context.Context) core.ToolVersions {
			calls++
			return core.ToolVersions{Helm: "v3.19.0", Kustomize: "v5.8.1", Kubectl: "v0.36.1", Jsonnet: "v0.22.0"}
		},
	}
	e := newEnv(t, envOptions{version: vi})
	ctx := context.Background()

	v, err := e.client("").Version(ctx)
	require.NoError(t, err)
	assert.Equal(t, &apiclient.Version{Version: "v1.2.3"}, v, "unauthenticated callers only get the version")
	assert.Zero(t, calls, "tool versions are computed lazily")

	full := &apiclient.Version{
		Version: "v1.2.3", BuildDate: "2026-10-16T00:00:00Z", GitCommit: "abc123", GitTreeState: "clean",
		GoVersion: runtime.Version(), Compiler: runtime.Compiler, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		HelmVersion: "v3.19.0", KustomizeVersion: "v5.8.1", KubectlVersion: "v0.36.1", JsonnetVersion: "v0.22.0",
		ExtraBuildInfo: map[string]string{"distro": "test"},
	}
	c := e.client("dave")
	v, err = c.Version(ctx)
	require.NoError(t, err)
	assert.Equal(t, full, v)
	var alias apiclient.Version
	require.NoError(t, c.Do(ctx, http.MethodGet, apiclient.PathVersionAlias, nil, &alias))
	assert.Equal(t, full, &alias)
	assert.Equal(t, 1, calls, "tool versions are cached")

	e.holder.setCM(func(cm map[string]string) { cm["users.anonymous.enabled"] = "true" })
	v, err = e.client("").Version(ctx)
	require.NoError(t, err)
	assert.Equal(t, full, v, "anonymous access reveals the full version")
}

func TestBuildVersionInfo(t *testing.T) {
	t.Parallel()
	vi := core.BuildVersionInfo()
	assert.NotEmpty(t, vi.Version)
	tools := core.ExecToolVersions("/nonexistent/helm", "/nonexistent/kustomize")(context.Background())
	assert.Contains(t, tools.Helm, "/nonexistent/helm", "tool errors are reported as the version, as upstream")
	assert.Contains(t, tools.Kustomize, "/nonexistent/kustomize")
}
