package notifications_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/internal/miscapitest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/notifications"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	slackToken = "xoxb-super-secret-slack-token"
	hookURL    = "https://hooks.internal.example/notify"
)

// configured is a notifications ConfigMap with two services (one using a Secret
// reference), one trigger with descriptions and one template.
func configured() (map[string]string, map[string]string) {
	cm := map[string]string{
		"service.slack":        "token: $slack-token\n",
		"service.webhook.test": "url: " + hookURL + "\nheaders:\n- name: Authorization\n  value: Bearer $hook-token\n",
		"trigger.on-custom": "- when: app.status.sync.status == 'Synced'\n  description: Custom synced\n  send: [custom]\n" +
			"- when: app.status.health.status == 'Degraded'\n  description: Custom degraded\n  send: [custom]\n" +
			"- when: 'false'\n  description: Custom synced\n  send: [custom]\n",
		"template.custom": "message: Application {{.app.metadata.name}} changed\n",
	}
	secret := map[string]string{"slack-token": slackToken, "hook-token": "hook-secret-value"}
	return cm, secret
}

func newEnv(t *testing.T, cfg notifications.Config, anonymous bool) *miscapitest.Env {
	t.Helper()
	cm := miscapitest.Accounts("alice", "nobody")
	if anonymous {
		cm["users.anonymous.enabled"] = "true"
	}
	return miscapitest.New(t, miscapitest.Options{
		CM:   cm,
		RBAC: miscapitest.Policy("g, alice, role:readonly"),
	}, func(env *miscapitest.Env) []miscapitest.Registrar {
		return []miscapitest.Registrar{notifications.New(notifications.Deps{Config: cfg, Settings: env.Settings})}
	})
}

func names[T any](items []T, name func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, name(it))
	}
	return out
}

var catalogTriggers = []string{"on-created", "on-deleted", "on-deployed", "on-health-degraded", "on-sync-failed", "on-sync-running", "on-sync-status-unknown", "on-sync-succeeded"}

func TestListsConfiguredNamesWithCatalog(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{}, false)
	env.Settings.SetNotifications(configured())
	ctx := context.Background()
	c := env.Login("nobody") // authenticated, no RBAC role at all

	svcs, err := c.ListNotificationServices(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"slack", "test"}, names(svcs.Items, func(s apiclient.NotificationService) string { return s.Name }))

	trs, err := c.ListNotificationTriggers(ctx)
	require.NoError(t, err)
	want := append([]string{"on-custom"}, catalogTriggers...)
	slices.Sort(want)
	require.Equal(t, want, names(trs.Items, func(s apiclient.NotificationTrigger) string { return s.Name }))
	for _, tr := range trs.Items {
		switch tr.Name {
		case "on-custom":
			require.Equal(t, "Custom synced; Custom degraded", tr.Description)
		case "on-created":
			require.Equal(t, "Application is created.", tr.Description)
		}
	}

	tps, err := c.ListNotificationTemplates(ctx)
	require.NoError(t, err)
	got := names(tps.Items, func(s apiclient.NotificationTemplate) string { return s.Name })
	require.Contains(t, got, "custom")
	require.Contains(t, got, "app-created")
	require.Len(t, got, 9)
}

func TestNeverReturnsServiceConfigurationOrSecrets(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{}, false)
	env.Settings.SetNotifications(configured())
	for _, path := range []string{apiclient.PathNotificationServices, apiclient.PathNotificationTriggers, apiclient.PathNotificationTemplates} {
		req, err := http.NewRequest(http.MethodGet, env.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+env.LoginToken("alice"))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, path)
		for _, leak := range []string{slackToken, "hook-secret-value", "placeholder", hookURL, "$slack-token", "Authorization", "{{"} {
			require.NotContains(t, string(body), leak, "%s leaks %q", path, leak)
		}
	}
}

