package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/internal/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/notifications/services"
)

type recordedEvent struct {
	obj                       runtime.Object
	typ, reason, action, note string
}

// recordingRecorder is an events.EventRecorder keeping every Event.
type recordingRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (r *recordingRecorder) Eventf(regarding, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{obj: regarding, typ: eventtype, reason: reason, action: action, note: fmt.Sprintf(note, args...)})
}

func (r *recordingRecorder) all() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

func notifCM(ns string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMapName, Namespace: ns}, Data: data}
}

func notifSecret(ns string, data map[string]string) *corev1.Secret {
	b := map[string][]byte{}
	for k, v := range data {
		b[k] = []byte(v)
	}
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns}, Data: b}
}

func testCompleted(t *testing.T, mutate func(*Config)) *completed {
	t.Helper()
	cfg := Config{RestConfig: &rest.Config{}, Namespace: "patrol", AllowPrivateEgress: true, ApplicationNamespaces: []string{"team-*"}}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := cfg.complete()
	require.NoError(t, err)
	return c
}

const secretsProbe = "{{if .secrets}}exposed{{else}}hidden{{end}}"

func renderProbe(t *testing.T, e *engineEntry) string {
	t.Helper()
	n, err := e.engine.Render(context.Background(), map[string]any{"metadata": map[string]any{"name": "a", "namespace": "team-a"}}, []string{"probe"}, services.Destination{Service: "console"})
	require.NoError(t, err)
	return n.Message
}

// NOTIF-054: self-service engines per namespace, isolated secrets, .secrets hidden everywhere.
func TestEngineFactorySelfService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fakeClient(t,
		notifCM("patrol", map[string]string{"template.probe": "message: '" + secretsProbe + "'", "service.webhook.cp": "url: https://cp.example.com\nheaders:\n- name: X\n  value: $cp-token\n"}),
		notifSecret("patrol", map[string]string{"cp-token": "control-plane-secret"}),
		notifCM("team-a", map[string]string{"template.probe": "message: '" + secretsProbe + "'", "service.webhook.t": "url: https://t.example.com\nheaders:\n- name: X\n  value: $cp-token\n"}),
		notifSecret("team-a", map[string]string{"tenant-token": "tenant-secret"}),
	)
	rec := &recordingRecorder{}

	// Self-service disabled: only the control-plane configuration, .secrets available.
	f := newEngineFactory(testCompleted(t, nil), c, rec, nil)
	got, err := f.forApp(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].tenant)
	assert.Equal(t, "exposed", renderProbe(t, got[0]))

	// Self-service enabled.
	f = newEngineFactory(testCompleted(t, func(c *Config) { c.SelfServiceEnabled = true }), c, rec, nil)
	got, err = f.forApp(ctx, "patrol")
	require.NoError(t, err)
	require.Len(t, got, 1, "a control-plane application uses only the control-plane configuration")
	assert.Equal(t, "hidden", renderProbe(t, got[0]), ".secrets is hidden for every configuration with self-service")

	got, err = f.forApp(ctx, "team-a")
	require.NoError(t, err)
	require.Len(t, got, 2)
	cp, tenant := got[0], got[1]
	assert.False(t, cp.tenant)
	assert.True(t, tenant.tenant)
	assert.Equal(t, "team-a", tenant.namespace)
	assert.True(t, cp.hasService("cp"))
	assert.False(t, cp.hasService("t"))
	assert.True(t, tenant.hasService("t"))
	assert.False(t, tenant.hasService("cp"))
	assert.Equal(t, "hidden", renderProbe(t, tenant))
	assert.Equal(t, map[string]string{"tenant-token": "tenant-secret"}, tenant.engine.Config().Secrets,
		"a namespace configuration only sees the Secret of its namespace")
	assert.NotContains(t, fmt.Sprint(tenant.engine.Config().Services), "control-plane-secret")
	assert.Contains(t, strings.Join(tenant.engine.Warnings(), "\n"), "cp-token", "the unresolved reference is reported")

	// No configuration in the application's namespace: control plane only.
	got, err = f.forApp(ctx, "team-b")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.False(t, got[0].tenant)
	assert.Empty(t, rec.all())
}

// NOTIF-006/014: engines are cached per configuration and rebuilt when the ConfigMap or
// Secret changes; an unusable configuration yields no engine and one Warning Event.
func TestEngineFactoryReloadAndInvalidConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cm := notifCM("patrol", map[string]string{"trigger.t": "- when: 'true'\n  send: [x]"})
	c := fakeClient(t, cm)
	rec := &recordingRecorder{}
	f := newEngineFactory(testCompleted(t, nil), c, rec, nil)

	first, err := f.get(ctx, "patrol", false)
	require.NoError(t, err)
	require.NotNil(t, first)
	again, err := f.get(ctx, "patrol", false)
	require.NoError(t, err)
	assert.Same(t, first, again, "unchanged configuration reuses the engine")

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cm), cm))
	cm.Data["trigger.u"] = "- when: 'false'\n  send: [x]"
	require.NoError(t, c.Update(ctx, cm))
	reloaded, err := f.get(ctx, "patrol", false)
	require.NoError(t, err)
	assert.NotSame(t, first, reloaded)
	assert.True(t, reloaded.hasTrigger("u"), "hot reload")

	// Adding the Secret also rebuilds.
	require.NoError(t, c.Create(ctx, notifSecret("patrol", map[string]string{"k": "v"})))
	withSecret, err := f.get(ctx, "patrol", false)
	require.NoError(t, err)
	assert.NotSame(t, reloaded, withSecret)

	// Invalid YAML: no engine, one Event on the ConfigMap, not repeated while unchanged.
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(cm), cm))
	cm.Data["trigger.u"] = "- when: [unclosed"
	require.NoError(t, c.Update(ctx, cm))
	for range 3 {
		bad, err := f.get(ctx, "patrol", false)
		require.NoError(t, err)
		assert.Nil(t, bad)
	}
	events := rec.all()
	require.Len(t, events, 1)
	assert.Equal(t, corev1.EventTypeWarning, events[0].typ)
	assert.Equal(t, ReasonInvalidNotificationsConfiguration, events[0].reason)
	assert.Equal(t, cm.Name, events[0].obj.(*corev1.ConfigMap).Name)

	// The control-plane configuration may be missing: an empty engine.
	empty, err := newEngineFactory(testCompleted(t, nil), fakeClient(t), rec, nil).get(ctx, "patrol", false)
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Empty(t, empty.services)
}

