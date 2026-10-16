package stack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
)

// fieldManager is the field manager of objects the tests apply.
const fieldManager = "patrol-integration-test"

// Scheme knows the built-in kinds, apiextensions and the Patrol CD kinds.
var Scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(s); err != nil {
			panic(fmt.Sprintf("build scheme: %v", err)) // static registration; cannot fail at run time
		}
	}
	return s
}()

// Cluster is a kube-apiserver used by the tests: the control cluster or a destination.
type Cluster struct {
	// Name is the destination name registered in Patrol ("in-cluster", "dest-1", ...).
	Name string
	// Server is the destination URL registered in Patrol (Cluster.spec.server).
	Server string
	// Auth is how Patrol authenticates to it.
	Auth AuthMode
	// Config, Kube, Dynamic and Client are the test's own admin access.
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Client  client.Client

	env *envtest.Environment
	// credentials is the data of the Patrol credentials Secret (caData, certData, ...).
	credentials map[string][]byte
	// dedicated destinations can be stopped and started.
	dedicated bool
	// apiserverLog is the kube-apiserver output of a dedicated destination.
	apiserverLog string
	mu           sync.Mutex
	stopped      bool
}

// Credentials returns a copy of the data of the credentials Secret Patrol uses for this
// destination (caData, certData/keyData or bearerToken; none for in-cluster).
func (c *Cluster) Credentials() map[string][]byte {
	out := make(map[string][]byte, len(c.credentials))
	for k, v := range c.credentials {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func (c *Cluster) stopEnv() error {
	if c.env == nil {
		return nil
	}
	return c.env.Stop()
}

// Stop stops the kube-apiserver of a dedicated destination (connections are refused; etcd
// keeps the data). Shared clusters cannot be stopped.
func (c *Cluster) Stop(t testing.TB) {
	t.Helper()
	if !c.dedicated {
		t.Fatalf("cluster %s is shared; use Options.DedicatedDestinations for a cluster that can be stopped", c.Name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if err := c.env.ControlPlane.APIServer.Stop(); err != nil {
		// envtest kills the process after the stop timeout (open watches delay the drain).
		if !strings.Contains(err.Error(), "timeout waiting for process") {
			t.Fatalf("stop the kube-apiserver of %s: %v\n%s", c.Name, err, tail(c.apiserverLog, 8<<10))
		}
		t.Logf("destination %s: kube-apiserver did not drain its connections within %s and was killed", c.Name, stopGrace)
	}
	c.stopped = true
	host := strings.TrimPrefix(strings.TrimPrefix(c.Server, "https://"), "http://")
	Eventually(t, 30*time.Second, "kube-apiserver of "+c.Name+" refusing connections", func() (bool, string) {
		conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
		if err != nil {
			return true, ""
		}
		_ = conn.Close()
		return false, "still accepting connections on " + host
	})
	t.Logf("destination %s: kube-apiserver stopped", c.Name)
}

// Start starts the kube-apiserver of a stopped dedicated destination on the same address,
// with the same serving certificate, client CA and service account keys.
func (c *Cluster) Start(t testing.TB) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stopped {
		return
	}
	if err := c.env.ControlPlane.APIServer.Start(); err != nil {
		t.Fatalf("start the kube-apiserver of %s again: %v", c.Name, err)
	}
	c.stopped = false
	Eventually(t, time.Minute, "kube-apiserver of "+c.Name+" serving", func() (bool, string) {
		_, err := c.Kube.Discovery().ServerVersion()
		return err == nil, fmt.Sprint(err)
	})
	t.Logf("destination %s: kube-apiserver started", c.Name)
}

// mapping returns the REST mapping of an object's kind (discovery-backed, refreshed for new
// CRDs).
func (c *Cluster) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	m, err := c.Client.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("map %s: %w", gvk, err)
	}
	return m, nil
}

// resource returns the dynamic client of an object's kind and namespace.
func (c *Cluster) resource(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	m, err := c.mapping(gvk)
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		return c.Dynamic.Resource(m.Resource), nil
	}
	return c.Dynamic.Resource(m.Resource).Namespace(namespace), nil
}

