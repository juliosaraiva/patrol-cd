package appset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestRunValidation(t *testing.T) {
	t.Parallel()
	require.ErrorContains(t, Run(context.Background(), RunConfig{}), "REST config is required")
	require.ErrorContains(t, Run(context.Background(), RunConfig{RestConfig: &rest.Config{}}), "namespace is required")
	require.Error(t, Run(context.Background(), RunConfig{RestConfig: &rest.Config{}, Config: Config{Namespace: "x", ApplicationSetNamespaces: []string{"/(/"}}}))
	_, err := NewReconciler(Config{Namespace: controlNS}, nil, nil, nil)
	require.Error(t, err)
}

// countingReader records which reader served a request.
type countingReader struct {
	client.Reader
	calls int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.calls++
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.calls++
	return c.Reader.List(ctx, list, opts...)
}

func TestSplitReader(t *testing.T) {
	t.Parallel()
	base := newFakeClient(t,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: tenantNS}},
		clusterObj("c", "https://c"),
	)
	cached, live := &countingReader{Reader: base}, &countingReader{Reader: base}
	s := &splitReader{cached: cached, live: live}
	ctx := context.Background()
	require.NoError(t, s.Get(ctx, client.ObjectKey{Namespace: tenantNS, Name: "s"}, &corev1.Secret{}))
	require.NoError(t, s.List(ctx, &corev1.ConfigMapList{}))
	assert.Equal(t, 2, live.calls, "Secrets and ConfigMaps are read from the API server")
	require.NoError(t, s.List(ctx, &v1alpha1.ClusterList{}))
	assert.Equal(t, 1, cached.calls, "everything else from the cache")
}

// The cache may not have observed an Application created by a previous reconcile yet: the
// create fails with AlreadyExists and the controller updates the live object instead.
func TestReconcileStaleCacheCreate(t *testing.T) {
	t.Parallel()
	as := listAppSet(t, controlNS, "set", elem("a", "dev"))
	existing := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: controlNS}}
	existing.Spec.Project = "default"
	existing.Spec.Source = &v1alpha1.ApplicationSource{RepoURL: "old"}
	base := newFakeClient(t, as, existing, appProject("default"))
	stale := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.Application); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: v1alpha1.GroupName, Resource: "applications"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r, err := NewReconciler(Config{Namespace: controlNS}, stale, base, nil)
	require.NoError(t, err)
	_, err = r.Reconcile(context.Background(), requestFor(as))
	require.NoError(t, err)
	app := listApps(t, base, controlNS)["a"]
	assert.Equal(t, "https://git.example/repo.git", app.Spec.Source.RepoURL)
	assert.Equal(t, "set", metav1.GetControllerOf(&app).Name)
}
