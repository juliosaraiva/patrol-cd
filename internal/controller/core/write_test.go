package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Operation{}).Build()
}

func testOperation(name, app string, labels map[string]string) *v1alpha1.Operation {
	return &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Spec:       v1alpha1.OperationSpec{Application: app, Sync: &v1alpha1.SyncOperation{}},
	}
}

// TestPatchStatusStrict: a writer holding a stale object never overwrites a newer status:
// the strict write fails with ErrConflict (also a 409) and leaves both the stored object and
// the caller's copy unchanged, while PatchStatus re-reads and re-applies a mutate derived
// from the object.
func TestPatchStatusStrict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := fakeClient(t, testOperation("op", "app", nil))
	stale := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "op"}, stale))

	newer := stale.DeepCopy()
	newer.Status.Phase = v1alpha1.OperationCancelled
	require.NoError(t, c.Status().Update(ctx, newer))

	work := stale.DeepCopy()
	changed, err := PatchStatusStrict(ctx, c, work, func(o *v1alpha1.Operation) error {
		o.Status.Phase = v1alpha1.OperationSucceeded
		return nil
	})
	require.Error(t, err)
	assert.False(t, changed)
	assert.True(t, errors.Is(err, ErrConflict), err)
	assert.True(t, apierrors.IsConflict(err), err)
	assert.Equal(t, stale.ResourceVersion, work.ResourceVersion, "the caller's object is not replaced")
	stored := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(stale), stored))
	assert.Equal(t, v1alpha1.OperationCancelled, stored.Status.Phase, "the newer status is kept")

	// A fresh object writes.
	changed, err = PatchStatusStrict(ctx, c, stored, func(o *v1alpha1.Operation) error {
		o.Status.Message = "done"
		return nil
	})
	require.NoError(t, err)
	assert.True(t, changed)

	// The re-reading variant applies a derived mutate over the newer object.
	work = stale.DeepCopy()
	changed, err = PatchStatus(ctx, c, c, work, func(o *v1alpha1.Operation) error {
		o.Status.Message += "!"
		return nil
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "done!", work.Status.Message)
	assert.Equal(t, v1alpha1.OperationCancelled, work.Status.Phase)

	// Metadata writes behave the same way.
	_, err = PatchObjectStrict(ctx, c, stale.DeepCopy(), func(o *v1alpha1.Operation) error {
		o.Labels = map[string]string{"x": "y"}
		return nil
	})
	assert.True(t, errors.Is(err, ErrConflict), err)
}

// TestListOperationsLive: Operations are listed by the application label plus the unlabelled
// ones, both filtered by spec.application.
func TestListOperationsLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	long := strings.Repeat("a", 70)
	label := func(app string) map[string]string {
		return map[string]string{v1alpha1.LabelKeyOperationApplication: app}
	}
	c := fakeClient(t,
		testOperation("a-labelled", "a", label("a")),
		testOperation("a-unlabelled", "a", nil),
		testOperation("b-labelled", "b", label("b")),
		testOperation("b-unlabelled", "b", nil),
		testOperation("mislabelled", "b", label("a")),
		testOperation("long-op", long, nil),
	)
	names := func(ops []v1alpha1.Operation) []string {
		var out []string
		for _, op := range ops {
			out = append(out, op.Name)
		}
		return out
	}
	ops, err := ListOperationsLive(ctx, c, "ns", "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a-labelled", "a-unlabelled"}, names(ops))
	ops, err = ListOperationsLive(ctx, c, "ns", "b")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b-labelled", "b-unlabelled"}, names(ops), "a label naming another application is not trusted")
	ops, err = ListOperationsLive(ctx, c, "ns", long)
	require.NoError(t, err)
	assert.Equal(t, []string{"long-op"}, names(ops))
	ops, err = ListOperationsLive(ctx, c, "other", "a")
	require.NoError(t, err)
	assert.Empty(t, ops)

	assert.Equal(t, "a", ApplicationLabelValue("a"))
	assert.Empty(t, ApplicationLabelValue(long))
}