// apply server-side applies one object (forced, test field manager).
func (c *Cluster) apply(ctx context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := c.resource(o.GroupVersionKind(), o.GetNamespace())
	if err != nil {
		return nil, err
	}
	out, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: fieldManager, Force: true})
	if err != nil {
		return nil, fmt.Errorf("apply %s %s/%s: %w", o.GetKind(), o.GetNamespace(), o.GetName(), err)
	}
	return out, nil
}

// Apply server-side applies every document of manifests (YAML or JSON). Namespaced
// objects without a namespace get namespace.
func (c *Cluster) Apply(t testing.TB, namespace, manifests string) []*unstructured.Unstructured {
	t.Helper()
	objs, err := decodeYAML(manifests)
	if err != nil {
		t.Fatalf("decode manifests: %v", err)
	}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		m, err := c.mapping(o.GroupVersionKind())
		if err != nil {
			t.Fatalf("%v", err)
		}
		if m.Scope.Name() == meta.RESTScopeNameNamespace && o.GetNamespace() == "" {
			o.SetNamespace(namespace)
		}
		res, err := c.apply(t.Context(), o)
		if err != nil {
			t.Fatalf("%v", err)
		}
		out = append(out, res)
	}
	return out
}

// TryGet reads an object (nil, nil when it does not exist).
func (c *Cluster) TryGet(ctx context.Context, apiVersion, kind, namespace, name string) (*unstructured.Unstructured, error) {
	ri, err := c.resource(schema.FromAPIVersionAndKind(apiVersion, kind), namespace)
	if err != nil {
		return nil, err
	}
	o, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return o, err
}

// Get reads an object and fails the test when it cannot.
func (c *Cluster) Get(t testing.TB, apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	o, err := c.TryGet(t.Context(), apiVersion, kind, namespace, name)
	if err != nil {
		t.Fatalf("get %s %s/%s in %s: %v", kind, namespace, name, c.Name, err)
	}
	if o == nil {
		t.Fatalf("%s %s/%s does not exist in %s", kind, namespace, name, c.Name)
	}
	return o
}

// Exists reports whether an object exists (and is not being deleted when live is true).
func (c *Cluster) Exists(t testing.TB, apiVersion, kind, namespace, name string) bool {
	t.Helper()
	o, err := c.TryGet(t.Context(), apiVersion, kind, namespace, name)
	if err != nil {
		t.Fatalf("get %s %s/%s in %s: %v", kind, namespace, name, c.Name, err)
	}
	return o != nil
}

// List lists objects of a kind in a namespace ("" for every namespace or cluster scope).
func (c *Cluster) List(t testing.TB, apiVersion, kind, namespace string, opts metav1.ListOptions) []unstructured.Unstructured {
	t.Helper()
	ri, err := c.resource(schema.FromAPIVersionAndKind(apiVersion, kind), namespace)
	if err != nil {
		t.Fatalf("%v", err)
	}
	l, err := ri.List(t.Context(), opts)
	if err != nil {
		t.Fatalf("list %s in %s/%s: %v", kind, c.Name, namespace, err)
	}
	return l.Items
}

// Delete deletes an object (missing is not an error).
func (c *Cluster) Delete(t testing.TB, apiVersion, kind, namespace, name string) {
	t.Helper()
	ri, err := c.resource(schema.FromAPIVersionAndKind(apiVersion, kind), namespace)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := ri.Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete %s %s/%s in %s: %v", kind, namespace, name, c.Name, err)
	}
}

// MergePatch applies an RFC 7386 merge patch to an object.
func (c *Cluster) MergePatch(t testing.TB, apiVersion, kind, namespace, name, patch string) *unstructured.Unstructured {
	t.Helper()
	ri, err := c.resource(schema.FromAPIVersionAndKind(apiVersion, kind), namespace)
	if err != nil {
		t.Fatalf("%v", err)
	}
	o, err := ri.Patch(t.Context(), name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		t.Fatalf("patch %s %s/%s in %s: %v", kind, namespace, name, c.Name, err)
	}
	return o
}

// SetStatus replaces the status of an object through the status subresource (envtest runs
// no controllers, so tests simulate Job completion or a Deployment rollout this way).
func (c *Cluster) SetStatus(t testing.TB, apiVersion, kind, namespace, name string, status map[string]any) {
	t.Helper()
	if err := c.setStatus(t.Context(), apiVersion, kind, namespace, name, func(map[string]any) map[string]any { return status }); err != nil {
		t.Fatalf("set the status of %s %s/%s in %s: %v", kind, namespace, name, c.Name, err)
	}
}

