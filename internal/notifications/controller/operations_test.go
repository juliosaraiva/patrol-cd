package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func fakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	scheme, err := newScheme()
	require.NoError(t, err)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Operation{}).Build()
}

func testOperation(name, app string, phase v1alpha1.OperationPhase) *v1alpha1.Operation {
	return &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.OperationSpec{
			Application: app,
			Sync:        &v1alpha1.SyncOperation{Revision: "abc", Manifests: []string{"kind: ConfigMap"}},
			InitiatedBy: v1alpha1.OperationInitiator{Username: "alice"},
		},
		Status: v1alpha1.OperationStatus{
			Phase:      phase,
			Message:    "msg",
			StartedAt:  timePtr("2026-01-01T00:00:00Z"),
			Conditions: []metav1.Condition{{Type: "X", Status: metav1.ConditionTrue, Reason: "Y", LastTransitionTime: mustTime("2026-01-01T00:00:00Z")}},
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "abc", Resources: []v1alpha1.ResourceResult{{Kind: "ConfigMap", Name: "cm", Version: "v1"}}},
			Progress:   &v1alpha1.OperationProgress{Wave: 1},
		},
	}
}

func TestOperationGetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fakeClient(t, testOperation("done", "app", v1alpha1.OperationSucceeded), testOperation("running", "app", v1alpha1.OperationRunning))
	g := NewOperationGetter(c, 0).(*operationGetter)

	op, err := g.GetOperation(ctx, "apps", "done")
	require.NoError(t, err)
	spec := op["spec"].(map[string]any)
	status := op["status"].(map[string]any)
	assert.Equal(t, "app", spec["application"])
	assert.NotContains(t, spec["sync"], "manifests", "rendered manifests are never kept")
	assert.Equal(t, "Succeeded", status["phase"])
	assert.NotContains(t, status, "conditions")
	assert.NotContains(t, status, "progress")
	assert.NotContains(t, op, "metadata")
	assert.Equal(t, 1, g.cache.len(), "terminal Operations are cached")

	// A terminal Operation no longer changes: served from the cache.
	require.NoError(t, c.Delete(ctx, &v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: "apps"}}))
	again, err := g.GetOperation(ctx, "apps", "done")
	require.NoError(t, err)
	assert.Equal(t, op, again)

	// Non-terminal Operations are always read.
	_, err = g.GetOperation(ctx, "apps", "running")
	require.NoError(t, err)
	assert.Equal(t, 1, g.cache.len())
	running := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "apps", Name: "running"}, running))
	running.Status.Phase = v1alpha1.OperationFailed
	require.NoError(t, c.Status().Update(ctx, running))
	op, err = g.GetOperation(ctx, "apps", "running")
	require.NoError(t, err)
	assert.Equal(t, "Failed", op["status"].(map[string]any)["phase"])

	_, err = g.GetOperation(ctx, "apps", "missing")
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "NotFound is preserved for TemplateApplication")
}

func TestWeightedLRU(t *testing.T) {
	t.Parallel()
	c := newWeightedLRU(800)
	v := func(i int) map[string]any { return map[string]any{"i": i} }
	for i := range 10 {
		c.add(fmt.Sprint(i), v(i), 100)
	}
	assert.Equal(t, 8, c.len(), "bounded by weight")
	_, ok := c.get("0")
	assert.False(t, ok, "least recently used evicted")
	_, ok = c.get("9")
	assert.True(t, ok)

	// get refreshes recency.
	_, _ = c.get("2")
	c.add("10", v(10), 100)
	_, ok = c.get("2")
	assert.True(t, ok)
	_, ok = c.get("3")
	assert.False(t, ok)

	// Replacing a key keeps one entry and the right weight.
	c.add("10", v(11), 50)
	got, ok := c.get("10")
	require.True(t, ok)
	assert.Equal(t, 11, got["i"])
	assert.Equal(t, int64(750), c.used)

	// Values above an eighth of the budget are not kept; zero weights count as one.
	c.add("big", v(0), 101)
	_, ok = c.get("big")
	assert.False(t, ok)
	c.add("zero", v(0), 0)
	_, ok = c.get("zero")
	assert.True(t, ok)
}

func TestWeightedLRUConcurrent(t *testing.T) {
	t.Parallel()
	c := newWeightedLRU(1 << 10)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 500 {
				k := fmt.Sprint((i + j) % 37)
				c.add(k, map[string]any{}, int64(j%100+1))
				_, _ = c.get(k)
			}
		})
	}
	wg.Wait()
	assert.LessOrEqual(t, c.used, c.max)
}
