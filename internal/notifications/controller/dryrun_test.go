package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

const dryRunApp = `{
  "apiVersion": "cd.patrol.example/v1alpha1", "kind": "Application",
  "metadata": {"name": "guestbook", "namespace": "apps",
    "annotations": {"notifications.cd.patrol.example/subscribe.on-synced.hook": ""}},
  "spec": {"project": "default", "source": {"repoURL": "https://git.example.com/guestbook.git"}},
  "status": {
    "sync": {"status": "Synced"},
    "health": {"status": "Healthy"},
    "reconciledAt": "2026-01-01T00:06:00Z",
    "operationState": {"operationName": "guestbook-1", "phase": "Succeeded",
      "startedAt": "2026-01-01T00:00:00Z", "finishedAt": "2026-01-01T00:05:00Z",
      "syncResult": {"revision": "abc"}}
  }
}`

func dryRunConfig(url string) map[string]string {
	return map[string]string{
		"service.webhook.hook": "url: " + url + "\nretryMax: 0\n",
		"template.synced": `message: "{{.app.metadata.name}} synced {{.app.status.operationState.syncResult.revision}} ({{len .app.status.operationState.syncResult.resources}} resources) {{.secrets.token}}"
webhook:
  hook:
    method: POST
    body: '{"app": "{{.app.metadata.name}}"}'
`,
		"trigger.on-synced": `- when: app.status.operationState.phase in ['Succeeded']
  send: [synced]
- when: app.status.operationState.phase in ['Failed']
  send: [synced]`,
	}
}

type countingServer struct {
	*httptest.Server
	hits atomic.Int32
}

func newCountingServer(t *testing.T) *countingServer {
	t.Helper()
	s := &countingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

func newTestDryRun(t *testing.T, url string, out *bytes.Buffer) *DryRun {
	t.Helper()
	ops := &fakeOps{ops: map[string]map[string]any{"apps/guestbook-1": operationFixture("guestbook", "Succeeded")}}
	d, err := NewDryRun(dryRunConfig(url), map[string][]byte{"token": []byte("tok")}, DryRunOptions{Operations: ops, Output: out, DisableDefaultCatalog: true})
	require.NoError(t, err)
	return d
}

// NOTIF-107: "trigger run" evaluates every condition without sending.
func TestDryRunTriggerRun(t *testing.T) {
	t.Parallel()
	srv := newCountingServer(t)
	d := newTestDryRun(t, srv.URL, nil)
	res, err := d.TriggerRun(context.Background(), "on-synced", jsonObj(t, dryRunApp))
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.True(t, res[0].Triggered)
	assert.False(t, res[1].Triggered)
	assert.Equal(t, []string{"synced"}, res[0].Templates)
	assert.Zero(t, srv.hits.Load())

	_, err = d.TriggerRun(context.Background(), "nope", jsonObj(t, dryRunApp))
	require.ErrorIs(t, err, notifications.ErrTriggerNotFound)
	assert.Contains(t, d.Config().Triggers, "on-synced")
}

// NOTIF-106: "template notify" renders with the controller's application view and prints to
// the console by default; other recipients are really delivered.
func TestDryRunTemplateNotify(t *testing.T) {
	t.Parallel()
	srv := newCountingServer(t)
	var out bytes.Buffer
	d := newTestDryRun(t, srv.URL, &out)
	res, err := d.TemplateNotify(context.Background(), "synced", jsonObj(t, dryRunApp), nil)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, notifications.StatusSent, res[0].Status)
	assert.Equal(t, services.Destination{Service: "console", Recipient: "stdout"}, res[0].Destination)
	assert.Contains(t, out.String(), "guestbook synced abc (1 resources) tok", "operation resources and .secrets are available to the CLI")
	assert.Zero(t, srv.hits.Load())

	res, err = d.TemplateNotify(context.Background(), "synced", jsonObj(t, dryRunApp), []string{"hook"})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, notifications.StatusSent, res[0].Status, "%v", res[0].Err)
	assert.Equal(t, int32(1), srv.hits.Load())

	_, err = d.TemplateNotify(context.Background(), "missing", jsonObj(t, dryRunApp), nil)
	require.ErrorIs(t, err, notifications.ErrTemplateNotFound)
	_, err = d.TemplateNotify(context.Background(), "synced", jsonObj(t, dryRunApp), []string{":x"})
	require.ErrorContains(t, err, "invalid recipient")
}

