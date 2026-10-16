package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
)

func (h *harness) plan() *Plan {
	h.t.Helper()
	in := h.in
	in.Live = h.kube.live(in.Targets)
	p, err := h.engine.Plan(context.Background(), &in)
	require.NoError(h.t, err)
	return p
}

type taskView struct {
	Phase v1alpha1.SyncPhase
	Wave  int
	Kind  string
	Name  string
}

func viewOf(p *Plan) []taskView {
	var out []taskView
	for _, t := range p.Tasks {
		out = append(out, taskView{t.Phase, t.Wave, t.Key.Kind, t.Key.Name})
	}
	return out
}

func TestPlanOrdering(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		newObj("apps/v1", "Deployment", testNS, "web"),
		cm("b-config"),
		cm("a-config"),
		newObj("v1", "Service", testNS, "web"),
		wave(cm("late"), 1),
		wave(newObj("v1", "Secret", testNS, "early"), -1),
		hookObj(pod("pre"), "PreSync"),
		hookObj(pod("post"), "PostSync"),
		hookObj(pod("onfail"), "SyncFail"),
		newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader"),
	)
	p := h.plan()
	require.True(t, p.Valid, p.Errors)
	assert.Equal(t, []taskView{
		{"PreSync", 0, "Pod", "pre"},
		{"Sync", -1, "Secret", "early"},
		{"Sync", 0, "ConfigMap", "a-config"},
		{"Sync", 0, "ConfigMap", "b-config"},
		{"Sync", 0, "ClusterRole", "reader"},
		{"Sync", 0, "Service", "web"},
		{"Sync", 0, "Deployment", "web"},
		{"Sync", 1, "ConfigMap", "late"},
		{"PostSync", 0, "Pod", "post"},
		{"SyncFail", 0, "Pod", "onfail"},
	}, viewOf(p))
	assert.True(t, p.MultiStep)
	assert.Equal(t, 9, p.TotalTasks)
	// Cluster-scoped kinds do not get the destination namespace; namespaced kinds do.
	assert.Equal(t, "", p.Tasks[4].Key.Namespace)
	assert.Equal(t, testNS, p.Tasks[2].Key.Namespace)
}

func TestKindOrder(t *testing.T) {
	t.Parallel()
	assert.Less(t, KindOrder("Namespace"), KindOrder("CustomResourceDefinition"))
	assert.Less(t, KindOrder("CustomResourceDefinition"), KindOrder("ClusterRole"))
	assert.Less(t, KindOrder("ServiceAccount"), KindOrder("Secret"))
	assert.Less(t, KindOrder("Deployment"), KindOrder("Job"))
	assert.Less(t, KindOrder("APIService"), KindOrder("MyCustomKind"))
	assert.Equal(t, 0, KindOrder("MyCustomKind"))
	for i := 1; i < len(kindOrderList); i++ {
		assert.Less(t, KindOrder(kindOrderList[i-1]), KindOrder(kindOrderList[i]))
	}
}

func TestPlanDependencyAdjustment(t *testing.T) {
	t.Parallel()
	t.Run("namespace moves to the wave of its first resource", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, wave(namespace("other"), 5), wave(newObj("v1", "ConfigMap", "other", "cfg"), 0))
		p := h.plan()
		assert.Equal(t, []taskView{{"Sync", 0, "Namespace", "other"}, {"Sync", 0, "ConfigMap", "cfg"}}, viewOf(p))
	})
	t.Run("CRD moves before its first custom resource", func(t *testing.T) {
		t.Parallel()
		cr := wave(newObj("example.com/v1", "Widget", testNS, "w1"), 0)
		h := newHarness(t, wave(crd("example.com", "Widget", "widgets", "Namespaced"), 2), cr)
		p := h.plan()
		require.True(t, p.Valid, p.Errors)
		assert.Equal(t, []taskView{{"Sync", 0, "CustomResourceDefinition", "widgets.example.com"}, {"Sync", 0, "Widget", "w1"}}, viewOf(p))
		assert.True(t, p.Tasks[1].SkipDryRun, "CR of a CRD in the same sync skips the dry run")
		assert.Equal(t, testNS, p.Tasks[1].Key.Namespace)
	})
	t.Run("namespace used by a PreSync hook moves into PreSync", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, namespace("other"), hookObj(newObj("v1", "Pod", "other", "migrate"), "PreSync"))
		p := h.plan()
		assert.Equal(t, []taskView{{"PreSync", 0, "Namespace", "other"}, {"PreSync", 0, "Pod", "migrate"}}, viewOf(p))
	})
}