func TestEngineFactoryTenantConfigRemovedAndReadErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantCM := notifCM("team-a", map[string]string{"trigger.t": "- when: 'true'\n  send: [x]"})
	c := fakeClient(t, tenantCM)
	f := newEngineFactory(testCompleted(t, func(c *Config) { c.SelfServiceEnabled = true }), c, nil, nil)
	e, err := f.get(ctx, "team-a", true)
	require.NoError(t, err)
	require.NotNil(t, e)
	require.NoError(t, c.Delete(ctx, tenantCM))
	e, err = f.get(ctx, "team-a", true)
	require.NoError(t, err)
	assert.Nil(t, e)
	f.mu.Lock()
	_, cached := f.entries["team-a"]
	f.mu.Unlock()
	assert.False(t, cached, "a removed namespace configuration is forgotten")

	boom := errors.New("api unavailable")
	scheme, err := newScheme()
	require.NoError(t, err)
	failing := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return boom
		},
	}).Build()
	_, err = newEngineFactory(testCompleted(t, nil), failing, nil, nil).forApp(ctx, "patrol")
	require.ErrorIs(t, err, boom)
}

// T8/NOTIF-054: a namespace configuration never inherits the control-plane egress policy:
// its webhook to a range the control plane may reach (here loopback) is refused unless
// TenantEgressAllowedPrefixes opens it.
func TestEngineFactoryTenantEgress(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	data := map[string]string{
		"service.webhook.hook": "url: " + srv.URL + "\nretryMax: 0\n",
		"template.t":           "webhook:\n  hook:\n    method: POST\n    body: hi\n",
	}
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	tests := []struct {
		name       string
		mutate     func(*Config)
		tenantSent bool
	}{
		{name: "private egress for the control plane only", mutate: func(c *Config) { c.AllowPrivateEgress = true }},
		{name: "control-plane allow-list only", mutate: func(c *Config) { c.AllowPrivateEgress, c.EgressAllowedPrefixes = false, loopback }},
		{name: "custom control-plane dialer only", mutate: func(c *Config) { c.AllowPrivateEgress, c.Network.DialContext = false, (&net.Dialer{}).DialContext }},
		{name: "tenant allow-list", mutate: func(c *Config) { c.AllowPrivateEgress, c.TenantEgressAllowedPrefixes = true, loopback }, tenantSent: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newEngineFactory(testCompleted(t, func(c *Config) {
				c.SelfServiceEnabled = true
				tc.mutate(c)
			}), fakeClient(t, notifCM("patrol", data), notifCM("team-a", data)), nil, nil)
			engines, err := f.forApp(ctx, "team-a")
			require.NoError(t, err)
			require.Len(t, engines, 2)
			app := map[string]any{"metadata": map[string]any{"name": "a", "namespace": "team-a"}}
			dest := []services.Destination{{Service: "hook"}}

			res, err := engines[0].engine.NotifyTemplate(ctx, app, "t", dest)
			require.NoError(t, err)
			require.Len(t, res, 1)
			assert.Equal(t, notifications.StatusSent, res[0].Status, "the control plane reaches its allowed range: %v", res[0].Err)

			res, err = engines[1].engine.NotifyTemplate(ctx, app, "t", dest)
			require.NoError(t, err)
			require.Len(t, res, 1)
			if tc.tenantSent {
				assert.Equal(t, notifications.StatusSent, res[0].Status, "%v", res[0].Err)
				return
			}
			assert.Equal(t, notifications.StatusFailed, res[0].Status)
			require.ErrorIs(t, res[0].Err, services.ErrURLNotAllowed)
			assert.Equal(t, "the endpoint is not allowed", failureClass(res[0].Err))
		})
	}
	assert.Equal(t, int32(len(tests)+1), hits.Load(), "one control-plane delivery per case plus the allowed tenant one")
}

func TestTruncateUTF8(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"abcdefghij", 8, "abcde..."},
		{"ééééé", 7, "éé..."},
		{"abcdef", 3, "abc"},
		{"ééé", 3, "é"},
		{"abc", 0, ""},
		{"abc", -1, ""},
	}
	for _, tc := range tests {
		got := truncateUTF8(tc.in, tc.n)
		assert.Equal(t, tc.want, got, "%q/%d", tc.in, tc.n)
		assert.LessOrEqual(t, len(got), max(tc.n, 0))
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "u1", ResourceVersion: "1"}}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{UID: "u2", ResourceVersion: "5"}}
	fps := map[string]bool{
		fingerprint(nil, nil): true,
		fingerprint(cm, nil):  true,
		fingerprint(nil, s):   true,
		fingerprint(cm, s):    true,
	}
	assert.Len(t, fps, 4)
	cm2 := cm.DeepCopy()
	cm2.ResourceVersion = "2"
	assert.NotEqual(t, fingerprint(cm, s), fingerprint(cm2, s))
}
