package examples_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

const fieldManager = "patrol-examples-test"

// warningCollector records API server warnings (deprecations, finalizer name advice, ...).
type warningCollector struct {
	mu   sync.Mutex
	seen map[string]int
}

func (w *warningCollector) HandleWarningHeader(_ int, _ string, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]int{}
	}
	w.seen[text]++
}

func (w *warningCollector) list() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.seen))
	for text, n := range w.seen {
		out = append(out, fmt.Sprintf("%dx %s", n, text))
	}
	sort.Strings(out)
	return out
}

// apiServer wraps the envtest API server.
type apiServer struct {
	dyn      dynamic.Interface
	mapper   meta.ResettableRESTMapper
	client   client.Client
	warnings *warningCollector

	mu         sync.Mutex
	namespaces map[string]bool
	dryRuns    map[string]int // GroupKind → successful dry-run creates
}

func newAPIServer(t *testing.T, cfg *rest.Config) *apiServer {
	t.Helper()
	wc := &warningCollector{}
	cfg.WarningHandler = wc
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return &apiServer{
		dyn:        dyn,
		mapper:     restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
		client:     c,
		warnings:   wc,
		namespaces: map[string]bool{},
		dryRuns:    map[string]int{},
	}
}

func (a *apiServer) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		a.mapper.Reset()
		m, err = a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not served by the API server: %w", gvk, err)
	}
	return m, nil
}

// namespaced reports whether gvk is a namespaced resource.
func (a *apiServer) namespaced(gvk schema.GroupVersionKind) (bool, error) {
	m, err := a.mapping(gvk)
	if err != nil {
		return false, err
	}
	return m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// ensureNamespace creates a namespace (for real) once.
func (a *apiServer) ensureNamespace(ctx context.Context, ns string) error {
	a.mu.Lock()
	done := a.namespaces[ns]
	a.mu.Unlock()
	if done {
		return nil
	}
	err := a.client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", ns, err)
	}
	a.mu.Lock()
	a.namespaces[ns] = true
	a.mu.Unlock()
	return nil
}

// resource returns the dynamic client of obj, defaulting the namespace of namespaced
// objects to defaultNS and creating the namespace when needed.
func (a *apiServer) resource(ctx context.Context, obj *unstructured.Unstructured, defaultNS string) (dynamic.ResourceInterface, error) {
	m, err := a.mapping(obj.GroupVersionKind())
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() != meta.RESTScopeNameNamespace {
		if obj.GetNamespace() != "" {
			return nil, fmt.Errorf("cluster-scoped %s %s sets metadata.namespace", obj.GetKind(), obj.GetName())
		}
		return a.dyn.Resource(m.Resource), nil
	}
	ns := obj.GetNamespace()
	if ns == "" {
		if defaultNS == "" {
			return nil, fmt.Errorf("namespaced %s %s has no namespace and no default", obj.GetKind(), obj.GetName())
		}
		ns = defaultNS
		obj.SetNamespace(ns)
	}
	if err := a.ensureNamespace(ctx, ns); err != nil {
		return nil, err
	}
	return a.dyn.Resource(m.Resource).Namespace(ns), nil
}

// dryRun validates obj with a server-side dry-run create (strict field validation: unknown
// or duplicate fields are errors). An object that already exists (namespaces created for
// other objects, CRDs installed by envtest) is validated with a dry-run server-side apply.
func (a *apiServer) dryRun(ctx context.Context, obj *unstructured.Unstructured, defaultNS string) error {
	obj = obj.DeepCopy()
	ri, err := a.resource(ctx, obj, defaultNS)
	if err != nil {
		return err
	}
	_, err = ri.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict, FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) && obj.GetName() != "" {
		data, merr := json.Marshal(obj.Object)
		if merr != nil {
			return merr
		}
		_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
			DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict, FieldManager: fieldManager, Force: ptr.To(true),
		})
	}
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.dryRuns[obj.GroupVersionKind().GroupKind().String()]++
	a.mu.Unlock()
	return nil
}

// create creates obj for real (seeding objects the generators read); an existing object is
// left alone.
func (a *apiServer) create(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	obj = obj.DeepCopy()
	ri, err := a.resource(ctx, obj, "")
	if err != nil {
		return false, err
	}
	_, err = ri.Create(ctx, obj, metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict, FieldManager: fieldManager})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	return err == nil, err
}

// apply server-side applies obj for real.
func (a *apiServer) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := a.resource(ctx, obj, "")
	if err != nil {
		return err
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: fieldManager, Force: ptr.To(true)})
	return err
}

func (a *apiServer) dryRunSummary() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := make([]string, 0, len(a.dryRuns))
	total := 0
	for k, n := range a.dryRuns {
		keys = append(keys, fmt.Sprintf("%s=%d", k, n))
		total += n
	}
	sort.Strings(keys)
	return fmt.Sprintf("%d dry-run creates: %s", total, strings.Join(keys, " "))
}