func TestPlanPruneWaves(t *testing.T) {
	t.Parallel()
	mk := func(name string, w int) *unstructured.Unstructured { return wave(pod(name), w) }
	type tc struct {
		name      string
		live      map[string]int
		targetW   map[string]int
		opts      v1alpha1.SyncOptions
		pruneLast []string
		want      map[string]int
	}
	tests := []tc{
		{name: "same wave all prune", live: map[string]int{"p1": 0, "p2": 0}, want: map[string]int{"p1": 0, "p2": 0}},
		{name: "different waves all prune are mirrored",
			live: map[string]int{"ns": 0, "p1": 1, "p2": 2, "p3": 3, "p4": 4},
			want: map[string]int{"ns": 4, "p1": 3, "p2": 2, "p3": 1, "p4": 0}},
		{name: "mixed: only prune waves are mirrored",
			targetW: map[string]int{"ns": 0, "p2": 2},
			live:    map[string]int{"ns": 0, "p1": 1, "p3": 3, "p4": 4},
			want:    map[string]int{"ns": 0, "p1": 4, "p2": 2, "p3": 3, "p4": 1}},
		{name: "PruneLast app level",
			targetW: map[string]int{"ns": 0},
			live:    map[string]int{"ns": 0, "p1": 1, "p2": 2, "p3": 3, "p4": 4},
			opts:    v1alpha1.SyncOptions{"PruneLast=true"},
			want:    map[string]int{"ns": 0, "p1": 5, "p2": 5, "p3": 5, "p4": 5}},
		{name: "PruneLast on individual live objects",
			targetW:   map[string]int{"ns": 0},
			live:      map[string]int{"ns": 0, "p1": 1, "p2": 2, "p3": 3, "p4": 4},
			pruneLast: []string{"p2"},
			want:      map[string]int{"ns": 0, "p1": 4, "p2": 5, "p3": 2, "p4": 1}},
		{name: "PruneLast same wave", targetW: map[string]int{"p1": 0}, live: map[string]int{"p2": 0, "p3": 0},
			opts: v1alpha1.SyncOptions{"PruneLast=true"}, want: map[string]int{"p1": 0, "p2": 1, "p3": 1}},
		{name: "PruneLast different waves", targetW: map[string]int{"p1": 2}, live: map[string]int{"p2": 1, "p3": 7},
			opts: v1alpha1.SyncOptions{"PruneLast=true"}, want: map[string]int{"p1": 2, "p2": 8, "p3": 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var targets []*unstructured.Unstructured
			for n, w := range tt.targetW {
				targets = append(targets, mk(n, w))
			}
			h := newHarness(t, targets...)
			h.in.SyncOptions = tt.opts
			for n, w := range tt.live {
				obj := mk(n, w)
				for _, pl := range tt.pruneLast {
					if pl == n {
						annotate(obj, v1alpha1.AnnotationKeySyncOptions, "PruneLast=true")
					}
				}
				h.kube.put(obj)
			}
			got := map[string]int{}
			for _, task := range h.plan().Tasks {
				got[task.Key.Name] = task.Wave
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPlanSelectiveSync(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("a"), cm("b"), newObj("v1", "ConfigMap", "other", "a"), hookObj(pod("hook"), "PreSync"))
	h.in.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "a", Namespace: testNS}}
	p := h.plan()
	assert.Equal(t, []taskView{{"Sync", 0, "ConfigMap", "a"}}, viewOf(p))
	assert.Equal(t, testNS, p.Tasks[0].Key.Namespace)
	assert.True(t, p.SkipHooks)

	h.in.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "a"}}
	assert.Len(t, h.plan().Tasks, 2, "empty namespace matches every namespace")

	h.in.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "*", Namespace: testNS}}
	assert.Len(t, h.plan().Tasks, 2, "wildcard name")

	h.in.Resources = []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "ConfigMap", Name: "a"}}
	assert.Empty(t, h.plan().Tasks, "group must match")
}