func TestDisableDefaultCatalog(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{DisableDefaultCatalog: true}, false)
	env.Settings.SetNotifications(configured())
	c := env.Login("alice")
	trs, err := c.ListNotificationTriggers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationTrigger{{Name: "on-custom", Description: "Custom synced; Custom degraded"}}, trs.Items)
	tps, err := c.ListNotificationTemplates(context.Background())
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationTemplate{{Name: "custom"}}, tps.Items)
}

func TestMissingConfigMapIsEmpty(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{}, false)
	c := env.Login("alice")
	ctx := context.Background()
	svcs, err := c.ListNotificationServices(ctx)
	require.NoError(t, err)
	require.NotNil(t, svcs.Items)
	require.Empty(t, svcs.Items)
	trs, err := c.ListNotificationTriggers(ctx)
	require.NoError(t, err)
	require.Empty(t, trs.Items)
	tps, err := c.ListNotificationTemplates(ctx)
	require.NoError(t, err)
	require.Empty(t, tps.Items)
}

func TestInvalidConfigurationIs500AndRecovers(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{}, false)
	env.Settings.SetNotifications(map[string]string{
		"service.slack":   "token: secret-in-the-config\n",
		"trigger.broken":  "- when: [unterminated\n  send: {{{",
		"template.custom": "message: hi\n",
	}, nil)
	c := env.Login("alice")
	ctx := context.Background()
	for _, call := range []func() error{
		func() error { _, err := c.ListNotificationServices(ctx); return err },
		func() error { _, err := c.ListNotificationTriggers(ctx); return err },
		func() error { _, err := c.ListNotificationTemplates(ctx); return err },
	} {
		err := call()
		var ae *apiclient.APIError
		require.ErrorAs(t, err, &ae)
		require.Equal(t, http.StatusInternalServerError, ae.StatusCode)
		require.Equal(t, apiclient.CodeInternal, ae.Code())
		require.NotContains(t, ae.Problem.Detail, "secret-in-the-config")
		require.NotContains(t, ae.Problem.Detail, "unterminated")
	}
	// Fixing the ConfigMap is picked up without restart.
	env.Settings.SetNotifications(map[string]string{"service.webhook.fixed": "url: https://example.com\n"}, nil)
	svcs, err := c.ListNotificationServices(ctx)
	require.NoError(t, err)
	require.Equal(t, []apiclient.NotificationService{{Name: "fixed"}}, svcs.Items)
}

func TestAuthorization(t *testing.T) {
	t.Parallel()
	env := newEnv(t, notifications.Config{}, false)
	env.Settings.SetNotifications(configured())
	ctx := context.Background()

	_, err := env.Anonymous().ListNotificationServices(ctx)
	require.True(t, apiclient.IsUnauthenticated(err), "got %v", err)
	_, err = env.ClientWithToken("garbage.token.value").ListNotificationTriggers(ctx)
	require.True(t, apiclient.IsUnauthenticated(err), "got %v", err)

	// Project tokens and SSO sessions are authenticated callers too (no RBAC check).
	_, err = env.ProjectClient("team-a", "ci").ListNotificationTemplates(ctx)
	require.NoError(t, err)
	_, err = env.ClientWithToken(env.SSOToken("user-1", "u@example.com", "devs")).ListNotificationServices(ctx)
	require.NoError(t, err)

	// Anonymous access when users.anonymous.enabled is true.
	anon := newEnv(t, notifications.Config{}, true)
	anon.Settings.SetNotifications(configured())
	svcs, err := anon.Anonymous().ListNotificationServices(ctx)
	require.NoError(t, err)
	require.Len(t, svcs.Items, 2)
}

func TestUnavailableWithoutSettings(t *testing.T) {
	t.Parallel()
	env := miscapitest.New(t, miscapitest.Options{CM: miscapitest.Accounts("alice")}, func(*miscapitest.Env) []miscapitest.Registrar {
		return []miscapitest.Registrar{notifications.New(notifications.Deps{})}
	})
	_, err := env.Login("alice").ListNotificationServices(context.Background())
	require.True(t, apiclient.IsUnavailable(err), "got %v", err)
}
