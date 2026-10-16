package sync

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

const envInstanceLabel = "app.kubernetes.io/instance"

var (
	gvrConfigMap          = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	gvrSecret             = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	gvrService            = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	gvrRole               = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	gvrRoleBinding        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	gvrClusterRole        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	envListedResources    = []schema.GroupVersionResource{gvrConfigMap, gvrSecret, gvrService, gvrRole, gvrRoleBinding, gvrClusterRole, gvrNamespace, gvrCRD}
	gvkConfigMap          = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	envDefaultSyncOptions = v1alpha1.SyncOptions{"PrunePropagationPolicy=background"}
)

// env drives the engine against the envtest API server like the Operation reconciler:
// managed live state is listed by tracking label (plus a GET per target), results and
// progress of every step are persisted into the next input.
type env struct {
	t      *testing.T
	cfg    *rest.Config
	dyn    dynamic.Interface
	ns     string
	app    string
	health *healthByName
	extra  []schema.GroupVersionResource
	engine *Engine
	ecfg   Config
	in     Input
	last   *StepResult
	// stripManagedFields drops metadata.managedFields from the live state, like the
	// internal/cache default (Settings.RetainManagedFields=false).
	stripManagedFields bool
}

func newEnv(t *testing.T, targets ...*unstructured.Unstructured) *env {
	t.Helper()
	cfg := envConfig(t)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	ns := "sync-" + rand.String(8)
	_, err = dyn.Resource(gvrNamespace).Create(context.Background(), newObj("v1", "Namespace", "", ns), metav1.CreateOptions{})
	require.NoError(t, err)
	e := &env{t: t, cfg: cfg, dyn: dyn, ns: ns, app: ns, health: newHealth(),
		ecfg: Config{WaveDelay: 0, PollInterval: 100 * time.Millisecond}}
	e.restart()
	e.in = Input{
		OperationUID: "op-" + rand.String(6),
		App:          AppRef{Name: e.app, Namespace: "patrol", Destination: v1alpha1.ApplicationDestination{Server: cfg.Host, Namespace: ns}},
		Targets:      targets,
		Prune:        true,
		SyncOptions:  slices.Clone(envDefaultSyncOptions),
		StartedAt:    time.Now().Truncate(time.Second),
		SetTracking: func(obj *unstructured.Unstructured) error {
			l := obj.GetLabels()
			if l == nil {
				l = map[string]string{}
			}
			l[envInstanceLabel] = e.app
			obj.SetLabels(l)
			return nil
		},
	}
	return e
}

// restart discards the engine and its client, as a controller restart would.
func (e *env) restart() {
	e.t.Helper()
	kube, err := NewKubeClient(e.cfg, KubeClientOptions{DeleteWaitTimeout: 20 * time.Second})
	require.NoError(e.t, err)
	eng, err := NewEngine(kube, e.healthFn, e.ecfg)
	require.NoError(e.t, err)
	e.engine = eng
}

func (e *env) healthFn(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
	if hs, _ := e.health.fn(obj); hs != nil {
		return hs, nil
	}
	return health.Assess(obj, nil)
}

// newOperation starts a new operation for the same application.
func (e *env) newOperation(targets ...*unstructured.Unstructured) {
	e.in.OperationUID = "op-" + rand.String(6)
	e.in.Targets = targets
	e.in.Previous = nil
	e.in.Progress = nil
	e.in.StartedAt = time.Now().Truncate(time.Second)
}

func (e *env) liveState() map[ResourceKey]*unstructured.Unstructured {
	e.t.Helper()
	ctx := context.Background()
	out := map[ResourceKey]*unstructured.Unstructured{}
	for _, gvr := range slices.Concat(envListedResources, e.extra) {
		list, err := e.dyn.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: envInstanceLabel + "=" + e.app})
		if err != nil {
			continue // e.g. a CRD that does not exist yet
		}
		for i := range list.Items {
			obj := list.Items[i]
			out[keyOf(&obj)] = &obj
		}
	}
	kube, err := NewKubeClient(e.cfg, KubeClientOptions{})
	require.NoError(e.t, err)
	for _, t := range e.in.Targets {
		ns := t.GetNamespace()
		if ns == "" {
			ns = e.ns
		}
		res, err := kube.Resource(ctx, t.GroupVersionKind())
		if err != nil {
			continue
		}
		if !res.Namespaced {
			ns = ""
		}
		obj, err := kube.Get(ctx, t.GroupVersionKind(), ns, t.GetName())
		if err == nil {
			out[keyOf(obj)] = obj
		}
	}
	if e.stripManagedFields {
		for _, obj := range out {
			obj.SetManagedFields(nil)
		}
	}
	return out
}

func (e *env) step() *StepResult {
	e.t.Helper()
	in := e.in
	in.Live = e.liveState()
	res, err := e.engine.Step(context.Background(), &in)
	require.NoError(e.t, err)
	e.persist(res)
	return res
}

func (e *env) persist(res *StepResult) {
	e.in.Previous = slices.Clone(res.Results)
	e.in.Progress = res.Progress
	e.last = res
}

func (e *env) run(maxSteps int) *StepResult {
	e.t.Helper()
	var res *StepResult
	for i := 0; i < maxSteps; i++ {
		res = e.step()
		if res.Phase != v1alpha1.OperationRunning {
			return res
		}
		time.Sleep(min(res.RequeueAfter, time.Second))
	}
	e.t.Fatalf("operation did not finish after %d steps: %s %q", maxSteps, res.Phase, res.Message)
	return nil
}

func (e *env) get(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if ns == "" {
		return e.dyn.Resource(gvr).Get(context.Background(), name, metav1.GetOptions{})
	}
	return e.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
}

