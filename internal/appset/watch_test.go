package appset

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestShouldRequeueForApplication(t *testing.T) {
	t.Parallel()
	base := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{}}}
	base.Spec.Project = "default"
	mod := func(f func(*v1alpha1.Application)) *v1alpha1.Application {
		c := base.DeepCopy()
		f(c)
		return c
	}
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Minute))
	tests := []struct {
		name        string
		cur         *v1alpha1.Application
		plain, prog bool
	}{
		{"nil vs empty labels", mod(func(a *v1alpha1.Application) { a.Labels = nil }), false, false},
		{"spec", mod(func(a *v1alpha1.Application) { a.Spec.Project = "other" }), true, true},
		{"annotation", mod(func(a *v1alpha1.Application) { a.Annotations = map[string]string{"x": "y"} }), true, true},
		{"finalizer", mod(func(a *v1alpha1.Application) { a.Finalizers = []string{"f"} }), true, true},
		{"reconciledAt only", mod(func(a *v1alpha1.Application) { a.Status.ReconciledAt = &now }), false, false},
		{"health", mod(func(a *v1alpha1.Application) { a.Status.Health.Status = v1alpha1.HealthStatusHealthy }), false, true},
		{"sync", mod(func(a *v1alpha1.Application) { a.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced }), false, true},
		{"operation appears", mod(func(a *v1alpha1.Application) {
			a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationRunning, StartedAt: &now}
		}), false, true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.plain, ShouldRequeueForApplication(base, tt.cur, false), tt.name)
		assert.Equal(t, tt.prog, ShouldRequeueForApplication(base, tt.cur, true), tt.name+" (progressive)")
	}
	withOp := mod(func(a *v1alpha1.Application) {
		a.Status.OperationState = &v1alpha1.ApplicationOperationState{Phase: v1alpha1.OperationRunning, StartedAt: &now}
	})
	restarted := withOp.DeepCopy()
	restarted.Status.OperationState.StartedAt = &later
	assert.True(t, ShouldRequeueForApplication(withOp, restarted, true))
	assert.False(t, ShouldRequeueForApplication(withOp, withOp.DeepCopy(), true))
	assert.False(t, ShouldRequeueForApplication(nil, base, true))
}

func TestShouldRequeueForApplicationSet(t *testing.T) {
	t.Parallel()
	base := listAppSet(t, controlNS, "set", elem("a", "dev"))
	mod := func(f func(*v1alpha1.ApplicationSet)) *v1alpha1.ApplicationSet {
		c := base.DeepCopy()
		f(c)
		return c
	}
	withRefresh := mod(func(a *v1alpha1.ApplicationSet) {
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyApplicationSetRefresh: "true"}
	})
	now := metav1.Now()
	tests := []struct {
		name        string
		old, cur    *v1alpha1.ApplicationSet
		plain, prog bool
	}{
		{"spec", base, mod(func(a *v1alpha1.ApplicationSet) { a.Spec.GoTemplate = true }), true, true},
		{"status conditions only", base, mod(func(a *v1alpha1.ApplicationSet) {
			a.Status.Conditions = []metav1.Condition{{Type: "X"}}
		}), false, false},
		{"application status", base, mod(func(a *v1alpha1.ApplicationSet) {
			a.Status.ApplicationStatus = []v1alpha1.ApplicationSetApplicationStatus{{Application: "a", Status: v1alpha1.ProgressiveSyncWaiting}}
		}), false, true},
		{"refresh added", base, withRefresh, true, true},
		{"refresh removed", withRefresh, base, false, false},
		{"deletion", base, mod(func(a *v1alpha1.ApplicationSet) { a.DeletionTimestamp = &now }), true, true},
		{"labels", base, mod(func(a *v1alpha1.ApplicationSet) { a.Labels = map[string]string{"a": "b"} }), true, true},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.plain, ShouldRequeueForApplicationSet(tt.old, tt.cur, false), tt.name)
		assert.Equal(t, tt.prog, ShouldRequeueForApplicationSet(tt.old, tt.cur, true), tt.name+" (progressive)")
	}
}

func rawGen(t *testing.T, v any) *apiextensionsv1.JSON {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return &apiextensionsv1.JSON{Raw: b}
}

