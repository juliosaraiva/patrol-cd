package appset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const (
	controlNS = "patrol"
	tenantNS  = "team-a"
	inCluster = v1alpha1.KubernetesInternalAPIServerAddr
)

func newTestScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, v1alpha1.AddToScheme(s))
	return s
}

// newFakeClient returns a fake client with the owner index, status subresources, and UIDs
// assigned on create (as the API server does).
func newFakeClient(t testing.TB, objs ...client.Object) client.WithWatch {
	t.Helper()
	for _, o := range objs {
		if o.GetUID() == "" {
			o.SetUID(types.UID(uuid.NewString()))
		}
	}
	return fake.NewClientBuilder().
		WithScheme(newTestScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.ApplicationSet{}, &v1alpha1.Application{}, &v1alpha1.Operation{}).
		WithIndex(&v1alpha1.Application{}, OwnerIndex, OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(uuid.NewString()))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
}

// recorder collects events.
type recorder struct {
	mu     sync.Mutex
	events []string
}

var _ events.EventRecorder = (*recorder)(nil)

func (r *recorder) Eventf(_ runtime.Object, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf("%s %s %s %s", eventtype, reason, action, fmt.Sprintf(note, args...)))
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// fixedClock is a settable clock.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestReconciler(t testing.TB, c client.Client, mutate func(*Config)) (*Reconciler, *recorder) {
	t.Helper()
	cfg := Config{Namespace: controlNS, ApplicationSetNamespaces: []string{"team-*"}}
	if mutate != nil {
		mutate(&cfg)
	}
	rec := &recorder{}
	r, err := NewReconciler(cfg, c, c, rec)
	require.NoError(t, err)
	return r, rec
}

func appProject(name string, mutate ...func(*v1alpha1.AppProject)) *v1alpha1.AppProject {
	p := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func clusterObj(name, server string) *v1alpha1.Cluster {
	return &v1alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: controlNS},
		Spec:       v1alpha1.ClusterSpec{Name: name, Server: server},
	}
}

func jsonList(t testing.TB, elems ...map[string]any) []apiextensionsv1.JSON {
	t.Helper()
	out := make([]apiextensionsv1.JSON, 0, len(elems))
	for _, e := range elems {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		out = append(out, apiextensionsv1.JSON{Raw: b})
	}
	return out
}

// listAppSet returns an ApplicationSet with a List generator producing one Application per
// element ({{name}} → app name, {{env}} → env label).
func listAppSet(t testing.TB, namespace, name string, elems ...map[string]any) *v1alpha1.ApplicationSet {
	t.Helper()
	return &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.ApplicationSetSpec{
			Generators: []v1alpha1.ApplicationSetGenerator{{List: &v1alpha1.ListGenerator{Elements: jsonList(t, elems...)}}},
			Template: v1alpha1.ApplicationSetTemplate{
				ApplicationSetTemplateMeta: v1alpha1.ApplicationSetTemplateMeta{
					Name:   "{{name}}",
					Labels: map[string]string{"env": "{{env}}"},
				},
				Spec: v1alpha1.ApplicationSpec{
					Project: "default",
					Source:  &v1alpha1.ApplicationSource{RepoURL: "https://git.example/repo.git", Path: "apps/{{name}}", TargetRevision: "HEAD"},
					Destination: v1alpha1.ApplicationDestination{
						Server: inCluster, Namespace: "{{name}}",
					},
				},
			},
		},
	}
}

func elem(name, env string) map[string]any { return map[string]any{"name": name, "env": env} }

func reconcileOnce(t testing.TB, r *Reconciler, appSet *v1alpha1.ApplicationSet) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), requestFor(appSet))
	require.NoError(t, err)
}

func requestFor(appSet *v1alpha1.ApplicationSet) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: appSet.Namespace, Name: appSet.Name}}
}

func getAppSet(t testing.TB, c client.Client, namespace, name string) *v1alpha1.ApplicationSet {
	t.Helper()
	as := &v1alpha1.ApplicationSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, as))
	return as
}

func listApps(t testing.TB, c client.Client, namespace string) map[string]v1alpha1.Application {
	t.Helper()
	var list v1alpha1.ApplicationList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(namespace)))
	out := map[string]v1alpha1.Application{}
	for _, a := range list.Items {
		out[a.Name] = a
	}
	return out
}

func findCondition(as *v1alpha1.ApplicationSet, typ string) *metav1.Condition {
	for i := range as.Status.Conditions {
		if as.Status.Conditions[i].Type == typ {
			return &as.Status.Conditions[i]
		}
	}
	return nil
}

func conditionSummary(as *v1alpha1.ApplicationSet) []string {
	var out []string
	for _, c := range as.Status.Conditions {
		out = append(out, strings.Join([]string{c.Type, string(c.Status), c.Reason, c.Message}, "|"))
	}
	return out
}