func (e *env) mustGet(gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	e.t.Helper()
	obj, err := e.get(gvr, ns, name)
	require.NoError(e.t, err)
	return obj
}

func (e *env) gone(gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := e.get(gvr, ns, name)
	return apierrors.IsNotFound(err)
}

// seed creates an object directly (not through the engine), tracked when track is true.
func (e *env) seed(gvr schema.GroupVersionResource, obj *unstructured.Unstructured, tracked bool, manager string) *unstructured.Unstructured {
	e.t.Helper()
	obj = obj.DeepCopy()
	if tracked {
		l := obj.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l[envInstanceLabel] = e.app
		obj.SetLabels(l)
	}
	ri := dynamic.ResourceInterface(e.dyn.Resource(gvr))
	if obj.GetNamespace() != "" {
		ri = e.dyn.Resource(gvr).Namespace(obj.GetNamespace())
	}
	out, err := ri.Create(context.Background(), obj, metav1.CreateOptions{FieldManager: manager})
	require.NoError(e.t, err)
	return out
}

func (e *env) cm(name string, data map[string]string) *unstructured.Unstructured {
	u := newObj("v1", "ConfigMap", e.ns, name)
	if data != nil {
		m := map[string]any{}
		for k, v := range data {
			m[k] = v
		}
		u.Object["data"] = m
	}
	return u
}

func (e *env) cmData(name string) map[string]string {
	e.t.Helper()
	obj := e.mustGet(gvrConfigMap, e.ns, name)
	data, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	return data
}

func managers(obj *unstructured.Unstructured) []string {
	var out []string
	for _, f := range obj.GetManagedFields() {
		out = append(out, f.Manager+"/"+string(f.Operation))
	}
	return out
}

func TestEnvApplyAndPruneClientSide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(gvrConfigMap, e.cm("extra", nil), true, "")
	e.seed(gvrConfigMap, e.cm("unmanaged", nil), false, "")
	e.newOperation(e.cm("a", map[string]string{"k1": "v1", "k2": "v2"}), e.cm("b", nil))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "successfully synced (all tasks run)", res.Message)
	assert.Equal(t, "configmap/a created", findResult(res.Results, "a", "").Message)
	assert.Equal(t, "pruned", findResult(res.Results, "extra", "").Message)
	assert.True(t, e.gone(gvrConfigMap, e.ns, "extra"))
	assert.False(t, e.gone(gvrConfigMap, e.ns, "unmanaged"), "untracked objects are not pruned")
	a := e.mustGet(gvrConfigMap, e.ns, "a")
	assert.Contains(t, a.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"], `"k2":"v2"`)
	assert.Equal(t, e.app, a.GetLabels()[envInstanceLabel])

	// Second operation: a key removed from the manifest is removed (three-way merge), a key
	// added by someone else is kept.
	patch := []byte(`{"data":{"foreign":"x"}}`)
	_, err := e.dyn.Resource(gvrConfigMap).Namespace(e.ns).Patch(context.Background(), "a", types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "other"})
	require.NoError(t, err)
	e.newOperation(e.cm("a", map[string]string{"k1": "v1"}), e.cm("b", nil))
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, map[string]string{"k1": "v1", "foreign": "x"}, e.cmData("a"))
	assert.Equal(t, "configmap/a configured", findResult(res.Results, "a", "").Message)
	assert.Equal(t, "configmap/b unchanged", findResult(res.Results, "b", "").Message)
}

func TestEnvServerSideApplyAndMigration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// Objects previously applied with kubectl client-side apply.
	e.seed(gvrConfigMap, e.cm("migrated", map[string]string{"a": "1", "b": "2"}), true, DefaultClientSideApplyMigrationManager)
	e.seed(gvrConfigMap, e.cm("not-migrated", map[string]string{"a": "1", "b": "2"}), false, DefaultClientSideApplyMigrationManager)
	e.in.SyncOptions = append(e.in.SyncOptions, "ServerSideApply=true")
	e.newOperation(e.cm("migrated", map[string]string{"a": "1"}), e.cm("fresh", map[string]string{"x": "y"}),
		annotate(e.cm("csa", nil), v1alpha1.AnnotationKeySyncOptions, "ServerSideApply=false"))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "configmap/fresh serverside-applied", findResult(res.Results, "fresh", "").Message)
	assert.Equal(t, map[string]string{"a": "1"}, e.cmData("migrated"), "migrated CSA fields can be removed by SSA")
	m := managers(e.mustGet(gvrConfigMap, e.ns, "migrated"))
	assert.Contains(t, m, DefaultManager+"/Apply")
	assert.NotContains(t, m, DefaultClientSideApplyMigrationManager+"/Update")
	assert.Contains(t, managers(e.mustGet(gvrConfigMap, e.ns, "csa")), DefaultManager+"/Update", "ServerSideApply=false on the resource wins")

	e.in.SyncOptions = append(e.in.SyncOptions, "ClientSideApplyMigration=false")
	e.newOperation(e.cm("not-migrated", map[string]string{"a": "1"}))
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, map[string]string{"a": "1", "b": "2"}, e.cmData("not-migrated"), "without migration the kubectl-owned field stays")
	assert.Contains(t, managers(e.mustGet(gvrConfigMap, e.ns, "not-migrated")), DefaultClientSideApplyMigrationManager+"/Update")
}