func TestPlanHooks(t *testing.T) {
	t.Parallel()
	gen := hookObj(pod(""), "PreSync,PostSync")
	gen.SetGenerateName("migrate-")
	named := hookObj(pod("named"), "Sync", "HookSucceeded")
	h := newHarness(t, gen, named)
	p := h.plan()
	require.Len(t, p.Tasks, 3)
	pre, sync, post := p.Tasks[0], p.Tasks[1], p.Tasks[2]
	assert.Equal(t, v1alpha1.SyncPhasePreSync, pre.Phase)
	assert.Equal(t, HookName("migrate-", "op-uid-1", "PreSync", 0, 0), pre.Key.Name)
	assert.Equal(t, HookName("migrate-", "op-uid-1", "PostSync", 0, 0), post.Key.Name)
	assert.NotEqual(t, pre.Key.Name, post.Key.Name)
	assert.Equal(t, "named", sync.Key.Name)
	assert.Equal(t, v1alpha1.HookTypeSync, sync.HookType)
	assert.Equal(t, []v1alpha1.HookDeletePolicy{"HookSucceeded"}, sync.DeletePolicies)
	assert.Empty(t, gen.GetName(), "input is not mutated")
	for _, task := range p.Tasks {
		assert.True(t, task.Hook)
		assert.Contains(t, task.Target.GetFinalizers(), v1alpha1.FinalizerHook)
		assert.Equal(t, "op-uid-1/0/"+string(task.Phase), task.Target.GetAnnotations()[AnnotationKeyOperation])
		assert.Equal(t, ActionRunHook, task.Action)
	}

	// The same operation always yields the same names (restart safety).
	assert.Equal(t, viewOf(p), viewOf(h.plan()))

	// The apply strategy skips hooks.
	h.in.Strategy = &v1alpha1.SyncStrategy{Apply: &v1alpha1.SyncStrategyApply{}}
	assert.Empty(t, h.plan().Tasks)
}

func TestPlanIgnoredAndSkippedObjects(t *testing.T) {
	t.Parallel()
	h := newHarness(t,
		annotate(cm("skipped"), v1alpha1.AnnotationKeyHook, "Skip"),
		annotate(cm("unknown"), v1alpha1.AnnotationKeyHook, "Foo"),
		annotate(cm("helmtest"), AnnotationKeyHelmHook, "test"),
		annotate(cm("pre-delete"), v1alpha1.AnnotationKeyHook, "PreDelete"),
		annotate(newObj("apiextensions.k8s.io/v1", kindCRD, "", "x.example.com"), AnnotationKeyHelmHook, "crd-install"),
	)
	// A live object for the skipped manifest exists and must not be pruned.
	h.kube.put(cm("skipped"))
	p := h.plan()
	assert.Equal(t, []taskView{{"Sync", 0, "CustomResourceDefinition", "x.example.com"}}, viewOf(p))
}

func TestPlanLiveHooksNeverPruned(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("keep"))
	h.kube.put(hookObj(pod("old-hook"), "PreSync"), cm("keep"), cm("extra"))
	p := h.plan()
	assert.Equal(t, []taskView{{"Sync", 0, "ConfigMap", "extra"}, {"Sync", 0, "ConfigMap", "keep"}}, viewOf(p))
	assert.Equal(t, ActionPrune, p.Tasks[0].Action)
	assert.Equal(t, []string{"1 resources will be pruned"}, p.Warnings)
}

