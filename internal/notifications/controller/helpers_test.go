package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/notifications"
)

// receiver is an httptest webhook endpoint recording every request.
type receiver struct {
	srv    *httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	got    []received
}

type received struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(http.StatusOK)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		rec := received{Method: req.Method, Path: req.URL.Path, Header: req.Header.Clone(), Raw: string(b)}
		_ = json.Unmarshal(b, &rec.Body)
		status := int(r.status.Load())
		if status == http.StatusOK {
			r.mu.Lock()
			r.got = append(r.got, rec)
			r.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

// count returns the number of accepted requests whose JSON body has field == value.
func (r *receiver) count(field, value string) int {
	n := 0
	for _, rec := range r.requests() {
		if v, _ := rec.Body[field].(string); v == value {
			n++
		}
	}
	return n
}

func (r *receiver) find(field, value string) (received, bool) {
	for _, rec := range r.requests() {
		if v, _ := rec.Body[field].(string); v == value {
			return rec, true
		}
	}
	return received{}, false
}

func testLogger() *slog.Logger {
	if os.Getenv("NOTIFICATIONS_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func baseConfig(ns string, reg *prometheus.Registry) Config {
	return Config{
		RestConfig:         testCfg,
		Namespace:          ns,
		ResyncPeriod:       time.Second,
		AllowPrivateEgress: true, // httptest listens on loopback
		Registry:           reg,
		Logger:             testLogger(),
	}
}

// startController runs the controller until the returned stop function (also registered as a
// cleanup) is called; stop asserts a clean exit.
func startController(t *testing.T, cfg Config) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Error("controller did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func mkNamespace(ctx context.Context, t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		err := envClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
		if !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}
}

func mkConfig(ctx context.Context, t *testing.T, ns string, data map[string]string, secret map[string]string) {
	t.Helper()
	require.NoError(t, envClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: DefaultConfigMapName, Namespace: ns}, Data: data}))
	if secret != nil {
		require.NoError(t, envClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DefaultSecretName, Namespace: ns}, StringData: secret}))
	}
}

func mkProject(ctx context.Context, t *testing.T, ns, name, description string, annotations map[string]string) {
	t.Helper()
	require.NoError(t, envClient.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
		Spec: v1alpha1.AppProjectSpec{
			Description:  description,
			SourceRepos:  []string{"*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		},
	}))
}

func mkApp(ctx context.Context, t *testing.T, ns, name, project string, labels, annotations map[string]string) types.NamespacedName {
	t.Helper()
	app := &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: annotations},
		Spec: v1alpha1.ApplicationSpec{
			Source:      &v1alpha1.ApplicationSource{RepoURL: "https://github.com/example/" + name + ".git", Path: "deploy", TargetRevision: "HEAD"},
			Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: name},
			Project:     project,
		},
	}
	require.NoError(t, envClient.Create(ctx, app))
	return client.ObjectKeyFromObject(app)
}

// updateApp applies mutate to the Application (metadata/spec) with conflict retries.
func updateApp(ctx context.Context, t *testing.T, key types.NamespacedName, mutate func(*v1alpha1.Application)) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app := &v1alpha1.Application{}
		if err := envClient.Get(ctx, key, app); err != nil {
			return err
		}
		mutate(app)
		return envClient.Update(ctx, app)
	}))
}

// setAppStatus applies mutate to the Application status with conflict retries (the controller
// patches annotations concurrently).
func setAppStatus(ctx context.Context, t *testing.T, key types.NamespacedName, mutate func(*v1alpha1.ApplicationStatus)) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app := &v1alpha1.Application{}
		if err := envClient.Get(ctx, key, app); err != nil {
			return err
		}
		mutate(&app.Status)
		return envClient.Status().Update(ctx, app)
	}))
}

func mkOperation(ctx context.Context, t *testing.T, ns, name, app string, spec v1alpha1.OperationSpec, status v1alpha1.OperationStatus) {
	t.Helper()
	spec.Application = app
	op := &v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
	require.NoError(t, envClient.Create(ctx, op))
	setOperationStatus(ctx, t, types.NamespacedName{Namespace: ns, Name: name}, status)
}

func setOperationStatus(ctx context.Context, t *testing.T, key types.NamespacedName, status v1alpha1.OperationStatus) {
	t.Helper()
	require.NoError(t, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		op := &v1alpha1.Operation{}
		if err := envClient.Get(ctx, key, op); err != nil {
			return err
		}
		op.Status = status
		return envClient.Status().Update(ctx, op)
	}))
}

// deliveryState returns the parsed delivery-state annotation of an Application.
func deliveryState(ctx context.Context, t *testing.T, key types.NamespacedName) notifications.DeliveryState {
	t.Helper()
	app := &v1alpha1.Application{}
	require.NoError(t, envClient.Get(ctx, key, app))
	return notifications.DeliveryStateFromAnnotations(app.Annotations, notifications.DefaultAnnotationPrefix)
}

// failureEvents returns the NotificationDeliveryFailed Events regarding an Application.
func failureEvents(ctx context.Context, t *testing.T, key types.NamespacedName) []eventsv1.Event {
	t.Helper()
	list := &eventsv1.EventList{}
	require.NoError(t, envClient.List(ctx, list, client.InNamespace(key.Namespace)))
	var out []eventsv1.Event
	for _, e := range list.Items {
		if e.Regarding.Name == key.Name && e.Regarding.Kind == "Application" && e.Reason == ReasonNotificationDeliveryFailed {
			out = append(out, e)
		}
	}
	return out
}

// counter returns the value of a counter with exactly the given labels (0 when absent).
func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metrics:
		for _, m := range mf.GetMetric() {
			if len(m.GetLabel()) != len(labels) {
				continue
			}
			for _, lp := range m.GetLabel() {
				if labels[lp.GetName()] != lp.GetValue() {
					continue metrics
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// counterSum sums every series of a counter.
func counterSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	total := 0.0
	for _, mf := range mfs {
		if mf.GetName() == name {
			for _, m := range mf.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func mustTime(s string) metav1.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return metav1.NewTime(tm)
}

func timePtr(s string) *metav1.Time {
	tm := mustTime(s)
	return &tm
}