func TestEnvReplaceAndForce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	immutable := func(name, value string) *unstructured.Unstructured {
		u := e.cm(name, map[string]string{"v": value})
		u.Object["immutable"] = true
		return u
	}
	plain := e.seed(gvrConfigMap, e.cm("plain", map[string]string{"v": "old"}), true, "")
	failing := e.seed(gvrConfigMap, immutable("failing", "old"), true, "")
	forced := e.seed(gvrConfigMap, immutable("forced", "old"), true, "")
	replacedForce := e.seed(gvrConfigMap, immutable("replaced-force", "old"), true, "")

	// Without force an immutable change fails.
	e.newOperation(immutable("failing", "new"))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Contains(t, findResult(res.Results, "failing", "").Message, "immutable")
	assert.Equal(t, failing.GetUID(), e.mustGet(gvrConfigMap, e.ns, "failing").GetUID())

	// Force=true deletes and re-creates; Replace=true replaces (PUT); both re-create.
	e.newOperation(
		annotate(immutable("forced", "new"), v1alpha1.AnnotationKeySyncOptions, "Force=true"),
		annotate(e.cm("plain", map[string]string{"v": "new"}), v1alpha1.AnnotationKeySyncOptions, "Replace=true"),
		annotate(immutable("replaced-force", "new"), v1alpha1.AnnotationKeySyncOptions, "Replace=true,Force=true"),
		annotate(e.cm("created-by-replace", nil), v1alpha1.AnnotationKeySyncOptions, "Replace=true"),
		immutable("failing", "old"),
	)
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "configmap/created-by-replace created", findResult(res.Results, "created-by-replace", "").Message)
	assert.Equal(t, "configmap/replaced-force replaced", findResult(res.Results, "replaced-force", "").Message)
	got := e.mustGet(gvrConfigMap, e.ns, "forced")
	assert.NotEqual(t, forced.GetUID(), got.GetUID(), "re-created")
	assert.Equal(t, map[string]string{"v": "new"}, e.cmData("forced"))
	assert.Equal(t, plain.GetUID(), e.mustGet(gvrConfigMap, e.ns, "plain").GetUID(), "replace keeps the object")
	assert.Equal(t, "configmap/plain replaced", findResult(res.Results, "plain", "").Message)
	assert.Empty(t, e.mustGet(gvrConfigMap, e.ns, "plain").GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"])
	assert.NotEqual(t, replacedForce.GetUID(), e.mustGet(gvrConfigMap, e.ns, "replaced-force").GetUID())

	// Strategy-level force.
	strat := e.seed(gvrConfigMap, immutable("strategy", "old"), true, "")
	e.newOperation(immutable("strategy", "new"))
	e.in.Strategy = &v1alpha1.SyncStrategy{Hook: &v1alpha1.SyncStrategyHook{SyncStrategyApply: v1alpha1.SyncStrategyApply{Force: true}}}
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.NotEqual(t, strat.GetUID(), e.mustGet(gvrConfigMap, e.ns, "strategy").GetUID())
}

func widgetCRD(group string) *unstructured.Unstructured {
	u := crd(group, "Widget", "widgets", "Namespaced")
	_ = unstructured.SetNestedField(u.Object, "widget", "spec", "names", "singular")
	_ = unstructured.SetNestedField(u.Object, "WidgetList", "spec", "names", "listKind")
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"name": "v1", "served": true, "storage": true,
		"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}},
	}}, "spec", "versions")
	return u
}

func TestEnvCRDAndCustomResourceInOneSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	group := e.ns + ".example.com"
	e.extra = []schema.GroupVersionResource{{Group: group, Version: "v1", Resource: "widgets"}}
	widget := func(name string) *unstructured.Unstructured {
		w := newObj(group+"/v1", "Widget", "", name)
		w.Object["spec"] = map[string]any{"size": int64(3)}
		return w
	}

	// Without the CRD and without the option the task is invalid.
	e.newOperation(widget("orphan"))
	res := e.run(3)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.True(t, strings.HasPrefix(res.Message, "one or more synchronization tasks are not valid: "), res.Message)
	assert.Equal(t, fmt.Sprintf(`The Kubernetes API could not find %s/Widget for requested resource %s/orphan. Make sure the "Widget" CRD is installed on the destination cluster.`, group, e.ns), res.Results[0].Message)

	// With SkipDryRunOnMissingResource the dry run is skipped, the apply still fails.
	e.newOperation(annotate(widget("annotated"), v1alpha1.AnnotationKeySyncOptions, "SkipDryRunOnMissingResource=true"))
	res = e.run(3)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Contains(t, res.Results[0].Message, "Make sure the \"Widget\" CRD is installed")

	// CRD and CR in the same sync: no option needed; the CRD is applied first.
	e.newOperation(widget("w1"), widgetCRD(group))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"widgets." + group, "w1"}, names(res.Results))
	w := e.mustGet(schema.GroupVersionResource{Group: group, Version: "v1", Resource: "widgets"}, e.ns, "w1")
	assert.Equal(t, e.app, w.GetLabels()[envInstanceLabel])

	// Replace=true on a CRD updates it in place: instances survive.
	e.newOperation(widget("w1"), annotate(widgetCRD(group), v1alpha1.AnnotationKeySyncOptions, "Replace=true"))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "CustomResourceDefinition/widgets."+group+" updated", findResult(res.Results, "widgets."+group, "").Message)
	assert.Equal(t, w.GetUID(), e.mustGet(schema.GroupVersionResource{Group: group, Version: "v1", Resource: "widgets"}, e.ns, "w1").GetUID())
}

