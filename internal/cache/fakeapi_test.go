package cache

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
)

// fakeResource is one API resource served by a fakeCluster's discovery.
type fakeResource struct {
	gv         schema.GroupVersion
	resource   string
	kind       string
	namespaced bool
}

func (r fakeResource) gvr() schema.GroupVersionResource { return r.gv.WithResource(r.resource) }

var (
	fooGK      = schema.GroupKind{Group: "foo.example", Kind: "Foo"}
	cmResource = fakeResource{gv: schema.GroupVersion{Version: "v1"}, resource: "configmaps", kind: "ConfigMap", namespaced: true}
	fooV1      = fakeResource{gv: schema.GroupVersion{Group: "foo.example", Version: "v1"}, resource: "foos", kind: "Foo", namespaced: true}
	fooV2      = fakeResource{gv: schema.GroupVersion{Group: "foo.example", Version: "v2"}, resource: "foos", kind: "Foo", namespaced: true}
)

type scopeKey struct {
	gvr schema.GroupVersionResource
	ns  string
}

// fakeCluster is a hermetic API server for cache tests: an httptest server serves discovery
// (legacy, unaggregated) and single-object GETs, while list and watch go through fakeScopes
// injected with WithListerWatcherWrapper.
type fakeCluster struct {
	srv *httptest.Server

	mu        sync.Mutex
	resources []fakeResource
	gvStatus  map[schema.GroupVersion]int
	objects   map[string]*unstructured.Unstructured
	getStatus map[string]int
	gate      chan struct{}
	versions  int
	gets      map[string]int
	scopes    map[scopeKey]*fakeScope
}

func newFakeCluster(t *testing.T, resources ...fakeResource) *fakeCluster {
	t.Helper()
	f := &fakeCluster{
		resources: resources,
		gvStatus:  map[schema.GroupVersion]int{},
		objects:   map[string]*unstructured.Unstructured{},
		getStatus: map[string]int{},
		gets:      map[string]int{},
		scopes:    map[scopeKey]*fakeScope{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCluster) config() *rest.Config {
	return &rest.Config{Host: f.srv.URL, Timeout: 10 * time.Second}
}

// options returns the cache options that route list/watch to the fake scopes and make watch
// retries fast.
func (f *fakeCluster) options() []Option {
	return []Option{
		WithLogger(slog.New(slog.DiscardHandler)),
		WithListerWatcherWrapper(func(api APIResourceInfo, ns string, _ ListerWatcher) ListerWatcher {
			return f.scope(api.GroupVersionResource, ns)
		}),
		func(o *options) {
			o.watchBackoff = wait.Backoff{Duration: 5 * time.Millisecond, Factor: 1, Steps: 1 << 30}
			o.minWatch = 0
		},
	}
}

// start creates a cache on the fake cluster and syncs it.
func (f *fakeCluster) start(t *testing.T, s Settings) *Cluster {
	t.Helper()
	if s.EventProcessingInterval == 0 {
		s.EventProcessingInterval = -1
	}
	c, err := New(f.config(), s, f.options()...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, c.EnsureSynced(ctx))
	return c
}

func (f *fakeCluster) scope(gvr schema.GroupVersionResource, ns string) *fakeScope {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := scopeKey{gvr, ns}
	s := f.scopes[k]
	if s == nil {
		s = &fakeScope{}
		f.scopes[k] = s
	}
	return s
}

func (f *fakeCluster) setResources(resources ...fakeResource) {
	f.mu.Lock()
	f.resources = resources
	f.mu.Unlock()
}

// failDiscovery makes discovery of gv answer with code (0 restores it).
func (f *fakeCluster) failDiscovery(gv schema.GroupVersion, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == 0 {
		delete(f.gvStatus, gv)
		return
	}
	f.gvStatus[gv] = code
}

// setObject serves obj (or, when code is not 0, that status) on GET path.
func (f *fakeCluster) setObject(path string, obj *unstructured.Unstructured, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if obj != nil {
		f.objects[path] = obj
	}
	if code != 0 {
		f.getStatus[path] = code
	}
}

func (f *fakeCluster) getCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[path]
}

func (f *fakeCluster) totalGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.gets {
		n += v
	}
	return n
}

func (f *fakeCluster) versionRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versions
}