func TestPlanNamespaceAutoCreation(t *testing.T) {
	t.Parallel()
	t.Run("no task when a manifest defines the namespace", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, namespace(testNS), cm("a"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		p := h.plan()
		assert.Equal(t, []taskView{{"Sync", 0, "Namespace", testNS}, {"Sync", 0, "ConfigMap", "a"}}, viewOf(p))
	})
	t.Run("PreSync task when missing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		p := h.plan()
		assert.Equal(t, []taskView{{"PreSync", 0, "Namespace", testNS}, {"Sync", 0, "ConfigMap", "a"}}, viewOf(p))
		assert.Equal(t, ActionCreate, p.Tasks[0].Action)
	})
	t.Run("no task when existing and unmanaged", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.kube.putUntracked(namespace(testNS))
		assert.Equal(t, []taskView{{"Sync", 0, "ConfigMap", "a"}}, viewOf(h.plan()))
	})
	t.Run("managed metadata applies every time with server-side apply", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		h.in.ManagedNamespaceMetadata = &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"note": "x"}}
		h.kube.putUntracked(namespace(testNS))
		p := h.plan()
		require.Len(t, p.Tasks, 2)
		ns := p.Tasks[0].Target
		assert.Equal(t, map[string]string{"team": "a"}, ns.GetLabels())
		assert.Equal(t, map[string]string{"note": "x", v1alpha1.AnnotationKeySyncOptions: "ServerSideApply=true"}, ns.GetAnnotations())
		assert.NotNil(t, p.Tasks[0].Live)
	})
	t.Run("tracking only when requested", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"))
		h.in.SyncOptions = v1alpha1.SyncOptions{"CreateNamespace=true"}
		assert.Empty(t, h.plan().Tasks[0].Target.GetLabels()[trackingLabel], "upstream parity: no tracking")
		h.in.TrackManagedNamespace = true
		assert.Equal(t, "app", h.plan().Tasks[0].Target.GetLabels()[trackingLabel])
	})
	t.Run("no option, no task", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"))
		assert.Len(t, h.plan().Tasks, 1)
	})
}

func TestPlanValidation(t *testing.T) {
	t.Parallel()
	t.Run("unknown kind", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"), newObj("example.com/v1", "Widget", testNS, "w"))
		p := h.plan()
		assert.False(t, p.Valid)
		require.Len(t, p.Errors, 1)
		assert.Contains(t, p.Errors[0], "the server could not find the requested resource")
	})
	t.Run("SkipDryRunOnMissingResource annotation", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, annotate(newObj("example.com/v1", "Widget", testNS, "w"), v1alpha1.AnnotationKeySyncOptions, "SkipDryRunOnMissingResource=true"))
		p := h.plan()
		assert.True(t, p.Valid)
		assert.True(t, p.Tasks[0].SkipDryRun)
	})
	t.Run("project permission", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, cm("a"), newObj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "admin"))
		var seen []string
		h.in.Permission = func(obj *unstructured.Unstructured, namespaced bool) error {
			seen = append(seen, fmt.Sprintf("%s:%v", obj.GetKind(), namespaced))
			if obj.GetKind() == "ClusterRole" {
				return errors.New("resource rbac.authorization.k8s.io:ClusterRole is not permitted in project default")
			}
			return nil
		}
		p := h.plan()
		assert.False(t, p.Valid)
		assert.Equal(t, []string{"resource rbac.authorization.k8s.io:ClusterRole is not permitted in project default"}, p.Errors)
		assert.ElementsMatch(t, []string{"ConfigMap:true", "ClusterRole:false"}, seen)
	})
}

func TestPlanApplyOutOfSyncOnlyAndPreview(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("same"), cm("changed"), cm("new"), hookObj(pod("hook"), "PostSync"))
	h.kube.put(cm("same"), cm("changed"), cm("gone"))
	h.in.DiffResults = map[ResourceKey]bool{
		{Kind: "ConfigMap", Namespace: testNS, Name: "same"}:    false,
		{Kind: "ConfigMap", Namespace: testNS, Name: "changed"}: true,
	}
	h.in.SyncOptions = v1alpha1.SyncOptions{"ApplyOutOfSyncOnly=true"}
	p := h.plan()
	actions := map[string]Action{}
	for _, task := range p.Tasks {
		actions[task.Key.Name] = task.Action
	}
	assert.Equal(t, map[string]Action{"same": ActionSkip, "changed": ActionUpdate, "new": ActionCreate, "gone": ActionPrune, "hook": ActionRunHook}, actions)
	var preview []string
	for _, task := range p.Preview() {
		preview = append(preview, task.Key.Name+":"+string(task.Action))
	}
	assert.Equal(t, []string{"changed:update", "gone:prune", "new:create", "hook:run-hook"}, preview)

	h.in.SyncOptions = nil
	h.in.Prune = false
	actions = map[string]Action{}
	for _, task := range h.plan().Tasks {
		actions[task.Key.Name] = task.Action
	}
	assert.Equal(t, ActionUnchanged, actions["same"])
	assert.Equal(t, ActionPruneSkipped, actions["gone"])
}