// Evaluate shows what the controller would send now, without sending or recording.
func TestDryRunEvaluate(t *testing.T) {
	t.Parallel()
	srv := newCountingServer(t)
	d := newTestDryRun(t, srv.URL, nil)
	app := jsonObj(t, dryRunApp)
	ev, err := d.Evaluate(context.Background(), app)
	require.NoError(t, err)
	require.False(t, ev.Skipped)
	require.Len(t, ev.Results, 1)
	r := ev.Results[0]
	assert.Equal(t, notifications.StatusDryRun, r.Status)
	require.NotNil(t, r.Notification)
	assert.Equal(t, "guestbook synced abc (1 resources) tok", r.Notification.Message)
	assert.Zero(t, srv.hits.Load(), "nothing is sent")
	_, recorded := app["metadata"].(map[string]any)["annotations"].(map[string]any)[notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix)]
	assert.False(t, recorded, "the application is not modified")

	// Already delivered according to the application's delivery state.
	key := notifications.StateKey("", "on-synced", notifications.ConditionResult{Key: r.ConditionKey}, r.Destination)
	app["metadata"].(map[string]any)["annotations"].(map[string]any)[notifications.DeliveryStateAnnotation(notifications.DefaultAnnotationPrefix)] = `{"` + key + `": 1}`
	ev, err = d.Evaluate(context.Background(), app)
	require.NoError(t, err)
	require.Len(t, ev.Results, 1)
	assert.Equal(t, notifications.StatusSkippedDuplicate, ev.Results[0].Status)

	// NOTIF-010: sync status older than the operation.
	stale := jsonObj(t, dryRunApp)
	stale["status"].(map[string]any)["reconciledAt"] = "2026-01-01T00:04:00Z"
	ev, err = d.Evaluate(context.Background(), stale)
	require.NoError(t, err)
	assert.True(t, ev.Skipped)
	assert.NotEmpty(t, ev.SkipReason)
	assert.Empty(t, ev.Results)
}

func TestNewDryRunInvalidConfig(t *testing.T) {
	t.Parallel()
	_, err := NewDryRun(map[string]string{"trigger.x": "- when: [unclosed"}, map[string][]byte{"k": []byte("hidden-value")}, DryRunOptions{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hidden-value")
}

func TestParseRecipient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    services.Destination
		wantErr bool
	}{
		{in: "slack:my-channel", want: services.Destination{Service: "slack", Recipient: "my-channel"}},
		{in: "email:a@example.com:x", want: services.Destination{Service: "email", Recipient: "a@example.com:x"}},
		{in: "hook", want: services.Destination{Service: "hook"}},
		{in: " console:stdout ", want: services.Destination{Service: "console", Recipient: "stdout"}},
		{in: "", wantErr: true},
		{in: ":x", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseRecipient(tc.in)
		if tc.wantErr {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestParseApplication(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "yaml", in: "apiVersion: cd.patrol.example/v1alpha1\nkind: Application\nmetadata:\n  name: a\n"},
		{name: "json", in: `{"kind": "Application", "metadata": {"name": "a"}}`},
		{name: "wrong kind", in: "kind: AppProject\nmetadata:\n  name: a\n", wantErr: "not an Application"},
		{name: "no metadata", in: "kind: Application\n", wantErr: "no metadata"},
		{name: "empty", in: "", wantErr: "empty"},
		{name: "invalid", in: "a: [", wantErr: "decoding"},
		{name: "too large", in: strings.Repeat("#", maxManifestBytes+1), wantErr: "exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj, err := ParseApplication([]byte(tc.in))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "a", obj["metadata"].(map[string]any)["name"])
		})
	}
}

func TestLoadConfigAndGetApplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fakeClient(t,
		notifCM("patrol", map[string]string{"trigger.t": "- when: 'true'\n  send: [x]"}),
		notifSecret("patrol", map[string]string{"k": "v"}),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "custom-cm", Namespace: "patrol"}, Data: map[string]string{"context": "a: b"}},
		&v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "guestbook", Namespace: "patrol"}},
	)
	cm, secret, err := LoadConfig(ctx, c, "patrol", "", "")
	require.NoError(t, err)
	assert.Contains(t, cm, "trigger.t")
	assert.Equal(t, []byte("v"), secret["k"])

	cm, secret, err = LoadConfig(ctx, c, "patrol", "custom-cm", "missing-secret")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"context": "a: b"}, cm)
	assert.Empty(t, secret, "missing objects are empty configuration")

	cm, secret, err = LoadConfig(ctx, c, "elsewhere", "", "")
	require.NoError(t, err)
	assert.Empty(t, cm)
	assert.Empty(t, secret)

	app, err := GetApplication(ctx, c, "patrol", "guestbook")
	require.NoError(t, err)
	assert.Equal(t, "Application", app["kind"])
	_, err = GetApplication(ctx, c, "patrol", "missing")
	require.ErrorContains(t, err, "getting application patrol/missing")
}
