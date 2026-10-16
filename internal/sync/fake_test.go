package sync

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/health"
)

const testNS = "dest-ns"

type fakeKind struct {
	version    string
	resource   string
	namespaced bool
}

// fakeKube is an in-memory Kube for unit tests. Objects with finalizers are only marked for
// deletion; everything else is removed immediately.
type fakeKube struct {
	mu    gosync.Mutex
	kinds map[schema.GroupKind]fakeKind
	objs  map[ResourceKey]*unstructured.Unstructured
	rv    int
	calls []string

	applyErr  map[string]error // wet apply/create/replace errors by object name
	dryRunErr map[string]error // dry-run errors by object name
	deleteErr map[string]error
	getErr    map[string]error
}

func newFakeKube() *fakeKube {
	f := &fakeKube{
		kinds:     map[schema.GroupKind]fakeKind{},
		objs:      map[ResourceKey]*unstructured.Unstructured{},
		applyErr:  map[string]error{},
		dryRunErr: map[string]error{},
		deleteErr: map[string]error{},
		getErr:    map[string]error{},
	}
	for _, k := range []struct {
		gk         schema.GroupKind
		v, r       string
		namespaced bool
	}{
		{schema.GroupKind{Kind: "Pod"}, "v1", "pods", true},
		{schema.GroupKind{Kind: "ConfigMap"}, "v1", "configmaps", true},
		{schema.GroupKind{Kind: "Secret"}, "v1", "secrets", true},
		{schema.GroupKind{Kind: "Service"}, "v1", "services", true},
		{schema.GroupKind{Kind: "Namespace"}, "v1", "namespaces", false},
		{schema.GroupKind{Group: "apps", Kind: "Deployment"}, "v1", "deployments", true},
		{schema.GroupKind{Group: "batch", Kind: "Job"}, "v1", "jobs", true},
		{schema.GroupKind{Group: groupCRD, Kind: kindCRD}, "v1", "customresourcedefinitions", false},
		{schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}, "v1", "clusterroles", false},
	} {
		f.kinds[k.gk] = fakeKind{version: k.v, resource: k.r, namespaced: k.namespaced}
	}
	return f
}

func (f *fakeKube) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeKube) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeKube) Resource(_ context.Context, gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.kinds[gvk.GroupKind()]
	if !ok || (gvk.Version != "" && gvk.Version != k.version) {
		return nil, fmt.Errorf("failed to discover server resources for group version %s: %w", gvk.GroupVersion(),
			&apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound,
				Message: "the server could not find the requested resource"}})
	}
	return &metav1.APIResource{Name: k.resource, Kind: gvk.Kind, Group: gvk.Group, Version: k.version, Namespaced: k.namespaced}, nil
}

func (f *fakeKube) key(gvk schema.GroupVersionKind, ns, name string) ResourceKey {
	if k, ok := f.kinds[gvk.GroupKind()]; ok && !k.namespaced {
		ns = ""
	}
	return ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: ns, Name: name}
}

func (f *fakeKube) Get(_ context.Context, gvk schema.GroupVersionKind, ns, name string) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.getErr[name]; err != nil {
		return nil, err
	}
	obj, ok := f.objs[f.key(gvk, ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind)}, name)
	}
	return obj.DeepCopy(), nil
}