func TestEnvValidateFalse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bad := func(name string) *unstructured.Unstructured {
		u := e.cm(name, map[string]string{"k": "v"})
		u.Object["unknownField"] = "x"
		return u
	}
	e.newOperation(bad("strict"))
	res := e.run(3)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	assert.Contains(t, res.Results[0].Message, `unknown field "unknownField"`)
	assert.True(t, e.gone(gvrConfigMap, e.ns, "strict"))

	e.newOperation(annotate(bad("lenient"), v1alpha1.AnnotationKeySyncOptions, "Validate=false"))
	res = e.run(3)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, map[string]string{"k": "v"}, e.cmData("lenient"))

	e.in.SyncOptions = append(e.in.SyncOptions, "Validate=false")
	e.newOperation(bad("app-level"))
	res = e.run(3)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
}

func TestEnvCreateNamespaceWithManagedMetadata(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	target := "sync-created-" + rand.String(6)
	e.in.App.Destination.Namespace = target
	e.in.SyncOptions = append(e.in.SyncOptions, "CreateNamespace=true")
	e.in.ManagedNamespaceMetadata = &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "a"}, Annotations: map[string]string{"owner": "x"}}
	e.newOperation(newObj("v1", "ConfigMap", "", "in-new-ns"))
	res := e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	nsRes := findResult(res.Results, target, v1alpha1.SyncPhasePreSync)
	require.NotNil(t, nsRes)
	assert.Equal(t, "namespace/"+target+" serverside-applied", nsRes.Message)
	assert.Equal(t, e.in.ManagedNamespaceMetadata, res.ManagedNamespaceMetadata)
	ns := e.mustGet(gvrNamespace, "", target)
	assert.Equal(t, "a", ns.GetLabels()["team"])
	assert.Equal(t, "x", ns.GetAnnotations()["owner"])
	assert.Empty(t, ns.GetLabels()[envInstanceLabel], "the managed namespace carries no tracking")
	_, err := e.get(gvrConfigMap, target, "in-new-ns")
	require.NoError(t, err)

	// A label set by someone else survives; a changed managed label is updated.
	_, err = e.dyn.Resource(gvrNamespace).Patch(context.Background(), target, types.MergePatchType, []byte(`{"metadata":{"labels":{"foreign":"y"}}}`), metav1.PatchOptions{FieldManager: "other"})
	require.NoError(t, err)
	e.in.ManagedNamespaceMetadata = &v1alpha1.ManagedNamespaceMetadata{Labels: map[string]string{"team": "b"}}
	e.newOperation(newObj("v1", "ConfigMap", "", "in-new-ns"))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	ns = e.mustGet(gvrNamespace, "", target)
	assert.Equal(t, "b", ns.GetLabels()["team"])
	assert.Equal(t, "y", ns.GetLabels()["foreign"])
	assert.Empty(t, ns.GetAnnotations()["owner"], "annotations dropped from the metadata are removed")

	// Without managed metadata an existing namespace is left alone.
	e.in.ManagedNamespaceMetadata = nil
	e.newOperation(newObj("v1", "ConfigMap", "", "in-new-ns"))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Nil(t, findResult(res.Results, target, ""))
}

func TestEnvPrunePropagationPolicies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		policy    string
		finalizer string
	}{
		{"foreground", metav1.FinalizerDeleteDependents},
		{"orphan", metav1.FinalizerOrphanDependents},
		{"background", ""},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.seed(gvrConfigMap, e.cm("victim", nil), true, "")
			e.in.SyncOptions = v1alpha1.SyncOptions{"PrunePropagationPolicy=" + tc.policy}
			e.newOperation(e.cm("keep", nil))
			res := e.step()
			if tc.finalizer == "" {
				require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
				assert.True(t, e.gone(gvrConfigMap, e.ns, "victim"))
				return
			}
			// envtest runs no garbage collector: the object stays with the GC finalizer.
			victim := e.mustGet(gvrConfigMap, e.ns, "victim")
			assert.NotNil(t, victim.GetDeletionTimestamp())
			assert.Contains(t, victim.GetFinalizers(), tc.finalizer)
			// A non-final wave would wait; this single-wave sync is done after the apply.
			assert.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
			victim.SetFinalizers(nil)
			_, err := e.dyn.Resource(gvrConfigMap).Namespace(e.ns).Update(context.Background(), victim, metav1.UpdateOptions{})
			require.NoError(t, err)
		})
	}
}

func TestEnvPruneWaitsForTerminatingObjects(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(gvrConfigMap, wave(e.cm("old", nil), 0), true, "")
	e.in.SyncOptions = v1alpha1.SyncOptions{"PrunePropagationPolicy=foreground"}
	e.newOperation(wave(e.cm("next", nil), 1))
	res := e.step()
	assert.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, "pruned", findResult(res.Results, "old", "").Message)
	res = e.step()
	assert.Equal(t, "waiting for deletion of /ConfigMap/old", res.Message, "the foreground deletion never completes without a garbage collector")
	assert.Nil(t, findResult(res.Results, "next", ""))

	old := e.mustGet(gvrConfigMap, e.ns, "old")
	old.SetFinalizers(nil)
	_, err := e.dyn.Resource(gvrConfigMap).Namespace(e.ns).Update(context.Background(), old, metav1.UpdateOptions{})
	require.NoError(t, err)
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"old", "next"}, names(res.Results))
}

