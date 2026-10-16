package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notificationsCM writes a notifications ConfigMap with a webhook service named "test"
// pointing at url and a template using it.
func notificationsCM(t *testing.T, url string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cm.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: patrol-notifications-cm
data:
  service.webhook.test: |
    url: `+url+`
    headers:
    - name: Authorization
      value: Bearer $webhook-token
  template.deploy-hook: |
    message: Application {{.app.metadata.name}} is {{.app.status.sync.status}}
    webhook:
      test:
        method: POST
        path: /hooks/{{.app.metadata.name}}
        body: |
          {"app": "{{.app.metadata.name}}", "sync": "{{.app.status.sync.status}}"}
  trigger.on-unknown: |
    - when: app.status.sync.status == 'Unknown'
      send: [deploy-hook]
    - when: app.status.health.status == 'Degraded'
      send: [deploy-hook]
`), 0o600))
	return path
}

type recordedRequest struct {
	path, body, auth string
}

func webhookServer(t *testing.T) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recordedRequest{path: r.URL.Path, body: string(body), auth: r.Header.Get("Authorization")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), got...)
	}
}

func TestNotificationsOffline(t *testing.T) {
	t.Parallel()
	srv, requests := webhookServer(t)
	cm := notificationsCM(t, srv.URL)
	sec := "testdata/notifications/secret.yaml"
	n := func(args ...string) (string, string, error) {
		return offline(t, append(append([]string{"admin", "notifications"}, args...), "--config-map", cm, "--secret", sec)...)
	}

	out, _, err := n("trigger", "get")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(out, "NAME"), out)
	assert.Contains(t, out, "on-unknown")
	assert.Contains(t, out, "app.status.sync.status == 'Unknown'")
	assert.Contains(t, out, "on-sync-failed", "the default catalog is merged")

	out, _, err = n("trigger", "get", "on-unknown", "-o", "yaml")
	require.NoError(t, err)
	assert.Contains(t, out, "when: app.status.sync.status == 'Unknown'")
	assert.Contains(t, out, "- deploy-hook")
	out, _, err = n("trigger", "get", "-o", "name")
	require.NoError(t, err)
	assert.Contains(t, out, "on-unknown\n")
	_, _, err = n("trigger", "get", "nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger with name 'nope' is not defined")

	out, _, err = n("template", "get", "deploy-hook", "-o", "json")
	require.NoError(t, err)
	assert.Contains(t, out, `"message": "Application {{.app.metadata.name}} is {{.app.status.sync.status}}"`)
	out, _, err = n("template", "get")
	require.NoError(t, err)
	assert.Contains(t, out, "deploy-hook")
	assert.Contains(t, out, "app-sync-succeeded")
	_, _, err = n("template", "get", "-o", "table")
	require.EqualError(t, err, "unknown output format: table")

	out, _, err = n("trigger", "run", "on-unknown", "testdata/notifications/app-unknown.yaml")
	require.NoError(t, err)
	assert.Equal(t, "CONDITION                               RESULT\napp.status.sync.status == 'Unknown'     true\napp.status.health.status == 'Degraded'  false\n", out)
	out, _, err = n("trigger", "run", "on-unknown", "testdata/notifications/app-synced.yaml")
	require.NoError(t, err)
	assert.Contains(t, out, "'Unknown'     false")
	_, _, err = n("trigger", "run", "missing", "testdata/notifications/app-synced.yaml")
	require.Error(t, err)

	// Default recipient: printed, nothing delivered.
	out, _, err = n("template", "notify", "deploy-hook", "testdata/notifications/app-unknown.yaml")
	require.NoError(t, err)
	assert.Contains(t, out, "Application guestbook is Unknown")
	assert.Empty(t, requests())

	// Dry run (default) for a real service: rendered, not sent; secrets are never printed.
	// Console recipients are still printed.
	out, errOut, err := n("template", "notify", "deploy-hook", "testdata/notifications/app-unknown.yaml", "--recipient", "test", "--recipient", "console:stdout")
	require.NoError(t, err)
	assert.Contains(t, out, "# test: (dry run, not sent)")
	assert.Contains(t, out, `{"app": "guestbook", "sync": "Unknown"}`)
	assert.Contains(t, out, "Application guestbook is Unknown")
	assert.NotContains(t, out+errOut, "test-token-value")
	assert.Contains(t, errOut, "pass --dry-run=false to send them")
	assert.Empty(t, requests())

	// --dry-run=false delivers.
	_, _, err = n("template", "notify", "deploy-hook", "testdata/notifications/app-unknown.yaml", "--recipient", "test", "--dry-run=false")
	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "/hooks/guestbook", got[0].path)
	assert.JSONEq(t, `{"app": "guestbook", "sync": "Unknown"}`, got[0].body)
	assert.Equal(t, "Bearer test-token-value", got[0].auth)

	_, _, err = n("template", "notify", "missing", "testdata/notifications/app-unknown.yaml")
	require.Error(t, err)
	for _, dry := range []string{"--dry-run=true", "--dry-run=false"} {
		_, _, err = n("template", "notify", "deploy-hook", "testdata/notifications/app-unknown.yaml", "--recipient", "nosuchservice:x", dry)
		require.ErrorContains(t, err, "failed to notify 'nosuchservice:x'", dry)
		assert.ErrorContains(t, err, "notification service 'nosuchservice' is not supported", dry)
	}
}

func TestNotificationsCluster(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "notif")
	srv, requests := webhookServer(t)
	cmPath := notificationsCM(t, srv.URL)
	applyFile(t, ctx, ns, cmPath)
	applyFile(t, ctx, ns, "testdata/notifications/secret.yaml")
	app := applyFile(t, ctx, ns, filepath.Join(examplesDir, "apps", "plain-yaml", "application.yaml"))[0]
	t.Cleanup(func() { removeFinalizers(t, ctx, app) })
	applyFile(t, ctx, ns, filepath.Join(examplesDir, "platform", "projects", "default.yaml"))

	tio := newTestIO(ns)
	require.NoError(t, tio.run(ctx, "admin", "notifications", "trigger", "get", "on-unknown", "-o", "name"))
	assert.Equal(t, "on-unknown\n", tio.out.String())

	// Without status the sync status is empty: the trigger does not fire.
	require.NoError(t, tio.run(ctx, "admin", "notifications", "trigger", "run", "on-unknown", "guestbook-plain"))
	assert.Contains(t, tio.out.String(), "'Unknown'     false")

	require.NoError(t, tio.run(ctx, "admin", "notifications", "template", "notify", "deploy-hook", "guestbook-plain", "--recipient", "test"))
	assert.Empty(t, requests(), "dry run by default")
	require.NoError(t, tio.run(ctx, "admin", "notifications", "template", "notify", "deploy-hook", "guestbook-plain", "--recipient", "test", "--dry-run=false"))
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "/hooks/guestbook-plain", got[0].path)

	err := tio.run(ctx, "admin", "notifications", "trigger", "run", "on-unknown", "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}