// holdVersion blocks /version requests until the returned (idempotent) release is called.
func (f *fakeCluster) holdVersion() func() {
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func (f *fakeCluster) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/version":
		f.mu.Lock()
		f.versions++
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"major": "1", "minor": "36", "gitVersion": "v1.36.0"})
	case p == "/api":
		writeJSON(w, http.StatusOK, &metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}})
	case p == "/apis":
		writeJSON(w, http.StatusOK, f.groupList())
	case p == "/api/v1":
		f.writeResourceList(w, schema.GroupVersion{Version: "v1"})
	case strings.HasPrefix(p, "/apis/") && strings.Count(p, "/") == 3:
		parts := strings.Split(p, "/")
		f.writeResourceList(w, schema.GroupVersion{Group: parts[2], Version: parts[3]})
	case r.Method == http.MethodGet && r.URL.Query().Get("watch") == "":
		f.mu.Lock()
		f.gets[p]++
		code, obj := f.getStatus[p], f.objects[p]
		f.mu.Unlock()
		switch {
		case code != 0:
			writeStatus(w, code)
		case obj == nil:
			writeStatus(w, http.StatusNotFound)
		default:
			writeJSON(w, http.StatusOK, obj.Object)
		}
	default:
		writeStatus(w, http.StatusMethodNotAllowed)
	}
}

func (f *fakeCluster) groupList() *metav1.APIGroupList {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"}}
	idx := map[string]int{}
	seen := map[schema.GroupVersion]bool{}
	for _, r := range f.resources {
		if r.gv.Group == "" || seen[r.gv] {
			continue
		}
		seen[r.gv] = true
		v := metav1.GroupVersionForDiscovery{GroupVersion: r.gv.String(), Version: r.gv.Version}
		i, ok := idx[r.gv.Group]
		if !ok { // the first version listed is the preferred one
			idx[r.gv.Group] = len(out.Groups)
			out.Groups = append(out.Groups, metav1.APIGroup{Name: r.gv.Group, PreferredVersion: v})
			i = len(out.Groups) - 1
		}
		out.Groups[i].Versions = append(out.Groups[i].Versions, v)
	}
	return out
}

func (f *fakeCluster) writeResourceList(w http.ResponseWriter, gv schema.GroupVersion) {
	f.mu.Lock()
	code := f.gvStatus[gv]
	list := &metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: gv.String()}
	for _, r := range f.resources {
		if r.gv == gv {
			list.APIResources = append(list.APIResources, metav1.APIResource{
				Name: r.resource, Kind: r.kind, Namespaced: r.namespaced, Verbs: metav1.Verbs{"get", "list", "watch"},
			})
		}
	}
	f.mu.Unlock()
	switch {
	case code != 0:
		writeStatus(w, code)
	case len(list.APIResources) == 0 && gv.Group != "":
		writeStatus(w, http.StatusNotFound)
	default:
		writeJSON(w, http.StatusOK, list)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int) {
	reason := map[int]metav1.StatusReason{
		http.StatusForbidden:          metav1.StatusReasonForbidden,
		http.StatusNotFound:           metav1.StatusReasonNotFound,
		http.StatusMethodNotAllowed:   metav1.StatusReasonMethodNotAllowed,
		http.StatusServiceUnavailable: metav1.StatusReasonServiceUnavailable,
	}[code]
	writeJSON(w, code, &metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Code: int32(code), Reason: reason, Message: http.StatusText(code),
	})
}

// fakeScope lists and watches one API in one scope. Watches never emit events; stopWatches
// ends them so that the watcher reconnects.
type fakeScope struct {
	mu       sync.Mutex
	items    []*unstructured.Unstructured
	listErr  error
	watchErr error
	watches  []*watch.FakeWatcher
}

func (s *fakeScope) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	l := &unstructured.UnstructuredList{}
	l.SetResourceVersion("1")
	for _, it := range s.items {
		l.Items = append(l.Items, *it.DeepCopy())
	}
	return l, nil
}

func (s *fakeScope) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchErr != nil {
		return nil, s.watchErr
	}
	w := watch.NewFake()
	s.watches = append(s.watches, w)
	return w, nil
}

func (s *fakeScope) set(fn func(*fakeScope)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *fakeScope) stopWatches() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.watches {
		w.Stop()
	}
	s.watches = nil
}

func notFound(gr schema.GroupResource) error { return apierrors.NewNotFound(gr, "") }

// registration returns a copy of the current registration of gk.
func registration(c *Cluster, gk schema.GroupKind) (apiWatch, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	w := c.apis[gk]
	if w == nil {
		return apiWatch{}, false
	}
	return *w, true
}

// waitApplied waits until the processor has applied every pending sync/refresh operation.
// The processor takes and applies pending items in one write-locked section, so an empty
// pending list means they are visible to readers.
func waitApplied(t *testing.T, c *Cluster) {
	t.Helper()
	c.wakeProcessor()
	eventually(t, 5*time.Second, func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return len(c.pending) == 0
	}, "pending operations applied")
}
