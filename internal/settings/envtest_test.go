package settings

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

var nsCounter atomic.Int32

// envNamespace creates a fresh namespace on the envtest API server.
func envNamespace(t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	name := fmt.Sprintf("settings-%d-%d", time.Now().UnixNano()%1_000_000, nsCounter.Add(1))
	_, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return name
}

func envClient(t *testing.T) kubernetes.Interface {
	t.Helper()
	cs, err := kubernetes.NewForConfig(envConfig(t))
	require.NoError(t, err)
	return cs
}

func inNS(ns string, obj metav1.Object) {
	obj.SetNamespace(ns)
	obj.SetResourceVersion("")
}

// TestEnvtestManagerReload runs the Manager against a real API server: field-selected
// per-name informers, the part-of label requirement, debounced reloads, per-key isolation and
// Events recorded through the events API.
func TestEnvtestManagerReload(t *testing.T) {
	t.Parallel()
	cs := envClient(t)
	ns := envNamespace(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cm := newCM(DefaultConfigMapName, map[string]string{KeyURL: "https://one.example.com", KeyResourceRespectRBAC: "normal"})
	cm.Labels = nil // not yet labelled: ignored (OPS-012)
	inNS(ns, cm)
	_, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	sec := newSecret(DefaultSecretName, baseSecretData(), true)
	inNS(ns, sec)
	_, err = cs.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
	require.NoError(t, err)
	decoy := newCM("not-settings", map[string]string{KeyURL: "https://decoy"})
	inNS(ns, decoy)
	_, err = cs.CoreV1().ConfigMaps(ns).Create(ctx, decoy, metav1.CreateOptions{})
	require.NoError(t, err)

	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events(ns)})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "patrol-settings-test"})

	m, err := NewManager(cs, ManagerOptions{Namespace: ns, Debounce: 50 * time.Millisecond, Recorder: recorder, Logger: quietLogger()})
	require.NoError(t, err)
	require.NoError(t, m.Start(ctx))
	assert.Equal(t, "", m.Current().URL(), "unlabelled patrol-cm is ignored")
	requireIssue(t, m.Current(), DefaultConfigMapName, "", SeverityError, "not labelled")
	assert.Equal(t, testServerKey, m.Current().ServerSecretKey().Reveal())

	notes := make(chan notification, 16)
	m.Subscribe(func(old, new *Settings) { notes <- notification{old, new} })

	// Labelling the ConfigMap makes it visible without a restart.
	cur, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, DefaultConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cur.Labels = partOfLabels()
	_, err = cs.CoreV1().ConfigMaps(ns).Update(ctx, cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case n := <-notes:
		assert.Equal(t, "", n.old.URL())
		assert.Equal(t, "https://one.example.com", n.new.URL())
		assert.Equal(t, "normal", n.new.RespectRBAC())
	case <-time.After(30 * time.Second):
		t.Fatal("no notification after labelling patrol-cm")
	}

	// A bad value keeps the last valid one; the rest of the ConfigMap still applies.
	cur, err = cs.CoreV1().ConfigMaps(ns).Get(ctx, DefaultConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	cur.Data = map[string]string{KeyURL: "https://two.example.com", KeyResourceRespectRBAC: "always"}
	_, err = cs.CoreV1().ConfigMaps(ns).Update(ctx, cur, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case n := <-notes:
		assert.Equal(t, "https://two.example.com", n.new.URL())
		assert.Equal(t, "normal", n.new.RespectRBAC())
		requireIssue(t, n.new, DefaultConfigMapName, KeyResourceRespectRBAC, SeverityError, "keeping the last valid value")
	case <-time.After(30 * time.Second):
		t.Fatal("no notification after the second update")
	}

	// The InvalidSettings Event is recorded on patrol-cm, naming the key.
	require.Eventually(t, func() bool {
		evs, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, e := range evs.Items {
			if e.Reason == EventReasonInvalidSettings && e.InvolvedObject.Kind == "ConfigMap" && e.InvolvedObject.Name == DefaultConfigMapName &&
				strings.HasPrefix(e.Message, "resource.respectRBAC: invalid value for resource.respectRBAC: always") {
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond)

	// Changes to other ConfigMaps in the namespace do not trigger notifications.
	d, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "not-settings", metav1.GetOptions{})
	require.NoError(t, err)
	d.Data = map[string]string{KeyURL: "https://decoy-2"}
	_, err = cs.CoreV1().ConfigMaps(ns).Update(ctx, d, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case n := <-notes:
		t.Fatalf("unexpected notification: %s", n.new.URL())
	case <-time.After(500 * time.Millisecond):
	}
}

// TestEnvtestEnsureServerSecretKeyConcurrent: replicas bootstrapping concurrently against a
// real API server agree on one key, and exactly one of them generates it (OPS-030).
func TestEnvtestEnsureServerSecretKeyConcurrent(t *testing.T) {
	t.Parallel()
	cs := envClient(t)
	for _, existing := range []bool{false, true} {
		ns := envNamespace(t, cs)
		if existing {
			sec := newSecret(DefaultSecretName, map[string]string{"admin.password": "hash"}, true)
			inNS(ns, sec)
			_, err := cs.CoreV1().Secrets(ns).Create(context.Background(), sec, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		const replicas = 8
		var wg sync.WaitGroup
		var generated atomic.Int32
		errs := make(chan error, replicas)
		for range replicas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				gen, err := EnsureServerSecretKey(context.Background(), cs, ns, WithBootstrapLogger(quietLogger()))
				if err != nil {
					errs <- err
					return
				}
				if gen {
					generated.Add(1)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		assert.Equal(t, int32(1), generated.Load(), "existing=%v", existing)
		sec, err := cs.CoreV1().Secrets(ns).Get(context.Background(), DefaultSecretName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Len(t, sec.Data[SecretKeyServerSecretKey], ServerSecretKeyLength)
		if existing {
			assert.Equal(t, []byte("hash"), sec.Data["admin.password"])
		}
		s := Parse(Input{Namespace: ns, Secrets: []*corev1.Secret{sec}})
		_, err = s.SigningKeys()
		assert.NoError(t, err, "the generated key is usable by internal/auth")
	}
}

// TestEnvtestReferencedSecretLabel: "$<secret>:<key>" resolves only through labelled Secrets,
// which the label-selected informer watches.
func TestEnvtestReferencedSecretLabel(t *testing.T) {
	t.Parallel()
	cs := envClient(t)
	ns := envNamespace(t, cs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objs := []runtime.Object{
		newCM(DefaultConfigMapName, map[string]string{KeyOIDCConfig: "issuer: https://idp.example.com\nclientID: c\nclientSecret: $idp:secret\n"}),
		newSecret(DefaultSecretName, baseSecretData(), true),
		newSecret("idp", map[string]string{"secret": "unlabelled-value"}, false),
	}
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.ConfigMap:
			inNS(ns, v)
			_, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, v, metav1.CreateOptions{})
			require.NoError(t, err)
		case *corev1.Secret:
			inNS(ns, v)
			_, err := cs.CoreV1().Secrets(ns).Create(ctx, v, metav1.CreateOptions{})
			require.NoError(t, err)
		}
	}
	m, err := NewManager(cs, ManagerOptions{Namespace: ns, Debounce: 50 * time.Millisecond, Logger: quietLogger()})
	require.NoError(t, err)
	require.NoError(t, m.Start(ctx))
	assert.Equal(t, "$idp:secret", m.Current().OIDCConfig().ClientSecret.Reveal(), "unlabelled Secret not resolved")

	idp, err := cs.CoreV1().Secrets(ns).Get(ctx, "idp", metav1.GetOptions{})
	require.NoError(t, err)
	idp.Labels = partOfLabels()
	_, err = cs.CoreV1().Secrets(ns).Update(ctx, idp, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		c := m.Current().OIDCConfig()
		return c != nil && c.ClientSecret.Reveal() == "unlabelled-value"
	}, 30*time.Second, 50*time.Millisecond)
}