func TestPlanPruneConfirmation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kube.put(annotate(cm("guarded"), v1alpha1.AnnotationKeySyncOptions, "Prune=confirm"), cm("free"))
	p := h.plan()
	assert.Equal(t, []ResourceKey{{Kind: "ConfigMap", Namespace: testNS, Name: "guarded"}}, p.PendingConfirmation)
	assert.Equal(t, []string{"all 2 resources of the application will be pruned"}, p.Warnings)

	h.in.App.Annotations = map[string]string{v1alpha1.AnnotationKeyDeletionApproved: h.clock.Now().Format("2006-01-02T15:04:05Z07:00")}
	assert.Empty(t, h.plan().PendingConfirmation)
}

func TestPlanSharedResource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("shared"), cm("mine"))
	h.kube.put(cm("shared"), cm("mine"))
	h.in.SharedWith = func(live *unstructured.Unstructured) (string, bool) {
		return "patrol/other", live.GetName() == "shared"
	}
	assert.Empty(t, h.plan().SharedResource, "only checked with FailOnSharedResource")
	h.in.SyncOptions = v1alpha1.SyncOptions{"FailOnSharedResource=true"}
	assert.Equal(t, "ConfigMap/shared is part of applications patrol/app and patrol/other", h.plan().SharedResource)
	res := h.step()
	assert.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "Shared resource found: ConfigMap/shared is part of applications patrol/app and patrol/other", res.Message)
	assert.Empty(t, h.kube.callsWith("apply"))
}

func TestPlanSelfReferencedFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kube.put(cm("copied"), cm("real"))
	h.in.IsSelfReferenced = func(live, _ *unstructured.Unstructured) bool { return live.GetName() != "copied" }
	assert.Equal(t, []taskView{{"Sync", 0, "ConfigMap", "real"}}, viewOf(h.plan()))
}

func TestReconcileDedupByUID(t *testing.T) {
	t.Parallel()
	live := map[ResourceKey]*unstructured.Unstructured{}
	a := newObj("v1", "Event", testNS, "e")
	a.SetUID(types.UID("same"))
	b := newObj("events.k8s.io/v1", "Event", testNS, "e")
	b.SetUID(types.UID("same"))
	live[keyOf(a)] = a
	live[keyOf(b)] = b
	dedupLiveByUID(nil, live)
	assert.Len(t, live, 1)

	live[keyOf(a)] = a
	live[keyOf(b)] = b
	dedupLiveByUID([]*unstructured.Unstructured{b}, live)
	assert.Equal(t, []string{keyOf(b).String()}, keysOf(live), "the key referenced by a target is kept")
}

func TestPlanTrackingAndSetTrackingError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, cm("a"))
	h.in.SetTracking = func(obj *unstructured.Unstructured) error {
		obj.SetLabels(map[string]string{"app.kubernetes.io/instance": "app"})
		return nil
	}
	p := h.plan()
	assert.Equal(t, "app", p.Tasks[0].Target.GetLabels()["app.kubernetes.io/instance"])

	h.in.SetTracking = func(*unstructured.Unstructured) error { return errors.New("boom") }
	res := h.step()
	assert.Equal(t, v1alpha1.OperationError, res.Phase)
	assert.Equal(t, "failed to set tracking metadata on /ConfigMap/dest-ns/a: boom", res.Message)
}