func (f *fakeKube) store(obj *unstructured.Unstructured, op string, dryRun bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.kinds[obj.GroupVersionKind().GroupKind()]; !ok {
		return "", apierrors.NewNotFound(schema.GroupResource{Group: obj.GroupVersionKind().Group, Resource: obj.GetKind()}, "")
	}
	if dryRun {
		f.record("dryrun %s %s", op, obj.GetName())
		if err := f.dryRunErr[obj.GetName()]; err != nil {
			return "", err
		}
		return opMessage(obj, "configured", true), nil
	}
	f.record("%s %s/%s", op, obj.GetNamespace(), obj.GetName())
	if err := f.applyErr[obj.GetName()]; err != nil {
		return "", err
	}
	k := f.key(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())
	c := obj.DeepCopy()
	if !f.kinds[obj.GroupVersionKind().GroupKind()].namespaced {
		c.SetNamespace("")
	}
	f.rv++
	c.SetResourceVersion(strconv.Itoa(f.rv))
	verb := "configured"
	if old, ok := f.objs[k]; ok {
		c.SetUID(old.GetUID())
	} else {
		c.SetUID(types.UID(fmt.Sprintf("uid-%d", f.rv)))
		verb = "created"
	}
	f.objs[k] = c
	if c.GetKind() == kindCRD {
		group, _, _ := unstructured.NestedString(c.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(c.Object, "spec", "names", "kind")
		plural, _, _ := unstructured.NestedString(c.Object, "spec", "names", "plural")
		scope, _, _ := unstructured.NestedString(c.Object, "spec", "scope")
		f.kinds[schema.GroupKind{Group: group, Kind: kind}] = fakeKind{version: "v1", resource: plural, namespaced: scope == "Namespaced"}
	}
	return opMessage(obj, verb, false), nil
}

func (f *fakeKube) ClientSideApply(_ context.Context, obj *unstructured.Unstructured, o ApplyOptions) (string, error) {
	return f.store(obj, "apply", o.DryRun)
}

func (f *fakeKube) ServerSideApply(_ context.Context, obj *unstructured.Unstructured, o ApplyOptions) (string, error) {
	return f.store(obj, "ssa", o.DryRun)
}

func (f *fakeKube) MigrateClientSideApply(_ context.Context, live *unstructured.Unstructured, m string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("migrate %s %s", live.GetName(), m)
	return nil
}

func (f *fakeKube) Replace(_ context.Context, obj *unstructured.Unstructured, o ReplaceOptions) (string, error) {
	op := "replace"
	if o.Force {
		op = "force-replace"
	}
	if _, err := f.store(obj, op, o.DryRun); err != nil {
		return "", err
	}
	return opMessage(obj, "replaced", o.DryRun), nil
}

func (f *fakeKube) Create(_ context.Context, obj *unstructured.Unstructured, o CreateOptions) (string, error) {
	f.mu.Lock()
	_, exists := f.objs[f.key(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())]
	f.mu.Unlock()
	if exists {
		gvk := obj.GroupVersionKind()
		return "", apierrors.NewAlreadyExists(schema.GroupResource{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind) + "s"}, obj.GetName())
	}
	return f.store(obj, "create", o.DryRun)
}

func (f *fakeKube) Update(_ context.Context, obj *unstructured.Unstructured, o UpdateOptions) (*unstructured.Unstructured, error) {
	if _, err := f.store(obj, "update", o.DryRun); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if o.DryRun {
		return obj.DeepCopy(), nil
	}
	stored := f.objs[f.key(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName())]
	if stored.GetDeletionTimestamp() != nil && len(stored.GetFinalizers()) == 0 {
		delete(f.objs, f.key(obj.GroupVersionKind(), obj.GetNamespace(), obj.GetName()))
	}
	return stored.DeepCopy(), nil
}

func (f *fakeKube) Delete(_ context.Context, gvk schema.GroupVersionKind, ns, name string, o DeleteOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete %s/%s %s", ns, name, o.PropagationPolicy)
	if err := f.deleteErr[name]; err != nil {
		return err
	}
	k := f.key(gvk, ns, name)
	obj, ok := f.objs[k]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: strings.ToLower(gvk.Kind)}, name)
	}
	if len(obj.GetFinalizers()) > 0 {
		now := metav1.Now()
		obj.SetDeletionTimestamp(&now)
		return nil
	}
	delete(f.objs, k)
	return nil
}

func (f *fakeKube) WaitForCRDEstablished(context.Context, string, time.Duration) error { return nil }

const trackingLabel = "test.patrol/app"

// live emulates the cluster cache's managed live objects: objects tracked by the test
// application plus objects at the key of a target.
func (f *fakeKube) live(targets []*unstructured.Unstructured) map[ResourceKey]*unstructured.Unstructured {
	f.mu.Lock()
	defer f.mu.Unlock()
	wanted := map[ResourceKey]bool{}
	for _, t := range targets {
		if t == nil {
			continue
		}
		ns := t.GetNamespace()
		if ns == "" {
			ns = testNS
		}
		wanted[f.key(t.GroupVersionKind(), ns, t.GetName())] = true
	}
	out := map[ResourceKey]*unstructured.Unstructured{}
	for k, v := range f.objs {
		if v.GetLabels()[trackingLabel] == "app" || wanted[k] {
			out[k] = v.DeepCopy()
		}
	}
	return out
}

// put stores objects as if applied by the application (tracked).
func (f *fakeKube) put(objs ...*unstructured.Unstructured) {
	for _, o := range objs {
		o = o.DeepCopy()
		track(o)
		f.putUntracked(o)
	}
}

