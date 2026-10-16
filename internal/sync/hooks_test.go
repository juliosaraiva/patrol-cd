package sync

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

func TestHookTypes(t *testing.T) {
	t.Parallel()
	hookKey, helmKey := v1alpha1.AnnotationKeyHook, AnnotationKeyHelmHook
	tests := []struct {
		name    string
		ann     []string
		isHook  bool
		skip    bool
		ignored bool
		types   []v1alpha1.HookType
		phases  []v1alpha1.SyncPhase
	}{
		{name: "plain resource", isHook: false, phases: []v1alpha1.SyncPhase{"Sync"}},
		{name: "PreSync", ann: []string{hookKey, "PreSync"}, isHook: true, types: []v1alpha1.HookType{"PreSync"}, phases: []v1alpha1.SyncPhase{"PreSync"}},
		{name: "CSV trimmed and deduped, phase order", ann: []string{hookKey, " PostSync , PreSync,PostSync"}, isHook: true,
			types: []v1alpha1.HookType{"PostSync", "PreSync"}, phases: []v1alpha1.SyncPhase{"PreSync", "PostSync"}},
		{name: "SyncFail", ann: []string{hookKey, "SyncFail"}, isHook: true, types: []v1alpha1.HookType{"SyncFail"}, phases: []v1alpha1.SyncPhase{"SyncFail"}},
		{name: "Skip alone", ann: []string{hookKey, "Skip"}, skip: true, types: []v1alpha1.HookType{"Skip"}},
		{name: "Skip with PreSync is a PreSync hook", ann: []string{hookKey, "Skip,PreSync"}, isHook: true,
			types: []v1alpha1.HookType{"Skip", "PreSync"}, phases: []v1alpha1.SyncPhase{"PreSync"}},
		{name: "unknown value is ignored", ann: []string{hookKey, "Foo"}, isHook: true, ignored: true},
		{name: "PreDelete is not a sync type", ann: []string{hookKey, "PreDelete"}, isHook: true, ignored: true},
		{name: "helm pre-install", ann: []string{helmKey, "pre-install"}, isHook: true, types: []v1alpha1.HookType{"PreSync"}, phases: []v1alpha1.SyncPhase{"PreSync"}},
		{name: "helm pre-install,pre-upgrade dedup by phase", ann: []string{helmKey, "pre-install,pre-upgrade"}, isHook: true,
			types: []v1alpha1.HookType{"PreSync", "PreSync"}, phases: []v1alpha1.SyncPhase{"PreSync"}},
		{name: "helm post-upgrade", ann: []string{helmKey, "post-upgrade"}, isHook: true, types: []v1alpha1.HookType{"PostSync"}, phases: []v1alpha1.SyncPhase{"PostSync"}},
		{name: "helm crd-install is a resource", ann: []string{helmKey, "crd-install"}, phases: []v1alpha1.SyncPhase{"Sync"}},
		{name: "helm test is ignored", ann: []string{helmKey, "test"}, isHook: true, ignored: true},
		{name: "helm pre-rollback is ignored", ann: []string{helmKey, "pre-rollback"}, isHook: true, ignored: true},
		{name: "patrol type wins over helm", ann: []string{hookKey, "Sync", helmKey, "pre-install"}, isHook: true,
			types: []v1alpha1.HookType{"Sync"}, phases: []v1alpha1.SyncPhase{"Sync"}},
		{name: "invalid patrol type falls back to helm", ann: []string{hookKey, "Foo", helmKey, "post-install"}, isHook: true,
			types: []v1alpha1.HookType{"PostSync"}, phases: []v1alpha1.SyncPhase{"PostSync"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			obj := annotate(pod("p"), tt.ann...)
			assert.Equal(t, tt.isHook, IsHook(obj), "IsHook")
			assert.Equal(t, tt.skip, IsSkip(obj), "IsSkip")
			assert.Equal(t, tt.ignored, IsIgnored(obj), "IsIgnored")
			assert.Equal(t, tt.types, HookTypes(obj), "HookTypes")
			assert.Equal(t, tt.phases, SyncPhases(obj), "SyncPhases")
		})
	}
}

func TestDeletePolicies(t *testing.T) {
	t.Parallel()
	pk, hk := v1alpha1.AnnotationKeyHookDeletePolicy, AnnotationKeyHelmHookDeletePolicy
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"BeforeHookCreation"}, DeletePolicies(pod("p")))
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"BeforeHookCreation"}, DeletePolicies(annotate(pod("p"), pk, "Garbage")))
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"HookSucceeded"}, DeletePolicies(annotate(pod("p"), pk, "HookSucceeded")))
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"HookFailed"}, DeletePolicies(annotate(pod("p"), hk, "hook-failed")))
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"HookSucceeded", "HookFailed", "BeforeHookCreation"},
		DeletePolicies(annotate(pod("p"), pk, "HookSucceeded", hk, "hook-failed,before-hook-creation,hook-succeeded")))
}

func TestWave(t *testing.T) {
	t.Parallel()
	wk, hw := v1alpha1.AnnotationKeySyncWave, AnnotationKeyHelmHookWeight
	assert.Equal(t, 0, Wave(pod("p")))
	assert.Equal(t, -2, Wave(annotate(pod("p"), wk, "-2")))
	assert.Equal(t, 3, Wave(annotate(pod("p"), wk, "abc", hw, "3")), "unparseable sync-wave falls back to hook-weight")
	assert.Equal(t, 0, Wave(annotate(pod("p"), hw, "x")))
	assert.Equal(t, -1, Wave(annotate(pod("p"), hw, "-1")), "hook-weight applies to any object")
	assert.Equal(t, 5, Wave(annotate(pod("p"), wk, "5", hw, "9")))
}

func TestDeleteHooksPartition(t *testing.T) {
	t.Parallel()
	pre := annotate(pod("pre"), v1alpha1.AnnotationKeyHook, "PreDelete")
	post := annotate(pod("post"), AnnotationKeyHelmHook, "post-delete")
	both := annotate(pod("both"), v1alpha1.AnnotationKeyHook, "PreDelete,PreSync")
	normal := pod("normal")
	assert.True(t, IsPreDeleteHook(pre))
	assert.True(t, IsPostDeleteHook(post))
	assert.False(t, IsPreDeleteHook(normal))
	objs, hasPre, hasPost := PartitionTargets([]*unstructured.Unstructured{pre, nil, post, both, normal})
	assert.True(t, hasPre)
	assert.True(t, hasPost)
	assert.Equal(t, []string{"both", "normal"}, []string{objs[0].GetName(), objs[1].GetName()})
	assert.Len(t, objs, 2)
}

func TestHookName(t *testing.T) {
	t.Parallel()
	n1 := HookName("migrate-", "uid-1", v1alpha1.SyncPhasePreSync, 0, 0)
	assert.Regexp(t, regexp.MustCompile(`^migrate-[0-9a-f]{8}$`), n1)
	assert.Equal(t, n1, HookName("migrate-", "uid-1", v1alpha1.SyncPhasePreSync, 0, 0), "deterministic")
	assert.NotEqual(t, n1, HookName("migrate-", "uid-2", v1alpha1.SyncPhasePreSync, 0, 0), "per operation")
	assert.NotEqual(t, n1, HookName("migrate-", "uid-1", v1alpha1.SyncPhasePostSync, 0, 0), "per phase")
	assert.NotEqual(t, n1, HookName("migrate-", "uid-1", v1alpha1.SyncPhasePreSync, 1, 0), "per wave")
	assert.NotEqual(t, n1, HookName("migrate-", "uid-1", v1alpha1.SyncPhasePreSync, 0, 1), "per index")
}