func TestRespectIgnoreDifferences(t *testing.T) {
	t.Parallel()
	n, err := diff.NewNormalizer(diff.NormalizerConfig{Rules: []diff.IgnoreRule{{Group: "apps", Kind: "Deployment", JSONPointers: []string{"/spec/replicas"}}}})
	require.NoError(t, err)
	target := newObj("apps/v1", "Deployment", testNS, "web")
	_ = unstructured.SetNestedField(target.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedSlice(target.Object, []any{map[string]any{"name": "web", "image": "nginx:2"}}, "spec", "template", "spec", "containers")
	live := target.DeepCopy()
	_ = unstructured.SetNestedField(live.Object, int64(5), "spec", "replicas")
	_ = unstructured.SetNestedSlice(live.Object, []any{map[string]any{"name": "web", "image": "nginx:1"}}, "spec", "template", "spec", "containers")
	_ = unstructured.SetNestedField(live.Object, int64(5), "status", "replicas")

	got, err := RespectIgnoreDifferences(context.Background(), target, live, n)
	require.NoError(t, err)
	replicas, _, _ := unstructured.NestedInt64(got.Object, "spec", "replicas")
	assert.Equal(t, int64(5), replicas, "ignored field keeps the live value")
	containers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	assert.Equal(t, "nginx:2", containers[0].(map[string]any)["image"], "other fields keep the target value")
	_, hasStatus, _ := unstructured.NestedMap(got.Object, "status")
	assert.False(t, hasStatus, "live status is never copied")

	// Through the engine.
	h := newHarness(t, target)
	h.kube.put(live)
	h.in.SyncOptions = v1alpha1.SyncOptions{"RespectIgnoreDifferences=true"}
	h.in.Normalizer = n
	p := h.plan()
	r, _, _ := unstructured.NestedInt64(p.Tasks[0].Target.Object, "spec", "replicas")
	assert.Equal(t, int64(5), r)
}

func TestDigest(t *testing.T) {
	t.Parallel()
	a, b := cm("a"), cm("b")
	assert.Equal(t, Digest([]*unstructured.Unstructured{a, b}), Digest([]*unstructured.Unstructured{b, nil, a}))
	assert.NotEqual(t, Digest([]*unstructured.Unstructured{a}), Digest([]*unstructured.Unstructured{b}))
	assert.Len(t, Digest(nil), 64)
}

func TestMatchesResources(t *testing.T) {
	t.Parallel()
	k := ResourceKey{Group: "apps", Kind: "Deployment", Namespace: "ns", Name: "web"}
	assert.True(t, matchesResources(k, nil))
	assert.True(t, matchesResources(k, []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "Deployment", Name: "web"}}))
	assert.True(t, matchesResources(k, []v1alpha1.SyncOperationResource{{Group: "*", Kind: "*", Name: "*", Namespace: "*"}}))
	assert.False(t, matchesResources(k, []v1alpha1.SyncOperationResource{{Group: "", Kind: "Deployment", Name: "web"}}))
	assert.False(t, matchesResources(k, []v1alpha1.SyncOperationResource{{Group: "apps", Kind: "Deployment", Name: "web", Namespace: "other"}}))
}

func TestPlanInvalidInput(t *testing.T) {
	t.Parallel()
	e, err := NewEngine(newFakeKube(), nil, Config{})
	require.NoError(t, err)
	_, err = e.Plan(context.Background(), &Input{App: AppRef{Name: "a"}})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = e.Step(context.Background(), nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = NewEngine(nil, nil, Config{})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAugmentMessages(t *testing.T) {
	t.Parallel()
	k := newFakeKube()
	k.kinds[schema.GroupKind{Group: "example.com", Kind: "Gadget"}] = fakeKind{version: "v2", resource: "gadgets", namespaced: true}
	rows := []v1alpha1.ResourceResult{
		{Group: "example.com", Version: "v1", Kind: "Widget", Namespace: "ns", Name: "w", Message: "x: the server could not find the requested resource"},
		{Group: "example.com", Version: "v1", Kind: "Gadget", Namespace: "ns", Name: "g", Message: "the server could not find the requested resource"},
		{Kind: "ConfigMap", Name: "big", Message: "ConfigMap \"big\" is invalid: metadata.annotations: Too long: must have at most 262144 bytes"},
	}
	augmentMessages(context.Background(), k, rows)
	assert.Equal(t, `The Kubernetes API could not find example.com/Widget for requested resource ns/w. Make sure the "Widget" CRD is installed on the destination cluster.`, rows[0].Message)
	assert.Equal(t, `The Kubernetes API could not find version "v1" of example.com/Gadget for requested resource ns/g. Version "v2" of example.com/Gadget is installed on the destination cluster.`, rows[1].Message)
	assert.True(t, strings.HasSuffix(rows[2].Message, "may cause resource deletion and re-creation."))
	again := rows[2].Message
	augmentMessages(context.Background(), k, rows)
	assert.Equal(t, again, rows[2].Message, "augmentation is idempotent")
}