func TestEnvPruneLastAndPruneOptions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(gvrConfigMap, e.cm("pruned-last", nil), true, "")
	e.seed(gvrConfigMap, annotate(e.cm("no-prune", nil), v1alpha1.AnnotationKeySyncOptions, "Prune=false"), true, "")
	e.seed(gvrConfigMap, annotate(e.cm("no-delete", nil), v1alpha1.AnnotationKeySyncOptions, "Delete=false"), true, "")
	e.in.SyncOptions = append(e.in.SyncOptions, "PruneLast=true")
	e.newOperation(e.cm("first", nil), wave(e.cm("second", nil), 1))
	res := e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"first", "second", "no-delete", "no-prune", "pruned-last"}, names(res.Results), "prunes run last")
	assert.Equal(t, "ignored (no prune)", findResult(res.Results, "no-prune", "").Message)
	assert.False(t, e.gone(gvrConfigMap, e.ns, "no-prune"))
	assert.True(t, e.gone(gvrConfigMap, e.ns, "no-delete"), "Delete=false does not affect pruning")
	assert.True(t, e.gone(gvrConfigMap, e.ns, "pruned-last"))

	// prune=false on the operation keeps everything.
	e.seed(gvrConfigMap, e.cm("extra", nil), true, "")
	e.in.Prune = false
	e.newOperation(e.cm("first", nil), wave(e.cm("second", nil), 1))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "ignored (requires pruning)", findResult(res.Results, "extra", "").Message)
	assert.False(t, e.gone(gvrConfigMap, e.ns, "extra"))
}

func TestEnvApplyOutOfSyncOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(gvrConfigMap, e.cm("unchanged", map[string]string{"live": "value"}), true, "")
	e.seed(gvrConfigMap, e.cm("changed", map[string]string{"v": "old"}), true, "")
	e.in.SyncOptions = append(e.in.SyncOptions, "ApplyOutOfSyncOnly=true")
	e.newOperation(e.cm("unchanged", map[string]string{"git": "value"}), e.cm("changed", map[string]string{"v": "new"}))
	// The comparison said "unchanged" is in sync (for example through ignore rules).
	e.in.DiffResults = map[ResourceKey]bool{
		{Kind: "ConfigMap", Namespace: e.ns, Name: "unchanged"}: false,
		{Kind: "ConfigMap", Namespace: e.ns, Name: "changed"}:   true,
	}
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"changed"}, names(res.Results))
	assert.Equal(t, map[string]string{"live": "value"}, e.cmData("unchanged"))
	assert.Equal(t, map[string]string{"v": "new"}, e.cmData("changed"))
}

func TestEnvMultiWaveWithRequeue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	clk := clocktesting.NewFakeClock(time.Now())
	e.ecfg.WaveDelay = 2 * time.Second
	e.ecfg.Clock = clk
	e.restart()
	e.newOperation(wave(e.cm("w2", nil), 2), wave(e.cm("w0", nil), 0), wave(newObj("v1", "Secret", "", "w-1"), -1))
	res := e.step()
	require.Equal(t, v1alpha1.OperationRunning, res.Phase)
	assert.Equal(t, 2*time.Second, res.RequeueAfter, "wave delay after a non-final wave")
	assert.Equal(t, []string{"w-1"}, names(res.Results))
	assert.Equal(t, int64(-1), res.Progress.Wave)

	// Called again before the delay elapsed: nothing new happens.
	clk.Step(500 * time.Millisecond)
	res = e.step()
	assert.Equal(t, []string{"w-1"}, names(res.Results))
	assert.Equal(t, "waiting for healthy state of /Secret/w-1", res.Message)
	assert.Equal(t, 1500*time.Millisecond, res.RequeueAfter)

	for i := 0; i < 20 && res.Phase == v1alpha1.OperationRunning; i++ {
		clk.Step(res.RequeueAfter)
		res = e.step()
	}
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"w-1", "w0", "w2"}, names(res.Results))
	rv := func(gvr schema.GroupVersionResource, n string) string {
		return e.mustGet(gvr, e.ns, n).GetResourceVersion()
	}
	assert.Less(t, rv(gvrSecret, "w-1"), rv(gvrConfigMap, "w0"))
	assert.Less(t, rv(gvrConfigMap, "w0"), rv(gvrConfigMap, "w2"))
	assert.Equal(t, int64(3), res.Progress.TotalTasks)
	// The last wave is applied but, as upstream, its health is not awaited.
	assert.Equal(t, int64(2), res.Progress.CompletedTasks)
}

func hookCM(e *env, name, phase string, policies ...string) *unstructured.Unstructured {
	return hookObj(e.cm(name, map[string]string{"run": "1"}), phase, policies...)
}

// hooksOf lists the hook objects this operation created, by phase.
func (e *env) hooksOf(op string) map[string][]string {
	e.t.Helper()
	list, err := e.dyn.Resource(gvrConfigMap).Namespace(e.ns).List(context.Background(), metav1.ListOptions{})
	require.NoError(e.t, err)
	out := map[string][]string{}
	for _, item := range list.Items {
		if v, ok := item.GetAnnotations()[AnnotationKeyOperation]; ok && strings.HasPrefix(v, op+"/") {
			phase := v[strings.LastIndex(v, "/")+1:]
			out[phase] = append(out[phase], item.GetName())
		}
	}
	return out
}

func TestEnvHookLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// A hook with a fixed name left by an earlier operation (BeforeHookCreation re-creates it).
	leftover := annotate(hookCM(e, "pre-named", "PreSync"), AnnotationKeyOperation, "old-op/0/PreSync")
	leftover.SetFinalizers([]string{v1alpha1.FinalizerHook})
	oldHook := e.seed(gvrConfigMap, leftover, true, "")

	gen := hookCM(e, "", "PostSync", "HookSucceeded")
	gen.SetGenerateName("post-")
	e.newOperation(hookCM(e, "pre-named", "PreSync"), hookCM(e, "pre-succeeded", "PreSync", "HookSucceeded"), e.cm("app", nil), gen)
	e.health.set("pre-named", health.StatusProgressing)
	e.health.set("pre-succeeded", health.StatusProgressing)
	postName := HookName("post-", e.in.OperationUID, "PostSync", 0, 2)

	res := e.step()
	assert.Equal(t, "waiting for deletion of hook /ConfigMap/pre-named", res.Message)
	res = e.step()
	assert.Equal(t, "waiting for completion of hook /ConfigMap/pre-named and 1 more hooks", res.Message)
	recreated := e.mustGet(gvrConfigMap, e.ns, "pre-named")
	assert.NotEqual(t, oldHook.GetUID(), recreated.GetUID(), "BeforeHookCreation re-created the hook")
	assert.Contains(t, recreated.GetFinalizers(), v1alpha1.FinalizerHook)
	assert.True(t, e.gone(gvrConfigMap, e.ns, "app"), "Sync phase waits for PreSync hooks")

	e.health.set("pre-named", health.StatusHealthy)
	e.health.set("pre-succeeded", health.StatusHealthy)
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "successfully synced (no more tasks)", res.Message)
	assert.Equal(t, []string{"pre-named", "pre-succeeded", "app", postName}, names(res.Results))
	assert.Equal(t, v1alpha1.HookTypePostSync, findResult(res.Results, postName, "").HookType)

	assert.NotContains(t, e.mustGet(gvrConfigMap, e.ns, "pre-named").GetFinalizers(), v1alpha1.FinalizerHook, "finalizer removed after completion")
	assert.True(t, e.gone(gvrConfigMap, e.ns, "pre-succeeded"), "HookSucceeded deleted after success")
	assert.True(t, e.gone(gvrConfigMap, e.ns, postName), "HookSucceeded deleted after success")
	assert.Equal(t, map[string][]string{"PreSync": {"pre-named"}}, e.hooksOf(e.in.OperationUID))
}

func TestEnvSyncFailHooksOnFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.newOperation(
		hookCM(e, "pre", "PreSync", "HookFailed"),
		e.cm("never", nil),
		hookCM(e, "on-fail", "SyncFail"),
	)
	e.health.set("pre", health.StatusDegraded)
	e.health.set("on-fail", health.StatusProgressing)
	res := e.step() // PreSync hook created
	require.Equal(t, v1alpha1.OperationRunning, res.Phase)
	res = e.step() // hook degraded: completion persisted before its cleanup
	require.Equal(t, "one or more tasks are running", res.Message)
	res = e.step() // HookFailed hook deleted, SyncFail hook started
	assert.Equal(t, "waiting for completion of hook /ConfigMap/on-fail", res.Message)
	assert.False(t, e.gone(gvrConfigMap, e.ns, "on-fail"))

	e.health.set("on-fail", health.StatusHealthy)
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase)
	assert.Equal(t, "one or more synchronization tasks completed unsuccessfully", res.Message)
	assert.True(t, e.gone(gvrConfigMap, e.ns, "never"), "later phases never ran")
	assert.True(t, e.gone(gvrConfigMap, e.ns, "pre"), "HookFailed hook deleted on failure")
	assert.Equal(t, v1alpha1.OperationSucceeded, findResult(res.Results, "on-fail", "").HookPhase)
}

func TestEnvTerminateRunningHooks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.newOperation(hookCM(e, "long", "Sync"), e.cm("app", nil))
	e.health.set("long", health.StatusProgressing)
	res := e.step()
	require.Equal(t, "waiting for completion of hook /ConfigMap/long and 1 more resources", res.Message)
	require.Contains(t, e.mustGet(gvrConfigMap, e.ns, "long").GetFinalizers(), v1alpha1.FinalizerHook)

	in := e.in
	in.Live = e.liveState()
	res, err := e.engine.Terminate(context.Background(), &in)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.OperationCancelled, res.Phase)
	assert.Equal(t, "Operation terminated", res.Message)
	r := findResult(res.Results, "long", "")
	assert.Equal(t, v1alpha1.OperationFailed, r.HookPhase)
	assert.Equal(t, "Terminated", r.Message)
	assert.True(t, e.gone(gvrConfigMap, e.ns, "long"), "running hook deleted")
	assert.False(t, e.gone(gvrConfigMap, e.ns, "app"), "applied resources are not rolled back")
}

func TestEnvRestartRecovery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pre := hookCM(e, "", "PreSync")
	pre.SetGenerateName("migrate-")
	post := hookCM(e, "", "PostSync")
	post.SetGenerateName("notify-")
	e.newOperation(pre, wave(e.cm("w0", nil), 0), wave(e.cm("w1", nil), 1), post)
	preName := HookName("migrate-", e.in.OperationUID, "PreSync", 0, 0)
	postName := HookName("notify-", e.in.OperationUID, "PostSync", 0, 1)
	e.health.set(preName, health.StatusProgressing)
	e.health.set(postName, health.StatusProgressing)

	res := e.step()
	require.Equal(t, "waiting for completion of hook /ConfigMap/"+preName, res.Message)

	// Crash after the hook was created but before the step's result was persisted: a new
	// engine runs from the previous (empty) state and must adopt the hook.
	e.restart()
	e.in.Previous, e.in.Progress = nil, nil
	res = e.step()
	assert.Equal(t, "waiting for completion of hook /ConfigMap/"+preName, res.Message)
	assert.Equal(t, map[string][]string{"PreSync": {preName}}, e.hooksOf(e.in.OperationUID), "no duplicate hook")

	// Normal restarts between steps resume from the persisted results.
	e.health.set(preName, health.StatusHealthy)
	for i := 0; i < 20; i++ {
		e.restart()
		res = e.step()
		if res.Phase != v1alpha1.OperationRunning || findResult(res.Results, postName, "") != nil {
			break
		}
		time.Sleep(res.RequeueAfter)
	}
	require.NotNil(t, findResult(res.Results, postName, ""), "PostSync hook started: %s", res.Message)
	assert.Equal(t, []string{preName, "w0", "w1", postName}, names(res.Results))

	// Crash again right after the PostSync hook was created.
	e.restart()
	e.in.Previous = slices.DeleteFunc(slices.Clone(e.in.Previous), func(r v1alpha1.ResourceResult) bool { return r.Name == postName })
	res = e.step()
	assert.Equal(t, "waiting for completion of hook /ConfigMap/"+postName, res.Message)

	e.health.set(postName, health.StatusHealthy)
	for i := 0; i < 20 && res.Phase == v1alpha1.OperationRunning; i++ {
		e.restart()
		res = e.step()
	}
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, map[string][]string{"PreSync": {preName}, "PostSync": {postName}}, e.hooksOf(e.in.OperationUID), "each hook created once")
	assert.Len(t, res.Results, 4)
}

func TestEnvDryRunOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(gvrConfigMap, e.cm("existing", map[string]string{"v": "old"}), true, "")
	e.seed(gvrConfigMap, e.cm("extra", nil), true, "")
	e.in.DryRun = true
	e.newOperation(e.cm("existing", map[string]string{"v": "new"}), e.cm("new", nil), hookCM(e, "hook", "PreSync"))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "pruned (dry run)", findResult(res.Results, "extra", "").Message)
	assert.Equal(t, "configmap/new created (dry run)", findResult(res.Results, "new", "").Message)
	assert.Equal(t, map[string]string{"v": "old"}, e.cmData("existing"), "cluster unchanged")
	assert.True(t, e.gone(gvrConfigMap, e.ns, "new"))
	assert.True(t, e.gone(gvrConfigMap, e.ns, "hook"))
	assert.False(t, e.gone(gvrConfigMap, e.ns, "extra"))
}

func TestEnvRBACRoleRefChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	binding := func(role string) *unstructured.Unstructured {
		b := newObj("rbac.authorization.k8s.io/v1", "RoleBinding", "", "binding")
		b.Object["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": role}
		b.Object["subjects"] = []any{map[string]any{"kind": "ServiceAccount", "name": "default", "namespace": e.ns}}
		return b
	}
	role := func(name string) *unstructured.Unstructured {
		r := newObj("rbac.authorization.k8s.io/v1", "Role", "", name)
		r.Object["rules"] = []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "verbs": []any{"get"}}}
		return r
	}
	e.newOperation(role("r1"), role("r2"), binding("r1"))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	first := e.mustGet(gvrRoleBinding, e.ns, "binding")

	e.newOperation(role("r1"), role("r2"), binding("r2"))
	res = e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	got := e.mustGet(gvrRoleBinding, e.ns, "binding")
	ref, _, _ := unstructured.NestedString(got.Object, "roleRef", "name")
	assert.Equal(t, "r2", ref, "roleRef changed without force")
	assert.NotEqual(t, first.GetUID(), got.GetUID())
}

func TestEnvSelectiveSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.newOperation(e.cm("a", nil), e.cm("b", nil), hookCM(e, "hook", "PreSync"))
	e.in.Resources = []v1alpha1.SyncOperationResource{{Kind: "ConfigMap", Name: "a"}}
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, []string{"a"}, names(res.Results))
	assert.True(t, e.gone(gvrConfigMap, e.ns, "b"))
	assert.True(t, e.gone(gvrConfigMap, e.ns, "hook"))
}

func TestEnvKubeClientDiscovery(t *testing.T) {
	t.Parallel()
	cfg := envConfig(t)
	k, err := NewKubeClient(cfg, KubeClientOptions{})
	require.NoError(t, err)
	ctx := context.Background()
	res, err := k.Resource(ctx, gvkConfigMap)
	require.NoError(t, err)
	assert.Equal(t, "configmaps", res.Name)
	assert.True(t, res.Namespaced)
	res, err = k.Resource(ctx, schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"})
	require.NoError(t, err)
	assert.Equal(t, "v1", res.Version)
	assert.False(t, res.Namespaced)
	_, err = k.Resource(ctx, schema.GroupVersionKind{Group: "nope.example.com", Version: "v1", Kind: "Nope"})
	assert.True(t, apierrors.IsNotFound(err), err)
	assert.Contains(t, err.Error(), msgServerCouldNotFind)
	_, err = k.Resource(ctx, schema.GroupVersionKind{Version: "v1", Kind: "Nope"})
	assert.True(t, apierrors.IsNotFound(err), err)

	err = k.WaitForCRDEstablished(ctx, "missing.example.com", 200*time.Millisecond)
	require.Error(t, err)
	assert.True(t, wait.Interrupted(err) || apierrors.IsNotFound(err) || strings.Contains(err.Error(), "not found"), err)
}

var gvrJob = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

func (e *env) job(name, image string) *unstructured.Unstructured {
	u := newObj("batch/v1", "Job", e.ns, name)
	u.Object["spec"] = map[string]any{
		"template": map[string]any{
			"spec": map[string]any{
				"restartPolicy": "Never",
				"containers":    []any{map[string]any{"name": "main", "image": image}},
			},
		},
	}
	return u
}

func jobImage(obj *unstructured.Unstructured) string {
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if len(containers) == 0 {
		return ""
	}
	m, _ := containers[0].(map[string]any)
	img, _ := m["image"].(string)
	return img
}

// A fixed-name hook re-created by BeforeHookCreation may change immutable fields: the dry
// run must not validate it against the old object the sync deletes first.
func TestEnvDryRunBeforeHookCreationImmutableChange(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.health.set("migrate", health.StatusHealthy)
	e.newOperation(hookObj(e.job("migrate", "busybox:1"), "PreSync"), e.cm("app", nil))
	res := e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	first := e.mustGet(gvrJob, e.ns, "migrate")
	require.Equal(t, "busybox:1", jobImage(first))

	e.newOperation(hookObj(e.job("migrate", "busybox:2"), "PreSync"), e.cm("app", nil))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	second := e.mustGet(gvrJob, e.ns, "migrate")
	assert.NotEqual(t, first.GetUID(), second.GetUID(), "re-created")
	assert.Equal(t, "busybox:2", jobImage(second))

	// A genuinely invalid change of a hook that is patched (not re-created) still fails the
	// dry run before anything is applied.
	e.newOperation(hookObj(e.job("migrate", "busybox:3"), "PreSync", "HookFailed"), e.cm("app", map[string]string{"changed": "yes"}))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase, res.Message)
	assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	assert.Contains(t, findResult(res.Results, "migrate", "").Message, "immutable")
	assert.Empty(t, e.cmData("app"), "nothing applied")
}

