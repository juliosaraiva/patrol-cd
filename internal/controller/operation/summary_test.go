package operation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	syncpkg "github.com/juliosaraiva/patrol-cd/internal/sync"
)

func ts(s int) *metav1.Time { t := metav1.NewTime(time.Unix(int64(s), 0)); return &t }

func TestShouldSummarize(t *testing.T) {
	t.Parallel()
	op := func(name string, phase v1alpha1.OperationPhase, started *metav1.Time) *v1alpha1.Operation {
		return &v1alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: v1alpha1.OperationStatus{Phase: phase, StartedAt: started}}
	}
	cur := &v1alpha1.ApplicationOperationState{OperationName: "old", Phase: v1alpha1.OperationSucceeded, StartedAt: ts(10)}
	assert.False(t, shouldSummarize(cur, op("rejected", v1alpha1.OperationRejected, nil)), "never admitted")
	assert.True(t, shouldSummarize(nil, op("first", v1alpha1.OperationSucceeded, ts(5))))
	assert.True(t, shouldSummarize(cur, op("old", v1alpha1.OperationFailed, ts(10))), "same operation")
	assert.True(t, shouldSummarize(cur, op("new", v1alpha1.OperationRunning, ts(9))), "the active operation takes over")
	assert.True(t, shouldSummarize(cur, op("newer", v1alpha1.OperationSucceeded, ts(11))))
	assert.False(t, shouldSummarize(cur, op("older", v1alpha1.OperationSucceeded, ts(10))), "a terminal operation never replaces a summary of the same start")
}

func TestSummary(t *testing.T) {
	t.Parallel()
	src := &v1alpha1.ApplicationSource{RepoURL: "r", Path: "p"}
	op := &v1alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "op"},
		Spec:       v1alpha1.OperationSpec{InitiatedBy: v1alpha1.OperationInitiator{Username: "alice"}},
		Status: v1alpha1.OperationStatus{
			Phase: v1alpha1.OperationSucceeded, Message: "done", StartedAt: ts(1), FinishedAt: ts(2), RetryCount: 1,
			SyncResult: &v1alpha1.SyncOperationResult{Revision: "sha", Source: src, Resources: []v1alpha1.ResourceResult{{Name: "x"}}},
		},
	}
	s := Summary(op)
	assert.Equal(t, "op", s.OperationName)
	assert.Equal(t, v1alpha1.OperationSucceeded, s.Phase)
	assert.Equal(t, int64(1), s.RetryCount)
	assert.Equal(t, "alice", s.InitiatedBy.Username)
	require.NotNil(t, s.SyncResult)
	assert.Equal(t, "sha", s.SyncResult.Revision)
	assert.Equal(t, src, s.SyncResult.Source)
	assert.NotSame(t, src, s.SyncResult.Source, "the summary owns its copy")
	op.Status.SyncResult = &v1alpha1.SyncOperationResult{Resources: []v1alpha1.ResourceResult{{Name: "x"}}}
	assert.Nil(t, Summary(op).SyncResult, "results without revisions are not summarized")
}

func TestFormatErrorConditionsAndDigest(t *testing.T) {
	t.Parallel()
	msg := formatErrorConditions([]compare.Condition{
		{Type: v1alpha1.ApplicationConditionComparisonError, Message: "a"},
		{Type: v1alpha1.ApplicationConditionSharedResourceWarning, Message: "ignored"},
		{Type: v1alpha1.ApplicationConditionInvalidSpecError, Message: "b"},
	})
	assert.Equal(t, "ComparisonError: a;InvalidSpecError: b", msg)
	assert.Empty(t, formatErrorConditions(nil))

	r1 := &compare.Result{ManifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{"b", "a"}}}}
	r2 := &compare.Result{ManifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{"a"}}, {Manifests: []string{"b"}}}}
	assert.Equal(t, renderedDigest(r1, &syncpkg.Input{}), renderedDigest(r2, &syncpkg.Input{}), "order independent")
	r3 := &compare.Result{ManifestResponses: []*apiclient.ManifestResponse{{Manifests: []string{"a", "c"}}}}
	assert.NotEqual(t, renderedDigest(r1, &syncpkg.Input{}), renderedDigest(r3, &syncpkg.Input{}))
	assert.Equal(t, syncpkg.Digest(nil), renderedDigest(&compare.Result{}, &syncpkg.Input{}), "local manifests fall back to the target digest")
	assert.Equal(t, "abc", truncate("abcdef", 3))
	assert.Equal(t, "a", truncate("aé", 2), "never splits a UTF-8 sequence")
}

func TestMergeSyncOptions(t *testing.T) {
	t.Parallel()
	assert.Nil(t, MergeSyncOptions(nil, nil))
	assert.Equal(t, v1alpha1.SyncOptions{"Prune=false"}, MergeSyncOptions(v1alpha1.SyncOptions{"Prune=false"}, nil))
	assert.Equal(t,
		v1alpha1.SyncOptions{"Prune=true", "ServerSideApply=true", "CreateNamespace=true"},
		MergeSyncOptions(v1alpha1.SyncOptions{"CreateNamespace=true", "Prune=false"}, v1alpha1.SyncOptions{"Prune=true", "ServerSideApply=true", "Prune=confirm"}),
		"operation options win, keys are unique")
	assert.Equal(t, v1alpha1.SyncOptions{"A=1"}, MergeSyncOptions(v1alpha1.SyncOptions{"malformed"}, v1alpha1.SyncOptions{"A=1"}))
}

func TestApplicationChangedPredicate(t *testing.T) {
	t.Parallel()
	p := applicationChanged()
	base := &v1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: "a", Generation: 1, Annotations: map[string]string{"x": "1"}}}
	statusOnly := base.DeepCopy()
	statusOnly.Status.Sync.Status = v1alpha1.SyncStatusCodeSynced
	assert.False(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}), "status-only updates are filtered")
	spec := base.DeepCopy()
	spec.Generation = 2
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: spec}))
	annotated := base.DeepCopy()
	annotated.Annotations[v1alpha1.AnnotationKeySkipReconcile] = "true"
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: annotated}))
	deleting := base.DeepCopy()
	deleting.DeletionTimestamp = ts(3)
	assert.True(t, p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: deleting}))
}