func TestUsesClustersGenerator(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	top := &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{Generators: []v1alpha1.ApplicationSetGenerator{{Clusters: &v1alpha1.ClusterGenerator{}}}}}
	assert.True(t, usesClustersGenerator(log, top))
	list := listAppSet(t, controlNS, "l", elem("a", "dev"))
	assert.False(t, usesClustersGenerator(log, list))

	matrix := &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{Generators: []v1alpha1.ApplicationSetGenerator{{Matrix: &v1alpha1.MatrixGenerator{
		Generators: []v1alpha1.ApplicationSetNestedGenerator{{List: &v1alpha1.ListGenerator{}}, {Clusters: &v1alpha1.ClusterGenerator{}}},
	}}}}}
	assert.True(t, usesClustersGenerator(log, matrix))

	nested := &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{Generators: []v1alpha1.ApplicationSetGenerator{{Merge: &v1alpha1.MergeGenerator{
		Generators: []v1alpha1.ApplicationSetNestedGenerator{
			{List: &v1alpha1.ListGenerator{}},
			{Matrix: rawGen(t, map[string]any{"generators": []any{map[string]any{"list": map[string]any{}}, map[string]any{"clusters": map[string]any{}}}})},
		},
	}}}}}
	assert.True(t, usesClustersGenerator(log, nested))

	malformed := &v1alpha1.ApplicationSet{Spec: v1alpha1.ApplicationSetSpec{Generators: []v1alpha1.ApplicationSetGenerator{{Matrix: &v1alpha1.MatrixGenerator{
		Generators: []v1alpha1.ApplicationSetNestedGenerator{{List: &v1alpha1.ListGenerator{}}, {Merge: &apiextensionsv1.JSON{Raw: []byte("{")}}},
	}}}}}
	assert.False(t, usesClustersGenerator(log, malformed))
}

func TestClusterEventHandler(t *testing.T) {
	t.Parallel()
	clusterSet := &v1alpha1.ApplicationSet{
		ObjectMeta: metav1.ObjectMeta{Name: "clusters", Namespace: tenantNS},
		Spec:       v1alpha1.ApplicationSetSpec{Generators: []v1alpha1.ApplicationSetGenerator{{Clusters: &v1alpha1.ClusterGenerator{}}}},
	}
	outside := clusterSet.DeepCopy()
	outside.Namespace = "other"
	list := listAppSet(t, tenantNS, "list", elem("a", "dev"))
	c := newFakeClient(t, clusterSet, outside, list)
	m, err := NewNamespaceMatcher(controlNS, []string{"team-*"})
	require.NoError(t, err)
	h := &clusterEventHandler{client: c, namespace: controlNS, namespaces: m, log: slog.New(slog.DiscardHandler)}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	h.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: clusterObj("c1", "https://c1")}, q)
	require.Equal(t, 1, q.Len())
	item, _ := q.Get()
	assert.Equal(t, "clusters", item.Name)
	assert.Equal(t, tenantNS, item.Namespace)
	q.Done(item)

	// Clusters outside the control-plane namespace are ignored.
	other := clusterObj("c2", "https://c2")
	other.Namespace = tenantNS
	h.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: other}, q)
	assert.Equal(t, 0, q.Len())

	// The controller's status writes do not requeue; changes a generator can see do.
	cur := clusterObj("c1", "https://c1")
	cur.Generation = 2
	statusOnly := cur.DeepCopy()
	statusOnly.Status.ServerVersion = "v1.36.0"
	h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectOld: cur, ObjectNew: statusOnly}, q)
	assert.Equal(t, 0, q.Len(), "status-only update")

	changes := map[string]func(*v1alpha1.Cluster){
		"spec":        func(c *v1alpha1.Cluster) { c.Generation = 3 },
		"labels":      func(c *v1alpha1.Cluster) { c.Labels = map[string]string{"env": "prod"} },
		"annotations": func(c *v1alpha1.Cluster) { c.Annotations = map[string]string{"team": "a"} },
	}
	for name, change := range changes {
		next := cur.DeepCopy()
		change(next)
		h.Update(context.Background(), event.TypedUpdateEvent[client.Object]{ObjectOld: cur, ObjectNew: next}, q)
		require.Equal(t, 1, q.Len(), name)
		item, _ := q.Get()
		q.Done(item)
		q.Forget(item)
	}
}
