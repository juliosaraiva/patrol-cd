package webhook

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

const testNS = "patrol"

func testScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

func fakeClient(t testing.TB, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

// logBuffer collects log output for assertions that secrets and bodies never appear.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// staticSettings returns a SettingsSource with the given webhook settings (defaults for
// the size limit when zero) and patrol-secret marked as loaded.
func staticSettings(w settings.Webhook) SettingsSource {
	if w.MaxPayloadSize == 0 {
		w.MaxPayloadSize = DefaultMaxPayloadSize
	}
	w.SecretsLoaded = true
	return func() settings.Webhook { return w }
}

type testHandler struct {
	*Handler
	client client.Client
	logs   *logBuffer
	reg    *prometheus.Registry
}

func newTestHandler(t testing.TB, cfg Config, objs ...client.Object) *testHandler {
	t.Helper()
	if cfg.Namespace == "" {
		cfg.Namespace = testNS
	}
	c := cfg.Client
	if c == nil {
		c = fakeClient(t, objs...)
		cfg.Client = c
	}
	if cfg.Settings == nil {
		cfg.Settings = staticSettings(settings.Webhook{})
	}
	logs := &logBuffer{}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	reg := prometheus.NewRegistry()
	cfg.Registerer = reg
	if cfg.BitbucketCloud.Credentials == nil && cfg.BitbucketCloud.HTTPClient == nil && len(cfg.BitbucketCloud.AllowedAPIURLs) == 0 {
		// Unit tests never call out; a test enabling the callback configures it.
		cfg.BitbucketCloud.Disabled = true
	}
	h, err := NewHandler(cfg)
	require.NoError(t, err)
	return &testHandler{Handler: h, client: c, logs: logs, reg: reg}
}

// start runs the workers until the test ends.
func (th *testHandler) start(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- th.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Minute):
			t.Error("handler did not stop")
		}
	})
}

func (th *testHandler) serve(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	th.ServeHTTP(w, req)
	return w
}

func (th *testHandler) requests(provider, result string) float64 {
	return testutil.ToFloat64(th.metrics.requests.WithLabelValues(provider, result))
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func newRequest(t testing.TB, target string, body []byte, headers map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.10:4321"
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func hmacHex(secret string, body []byte) string { return scm.SignHMACSHA256([]byte(secret), body) }

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// eventually polls cond until it holds or the timeout expires.
func eventually(t testing.TB, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

func annotations(t testing.TB, c client.Client, obj client.Object) map[string]string {
	t.Helper()
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj))
	return obj.GetAnnotations()
}

func gitApp(ns, name, repoURL, revision, path string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: "default"},
			Source:      &v1alpha1.ApplicationSource{RepoURL: repoURL, TargetRevision: revision, Path: path},
		},
	}
}

func secretObj(ns, name, key, value string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{key: []byte(value)}}
}

// conflictClient wraps c so that the first n patches fail with a conflict; *count
// records the injected conflicts.
func conflictClient(c client.WithWatch, n int, count *int) client.Client {
	var mu sync.Mutex
	return interceptor.NewClient(c, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			mu.Lock()
			if *count < n {
				*count++
				mu.Unlock()
				return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "applications"}, obj.GetName(), errors.New("injected conflict"))
			}
			mu.Unlock()
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