// putUntracked stores objects without tracking metadata.
func (f *fakeKube) putUntracked(objs ...*unstructured.Unstructured) {
	for _, o := range objs {
		if _, err := f.store(o, "seed", false); err != nil {
			panic(err)
		}
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func track(obj *unstructured.Unstructured) {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	l[trackingLabel] = "app"
	obj.SetLabels(l)
}

// healthByName is a HealthFunc returning fixed statuses by object name and no health for
// other objects.
type healthByName struct {
	mu gosync.Mutex
	m  map[string]health.Status
}

func newHealth() *healthByName { return &healthByName{m: map[string]health.Status{}} }

func (h *healthByName) set(name string, s health.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[name] = s
}

func (h *healthByName) fn(obj *unstructured.Unstructured) (*health.HealthStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.m[obj.GetName()]
	if !ok {
		return nil, nil
	}
	return &health.HealthStatus{Status: s, Message: string(s) + " message"}, nil
}

// --- object builders ---

func newObj(apiVersion, kind, ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	return u
}

func pod(name string) *unstructured.Unstructured { return newObj("v1", "Pod", testNS, name) }
func cm(name string) *unstructured.Unstructured  { return newObj("v1", "ConfigMap", testNS, name) }
func namespace(name string) *unstructured.Unstructured {
	return newObj("v1", "Namespace", "", name)
}

func annotate(obj *unstructured.Unstructured, kv ...string) *unstructured.Unstructured {
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		ann[kv[i]] = kv[i+1]
	}
	obj.SetAnnotations(ann)
	return obj
}

func hookObj(obj *unstructured.Unstructured, hookType string, policies ...string) *unstructured.Unstructured {
	annotate(obj, v1alpha1.AnnotationKeyHook, hookType)
	if len(policies) > 0 {
		annotate(obj, v1alpha1.AnnotationKeyHookDeletePolicy, strings.Join(policies, ","))
	}
	return obj
}

func wave(obj *unstructured.Unstructured, w int) *unstructured.Unstructured {
	return annotate(obj, v1alpha1.AnnotationKeySyncWave, strconv.Itoa(w))
}

func crd(group, kind, plural, scope string) *unstructured.Unstructured {
	u := newObj("apiextensions.k8s.io/v1", kindCRD, "", plural+"."+group)
	_ = unstructured.SetNestedField(u.Object, group, "spec", "group")
	_ = unstructured.SetNestedField(u.Object, kind, "spec", "names", "kind")
	_ = unstructured.SetNestedField(u.Object, plural, "spec", "names", "plural")
	_ = unstructured.SetNestedField(u.Object, scope, "spec", "scope")
	return u
}

// harness drives an Engine over a fakeKube like a reconciler: results and progress of each
// step are fed into the next.
type harness struct {
	t      *testing.T
	kube   *fakeKube
	health *healthByName
	clock  *clocktesting.FakeClock
	engine *Engine
	in     Input
	last   *StepResult
}

func newHarness(t *testing.T, targets ...*unstructured.Unstructured) *harness {
	t.Helper()
	h := &harness{t: t, kube: newFakeKube(), health: newHealth(), clock: clocktesting.NewFakeClock(time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))}
	h.rebuildEngine()
	h.in = Input{
		OperationUID: "op-uid-1",
		App:          AppRef{Name: "app", Namespace: "patrol", Destination: v1alpha1.ApplicationDestination{Server: "https://kubernetes.default.svc", Namespace: testNS}},
		Targets:      targets,
		Prune:        true,
		StartedAt:    h.clock.Now(),
		SetTracking: func(obj *unstructured.Unstructured) error {
			track(obj)
			return nil
		},
	}
	return h
}

func (h *harness) rebuildEngine() {
	e, err := NewEngine(h.kube, h.health.fn, Config{WaveDelay: 0, PollInterval: time.Second, Clock: h.clock})
	if err != nil {
		h.t.Fatal(err)
	}
	h.engine = e
}

// step runs one Step with fresh live state and persists its outcome into the input.
func (h *harness) step() *StepResult {
	h.t.Helper()
	in := h.in
	in.Live = h.kube.live(in.Targets)
	res, err := h.engine.Step(context.Background(), &in)
	if err != nil {
		h.t.Fatalf("step: %v", err)
	}
	h.persist(res)
	return res
}

func (h *harness) persist(res *StepResult) {
	h.in.Previous = slices.Clone(res.Results)
	h.in.Progress = res.Progress
	h.last = res
}

// run steps until the phase is terminal or max steps elapsed.
func (h *harness) run(max int) *StepResult {
	h.t.Helper()
	var res *StepResult
	for i := 0; i < max; i++ {
		res = h.step()
		if res.Phase != v1alpha1.OperationRunning {
			return res
		}
		h.clock.Step(res.RequeueAfter)
	}
	h.t.Fatalf("operation did not finish after %d steps: %s %q", max, res.Phase, res.Message)
	return nil
}

func findResult(rows []v1alpha1.ResourceResult, name string, phase v1alpha1.SyncPhase) *v1alpha1.ResourceResult {
	for i := range rows {
		if rows[i].Name == name && (phase == "" || rows[i].SyncPhase == phase) {
			return &rows[i]
		}
	}
	return nil
}

func names(rows []v1alpha1.ResourceResult) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

func keysOf(m map[ResourceKey]*unstructured.Unstructured) []string {
	var out []string
	for _, k := range slices.SortedFunc(maps.Keys(m), func(a, b ResourceKey) int { return strings.Compare(a.String(), b.String()) }) {
		out = append(out, k.String())
	}
	return out
}