// setStatus re-reads the object and writes the status computed from it (conflicts
// retried).
func (c *Cluster) setStatus(ctx context.Context, apiVersion, kind, namespace, name string, status func(obj map[string]any) map[string]any) error {
	ri, err := c.resource(schema.FromAPIVersionAndKind(apiVersion, kind), namespace)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		o, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		o.Object["status"] = status(o.Object)
		_, err = ri.UpdateStatus(ctx, o, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	})
}

// CreateNamespace creates a namespace (existing is fine).
func (c *Cluster) CreateNamespace(t testing.TB, name string, labels map[string]string) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	ns.SetLabels(labels)
	if _, err := c.apply(t.Context(), ns); err != nil {
		t.Fatalf("create namespace %s in %s: %v", name, c.Name, err)
	}
}

// Eventually polls cond until it reports true, failing the test with the last message after
// Timeout(timeout). Polling is the only waiting mechanism of the harness.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	if err := Poll(t.Context(), timeout, cond); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

// Poll polls cond every 200ms until it reports true or Timeout(timeout) passed.
func Poll(ctx context.Context, timeout time.Duration, cond func() (bool, string)) error {
	limit := Timeout(timeout)
	deadline := time.Now().Add(limit)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var last string
	for {
		ok, msg := cond()
		if ok {
			return nil
		}
		if msg != "" {
			last = msg
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not reached within %s; last state: %s", limit, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; last state: %s", ctx.Err(), last)
		case <-tick.C:
		}
	}
}

// Consistently fails the test when cond reports false at any poll during d (checks that
// something does NOT happen, e.g. a resource is not pruned).
func Consistently(t testing.TB, d time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if ok, msg := cond(); !ok {
			t.Fatalf("%s no longer holds: %s", what, msg)
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("%s: %v", what, t.Context().Err())
		case <-tick.C:
		}
	}
}

// gcResources are the kinds whose foreground/orphan deletions the fake garbage collector
// releases.
var gcResources = []schema.GroupVersionResource{
	{Version: "v1", Resource: "configmaps"},
	{Version: "v1", Resource: "secrets"},
	{Version: "v1", Resource: "services"},
	{Version: "v1", Resource: "serviceaccounts"},
	{Version: "v1", Resource: "pods"},
	{Group: "apps", Version: "v1", Resource: "deployments"},
	{Group: "apps", Version: "v1", Resource: "replicasets"},
	{Group: "apps", Version: "v1", Resource: "statefulsets"},
	{Group: "batch", Version: "v1", Resource: "jobs"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
}

// fakeGC releases foregroundDeletion/orphan finalizers in watched namespaces of a cluster
// (envtest runs no garbage collector, so cascading deletions would never finish).
type fakeGC struct {
	c  *Cluster
	mu sync.Mutex
	ns []string
}

func (g *fakeGC) watch(namespace string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Contains(g.ns, namespace) {
		g.ns = append(g.ns, namespace)
	}
}

func (g *fakeGC) run(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		g.mu.Lock()
		namespaces := slices.Clone(g.ns)
		g.mu.Unlock()
		for _, ns := range namespaces {
			for _, gvr := range gcResources {
				g.release(ctx, gvr, ns)
			}
		}
	}
}

func (g *fakeGC) release(ctx context.Context, gvr schema.GroupVersionResource, ns string) {
	ri := g.c.Dynamic.Resource(gvr).Namespace(ns)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return // destination stopped, or the context ended
	}
	for i := range list.Items {
		o := &list.Items[i]
		if o.GetDeletionTimestamp() == nil {
			continue
		}
		fins := slices.DeleteFunc(slices.Clone(o.GetFinalizers()), func(f string) bool {
			return f == metav1.FinalizerDeleteDependents || f == metav1.FinalizerOrphanDependents
		})
		if len(fins) == len(o.GetFinalizers()) {
			continue
		}
		o.SetFinalizers(fins)
		if _, err := ri.Update(ctx, o, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) && !errors.Is(err, context.Canceled) {
			continue // retried on the next tick
		}
	}
}

// tail returns the last n bytes of a file ("" when unreadable).
func tail(path string, n int64) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if int64(len(b)) > n {
		b = b[int64(len(b))-n:]
	}
	return string(b)
}
