package operation

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/controller/core"
)

func staleStepFixture(t *testing.T, stored *v1alpha1.Operation) (*Reconciler, client.Client) {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(sch))
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(stored).WithStatusSubresource(&v1alpha1.Operation{}).Build()
	r := &Reconciler{
		cfg: Config{Deps: core.Deps{Client: c, APIReader: c, Clock: clocktesting.NewFakeClock(time.Unix(1000, 0))}},
		c:   c,
		log: slog.New(slog.DiscardHandler),
	}
	return r, c
}

// TestStaleStepIsAbandoned: a step computed from an Operation that another writer changed in
// the meantime (a lease handover, a cancel) fails with core.ErrConflict instead of applying
// its stale values (sync-state-machine.md); a terminal phase is never overwritten.
func TestStaleStepIsAbandoned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	started := metav1.NewTime(time.Unix(900, 0))
	op := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "op", Namespace: "ns"},
		Spec:       v1alpha1.OperationSpec{Application: "app", Sync: &v1alpha1.SyncOperation{}, Retry: &v1alpha1.RetryStrategy{Limit: 5}},
	}
	r, c := staleStepFixture(t, op)
	key := client.ObjectKeyFromObject(op)
	stale := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, key, stale))
	stale.Status = v1alpha1.OperationStatus{Phase: v1alpha1.OperationRunning, StartedAt: &started, RetryCount: 1}
	require.NoError(t, c.Status().Update(ctx, stale))
	require.NoError(t, c.Get(ctx, key, stale))

	// Another writer moves the Operation on: a further retry, then cancellation.
	newer := stale.DeepCopy()
	newer.Status.RetryCount = 2
	newer.Status.Phase = v1alpha1.OperationCancelled
	newer.Status.Message = "Operation terminated"
	require.NoError(t, c.Status().Update(ctx, newer))

	for name, run := range map[string]func(s *step) error{
		"completion": func(s *step) error {
			_, err := s.complete(ctx, v1alpha1.OperationSucceeded, "successfully synced", v1alpha1.OperationReasonSucceeded)
			return err
		},
		"retry scheduling": func(s *step) error {
			_, err := s.attemptFailed(ctx, v1alpha1.OperationFailed, "boom", v1alpha1.OperationReasonSyncFailed)
			return err
		},
		"progress": func(s *step) error {
			return s.patch(ctx, func(st *v1alpha1.OperationStatus) { st.Message = "waiting for healthy state" })
		},
	} {
		s := &step{r: r, op: stale.DeepCopy(), log: r.log}
		err := run(s)
		require.Error(t, err, name)
		assert.True(t, errors.Is(err, core.ErrConflict), "%s: %v", name, err)
		got := &v1alpha1.Operation{}
		require.NoError(t, c.Get(ctx, key, got))
		assert.Equal(t, v1alpha1.OperationCancelled, got.Status.Phase, name)
		assert.Equal(t, int64(2), got.Status.RetryCount, name)
		assert.Equal(t, "Operation terminated", got.Status.Message, name)
	}

	// Even on the current object, a terminal phase is not replaced.
	cur := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, key, cur))
	s := &step{r: r, op: cur, log: r.log}
	_, err := s.complete(ctx, v1alpha1.OperationSucceeded, "late", v1alpha1.OperationReasonSucceeded)
	assert.True(t, errors.Is(err, errTerminalOverwrite), err)
	got := &v1alpha1.Operation{}
	require.NoError(t, c.Get(ctx, key, got))
	assert.Equal(t, v1alpha1.OperationCancelled, got.Status.Phase)
}
