package settings

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, msg)
}

type notification struct{ old, new *Settings }

func startManager(t *testing.T, client kubernetes.Interface, opts ManagerOptions) (*Manager, context.CancelFunc) {
	t.Helper()
	opts.Namespace = testNamespace
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	m, err := NewManager(client, opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, m.Start(ctx))
	return m, cancel
}

func updateCM(t *testing.T, client kubernetes.Interface, name string, data map[string]string) {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Data = data
	rv := cm.ResourceVersion
	cm.ResourceVersion = rv + "1" // the fake tracker does not bump resourceVersions
	_, err = client.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func TestManagerLoadsAndReloads(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(
		newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://one.example.com"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
	)
	var results []ReloadResult
	var mu sync.Mutex
	m, _ := startManager(t, client, ManagerOptions{Debounce: 20 * time.Millisecond, Metrics: Metrics{Reloaded: func(r ReloadResult) {
		mu.Lock()
		results = append(results, r)
		mu.Unlock()
	}}})
	assert.Equal(t, "https://one.example.com", m.Current().URL())
	assert.Equal(t, testServerKey, m.Current().ServerSecretKey().Reveal())
	mu.Lock()
	require.Len(t, results, 1)
	assert.True(t, results[0].Initial && results[0].Published)
	mu.Unlock()

	notes := make(chan notification, 10)
	unsubscribe := m.Subscribe(func(old, new *Settings) { notes <- notification{old, new} })
	updateCM(t, client, DefaultConfigMapName, map[string]string{KeyURL: "https://two.example.com"})
	select {
	case n := <-notes:
		assert.Equal(t, "https://one.example.com", n.old.URL())
		assert.Equal(t, "https://two.example.com", n.new.URL())
		assert.Same(t, n.new, m.Current())
	case <-time.After(10 * time.Second):
		t.Fatal("no notification after a ConfigMap change")
	}

	// A reload with unchanged input publishes nothing.
	m.kick()
	select {
	case n := <-notes:
		t.Fatalf("unexpected notification for unchanged input: %s", n.new.URL())
	case <-time.After(300 * time.Millisecond):
	}

	unsubscribe()
	unsubscribe() // idempotent
	updateCM(t, client, DefaultConfigMapName, map[string]string{KeyURL: "https://three.example.com"})
	waitFor(t, 10*time.Second, func() bool { return m.Current().URL() == "https://three.example.com" }, "third URL")
	select {
	case <-notes:
		t.Fatal("notified after unsubscribe")
	default:
	}
}

func TestManagerDebouncesBursts(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://u0"}), newSecret(DefaultSecretName, baseSecretData(), true))
	m, _ := startManager(t, client, ManagerOptions{Debounce: time.Second})
	var count atomic.Int32
	m.Subscribe(func(_, _ *Settings) { count.Add(1) })
	for i := 1; i <= 5; i++ {
		updateCM(t, client, DefaultConfigMapName, map[string]string{KeyURL: "https://u" + string(rune('0'+i))})
	}
	waitFor(t, 15*time.Second, func() bool { return m.Current().URL() == "https://u5" }, "last update")
	assert.Less(t, count.Load(), int32(5), "updates within the debounce window are coalesced")
	assert.GreaterOrEqual(t, count.Load(), int32(1))
}

func TestManagerPerKeyIsolationAndEvents(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(
		newCM(DefaultConfigMapName, map[string]string{KeyResourceRespectRBAC: "normal", "unknown.key": "x"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
	)
	rec := record.NewFakeRecorder(100)
	m, _ := startManager(t, client, ManagerOptions{Debounce: 20 * time.Millisecond, Recorder: rec})
	ev := <-rec.Events
	assert.Contains(t, ev, "Warning "+EventReasonSettingsWarning+" unknown.key: unknown key is ignored")

	updateCM(t, client, DefaultConfigMapName, map[string]string{KeyResourceRespectRBAC: "always", KeyURL: "https://new", "unknown.key": "x"})
	waitFor(t, 10*time.Second, func() bool { return m.Current().URL() == "https://new" }, "reload")
	assert.Equal(t, "normal", m.Current().RespectRBAC(), "the bad key keeps its last valid value")
	select {
	case ev := <-rec.Events:
		assert.Equal(t, "Warning "+EventReasonInvalidSettings+" resource.respectRBAC: invalid value for resource.respectRBAC: always; keeping the last valid value", ev)
	case <-time.After(5 * time.Second):
		t.Fatal("no event for the invalid key")
	}
	select {
	case ev := <-rec.Events:
		t.Fatalf("issues already reported must not be re-emitted: %s", ev)
	default:
	}
}

func TestManagerReferencedSecretsAndDeletion(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(
		newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://x", KeyOIDCConfig: "issuer: https://idp\nclientID: c\nclientSecret: $idp:secret\n"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
	)
	m, _ := startManager(t, client, ManagerOptions{Debounce: 20 * time.Millisecond})
	assert.Equal(t, "$idp:secret", m.Current().OIDCConfig().ClientSecret.Reveal())

	_, err := client.CoreV1().Secrets(testNamespace).Create(context.Background(), newSecret("idp", map[string]string{"secret": "resolved"}, true), metav1.CreateOptions{})
	require.NoError(t, err)
	waitFor(t, 10*time.Second, func() bool { return m.Current().OIDCConfig().ClientSecret.Reveal() == "resolved" }, "labelled Secret resolution")

	// An unlabelled Secret in the namespace is never resolvable.
	_, err = client.CoreV1().Secrets(testNamespace).Create(context.Background(), newSecret("plain", map[string]string{"k": "v"}, false), metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, client.CoreV1().ConfigMaps(testNamespace).Delete(context.Background(), DefaultConfigMapName, metav1.DeleteOptions{}))
	waitFor(t, 10*time.Second, func() bool { return m.Current().SourceState(SourceConfigMap) == SourceStateRetained }, "retained patrol-cm after deletion")
	assert.Equal(t, "https://x", m.Current().URL(), "the last loaded settings stay in force")
	assert.Equal(t, "resolved", m.Current().OIDCConfig().ClientSecret.Reveal())
	requireIssue(t, m.Current(), DefaultConfigMapName, "", SeverityError, "ConfigMap not found; keeping the settings last loaded from it")
}

// TestManagerKeepsLastLoadedSettingsWhenObjectsDisappear: unlabelling or deleting patrol-cm
// and deleting patrol-secret never publish a snapshot that re-enables a disabled account or
// clears webhook secrets; recreating the objects restores normal operation.
func TestManagerKeepsLastLoadedSettingsWhenObjectsDisappear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sec := baseSecretData()
	sec[SecretKeyAdminPassword] = "$2a$10$hash"
	sec[SecretKeyWebhookGitHub] = "gh"
	client := fake.NewClientset(
		newCM(DefaultConfigMapName, map[string]string{KeyAdminEnabled: "false", KeyURL: "https://x"}),
		newSecret(DefaultSecretName, sec, true),
	)
	rec := record.NewFakeRecorder(100)
	m, _ := startManager(t, client, ManagerOptions{Debounce: 20 * time.Millisecond, Recorder: rec})
	require.False(t, m.Current().Accounts()["admin"].Enabled)
	var mu sync.Mutex
	var published []*Settings
	m.Subscribe(func(_, n *Settings) {
		mu.Lock()
		published = append(published, n)
		mu.Unlock()
	})

	// Unlabel patrol-cm: the name-selected informer still delivers it; it is ignored.
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(ctx, DefaultConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cm.Labels = nil
	cm.ResourceVersion += "1"
	_, err = client.CoreV1().ConfigMaps(testNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	waitFor(t, 10*time.Second, func() bool { return m.Current().SourceState(SourceConfigMap) == SourceStateRetained }, "retained after unlabel")
	assert.Equal(t, "https://x", m.Current().URL())
	waitFor(t, 5*time.Second, func() bool {
		select {
		case ev := <-rec.Events:
			return strings.Contains(ev, "Warning "+EventReasonInvalidSettings) && strings.Contains(ev, "keeping the settings last loaded from it")
		default:
			return false
		}
	}, "an InvalidSettings Event on the unlabelled ConfigMap")

	// Delete patrol-cm, then patrol-secret.
	require.NoError(t, client.CoreV1().ConfigMaps(testNamespace).Delete(ctx, DefaultConfigMapName, metav1.DeleteOptions{}))
	waitFor(t, 10*time.Second, func() bool {
		return hasIssue(m.Current(), DefaultConfigMapName, "ConfigMap not found; keeping the settings last loaded from it")
	}, "retained after deletion")
	assert.False(t, m.Current().Accounts()["admin"].Enabled, "deleting patrol-cm must not re-enable a disabled admin")
	assert.Equal(t, "https://x", m.Current().URL())

	require.NoError(t, client.CoreV1().Secrets(testNamespace).Delete(ctx, DefaultSecretName, metav1.DeleteOptions{}))
	waitFor(t, 10*time.Second, func() bool { return m.Current().SourceState(SourceSecret) == SourceStateRetained }, "retained patrol-secret")
	assert.Equal(t, "gh", m.Current().Webhook().GitHubSecret.Reveal(), "webhook secrets survive the deletion")
	assert.Equal(t, testServerKey, m.Current().ServerSecretKey().Reveal())

	mu.Lock()
	require.NotEmpty(t, published)
	for _, s := range published {
		assert.False(t, s.Accounts()["admin"].Enabled, "no published snapshot re-enabled admin")
		assert.Equal(t, "gh", s.Webhook().GitHubSecret.Reveal(), "no published snapshot cleared the webhook secret")
	}
	mu.Unlock()

	// Recreating both objects restores normal operation.
	_, err = client.CoreV1().ConfigMaps(testNamespace).Create(ctx, newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://y"}), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Secrets(testNamespace).Create(ctx, newSecret(DefaultSecretName, baseSecretData(), true), metav1.CreateOptions{})
	require.NoError(t, err)
	waitFor(t, 10*time.Second, func() bool {
		c := m.Current()
		return c.URL() == "https://y" && c.SourceState(SourceConfigMap) == SourceStateLoaded && c.SourceState(SourceSecret) == SourceStateLoaded
	}, "reload after recreation")
	assert.True(t, m.Current().Accounts()["admin"].Enabled)
	assert.True(t, m.Current().Webhook().GitHubSecret.IsZero())
}

func hasIssue(s *Settings, object, want string) bool {
	for _, is := range s.Validate() {
		if is.Object == object && strings.Contains(is.Message, want) {
			return true
		}
	}
	return false
}

func TestManagerFiltersNamespaceAndNames(t *testing.T) {
	t.Parallel()
	other := newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://other-namespace"})
	other.Namespace = "elsewhere"
	client := fake.NewClientset(
		other,
		newCM("unrelated", map[string]string{KeyURL: "https://unrelated"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
	)
	m, _ := startManager(t, client, ManagerOptions{})
	assert.Equal(t, "", m.Current().URL())
}

func TestManagerSubscriberPanicIsRecovered(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(newCM(DefaultConfigMapName, map[string]string{}), newSecret(DefaultSecretName, baseSecretData(), true))
	m, _ := startManager(t, client, ManagerOptions{Debounce: 10 * time.Millisecond})
	m.Subscribe(func(_, _ *Settings) { panic("boom") })
	got := make(chan string, 4)
	m.Subscribe(func(_, n *Settings) { got <- n.URL() })
	updateCM(t, client, DefaultConfigMapName, map[string]string{KeyURL: "https://after-panic"})
	select {
	case u := <-got:
		assert.Equal(t, "https://after-panic", u)
	case <-time.After(10 * time.Second):
		t.Fatal("later subscribers must still be notified")
	}
}

func TestManagerOptionsAndLifecycle(t *testing.T) {
	t.Parallel()
	_, err := NewManager(nil, ManagerOptions{Namespace: "x"})
	assert.Error(t, err)
	_, err = NewManager(fake.NewClientset(), ManagerOptions{})
	assert.ErrorContains(t, err, "namespace is required")
	_, err = NewManager(fake.NewClientset(), ManagerOptions{Namespace: "x", Names: Names{ConfigMap: "a", RBACConfigMap: "a"}})
	assert.ErrorContains(t, err, "same object name")

	m, err := NewManager(fake.NewClientset(), ManagerOptions{Namespace: testNamespace, Logger: quietLogger()})
	require.NoError(t, err)
	require.NotNil(t, m.Current(), "defaults before Start")
	assert.Equal(t, DefaultUserSessionDuration, m.Current().UserSessionDuration())
	assert.NotNil(t, m.Subscribe(nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, m.Start(ctx))
	assert.ErrorContains(t, m.Start(ctx), "already started")

	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	m2, err := NewManager(fake.NewClientset(), ManagerOptions{Namespace: testNamespace, Logger: quietLogger()})
	require.NoError(t, err)
	err = m2.Start(canceled)
	if err != nil {
		assert.ErrorContains(t, err, "context canceled")
	}
}

func TestManagerDisableReferencedSecrets(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(
		newCM(DefaultConfigMapName, map[string]string{KeyOIDCConfig: "issuer: https://idp\nclientID: c\nclientSecret: $idp:secret\n"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
		newSecret("idp", map[string]string{"secret": "v"}, true),
	)
	m, _ := startManager(t, client, ManagerOptions{DisableReferencedSecrets: true})
	assert.Equal(t, "$idp:secret", m.Current().OIDCConfig().ClientSecret.Reveal())
}

func TestPrometheusMetrics(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	mt, err := NewPrometheusMetrics(reg)
	require.NoError(t, err)
	mt.Reloaded(ReloadResult{Published: true, Errors: 2, Warnings: 1, Duration: time.Millisecond})
	mt.Reloaded(ReloadResult{Published: false})
	expected := `
# HELP patrol_settings_issues Issues of the current settings snapshot by severity.
# TYPE patrol_settings_issues gauge
patrol_settings_issues{severity="error"} 2
patrol_settings_issues{severity="warning"} 1
# HELP patrol_settings_reloads_total Settings reloads by result.
# TYPE patrol_settings_reloads_total counter
patrol_settings_reloads_total{result="published"} 1
patrol_settings_reloads_total{result="unchanged"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "patrol_settings_issues", "patrol_settings_reloads_total"))
	_, err = NewPrometheusMetrics(reg)
	assert.Error(t, err, "double registration")
}

var _ EventRecorder = record.NewFakeRecorder(1)
var _ = corev1.EventTypeWarning
