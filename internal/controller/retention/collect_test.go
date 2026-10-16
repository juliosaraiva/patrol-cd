package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

// countingReader counts the API-server lists of retention.
type countingReader struct {
	client.Reader
	lists int
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	return r.Reader.List(ctx, list, opts...)
}

func TestNeeded(t *testing.T) {
	t.Parallel()
	ops := []v1alpha1.Operation{op("a", 1, v1alpha1.OperationSucceeded), op("b", 2, v1alpha1.OperationRunning), op("c", 3, v1alpha1.OperationRejected)}
	assert.True(t, Needed(ops, 1))
	assert.False(t, Needed(ops, 2), "non-terminal operations do not count")
	assert.True(t, Needed(ops, -1), "a negative limit keeps none")
	assert.False(t, Needed(nil, 0))
}

// TestCollectIfNeeded: the API server is listed only when the cached Operations exceed the
// limit; the Operation being completed counts with its new phase.
func TestCollectIfNeeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	mk := func(name string, created int, phase v1alpha1.OperationPhase) *v1alpha1.Operation {
		o := op(name, created, phase)
		o.Namespace, o.UID = "ns", types.UID("uid-"+name)
		o.CreationTimestamp = metav1.NewTime(time.Unix(int64(created), 0))
		o.Spec.Application = "app"
		o.Labels = map[string]string{v1alpha1.LabelKeyOperationApplication: "app"}
		return &o
	}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithIndex(&v1alpha1.Operation{}, core.OperationApplicationIndex, func(o client.Object) []string {
			return []string{o.(*v1alpha1.Operation).Spec.Application}
		}).
		WithObjects(mk("o1", 1, v1alpha1.OperationSucceeded), mk("o2", 2, v1alpha1.OperationFailed), mk("o3", 3, v1alpha1.OperationRunning)).
		Build()
	app := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"}, Spec: v1alpha1.ApplicationSpec{RevisionHistoryLimit: ptr.To[int64](2)}}
	// The API server already holds o3's completion; the cache (c) still shows it Running.
	server := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(mk("o1", 1, v1alpha1.OperationSucceeded), mk("o2", 2, v1alpha1.OperationFailed), mk("o3", 3, v1alpha1.OperationSucceeded)).
		Build()
	live := &countingReader{Reader: server}

	n, err := CollectIfNeeded(ctx, c, live, app, nil, nil)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Zero(t, live.lists, "within the limit: no API-server list")

	done := mk("o3", 3, v1alpha1.OperationSucceeded)
	n, err = CollectIfNeeded(ctx, c, live, app, done, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Positive(t, live.lists)
	err = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "o1"}, &v1alpha1.Operation{})
	assert.True(t, apierrors.IsNotFound(err), "the oldest terminal operation is deleted: %v", err)
	for _, name := range []string{"o2", "o3"} {
		assert.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &v1alpha1.Operation{}))
	}

	_, err = CollectIfNeeded(ctx, c, &failingReader{}, app, nil, nil)
	assert.NoError(t, err, "the cache shows 2 terminal operations again: nothing to list")
}

type failingReader struct{ client.Reader }

func (failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("unexpected list")
}