func schemaWidgetCRD(group string, fields ...string) *unstructured.Unstructured {
	u := widgetCRD(group)
	props := map[string]any{}
	for _, f := range fields {
		props[f] = map[string]any{"type": "string"}
	}
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{
		"name": "v1", "served": true, "storage": true,
		"schema": map[string]any{"openAPIV3Schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"spec": map[string]any{"type": "object", "properties": props},
			},
		}},
	}}, "spec", "versions")
	return u
}

// A CRD whose schema gains a field and a custom resource using it in the same sync: the
// custom resource must not be dry-run against the old schema.
func TestEnvDryRunCRDSchemaChangeInSameSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	group := e.ns + ".example.com"
	gvrWidget := schema.GroupVersionResource{Group: group, Version: "v1", Resource: "widgets"}
	e.extra = []schema.GroupVersionResource{gvrWidget}
	widget := func(spec map[string]any) *unstructured.Unstructured {
		w := newObj(group+"/v1", "Widget", "", "w1")
		w.Object["spec"] = spec
		return w
	}
	e.newOperation(schemaWidgetCRD(group, "size"), widget(map[string]any{"size": "3"}))
	res := e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)

	e.newOperation(schemaWidgetCRD(group, "size", "color"), widget(map[string]any{"size": "3", "color": "red"}))
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	color, _, _ := unstructured.NestedString(e.mustGet(gvrWidget, e.ns, "w1").Object, "spec", "color")
	assert.Equal(t, "red", color)

	// When the comparison reports the CRD unchanged, the custom resource is dry-run against
	// the served schema as usual.
	e.newOperation(schemaWidgetCRD(group, "size", "color"), widget(map[string]any{"size": "3", "shape": "round"}))
	e.in.DiffResults = map[ResourceKey]bool{{Group: groupCRD, Kind: kindCRD, Name: "widgets." + group}: false}
	res = e.run(10)
	require.Equal(t, v1alpha1.OperationFailed, res.Phase, res.Message)
	assert.Equal(t, "one or more objects failed to apply (dry run)", res.Message)
	assert.Contains(t, findResult(res.Results, "w1", "").Message, `unknown field "spec.shape"`)
}

// The cluster cache strips metadata.managedFields from retained manifests by default: the
// client-side apply migration must still happen.
func TestEnvServerSideApplyMigrationWithStrippedLiveState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.stripManagedFields = true
	e.seed(gvrConfigMap, e.cm("migrated", map[string]string{"a": "1", "b": "2"}), true, DefaultClientSideApplyMigrationManager)
	e.in.SyncOptions = append(e.in.SyncOptions, "ServerSideApply=true")
	e.newOperation(e.cm("migrated", map[string]string{"a": "1"}))
	res := e.run(5)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, map[string]string{"a": "1"}, e.cmData("migrated"), "migrated CSA fields can be removed by SSA")
	assert.NotContains(t, managers(e.mustGet(gvrConfigMap, e.ns, "migrated")), DefaultClientSideApplyMigrationManager+"/Update")
}

// Replace=true,Force=true deletes and re-creates once per operation attempt, even when a
// step's result is lost (crash, cancelled context, unpersisted transient abort).
func TestEnvReplaceForceIsIdempotentWithinAnAttempt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	orig := e.seed(gvrConfigMap, e.cm("job", map[string]string{"v": "old"}), true, "")
	target := annotate(e.cm("job", map[string]string{"v": "new"}), v1alpha1.AnnotationKeySyncOptions, "Replace=true,Force=true")
	e.newOperation(target)

	in := e.in
	in.Live = e.liveState()
	res, err := e.engine.Step(context.Background(), &in)
	require.NoError(t, err)
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	replaced := e.mustGet(gvrConfigMap, e.ns, "job")
	require.NotEqual(t, orig.GetUID(), replaced.GetUID())

	// The result was never persisted; a restarted engine runs the step again.
	e.restart()
	res = e.step()
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.Equal(t, "configmap/job replaced", findResult(res.Results, "job", "").Message)
	assert.Equal(t, replaced.GetUID(), e.mustGet(gvrConfigMap, e.ns, "job").GetUID(), "not replaced twice")

	// A retry attempt and a new operation replace again, like upstream.
	e.in.Attempt = 1
	e.in.Previous, e.in.Progress = nil, nil
	res = e.step()
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	again := e.mustGet(gvrConfigMap, e.ns, "job")
	assert.NotEqual(t, replaced.GetUID(), again.GetUID())
	e.newOperation(target)
	res = e.step()
	require.Equal(t, v1alpha1.OperationSucceeded, res.Phase, res.Message)
	assert.NotEqual(t, again.GetUID(), e.mustGet(gvrConfigMap, e.ns, "job").GetUID())
}
